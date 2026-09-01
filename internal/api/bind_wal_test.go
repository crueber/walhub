package api

// bind_wal_test.go exercises the walView over a real git serving copy plus a
// fake WalEngine: every §9 recipe (resolve/refs/tree/blob/commits/commit/
// summary) runs actual git commands against the materialized bare repo, and
// the manifest/log-backed surfaces (overview/settings/history/pushes) are
// driven through proto fixtures.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// --- fake engine ------------------------------------------------------------------

var _ WalEngine = (*fakeEngine)(nil)

type fakeEngine struct {
	syncCalls []wal.SyncLevel
	syncErr   error
	obj       wal.ObjectAccess
	objErr    error
	rev       uint64
	revErr    error
	man       *proto.Manifest
	manErr    error
	log       []*proto.LogEntry
	logErr    error
	pub       uint64
	pubErr    error
	published []byte
}

func (e *fakeEngine) Sync(_ context.Context, _ git.RepoId, level wal.SyncLevel) error {
	e.syncCalls = append(e.syncCalls, level)
	return e.syncErr
}

func (e *fakeEngine) ObjectAccess(context.Context, git.RepoId) (wal.ObjectAccess, error) {
	return e.obj, e.objErr
}

func (e *fakeEngine) Revision(context.Context, git.RepoId) (uint64, error) { return e.rev, e.revErr }

func (e *fakeEngine) Manifest(context.Context, git.RepoId) (*proto.Manifest, error) {
	return e.man, e.manErr
}

func (e *fakeEngine) ReadLog(context.Context, git.RepoId, uint64, uint64) ([]*proto.LogEntry, error) {
	return e.log, e.logErr
}

func (e *fakeEngine) PublishSettings(_ context.Context, _ git.RepoId, body []byte, _, _ string) (uint64, error) {
	e.published = body
	return e.pub, e.pubErr
}

// --- git fixture ------------------------------------------------------------------

// gfix holds the shas a serving-copy test needs.
type gfix struct {
	base  string         // first commit on main
	main  string         // head of main (the big.dat commit)
	bare  *git.LocalRepo // the serving copy
	bin   string         // the bin.dat commit (main~1)
	other string         // divergent commit on side (sibling of main)
	tag   string         // annotated tag name (targets base)
}

// runGit runs one git command in dir and returns trimmed stdout.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

// commit writes a file and commits it; returns the new head sha.
func commitFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", name)
	runGit(t, dir, "commit", "-m", name)
	return runGit(t, dir, "rev-parse", "HEAD")
}

