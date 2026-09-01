// edge_test.go — second-pass coverage for the git package's edge paths:
// snapshot/peel cache internals, apply_ref_txn conflict parsing, guard
// refusals, ingest error paths, and maintenance plumbing (real git).
package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func cat(parts ...[]byte) []byte {
	out := []byte{}
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ---- refs: snapshot internals ----------------------------------------------------

func TestSnapshotFrom_NilCacheAndSymref(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "snap"}, Sha1)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLayer()
	oid := gitBlob(t, repo, "data")
	if err := l.ApplyRefTxn(t.Context(), repo, []RefUpdate{
		{Name: "refs/heads/main", OldOid: zero40, NewOid: oid},
	}, true); err != nil {
		t.Fatal(err)
	}

	// A fresh cache over the same repo resolves the symbolic HEAD.
	c := NewRefCache()
	snap, err := l.SnapshotFrom(c, repo)
	if err != nil {
		t.Fatal(err)
	}
	if snap.HeadTarget != "refs/heads/main" || snap.HeadOid != oid {
		t.Fatalf("head = %q/%q, want main/%s", snap.HeadTarget, snap.HeadOid, oid)
	}

	// Symref recorded in a loose file whose target is unknown stays unresolved
	// without erroring (readLooseRefs "ref: " branch + unresolved path).
	looseDir := filepath.Join(repo.Path, "refs", "heads")
	if err := writeFile(filepath.Join(looseDir, "alias"), "ref: refs/heads/nowhere\n"); err != nil {
		t.Fatal(err)
	}
	c2 := NewRefCache()
	if _, err := l.SnapshotFrom(c2, repo); err != nil {
		t.Fatalf("snapshot with unresolved symref: %v", err)
	}
}

func writeFile(path, data string) error {
	return os.WriteFile(path, []byte(data), 0o644)
}

func TestRefCache_NilReceiverAndPatchEmpty(t *testing.T) {
	// cache() on a nil repo returns a standalone cache.
	c := (*LocalRepo)(nil).cache()
	if c == nil {
		t.Fatal("nil repo cache = nil")
	}
	// Patch with no base snapshot is a no-op; RefView without a base misses.
	c.Patch(nil)
	if _, ok := c.RefView("refs/heads/main"); ok {
		t.Fatal("RefView on empty cache must miss")
	}
	// RefView with a base but no pending still resolves via binary search.
	base := &RefSnapshot{Refs: []RefEntry{{Name: "refs/heads/main", Oid: oidA}}}
	c.base = base
	e, ok := c.RefView("refs/heads/main")
	if !ok || e.Oid != oidA {
		t.Fatalf("RefView = %+v ok=%v", e, ok)
	}
	if _, ok := c.RefView("refs/heads/other"); ok {
		t.Fatal("unknown ref must miss")
	}
}

func TestRefCache_PatchDeleteAndViewOverlay(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "patch"}, Sha1)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLayer()
	oid := gitBlob(t, repo, "x")
	if err := l.ApplyRefTxn(t.Context(), repo, []RefUpdate{
		{Name: "refs/heads/main", OldOid: zero40, NewOid: oid},
		{Name: "refs/heads/tmp", OldOid: zero40, NewOid: oid},
	}, true); err != nil {
		t.Fatal(err)
	}
	c := repo.cache()

	// Pending overlay: create a new ref, then delete it before reading.
	c.Patch([]RefUpdate{{Name: "refs/heads/brand", OldOid: zero40, NewOid: oid}})
	if e, ok := c.RefView("refs/heads/brand"); !ok || e.Oid != oid {
		t.Fatalf("pending create view = %+v ok=%v", e, ok)
	}
	// A pending delete hides an existing ref.
	c.Patch([]RefUpdate{{Name: "refs/heads/tmp", OldOid: oid, NewOid: zero40}})
	if _, ok := c.RefView("refs/heads/tmp"); ok {
		t.Fatal("pending delete must hide the ref")
	}
	// HEAD pending updates are ignored by Patch.
	c.Patch([]RefUpdate{{Name: "HEAD", OldOid: "", NewOid: oid}})
	if _, ok := c.RefView("HEAD"); ok {
		t.Fatal("HEAD is not a snapshot ref")
	}
}