// newGitFix builds a real bare serving copy: main = base→main, side = base→other
// (divergent), an annotated tag v1 on base, and blobs for the render recipes.
func newGitFix(t *testing.T) gfix {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	barePath := filepath.Join(dir, "walgit.git")
	runGit(t, dir, "init", "-b", "main", work)
	runGit(t, dir, "init", "--bare", "-b", "main", barePath)
	runGit(t, work, "config", "user.email", "t@example.com")
	runGit(t, work, "config", "user.name", "t")
	f := gfix{tag: "v1"}

	f.base = commitFile(t, work, "hello.txt", "hello world\n")
	commitFile(t, work, "docs/README.md", "# Demo\n")
	commitFile(t, work, "hello.txt", "hello again\n")

	// divergent sibling: branched from base, not an ancestor of main
	runGit(t, work, "checkout", "-q", "-b", "side", f.base)
	f.other = commitFile(t, work, "other.txt", "divergent\n")
	runGit(t, work, "checkout", "main")

	// annotated tag (peeled on push) + a binary blob + an oversized blob
	runGit(t, work, "tag", "-a", f.tag, "-m", "one", f.base)
	commitFile(t, work, "bin.dat", "ok\x00nul\n")
	big := make([]byte, 2<<20+17)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	if err := os.WriteFile(filepath.Join(work, "big.dat"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "big.dat")
	runGit(t, work, "commit", "-m", "big")

	runGit(t, work, "push", barePath, "main", "side", f.tag)
	runGit(t, barePath, "pack-refs", "--all")
	f.main = runGit(t, work, "rev-parse", "main")
	f.bin = runGit(t, work, "rev-parse", "main~1")
	f.bare = &git.LocalRepo{Root: dir, ID: git.RepoId{Owner: "demo", Name: "walgit"}, Path: barePath}
	return f
}

func walErrNotFound(detail string) error {
	return &wal.WalError{Kind: wal.WalErrNotFound, Detail: detail}
}

func testManifest() *proto.Manifest {
	return &proto.Manifest{
		Repo: "demo/walgit", HeadSeq: 9, MinSeq: 3, Revision: 42, ObjectFormat: "sha1",
		LogSegments: []*proto.LogSegmentRef{{Key: "log/0000000000000003.pb", FirstSeq: 3, LastSeq: 9}},
		Packs:       []*proto.PackRef{{Checksum: "abc", PackSize: 123}},
		UpdatedAt:   &proto.Timestamp{Seconds: 1700000000},
		Checkpoint:  &proto.CheckpointRef{Seq: 9, Key: "checkpoints/x/checkpoint.pb"},
		Settings: &proto.RepoSettings{
			Toml: "[bundles]\nenabled = true", Revision: 2, Author: "jane", Message: "tune",
			UpdatedAt: &proto.Timestamp{Seconds: 1700000100},
		},
	}
}

func pushEntry(seq uint64, refs ...proto.RefUpdate) *proto.LogEntry {
	txn := &proto.RefTransaction{Atomic: true}
	for _, u := range refs {
		uu := u
		txn.Updates = append(txn.Updates, &uu)
	}
	return &proto.LogEntry{Seq: seq, Kind: proto.EntryKindPush, Txn: txn,
		CreatedAt: &proto.Timestamp{Seconds: 1700000000 + int64(seq)}, Meta: map[string]string{"principal": "jane"}}
}

func settingsEntry(seq uint64, withSettings bool) *proto.LogEntry {
	e := &proto.LogEntry{Seq: seq, Kind: proto.EntryKindSettings, CreatedAt: &proto.Timestamp{Seconds: 1700000000 + int64(seq)}}
	if withSettings {
		e.Settings = &proto.RepoSettings{Revision: seq, Author: "jane", Message: "m", Toml: "[compaction]\ntrigger_count = 9"}
	}
	return e
}

// engineFixture wires Mount over a walView backed by the fake engine + real git.
type engineFixture struct {
	t      *testing.T
	eng    WalEngine
	env    *Env
	mux    *http.ServeMux
	lastID git.RepoId
}

func newEngineFixture(t *testing.T, eng WalEngine) *engineFixture {
	t.Helper()
	reg := &fakeRegistry{repos: map[string][]string{"demo": {"walgit"}}, created: map[string]bool{}}
	cfg := config.Defaults()
	cfg.Server.Auth.Mode = "token"
	cfg.Server.Auth.AnonymousRead = true
	cfg.Git.Binary = "git"
	env := NewEnv(store.NewMemory(), reg, cfg, eng, "test", "host-a")
	f := &engineFixture{t: t, eng: eng, env: env, mux: Mount(env).(*http.ServeMux)}
	return f
}

// do issues a bearer-authorized request against the mounted mux.
func (f *engineFixture) do(method, path string, body ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	var rd *strings.Reader
	if len(body) > 0 {
		rd = strings.NewReader(body[0])
	} else {
		rd = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, rd)
	r = r.WithContext(WithPrincipal(r.Context(), auth.Principal{Name: "jane", Write: true, Admin: true}))
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

// view builds a bare walView over the fixture engine (unit-level access).
func (f *engineFixture) view() *walView {
	return f.env.Repo.(*walView)
}

// --- pending (engine nil) surfaces answer 503 ---------------------------------------

func TestWalViewPending503(t *testing.T) {
	f := newEngineFixture(t, nil)
	if f.env.Repo != nil {
		t.Fatal("nil engine must leave Repo unwired")
	}
	for _, tc := range []struct{ method, path string }{
		{"GET", "/demo/walgit/api"},
		{"GET", "/demo/walgit/api/refs"},
		{"GET", "/demo/walgit/api/refs/branches"},
		{"GET", "/demo/walgit/api/resolve/main"},
		{"GET", "/demo/walgit/api/tree/main"},
		{"GET", "/demo/walgit/api/blob/main/hello.txt"},
		{"GET", "/demo/walgit/api/commits"},
		{"GET", "/demo/walgit/api/commit/abc"},
		{"GET", "/demo/walgit/api/overview"},
		{"GET", "/demo/walgit/api/settings"},
		{"DELETE", "/demo/walgit/api/settings"},
		{"GET", "/demo/walgit/api/settings/history"},
	} {
		w := f.do(tc.method, tc.path)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s = %d, want 503 (body %s)", tc.method, tc.path, w.Code, w.Body.String())
		}
		if ra := w.Header().Get("Retry-After"); ra == "" {
			t.Fatalf("%s %s missing Retry-After", tc.method, tc.path)
		}
	}
	// direct view checks: every entry point answers ErrPending
	v := &walView{}
	ctx := context.Background()
	if _, _, err := v.localView(ctx, repoID(), SyncServe); err != ErrPending {
		t.Fatalf("localView = %v", err)
	}
	if err := v.Sync(ctx, repoID(), SyncRefs); err != ErrPending {
		t.Fatalf("Sync = %v", err)
	}
	if _, err := v.Overview(ctx, repoID()); err != ErrPending {
		t.Fatalf("Overview = %v", err)
	}
	if _, err := v.Settings(ctx, repoID()); err != ErrPending {
		t.Fatalf("Settings = %v", err)
	}
	if _, err := v.PublishSettings(ctx, repoID(), nil, "", ""); err != ErrPending {
		t.Fatalf("PublishSettings = %v", err)
	}
	if _, err := v.HeadSeq(ctx, repoID()); err != ErrPending {
		t.Fatalf("HeadSeq = %v", err)
	}
	if _, err := v.PushHistory(ctx, repoID(), 5); err != ErrPending {
		t.Fatalf("PushHistory = %v", err)
	}
	if _, _, err := v.logEntries(ctx, repoID(), proto.EntryKindSettings); err != ErrPending {
		t.Fatalf("logEntries = %v", err)
	}
}

func repoID() git.RepoId { return git.RepoId{Owner: "demo", Name: "walgit"} }
func repoID2() git.RepoId {
	r := repoID()
	return r
}

// --- NewEnv + enums + conversions -----------------------------------------------------

func TestNewEnvWiring(t *testing.T) {
	eng := &fakeEngine{}
	cfg := config.Defaults()
	env := NewEnv(store.NewMemory(), nil, cfg, eng, "v1", "host-a")
	if env.Repo == nil {
		t.Fatal("engine non-nil must wire Repo")
	}
	v, ok := env.Repo.(*walView)
	if !ok {
		t.Fatalf("Repo is %T", env.Repo)
	}
	if v.binary != "git" {
		t.Fatalf("binary = %q", v.binary)
	}
	if v.engine != eng {
		t.Fatal("engine not wired")
	}
	if env.Version != "v1" || env.Hostname != "host-a" {
		t.Fatalf("env = %+v", env)
	}
	cfg.Git.Binary = "/opt/git"
	env2 := NewEnv(store.NewMemory(), nil, cfg, eng, "v1", "host-a")
	if v2 := env2.Repo.(*walView); v2.binary != "/opt/git" {
		t.Fatalf("custom binary = %q", v2.binary)
	}
}

func TestSyncLevelToWal(t *testing.T) {
	if syncLevelToWal(SyncRefs) != wal.LevelRefs || syncLevelToWal(SyncServe) != wal.LevelServe || syncLevelToWal(SyncFull) != wal.LevelFull {
		t.Fatal("level mapping broken")
	}
	if syncLevelToWal(SyncLevel(99)) != wal.LevelRefs {
		t.Fatal("default must be refs")
	}
	if SyncRefs.String() == "" || SyncServe.String() == "" || SyncFull.String() == "" {
		t.Fatal("String must render")
	}
	if SyncLevel(99).String() == "" {
		t.Fatal("unknown level must still render")
	}
}

func TestTaskFromWalShapes(t *testing.T) {
	// nil progress → omitted; nil log tail → []
	got := TaskRecordFromWal(wal.TaskRecord{ID: "t1", Kind: "gc", Repo: "demo/walgit", Hostname: "h", Started: "s", ElapsedMS: 5})
	if got.ID != "t1" || got.Kind != "gc" || got.Progress != nil || got.LogTail == nil || len(got.LogTail) != 0 {
		t.Fatalf("task = %+v", got)
	}
	// full conversion
	ok := true
	total := uint64(3)
	pct := 75.5
	src := wal.TaskRecord{
		ID: "t2", Kind: "bundle", Repo: "demo/walgit", Hostname: "h", Started: "s", Finished: "f",
		ElapsedMS: 9, OK: &ok, Summary: "done", Params: map[string]string{"k": "v"},
		LogTail:  []string{"a", "b"},
		Progress: &wal.Progress{Kind: "progress", Text: "tx", Label: "lb", Done: 2, Total: &total, Unit: "objs", Percent: &pct},
	}
	got = TaskRecordFromWal(src)
	if got.OK == nil || !*got.OK || len(got.LogTail) != 2 || got.Progress == nil {
		t.Fatalf("task = %+v", got)
	}
	if got.Progress.Kind != "progress" || got.Progress.Total == nil || *got.Progress.Total != 3 || got.Progress.Percent == nil || *got.Progress.Percent != 75.5 {
		t.Fatalf("progress = %+v", got.Progress)
	}
	if ProgressFromWal(wal.Progress{Kind: "notice", Text: "hi"}).Text != "hi" {
		t.Fatal("ProgressFromWal broken")
	}
	// wire shape is JSON-faithful (frozen doc 05 §6.8)
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "kind", "repo", "hostname", "started", "elapsed_ms", "ok", "summary", "progress", "log_tail"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("missing %q: %s", k, b)
		}
	}
}

func TestNotFoundOr(t *testing.T) {
	if err := notFoundOr(walErrNotFound("nope")); err == nil || !strings.Contains(err.Error(), ErrNotFound.Error()) || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("notFoundOr = %v", err)
	}
	plain := context.DeadlineExceeded
	if notFoundOr(plain) != plain {
		t.Fatal("non-wal error must pass through")
	}
}

// --- resolve / head / reflist over the real serving copy ------------------------------

func TestWalResolve(t *testing.T) {
	fix := newGitFix(t)
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 7}
	f := newEngineFixture(t, eng)
	v := f.view()
	ctx := context.Background()
	id := repoID()

	// empty rest → HEAD (default branch)
	res, err := v.Resolve(ctx, id, "")
	if err != nil || res.Ref != "refs/heads/main" || res.SHA != fix.main || res.Kind != "branch" || res.Revision != 7 {
		t.Fatalf("resolve HEAD = %+v, %v", res, err)
	}
	// branch + path
	res, err = v.Resolve(ctx, id, "main/docs/README.md")
	if err != nil || res.Kind != "branch" || res.Path != "docs/README.md" || res.SHA != fix.main {
		t.Fatalf("resolve branch+path = %+v, %v", res, err)
	}
	// tag with peel
	res, err = v.Resolve(ctx, id, "v1")
	if err != nil || res.Kind != "tag" || res.Ref != "refs/tags/v1" || res.SHA != fix.base {
		t.Fatalf("resolve tag = %+v, %v", res, err)
	}
	// branch beats tag at the same length
	res, err = v.Resolve(ctx, id, "main")
	if err != nil || res.Kind != "branch" {
		t.Fatalf("branch-beats-tag = %+v, %v", res, err)
	}
	// raw sha fallback (first segment), rest becomes path
	res, err = v.Resolve(ctx, id, fix.base+"/hello.txt")
	if err != nil || res.Kind != "commit" || res.Ref != "" || res.SHA != fix.base || res.Path != "hello.txt" {
		t.Fatalf("resolve sha = %+v, %v", res, err)
	}
	// unknown rev → ErrNotFound
	if _, err := v.Resolve(ctx, id, "nope-nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("resolve miss = %v", err)
	}
	// unborn HEAD (empty repo)
	empty := &git.LocalRepo{Root: t.TempDir(), ID: id, Path: emptyBare(t)}
	eng.obj = wal.ObjectAccess{Local: empty}
	// unborn HEAD (empty repo): either "unborn HEAD" or "HEAD" not found
	if _, err := v.Resolve(ctx, id, ""); err == nil || !strings.Contains(err.Error(), ErrNotFound.Error()) {
		t.Fatalf("unborn HEAD = %v", err)
	}
}