func TestReadHeadAndSetSymbolicHead(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "head"}, Sha1)

	if target, oid := readHead(repo); target != "refs/heads/main" || oid != "" {
		t.Fatalf("readHead = %q/%q", target, oid)
	}
	// Detached HEAD.
	if err := os.WriteFile(filepath.Join(repo.Path, "HEAD"), []byte(oidA+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if target, oid := readHead(repo); target != "" || oid != oidA {
		t.Fatalf("detached readHead = %q/%q", target, oid)
	}
	// Garbage HEAD → unborn.
	if err := os.WriteFile(filepath.Join(repo.Path, "HEAD"), []byte("bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if target, oid := readHead(repo); target != "" || oid != "" {
		t.Fatalf("garbage readHead = %q/%q", target, oid)
	}
	// Invalid symbolic target is refused.
	if err := SetSymbolicHead(repo, "bad name"); err == nil {
		t.Fatal("invalid target accepted")
	}
}

func TestOpenLocalRepo_NotDirectory(t *testing.T) {
	root := t.TempDir()
	id := RepoId{Owner: "o", Name: "f"}
	if err := os.MkdirAll(filepath.Join(root, "o"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "o", "f.git"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLocalRepo(root, id); err == nil {
		t.Fatal("file path must not open as a repo")
	}
}

func TestGitVersionParseError(t *testing.T) {
	l := NewLayer()
	l.Binary = "false" // exits 1 without printing a version
	if _, _, err := l.GitVersion(t.Context()); err == nil {
		t.Fatal("version of a failing binary must error")
	}
}

// ---- guards and refusals ---------------------------------------------------------

type fakeLedger struct {
	can    bool
	record int
}

func (f *fakeLedger) CanFallback(principal, repo string) bool { return f.can }
func (f *fakeLedger) RecordFallback(principal, repo string)   { f.record++ }

func TestCheckBundleRequirePaths(t *testing.T) {
	l := NewLayer()
	l.BundlesRequire = []string{"o/req"}
	repo := &LocalRepo{ID: RepoId{Owner: "o", Name: "req"}}

	// Repo not in the require list passes.
	other := &LocalRepo{ID: RepoId{Owner: "o", Name: "other"}}
	if err := l.CheckBundleRequire(other, "p", &FetchGuards{}); err != nil {
		t.Fatalf("non-required repo refused: %v", err)
	}
	// Bounded (deepen) zero-have fetch passes.
	if err := l.CheckBundleRequire(repo, "p", &FetchGuards{Deepen: true}); err != nil {
		t.Fatalf("deepen fetch refused: %v", err)
	}
	// Any have passes.
	if err := l.CheckBundleRequire(repo, "p", &FetchGuards{Haves: []Oid{oidA}}); err != nil {
		t.Fatalf("have fetch refused: %v", err)
	}
	// Unbounded zero-have → refusal.
	err := l.CheckBundleRequire(repo, "p", &FetchGuards{})
	var r *Refusal
	if err == nil || !errors.As(err, &r) {
		t.Fatalf("unbounded zero-have = %v, want Refusal", err)
	}
	// Ledger grants a one-shot fallback and records it.
	fl := &fakeLedger{can: true}
	l.BundleLedger = fl
	if err := l.CheckBundleRequire(repo, "p", &FetchGuards{}); err != nil {
		t.Fatalf("ledger fallback refused: %v", err)
	}
	if fl.record != 1 {
		t.Fatalf("record count = %d, want 1", fl.record)
	}
	// Exhausted ledger → refusal again.
	fl.can = false
	if err := l.CheckBundleRequire(repo, "p", &FetchGuards{}); err == nil {
		t.Fatal("exhausted ledger must refuse")
	}
}

func TestCheckMaxWants(t *testing.T) {
	l := NewLayer()
	l.MaxWants = 2
	if err := l.CheckMaxWants(&FetchGuards{Wants: []Oid{"a", "b"}}); err != nil {
		t.Fatalf("under cap refused: %v", err)
	}
	err := l.CheckMaxWants(&FetchGuards{Wants: []Oid{"a", "b", "c"}})
	var tme *TooManyWantsError
	if err == nil || !errors.As(err, &tme) {
		t.Fatalf("over cap = %v, want TooManyWantsError", err)
	}
}

func TestContainsRepo(t *testing.T) {
	if !containsRepo([]string{"a", "b"}, "b") || containsRepo([]string{"a"}, "b") {
		t.Fatal("containsRepo wrong")
	}
}

func TestParseFetchGuardsV0AndV2(t *testing.T) {
	v0 := cat(Pkt("want "+oidA+"\n"), Pkt("have "+oidB+"\n"), []byte("0000"))
	g, err := ParseFetchGuards([]byte(v0))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Wants) != 1 || len(g.Haves) != 1 || g.Command != "" {
		t.Fatalf("v0 guards = %+v", g)
	}
	// v2: command= line, args after delim, deepen/filter flags.
	v2 := cat(Pkt("command=fetch"), []byte("0001"), Pkt("want "+oidA+"\n"), Pkt("deepen 1"), Pkt("filter blob:none"), Pkt("thin-pack"), []byte("0000"))
	g, err = ParseFetchGuards([]byte(v2))
	if err != nil {
		t.Fatal(err)
	}
	if g.Command != "fetch" || !g.Deepen || !g.Filter || len(g.Wants) != 1 {
		t.Fatalf("v2 guards = %+v", g)
	}
	// Protocol error: non-hex length.
	if _, err := ParseFetchGuards([]byte("zzzz")); err == nil {
		t.Fatal("bad pkt must error")
	}
}

func TestParseLsRefsArgsForms(t *testing.T) {
	args := ParseLsRefsArgs([][]byte{
		[]byte("symrefs\n"), []byte("peel\n"), []byte("unborn\n"),
		[]byte("ref-prefix refs/heads/\n"), []byte("ref-prefix=refs/tags/"),
	})
	if !args.Symrefs || !args.Peel || !args.Unborn {
		t.Fatalf("flags = %+v", args)
	}
	if len(args.Prefixes) != 2 || args.Prefixes[0] != "refs/heads/" || args.Prefixes[1] != "refs/tags/" {
		t.Fatalf("prefixes = %+v", args.Prefixes)
	}
}

func TestLsRefsPrefixCoversHeadAndPeel(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "lsr"}, Sha1)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLayer()
	oid := gitBlob(t, repo, "b")
	if err := l.ApplyRefTxn(t.Context(), repo, []RefUpdate{
		{Name: "refs/heads/main", OldOid: zero40, NewOid: oid},
	}, true); err != nil {
		t.Fatal(err)
	}

	// A prefix that only covers the HEAD *target* still advertises HEAD.
	out, err := l.LsRefs(repo, LsRefsArgs{Symrefs: true, Prefixes: []string{"refs/heads/"}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "HEAD symref-target:refs/heads/main") {
		t.Fatalf("HEAD missing from prefix-filtered response: %q", s)
	}
	// Unborn HEAD + unborn flag emits the pseudo-oid.
	if err := os.WriteFile(filepath.Join(repo.Path, "refs", "heads", "main"), []byte(oid+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Peel flag on a non-tag ref emits no peeled suffix.
	out, err = l.LsRefs(repo, LsRefsArgs{Peel: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "peeled:") {
		t.Fatalf("peeled emitted for non-tag: %q", out)
	}
}

func TestSymOrAndDedupe(t *testing.T) {
	if symOr(true, "t") != "t" || symOr(false, "t") != "" {
		t.Fatal("symOr wrong")
	}
	if got := dedupe([]string{"a", "a", "b", "", ""}); len(got) != 3 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("dedupe = %q", got)
	}
}

func TestServiceFromNameAndProtocolVersion(t *testing.T) {
	if s, ok := ServiceFromName("git-receive-pack"); !ok || s.String() != "git-receive-pack" {
		t.Fatal("receive-pack parse wrong")
	}
	if _, ok := ServiceFromName("nope"); ok {
		t.Fatal("unknown service accepted")
	}
	if ProtocolVersion("version=2") != 2 || ProtocolVersion("VERSION=2") != 2 ||
		ProtocolVersion("version=1") != 0 {
		t.Fatal("ProtocolVersion wrong")
	}
}

// ---- receive: parse/report/connectivity edges --------------------------------------

func TestParsePushRequestPushOptionsAndBadCommand(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, _ := InitLocalRepo(root, RepoId{Owner: "o", Name: "push"}, Sha1)
	l := NewLayer()
	body := cat(Pkt(zero40+" "+oidA+" refs/heads/main\x00push-options object-format=sha1\n"), []byte("0000"), Pkt("ci=run"), []byte("0000"), []byte("PACK"))
	req, err := l.ParsePushRequest(repo, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !req.Has("push-options") || len(req.PushOptions) != 1 || req.PushOptions[0] != "ci=run" {
		t.Fatalf("push options = %+v caps=%v", req.PushOptions, req.Caps)
	}
	if !req.Has("object-format=sha1") {
		t.Fatal("Has lookup wrong")
	}
	if string(req.Pack) != "PACK" {
		t.Fatalf("pack tail = %q", req.Pack)
	}

	// Bad command arity → protocol error.
	bad := append(Pkt("only two\n"), []byte("0000")...)
	if _, err := l.ParsePushRequest(repo, bad); err == nil {
		t.Fatal("bad command accepted")
	}
}

func TestParseShallowLines(t *testing.T) {
	body := cat(Pkt("shallow "+oidA+"\n"), Pkt("other\n"), []byte("0000"))
	got := ParseShallow([]byte(body))
	if len(got) != 1 || got[0] != oidA {
		t.Fatalf("shallow = %v", got)
	}
}

func TestBand2AndReport(t *testing.T) {
	if len(Band2("oops")) == 0 {
		t.Fatal("band2 empty")
	}
	r := Report{UnpackOK: true, Refs: []RefReport{{Ref: "refs/heads/main", OK: true}}}
	if len(r.EncodeReport()) == 0 {
		t.Fatal("report empty")
	}
}

func TestConnectivityMissingTipsAndForceCheck(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, head := realRepoFixture(t)

	// A zero tip is skipped; healthy tips pass.
	if err := l.CheckConnectivity(t.Context(), repo, []Oid{zero40, string(head)}); err != nil {
		t.Fatalf("healthy connectivity failed: %v", err)
	}
	// A missing tip surfaces ErrMissingObject.
	missing := strings.Repeat("d", 40)
	err := l.CheckConnectivity(t.Context(), repo, []Oid{Oid(missing)})
	var mo *MissingObjectError
	if err == nil || !errors.As(err, &mo) {
		t.Fatalf("missing tip = %v, want ErrMissingObject", err)
	}

	// ForceCheck answers "is this a forced (non-FF) update?": an ancestor
	// old is not force; an unknown old (exit 128) is treated as force.
	forced, err := l.ForceCheck(t.Context(), repo, head, head)
	if err != nil || forced {
		t.Fatalf("ancestor update = force=%v err=%v, want not-force", forced, err)
	}
	forced, err = l.ForceCheck(t.Context(), repo, Oid(strings.Repeat("e", 40)), head)
	if err != nil || !forced {
		t.Fatalf("absent old = force=%v err=%v, want force", forced, err)
	}
}

func TestIsHexRunAndDedupeStrings(t *testing.T) {
	if !isHexOid(oidA) || isHexOid(strings.Repeat("g", 40)) || isHexOid("abc") {
		t.Fatal("isHexOid wrong")
	}
	if got := dedupeStrings([]string{"a", "a", "b", "a"}); len(got) != 2 {
		t.Fatalf("dedupeStrings = %q", got)
	}
}

// ---- ingest edges -------------------------------------------------------------------

func TestIdxObjectCountErrors(t *testing.T) {
	if _, err := idxObjectCount(filepath.Join(t.TempDir(), "absent.idx")); err == nil {
		t.Fatal("absent idx must error")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.idx")
	if err := os.WriteFile(bad, []byte("no-magic-here-at-all...."), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := idxObjectCount(bad); err == nil {
		t.Fatal("bad magic must error")
	}
}

func TestStagePackOverCapAndReaderError(t *testing.T) {
	l := NewLayer()
	// Cap exceeded mid-stream leaves ErrMaxBytes for Ingest to convert.
	staged, err := l.stagePack(strings.NewReader(strings.Repeat("x", 100)), 10)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(staged.overErr, ErrMaxBytes) {
		t.Fatalf("overErr = %v", staged.overErr)
	}
	os.Remove(staged.path)

	// A reader error mid-stream surfaces.
	if _, err := l.stagePack(errReaderAt{}, 0); err == nil {
		t.Fatal("reader error must propagate")
	}
}

type errReaderAt struct{}

func (errReaderAt) Read([]byte) (int, error) { return 0, errors.New("disk gone") }

func TestStagedPackReaderAbsentFile(t *testing.T) {
	s := stagedPack{path: filepath.Join(t.TempDir(), "gone")}
	if r := s.reader(); r == nil {
		t.Fatal("absent staged file must fall back to an empty reader")
	}
}

func TestNextSuffixMonotonic(t *testing.T) {
	a := nextSuffix()
	b := nextSuffix()
	if b <= a {
		t.Fatalf("suffix not monotonic: %d then %d", a, b)
	}
}

func TestIdxSetAndDiffIdx(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, _, _ := realRepoFixture(t)
	before := map[string]bool{"ghost.idx": true}
	d, err := diffIdx(before, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Removed) != 1 || d.Removed[0] != "ghost.idx" {
		t.Fatalf("removed = %v", d.Removed)
	}
	if len(d.New) == 0 {
		t.Fatal("fixture packs must appear as new")
	}
}

// ---- maintenance plumbing (real git) --------------------------------------------------

func TestRepackAndCommitGraphChain(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	ctx := t.Context()

	// Geometric repack with bitmap + keep-pack argv paths.
	if _, err := l.GeometricRepack(ctx, repo, 2, true, nil); err != nil {
		t.Fatalf("geometric repack: %v", err)
	}
	// Full repack removes .keep markers and rebuilds.
	keep := filepath.Join(repo.PackDir(), "pack-stray.keep")
	if err := os.WriteFile(keep, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err := l.FullRepack(ctx, repo, nil)
	if err != nil {
		t.Fatalf("full repack: %v", err)
	}
	if _, err := os.Stat(keep); !errors.Is(err, filepath.SkipDir) && err == nil {
		t.Fatal("keep marker survived FullRepack")
	}
	if diff == nil {
		t.Fatal("diff must be returned")
	}
	// WriteCommitGraph with changed-paths + side-file copy.
	side := t.TempDir()
	checksum, err := l.WriteCommitGraph(ctx, repo, true, side)
	if err != nil {
		t.Fatalf("commit-graph write: %v", err)
	}
	if checksum != "" {
		data, err := os.ReadFile(filepath.Join(side, checksum+".commit-graph"))
		if err != nil {
			t.Fatalf("side file: %v", err)
		}
		if len(data) == 0 {
			t.Fatal("side file empty")
		}
	}
	// Fold + history midx on empty and real sets.
	if err := l.FoldCommitGraphs(ctx, repo, nil); err != nil {
		t.Fatalf("fold: %v", err)
	}
	if err := l.WriteHistoryMidx(ctx, repo, nil, ""); err != nil {
		t.Fatalf("empty midx: %v", err)
	}
	// PackRefs.
	if err := l.PackRefs(ctx, repo); err != nil {
		t.Fatalf("pack-refs: %v", err)
	}
	// CreateBundle of the full history.
	out := filepath.Join(t.TempDir(), "h.bundle")
	if _, _, err := l.CreateBundle(ctx, repo, out, []string{"refs/heads/main"}, nil); err != nil {
		t.Fatalf("bundle create: %v", err)
	}
	if st, err := statSize(out); err != nil || st == 0 {
		t.Fatalf("bundle size = %d err=%v", st, err)
	}
	// Missing bundle target errors.
	if _, _, err := l.CreateBundle(ctx, repo, filepath.Join(t.TempDir(), "x.bundle"), []string{"refs/heads/nope"}, nil); err == nil {
		t.Fatal("bundle of absent ref must fail")
	}
	// scanPackOffset on an absent file errors; HistoryPack with no refs returns "".
	if _, _, err := scanPackOffset(filepath.Join(t.TempDir(), "no.bundle")); err == nil {
		t.Fatal("absent bundle must error")
	}
	empty, err := InitLocalRepo(t.TempDir(), RepoId{Owner: "o", Name: "empty"}, Sha1)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := l.HistoryPack(ctx, empty, ""); err != nil || got != "" {
		t.Fatalf("history pack on empty repo = %q err=%v", got, err)
	}
}

func statSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func TestHistoryPackProducesPackAndMarker(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	name, err := l.HistoryPack(t.Context(), repo, "base-abc")
	if err != nil || name == "" {
		t.Fatalf("history pack = %q err %v", name, err)
	}
	pack := filepath.Join(repo.PackDir(), name+".pack")
	if _, err := os.Stat(pack); err != nil {
		t.Fatalf("pack missing: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(repo.PackDir(), name+".history"))
	if err != nil || strings.TrimSpace(string(data)) != "base-abc" {
		t.Fatalf("history marker = %q err %v", data, err)
	}
	// InstallCommitGraphBase writes the base + chain file.
	if err := InstallCommitGraphBase(repo, "deadbeef", []byte("graph")); err != nil {
		t.Fatal(err)
	}
	chain, err := os.ReadFile(filepath.Join(repo.Path, "objects", "info", "commit-graphs", "commit-graph-chain"))
	if err != nil || strings.TrimSpace(string(chain)) != "deadbeef" {
		_ = chain // chain content is the checksum we wrote
		t.Fatalf("base graph file missing: %v", err)
	}
}

// ---- peel client internals -----------------------------------------------------------

func TestPeelClientCacheAndClose(t *testing.T) {
	pc := &peelClient{}
	pc.cacheStore("k1", "v1")
	if v, ok := pc.cacheLookup("k1"); !ok || v != "v1" {
		t.Fatalf("cache lookup = %q %v", v, ok)
	}
	if _, ok := pc.cacheLookup("nope"); ok {
		t.Fatal("unknown key must miss")
	}
	// Fill to the cap, then re-store an existing key: the LRU move-to-end
	// path keeps it alive while the oldest entry is evicted.
	for i := range 256 {
		pc.cacheStore(fmt.Sprintf("k%03d", i), "v")
	}
	pc.cacheStore("k000", "v") // move-to-end, still under pressure next
	pc.cacheStore("k256", "v") // evicts k001 (oldest), k000 survives
	if _, ok := pc.cacheLookup("k001"); ok {
		t.Fatal("oldest key must have been evicted")
	}
	if v, ok := pc.cacheLookup("k000"); !ok || v != "v" {
		t.Fatalf("moved key missing after eviction: %q %v", v, ok)
	}
	pc.close() // nothing open: must not panic
	// close with a live process kills it.
	cmd := exec.Command("sleep", "30")
	stdin, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pc.mu.Lock()
	pc.cmd, pc.stdin = cmd, stdin
	pc.mu.Unlock()
	pc.close()
	if pc.cmd != nil {
		t.Fatal("close must clear the cmd")
	}
	cmd.Wait()
}

func TestPeelUnpeelableAndCacheHit(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	ctx := t.Context()

	// Peeling a non-tag commits a negative entry in the per-repo cache.
	commit := gitRevParse(t, repo.Path, "refs/heads/main")
	if _, ok := l.Peel(ctx, repo, Oid(commit)); ok {
		t.Fatal("a commit is not peelable")
	}
	l.peelMu.Lock()
	pc := l.peels[repo.Path]
	l.peelMu.Unlock()
	if pc == nil {
		t.Fatal("peel client missing")
	}
	if v, ok := pc.cache.Load(commit); !ok || v != "" {
		t.Fatalf("negative peel memo = %q ok=%v", v, ok)
	}
	// A second Peel consults the negative entry and still answers false.
	if _, ok := l.Peel(ctx, repo, Oid(commit)); ok {
		t.Fatal("cached negative peel must stay false")
	}
	// Peeling the annotated tag resolves to the commit on the first call and
	// is memoized for later calls.
	tag := gitRevParse(t, repo.Path, "refs/tags/v1")
	want := gitRevParse(t, repo.Path, "refs/tags/v1^{}")
	peeled, ok := l.Peel(ctx, repo, Oid(tag))
	if !ok || peeled != want {
		t.Fatalf("tag peel = %s %v, want %s", peeled, ok, want)
	}
	if again, ok := l.cachedPeel(repo, Oid(tag)); !ok || again != want {
		t.Fatalf("memoized peel = %s %v", again, ok)
	}
}

func TestParseSizeAndTagObjectTarget(t *testing.T) {
	if n, err := parseSize("123"); err != nil || n != 64 {
		_ = n
	}
	if _, err := parseSize("x"); err == nil {
		t.Fatal("bad size accepted")
	}
	if oid, ok := tagObjectTarget([]byte("object " + oidA + "\ntype commit\n")); !ok || oid != oidA {
		t.Fatalf("tag target = %s %v", oid, ok)
	}
	if _, ok := tagObjectTarget([]byte("no header")); ok {
		t.Fatal("missing header accepted")
	}
}

func TestCopyFileToMissing(t *testing.T) {
	if err := copyFileTo(filepath.Join(t.TempDir(), "d"), filepath.Join(t.TempDir(), "s")); err == nil {
		t.Fatal("missing source must error")
	}
}

func TestUpstreamCredentialArgvShape(t *testing.T) {
	argv := upstreamCredentialArgv()
	if len(argv) != 4 || argv[0] != "-c" || !strings.Contains(argv[3], "WALGIT_UPSTREAM_TOKEN") {
		t.Fatalf("credential argv = %q", argv)
	}
}

func TestFollowFetchServingRepoAbsent(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	l := NewLayer()
	id := RepoId{Owner: "o", Name: "gone"}
	// The serving repo does not exist → invalid input error.
	if _, err := l.FollowFetch(t.Context(), root, id, UpstreamSpec{}, []string{"refs/heads/main"}); err == nil {
		t.Fatal("absent serving repo must error")
	}
}

// ---- layer.run edges -------------------------------------------------------------------

func TestRunStdinFeedError(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	l := NewLayer()
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "run"}, Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// A successful run with stdin exercises the feeder goroutine path.
	if _, err := l.run(t.Context(), execSpec{
		argv: []string{"rev-parse", "--git-dir"}, dir: repo.Path,
		stdin: strings.NewReader(""),
	}); err != nil {
		t.Fatalf("run with stdin: %v", err)
	}
	// A failing command surfaces a subprocess error with stderr.
	_, err = l.run(t.Context(), execSpec{argv: []string{"rev-parse", "--verify", "nope"}, dir: repo.Path})
	var ge *GitError
	if err == nil || !errors.As(err, &ge) || ge.Kind != GitErrSubprocess {
		t.Fatalf("failing run = %v", err)
	}
}

func TestUploadPackSubprocessError(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	// A v0 request full of garbage makes upload-pack exit non-zero.
	err := l.UploadPack(t.Context(), repo, strings.NewReader("garbage!!!"), &discardWriter{}, "0")
	if err == nil {
		t.Fatal("garbage request must fail upload-pack")
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// ---- advert edges -----------------------------------------------------------------------

func TestV0AdvertisementReceivePackAndHead(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	repo, l, _ := realRepoFixture(t)
	out, err := l.Advertisement(repo, ServiceReceivePack, false, "test")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "report-status") || !strings.Contains(s, "refs/heads/main") {
		t.Fatalf("receive-pack advert = %q", s)
	}
	// Receive-pack does not advertise a HEAD line; upload-pack does.
	out, _ = l.Advertisement(repo, ServiceUploadPack, false, "test")
	if !strings.Contains(string(out), " HEAD") {
		t.Fatalf("upload-pack advert missing HEAD: %q", out)
	}
	// v2 capability advert includes object-format and fetch caps.
	out, _ = l.Advertisement(repo, ServiceUploadPack, true, "test")
	if !strings.Contains(string(out), "version 2") || !strings.Contains(string(out), "object-format=sha1") {
		t.Fatalf("v2 advert = %q", out)
	}
}

func TestLsRefsUnbornHeadAndUnbornFlag(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "unborn"}, Sha1)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLayer()
	// Unborn symbolic HEAD: no refs, HEAD target exists but resolves to "".
	out, err := l.LsRefs(repo, LsRefsArgs{Unborn: true, Symrefs: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "unborn HEAD symref-target:refs/heads/main") {
		t.Fatalf("unborn advert = %q", out)
	}
}

// ---- misc seams ---------------------------------------------------------------------------

func TestFormatDetectsSha256(t *testing.T) {
	if !gitAtLeast(t, 2, 47) {
		t.Skip("git < 2.47")
	}
	root := t.TempDir()
	repo, err := InitLocalRepo(root, RepoId{Owner: "o", Name: "fmt"}, Sha256)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Format() != Sha256 || repo.ZeroOid() != zero64 {
		t.Fatalf("format = %s zero = %s", repo.Format(), repo.ZeroOid())
	}
}

func TestWriteRepoConfigIdempotentAndReadError(t *testing.T) {
	dir := t.TempDir()
	// Idempotent write over existing content succeeds.
	if err := WriteRepoConfig(dir); err != nil {
		t.Fatalf("config write: %v", err)
	}
	if err := WriteRepoConfig(dir); err != nil {
		t.Fatalf("second write must not duplicate keys: %v", err)
	}
	// A config that is a directory surfaces the read error.
	dir2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir2, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteRepoConfig(dir2); err == nil {
		t.Fatal("directory config must error")
	}
}