// emptyBare creates an empty bare repo (unborn HEAD).
func emptyBare(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.git")
	runGit(t, dir, "init", "--bare", "-b", "main", p)
	return p
}

func TestWalHeadAndSummary(t *testing.T) {
	fix := newGitFix(t)
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1}
	f := newEngineFixture(t, eng)
	v := f.view()
	ctx := context.Background()
	id := repoID()

	head, ok, err := v.Head(ctx, id)
	if err != nil || !ok || head.Name != "refs/heads/main" || head.SHA != fix.main {
		t.Fatalf("head = %+v, %v, %v", head, ok, err)
	}
	s, err := v.Summary(ctx, id)
	if err != nil || s.Branches != 2 || s.Tags != 1 {
		t.Fatalf("summary = %+v, %v", s, err)
	}
	if s.Head == nil || s.Head.Name != "refs/heads/main" {
		t.Fatalf("summary head = %+v", s.Head)
	}
	// unborn HEAD → (zero, false)
	empty := &git.LocalRepo{Root: t.TempDir(), ID: id, Path: emptyBare(t)}
	eng.obj = wal.ObjectAccess{Local: empty}
	if h, ok, err := v.Head(ctx, id); err != nil || ok || h.Name != "" {
		t.Fatalf("unborn head = %+v, %v, %v", h, ok, err)
	}
}

func TestWalRefList(t *testing.T) {
	fix := newGitFix(t)
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1}
	f := newEngineFixture(t, eng)
	v := f.view()
	ctx := context.Background()
	id := repoID()

	refs, more, err := v.RefList(ctx, id, "heads", RefQuery{})
	if err != nil || more || len(refs) != 2 || refs[0].Name != "refs/heads/main" || refs[1].Name != "refs/heads/side" {
		t.Fatalf("heads = %+v, more=%v, %v", refs, more, err)
	}
	// tag peel: annotated tag shas are the peeled commit
	tags, _, err := v.RefList(ctx, id, "tags", RefQuery{})
	if err != nil || len(tags) != 1 || tags[0].SHA != fix.base {
		t.Fatalf("tags = %+v, %v", tags, err)
	}
	// query filters (RefQuery.Prefix is directory-style; q matches substrings)
	refs, _, err = v.RefList(ctx, id, "heads", RefQuery{Q: "side"})
	if err != nil || len(refs) != 1 || refs[0].Name != "refs/heads/side" {
		t.Fatalf("q filter = %+v, %v", refs, err)
	}
	refs, _, err = v.RefList(ctx, id, "heads", RefQuery{Q: "MAIN"})
	if err != nil || len(refs) != 1 || refs[0].Name != "refs/heads/main" {
		t.Fatalf("q filter (case-insensitive) = %+v, %v", refs, err)
	}
	// after-pagination: exact fill then one extra scan confirms more
	refs, more, err = v.RefList(ctx, id, "heads", RefQuery{N: 1})
	if err != nil || !more || len(refs) != 1 || refs[0].Name != "refs/heads/main" {
		t.Fatalf("page 1 = %+v more=%v %v", refs, more, err)
	}
	refs, more, err = v.RefList(ctx, id, "heads", RefQuery{N: 2})
	if err != nil || more || len(refs) != 2 {
		t.Fatalf("exact fill = %+v more=%v %v", refs, more, err)
	}
	refs, more, err = v.RefList(ctx, id, "heads", RefQuery{N: 2, After: "refs/heads/main"})
	if err != nil || more || len(refs) != 1 || refs[0].Name != "refs/heads/side" {
		t.Fatalf("after = %+v more=%v %v", refs, more, err)
	}
	// unknown namespace → empty, never nil
	refs, _, err = v.RefList(ctx, id, "notes", RefQuery{})
	if err != nil || refs == nil || len(refs) != 0 {
		t.Fatalf("empty ns = %+v, %v", refs, err)
	}
}

// --- tree / blob / commits / commit over the serving copy ------------------------------

func TestWalTreeAndReadme(t *testing.T) {
	fix := newGitFix(t)
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1}
	f := newEngineFixture(t, eng)
	v := f.view()
	ctx := context.Background()
	id := repoID()
	// root tree: sorted entries, no readme probe at the root
	tr, err := v.Tree(ctx, id, fix.main, "")
	if err != nil || len(tr.Entries) != 4 || tr.Readme != nil || tr.Path != "" {
		t.Fatalf("root tree = %+v, %v", tr, err)
	}
	// subtree with README probe
	tr, err = v.Tree(ctx, id, fix.main, "docs")
	if err != nil || len(tr.Entries) != 1 || tr.Path != "docs" {
		t.Fatalf("docs tree = %+v, %v", tr, err)
	}
	if tr.Readme == nil || tr.Readme.Name != "README.md" || tr.Readme.Contents != "# Demo\n" {
		t.Fatalf("readme = %+v", tr.Readme)
	}
	if _, err := v.Tree(ctx, id, fix.main, "absent/dir"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing tree = %v", err)
	}
	// bad sha → 404
	if _, err := v.Tree(ctx, id, "deadbeef", ""); err == nil {
		t.Fatal("bad sha must 404")
	}
}

func TestWalBlobShapes(t *testing.T) {
	fix := newGitFix(t)
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1}
	f := newEngineFixture(t, eng)
	v := f.view()
	ctx := context.Background()
	id := repoID()

	b, err := v.Blob(ctx, id, fix.main, "hello.txt", false)
	if err != nil || !bytes.Equal(b.Contents, []byte("hello again\n")) || b.Binary || b.TooLarge || b.Size != 12 {
		t.Fatalf("text blob = %+v, %v", b, err)
	}
	bin, err := v.Blob(ctx, id, fix.main, "bin.dat", false)
	if err != nil || !bin.Binary {
		t.Fatalf("binary blob = %+v, %v", bin, err)
	}
	// oversized JSON render → TooLarge, no contents
	big, err := v.Blob(ctx, id, fix.main, "big.dat", false)
	if err != nil || !big.TooLarge || big.Contents != nil || big.Size != int64(2<<20)+17 {
		t.Fatalf("big blob = size %d tooLarge %v err %v", big.Size, big.TooLarge, err)
	}
	// raw download bypasses the cap
	raw, err := v.Blob(ctx, id, fix.main, "big.dat", true)
	if err != nil || raw.TooLarge || len(raw.Contents) != 2<<20+17 {
		t.Fatalf("raw blob = %d bytes, tooLarge %v, err %v", len(raw.Contents), raw.TooLarge, err)
	}
	// missing blob → 404
	if _, err := v.Blob(ctx, id, fix.main, "absent.txt", false); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing blob = %v", err)
	}
}

func TestWalCommits(t *testing.T) {
	fix := newGitFix(t)
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1}
	f := newEngineFixture(t, eng)
	v := f.view()
	ctx := context.Background()
	id := repoID()
	// all commits on main (5: base, docs, hello-again, bin.dat, big.dat)
	page, err := v.Commits(ctx, id, fix.main, "", 0, 0)
	if err != nil || page.More || len(page.Commits) != 5 {
		t.Fatalf("page = %d commits, more=%v, %v", len(page.Commits), page.More, err)
	}
	if page.Commits[0].SHA != fix.main || page.Commits[4].SHA != fix.base {
		t.Fatalf("order = %v..%v", page.Commits[0].SHA, page.Commits[4].SHA)
	}
	// n=2 → first page + more
	page, err = v.Commits(ctx, id, fix.main, "", 0, 2)
	if err != nil || !page.More || len(page.Commits) != 2 {
		t.Fatalf("page n=2 = %d, more=%v, %v", len(page.Commits), page.More, err)
	}
	// skip past the first
	page, err = v.Commits(ctx, id, fix.main, "", 1, 10)
	if err != nil || len(page.Commits) != 4 || page.Commits[3].SHA != fix.base {
		t.Fatalf("skip = %+v, %v", page, err)
	}
	// path filter keeps only commits touching docs/README.md
	page, err = v.Commits(ctx, id, fix.main, "docs/README.md", 0, 10)
	if err != nil || len(page.Commits) != 1 {
		t.Fatalf("path filter = %+v, %v", page, err)
	}
	if _, err := v.Commits(ctx, id, "nope", "", 0, 10); err == nil {
		t.Fatal("bad sha must 404")
	}

	detail, err := v.Commit(ctx, id, fix.main)
	if err != nil || detail.Commit.SHA != fix.main || detail.Patch == "" || len(detail.Stats) == 0 {
		t.Fatalf("detail = %+v stats %d, %v", detail.Commit.SHA, len(detail.Stats), err)
	}
	if _, err := v.Commit(ctx, id, "nope"); err == nil {
		t.Fatal("bad sha commit must 404")
	}
}

// --- manifest-backed surfaces -----------------------------------------------------------

func TestWalOverview(t *testing.T) {
	fix := newGitFix(t)
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1, man: testManifest()}
	f := newEngineFixture(t, eng)
	v := f.view()
	ctx := context.Background()
	id := repoID()

	ov, err := v.Overview(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Health.Status != "ok" || ov.Manifest.Version != 42 || ov.Manifest.NextSeq != 10 || ov.Manifest.MinSeq != 3 || ov.Manifest.Entries != 7 {
		t.Fatalf("manifest block = %+v", ov.Manifest)
	}
	if len(ov.Manifest.Segments) != 1 || ov.Manifest.Segments[0] != "log/0000000000000003.pb" {
		t.Fatalf("segments = %v", ov.Manifest.Segments)
	}
	if ov.Packs.Live != 1 || ov.Packs.LiveBytes != 123 || ov.Packs.Pushes != 1 {
		t.Fatalf("packs = %+v", ov.Packs)
	}
	if ov.Manifest.LastPush == nil || ov.Manifest.Checkpoint == nil || ov.Manifest.Checkpoint.(map[string]any)["seq"] != uint64(9) {
		t.Fatalf("checkpoint/push = %+v %+v", ov.Manifest.LastPush, ov.Manifest.Checkpoint)
	}
	if ov.Bundles == nil || ov.BundlePlan.Slots == nil || ov.Compactions == nil || ov.Node.Counters == nil {
		t.Fatalf("empty arrays must be non-nil: %+v", ov)
	}
	// nil optional fields
	eng.man = &proto.Manifest{Repo: "demo/walgit", HeadSeq: 0, MinSeq: 0}
	ov, err = v.Overview(ctx, id)
	if err != nil || ov.Manifest.LastPush != nil || ov.Manifest.Checkpoint != nil {
		t.Fatalf("minimal manifest = %+v, %v", ov.Manifest, err)
	}
	// manifest 404 mapping
	eng.man = nil
	eng.manErr = walErrNotFound("gone")
	if _, err := v.Overview(ctx, id); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("overview 404 = %v", err)
	}
}

func TestWalSettingsAndHistory(t *testing.T) {
	fix := newGitFix(t)
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1, man: testManifest()}
	f := newEngineFixture(t, eng)
	v := f.view()
	ctx := context.Background()
	id := repoID()

	doc, err := v.Settings(ctx, id)
	if err != nil || doc.Revision != 2 || doc.Author != "jane" || doc.Message != "tune" || doc.TOML != "[bundles]\nenabled = true" {
		t.Fatalf("settings doc = %+v, %v", doc, err)
	}
	if doc.UpdatedAt.IsZero() {
		t.Fatal("updated_at must convert")
	}
	// never published → zero doc, nil error
	eng.man = &proto.Manifest{Repo: "demo/walgit"}
	doc, err = v.Settings(ctx, id)
	if err != nil || doc.Revision != 0 {
		t.Fatalf("empty settings = %+v, %v", doc, err)
	}
	// publish passthrough
	rev, err := v.PublishSettings(ctx, id, []byte("[compaction]\ntrigger_count = 3"), "msg", "jane")
	if err != nil || rev != 0 || string(eng.published) != "[compaction]\ntrigger_count = 3" {
		t.Fatalf("publish = %d, %v, sent %q", rev, err, eng.published)
	}
	// headseq
	eng.man = testManifest()
	seq, err := v.HeadSeq(ctx, id)
	if err != nil || seq != 9 {
		t.Fatalf("headseq = %d, %v", seq, err)
	}

	// settings history filters the log
	eng.log = []*proto.LogEntry{
		pushEntry(3, proto.RefUpdate{Name: "refs/heads/main", OldOid: strings.Repeat("0", 40), NewOid: fix.base}),
		settingsEntry(4, true),
		settingsEntry(5, false),
	}
	h, err := v.SettingsHistory(ctx, id)
	if err != nil || h.MinSeq != 3 || len(h.Entries) != 2 {
		t.Fatalf("history = %+v, %v", h, err)
	}
	if h.Entries[0].Seq != 4 || h.Entries[0].Revision != 4 || h.Entries[0].Author != "jane" {
		t.Fatalf("entry 0 = %+v", h.Entries[0])
	}
	if h.Entries[1].Seq != 5 || h.Entries[1].Revision != 0 {
		t.Fatalf("entry without payload = %+v", h.Entries[1])
	}
	// empty log → empty history, minseq 0
	eng.man = &proto.Manifest{Repo: "demo/walgit"}
	h, err = v.SettingsHistory(ctx, id)
	if err != nil || h.MinSeq != 0 || len(h.Entries) != 0 || h.Entries == nil {
		t.Fatalf("empty history = %+v, %v", h, err)
	}
}

func TestWalPushHistoryAndForce(t *testing.T) {
	fix := newGitFix(t)
	zero := strings.Repeat("0", 40)
	eng := &fakeEngine{
		obj: wal.ObjectAccess{Local: fix.bare}, rev: 1,
		man: &proto.Manifest{Repo: "demo/walgit", HeadSeq: 9, MinSeq: 3},
	}
	f := newEngineFixture(t, eng)
	v := f.view()
	ctx := context.Background()
	id := repoID()

	eng.log = []*proto.LogEntry{
		pushEntry(3, proto.RefUpdate{Name: "refs/heads/main", OldOid: zero, NewOid: fix.base}),
		{Seq: 4, Kind: proto.EntryKindSettings, Settings: &proto.RepoSettings{Revision: 1}}, // filtered by kind
		{Seq: 5, Kind: proto.EntryKindPush, Txn: nil},                                       // filtered: no txn
		pushEntry(6, proto.RefUpdate{Name: "refs/heads/main", OldOid: fix.base, NewOid: fix.main}),
		pushEntry(7, proto.RefUpdate{Name: "refs/heads/side", OldOid: fix.main, NewOid: fix.other}),
	}
	recs, err := v.PushHistory(ctx, id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("records = %+v", recs)
	}
	if recs[0].Seq != 3 || len(recs[0].Refs) != 1 || recs[0].Refs[0].Force {
		t.Fatalf("create record = %+v (create is never force)", recs[0])
	}
	if recs[0].Principal != "jane" || recs[0].At.IsZero() || !recs[0].Atomic {
		t.Fatalf("meta = %+v", recs[0])
	}
	if recs[1].Seq != 6 || recs[1].Refs[0].Force {
		t.Fatalf("fast-forward must not be force: %+v", recs[1])
	}
	if !recs[2].Refs[0].Force {
		t.Fatalf("divergent update must be force: %+v", recs[2])
	}
	// last window: head 9, last 3 → from = 7
	eng.log = []*proto.LogEntry{pushEntry(7, proto.RefUpdate{Name: "refs/heads/main", OldOid: zero, NewOid: fix.base})}
	recs, err = v.PushHistory(ctx, id, 3)
	if err != nil || len(recs) != 1 || recs[0].Seq != 7 {
		t.Fatalf("window = %+v, %v", recs, err)
	}
	// last <= 0 → default 10
	eng.log = nil
	if recs, err := v.PushHistory(ctx, id, 0); err != nil || recs == nil || len(recs) != 0 {
		t.Fatalf("default last = %+v, %v", recs, err)
	}
	// head 0 → no log read
	eng.man = &proto.Manifest{Repo: "demo/walgit", HeadSeq: 0, MinSeq: 0}
	if recs, err := v.PushHistory(ctx, id, 10); err != nil || len(recs) != 0 {
		t.Fatalf("empty repo = %+v, %v", recs, err)
	}
}

// --- error plumbing through the handlers ------------------------------------------------

func TestWalViewErrorMapping(t *testing.T) {
	fix := newGitFix(t)
	ctx := context.Background()
	id := repoID()

	t.Run("sync errors", func(t *testing.T) {
		eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1}
		f := newEngineFixture(t, eng)
		v := f.view()

		eng.syncErr = walErrNotFound("gone")
		if err := v.Sync(ctx, id, SyncRefs); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("sync 404 = %v", err)
		}
		if _, _, err := v.localView(ctx, id, SyncServe); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("localView 404 = %v", err)
		}
		eng.syncErr = context.DeadlineExceeded
		if err := v.Sync(ctx, id, SyncRefs); err != context.DeadlineExceeded {
			t.Fatalf("sync passthrough = %v", err)
		}
		// level plumbing: serve syncs at serve level, full at full
		eng.syncErr = nil
		eng.syncCalls = nil
		if _, _, err := v.localView(ctx, id, SyncServe); err != nil || len(eng.syncCalls) != 1 || eng.syncCalls[0] != wal.LevelServe {
			t.Fatalf("serve level = %v %v", eng.syncCalls, err)
		}
		if _, _, err := v.localView(ctx, id, SyncFull); err != nil || len(eng.syncCalls) != 2 || eng.syncCalls[1] != wal.LevelFull {
			t.Fatalf("full level = %v %v", eng.syncCalls, err)
		}
	})

	t.Run("object access errors", func(t *testing.T) {
		eng := &fakeEngine{objErr: walErrNotFound("no access")}
		f := newEngineFixture(t, eng)
		if _, _, err := f.view().localView(ctx, id, SyncServe); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("obj err = %v", err)
		}
		eng.objErr = context.DeadlineExceeded
		if _, _, err := f.view().localView(ctx, id, SyncServe); err != context.DeadlineExceeded {
			t.Fatalf("obj passthrough = %v", err)
		}
	})

	t.Run("remote served", func(t *testing.T) {
		eng := &fakeEngine{obj: wal.ObjectAccess{Remote: &wal.RemoteReader{}}, rev: 1}
		f := newEngineFixture(t, eng)
		if _, _, err := f.view().localView(ctx, id, SyncServe); err != errRemoteServed {
			t.Fatalf("remote = %v", err)
		}
		w := f.do("GET", "/demo/walgit/api/tree/main")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("remote tree = %d, want 503", w.Code)
		}
	})

	t.Run("revision error", func(t *testing.T) {
		eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, revErr: context.DeadlineExceeded}
		f := newEngineFixture(t, eng)
		if _, _, err := f.view().localView(ctx, id, SyncServe); err != context.DeadlineExceeded {
			t.Fatalf("rev err = %v", err)
		}
	})
	t.Run("manifest + log errors", func(t *testing.T) {
		eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1}
		f := newEngineFixture(t, eng)
		v := f.view()
		eng.manErr = walErrNotFound("no manifest")
		if _, err := v.HeadSeq(ctx, id); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("headseq 404 = %v", err)
		}
		eng.pubErr = context.DeadlineExceeded
		if _, err := v.PublishSettings(ctx, id, nil, "", ""); err != context.DeadlineExceeded {
			t.Fatal("publish error passthrough expected")
		}
		eng.manErr = context.DeadlineExceeded
		if _, err := v.Settings(ctx, id); err != context.DeadlineExceeded {
			t.Fatalf("settings passthrough = %v", err)
		}
		eng.manErr = nil
		eng.man = &proto.Manifest{Repo: "demo/walgit", HeadSeq: 9, MinSeq: 3}
		eng.logErr = walErrNotFound("log gone")
		if _, err := v.PushHistory(ctx, id, 5); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("log 404 = %v", err)
		}
		if _, err := v.SettingsHistory(ctx, id); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("settings history 404 = %v", err)
		}
		// deriveForce surfaces a sync failure (needs a record to derive on)
		eng.logErr = nil
		eng.log = []*proto.LogEntry{pushEntry(6, proto.RefUpdate{Name: "refs/heads/main", OldOid: fix.base, NewOid: fix.main})}
		eng.syncErr = walErrNotFound("serve gone")
		recs, err := v.PushHistory(ctx, id, 10)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("deriveForce err = %v recs %d", err, len(recs))
		}
		eng.syncErr = nil
	})

	t.Run("happy handler paths", func(t *testing.T) {
		eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 1, man: testManifest()}
		eng.log = []*proto.LogEntry{
			pushEntry(3, proto.RefUpdate{Name: "refs/heads/main", OldOid: fix.base, NewOid: fix.main}),
			settingsEntry(4, true),
		}
		f := newEngineFixture(t, eng)
		w := f.do("GET", "/demo/walgit/api/overview")
		if w.Code != 200 {
			t.Fatalf("overview = %d body=%s", w.Code, w.Body.String())
		}
		var ov OverviewData
		if err := json.Unmarshal(w.Body.Bytes(), &ov); err != nil || ov.Manifest.Version != 42 {
			t.Fatalf("overview body = %s (%v)", w.Body.String(), err)
		}
		if w := f.do("GET", "/demo/walgit/api/settings"); w.Code != 200 {
			t.Fatalf("settings = %d", w.Code)
		}
		if w := f.do("GET", "/demo/walgit/api/settings/history"); w.Code != 200 {
			t.Fatalf("history = %d", w.Code)
		}
		if w := f.do("POST", "/demo/walgit/api/settings/validate", "{}"); w.Code != 200 {
			t.Fatalf("validate = %d body=%s", w.Code, w.Body.String())
		}
		// dry-run exercises PushHistory through the handler
		body := allowAllPolicy
		w = f.do("POST", "/demo/walgit/api/policy/dry-run", body)
		if w.Code != 200 {
			t.Fatalf("dry-run = %d body=%s", w.Code, w.Body.String())
		}
	})
}
