// cover_test.go — gap-closing units for the ≥95% gate (deterministic,
// no network): error branches, fallback paths, and direct shape tests.
package repoimport

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// --- constructor matrix ----------------------------------------------------------------

func TestNewMatrix(t *testing.T) {
	st := store.NewMemory()
	s := New(Deps{})
	if s.cfg == nil || s.git.Binary != "git" || s.hostname != "unknown" {
		t.Fatalf("zero deps must fall back: %+v", s)
	}
	if cap(s.clones) != 2 {
		t.Fatalf("default max_concurrent = %d, want 2", cap(s.clones))
	}
	cfg := config.Defaults()
	cfg.Import.MaxConcurrent = 0 // invalid-but-constructed: clamped, never zero
	s2 := New(Deps{Store: st, Cfg: cfg, GitBinary: "git-x", Hostname: "h"})
	if cap(s2.clones) != 1 {
		t.Fatalf("clamped max_concurrent = %d, want 1", cap(s2.clones))
	}
	r := newRunner("", cfg)
	if r.CloneTimeout != 1800*time.Second || r.GitTimeout != 300*time.Second {
		t.Fatalf("zero timeouts must default: %+v", r)
	}
	r2 := newRunner("git", &config.Config{})
	if r2.CacheDir == "" {
		t.Fatalf("empty cache dir must fall back to TempDir")
	}
	if pathEnv() == "" {
		t.Fatalf("pathEnv must never be empty")
	}
}

// --- pool / runner units -----------------------------------------------------------------

func TestPoolCancel(t *testing.T) {
	p := newPool(1)
	p.sem <- struct{}{} // saturate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.run(ctx, func() error { return nil }); err == nil {
		t.Fatalf("saturated pool + canceled ctx must fail")
	}
}

func TestScratchDirError(t *testing.T) {
	cfg := config.Defaults()
	cfg.Cache.Dir = filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(cfg.Cache.Dir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newRunner("git", cfg)
	if _, err := r.ScratchDir("a", "b"); err == nil {
		t.Fatalf("scratch under a file must fail")
	}
}

func TestForEachRefError(t *testing.T) {
	r := newRunner("git", config.Defaults())
	if _, err := r.ForEachRef(context.Background(), t.TempDir()); err == nil {
		t.Fatalf("for-each-ref outside a repo must fail")
	}
	if _, err := r.ShowObjectFormat(context.Background(), t.TempDir()); err == nil {
		t.Fatalf("rev-parse outside a repo must fail")
	}
}

func TestHeadTargetShapes(t *testing.T) {
	dir := t.TempDir()
	if got := HeadTarget(dir); got != "" {
		t.Fatalf("missing HEAD = %q, want empty", got)
	}
	writeFile(t, dir+"/HEAD", "deadbeef\n")
	if got := HeadTarget(dir); got != "" {
		t.Fatalf("detached HEAD = %q, want empty", got)
	}
	writeFile(t, dir+"/HEAD", "ref: refs/heads/main\n")
	if got := HeadTarget(dir); got != "refs/heads/main" {
		t.Fatalf("symref HEAD = %q", got)
	}
}

func TestPackTrailerShapes(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short.pack")
	writeFile(t, short, "tiny")
	if _, err := packTrailerChecksum(short, "sha1"); err == nil {
		t.Fatalf("short file must fail")
	}
	if _, err := packTrailerChecksum(filepath.Join(dir, "missing.pack"), "sha1"); err == nil {
		t.Fatalf("missing file must fail")
	}
	full := filepath.Join(dir, "full.pack")
	raw := append([]byte("PACKdata"), make([]byte, 32)...)
	for i := range raw[len(raw)-32:] {
		raw[len(raw)-32+i] = byte(i)
	}
	if err := os.WriteFile(full, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := packTrailerChecksum(full, "sha256")
	if err != nil || len(got) != 64 {
		t.Fatalf("sha256 trailer = %q err=%v", got, err)
	}
	got1, err := packTrailerChecksum(full, "sha1")
	if err != nil || len(got1) != 40 {
		t.Fatalf("sha1 trailer = %q err=%v", got1, err)
	}
}

func TestEnsurePackIdxRegenerates(t *testing.T) {
	remote := t.TempDir() + "/src"
	fixtureRepo(t, remote, 2, 0, 0)
	repackSingle(t, remote)
	packs, _ := filepath.Glob(filepath.Join(remote, ".git", "objects", "pack", "*.pack"))
	if len(packs) == 0 {
		t.Fatalf("no fixture packs")
	}
	// Copy the .pack ALONE (no .idx) → regeneration path.
	lonely := filepath.Join(t.TempDir(), "lonely.pack")
	raw, err := os.ReadFile(packs[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lonely, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	r := newRunner("git", config.Defaults())
	idx, err := r.EnsurePackIdx(context.Background(), lonely)
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if _, err := os.Stat(idx); err != nil {
		t.Fatalf("idx not produced: %v", err)
	}
	// Present idx → returned as-is.
	if idx2, err := r.EnsurePackIdx(context.Background(), lonely); err != nil || idx2 != idx {
		t.Fatalf("present idx = %q err=%v", idx2, err)
	}
	// Garbage pack → generation fails.
	bad := filepath.Join(t.TempDir(), "bad.pack")
	writeFile(t, bad, "not a pack file at all, far too short for anything")
	if _, err := r.EnsurePackIdx(context.Background(), bad); err == nil {
		t.Fatalf("garbage pack must fail")
	}
}

func TestDirSizeError(t *testing.T) {
	if _, err := dirSize(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatalf("missing dir must fail")
	}
}

func TestResolveHostLocalhost(t *testing.T) {
	ips, err := resolveHost("localhost")
	if err != nil || len(ips) == 0 {
		t.Fatalf("localhost must resolve without network: %v %v", ips, err)
	}
	if err := func() error {
		// checkPrivate with nil resolver → resolveHost path (localhost is loopback).
		return checkPrivate("localhost", false, nil)
	}(); err == nil || !strings.Contains(err.Error(), "private address") {
		t.Fatalf("localhost must trip deny-private: %v", err)
	}
}

func TestIsPrivateIPShapes(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		priv bool
	}{
		{"127.0.0.1", true}, {"::1", true},
		{"10.0.0.1", true}, {"172.16.0.1", true}, {"172.31.255.255", true},
		{"172.15.0.1", false}, {"172.32.0.1", false},
		{"192.168.1.1", true}, {"0.1.2.3", true},
		{"100.64.0.1", true}, {"100.127.255.255", true}, {"100.128.0.1", false},
		{"192.0.0.1", true}, {"192.0.1.1", false},
		{"169.254.1.1", true}, {"224.0.0.1", true},
		{"fd00::1", true}, {"fc12::9", true}, {"fe80::1", true},
		{"93.184.216.34", false}, {"2606:2800:220:1:248:1893:25c8:1946", false},
		{"8.8.8.8", false},
	} {
		if got := isPrivateIP(net.ParseIP(tc.ip)); got != tc.priv {
			t.Fatalf("isPrivateIP(%s) = %v, want %v", tc.ip, got, tc.priv)
		}
	}
}

// --- doc units -------------------------------------------------------------------------------

func TestWriteImportDocRaces(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	ttl := time.Hour
	mkClaim := func(src string) *ImportDoc {
		return &ImportDoc{Version: 1, SourceURL: src, SourceKind: "file", RequestedRefs: []string{}, HeadSHAs: map[string]string{}, Importer: "x", Format: "sha1", ImportedAt: nowRFC3339(), ClaimExpiresAt: time.Now().UTC().Add(ttl).Format(time.RFC3339)}
	}
	// Fresh claim.
	mode, _, ver, err := claimImportDoc(ctx, st, "acme", "r", mkClaim("file:///a.git"))
	if err != nil || mode != claimFresh || ver == "" {
		t.Fatalf("fresh = %v %q %v", mode, ver, err)
	}
	// Same source, in-progress → resume (different importer shares it).
	mode, _, _, err = claimImportDoc(ctx, st, "acme", "r", mkClaim("file:///a.git"))
	if err != nil || mode != claimResume {
		t.Fatalf("same-source in-progress = %v %v, want resume", mode, err)
	}
	// Different source, live claim, no manifest → 409, never adopt.
	if _, _, _, err := claimImportDoc(ctx, st, "acme", "r", mkClaim("file:///b.git")); statusCode(err) != 409 {
		t.Fatalf("foreign in-progress = %v, want 409", err)
	}
	// Complete the claim, then same source with no manifest (deleted
	// repo) → resume, not a success-over-absence adopt.
	doc := mkClaim("file:///a.git")
	doc.Complete = true
	if _, err := completeImportDoc(ctx, st, "acme", "r", doc, ver); err != nil {
		t.Fatalf("complete: %v", err)
	}
	mode, _, _, err = claimImportDoc(ctx, st, "acme", "r", mkClaim("file:///a.git"))
	if err != nil || mode != claimResume {
		t.Fatalf("complete without manifest = %v %v, want resume", mode, err)
	}
	// With the manifest present → adopt.
	if _, err := store.PutBytes(ctx, st, store.RepoPrefix("acme", "r")+store.Manifest, []byte("m"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	mode, landed, _, err := claimImportDoc(ctx, st, "acme", "r", mkClaim("file:///a.git"))
	if err != nil || mode != claimAdopt || landed == nil || !landed.Complete {
		t.Fatalf("adopt = %v %+v %v, want adopt+complete", mode, landed, err)
	}
	// Different source vs complete → 409, never adopt.
	if _, _, _, err := claimImportDoc(ctx, st, "acme", "r", mkClaim("file:///b.git")); statusCode(err) != 409 {
		t.Fatalf("foreign complete = %v, want 409", err)
	}
	// Corrupt existing → read error path.
	if _, err := store.PutBytes(ctx, st, importKey("acme", "corrupt"), []byte("{nope"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readImportDoc(ctx, st, "acme", "corrupt"); err == nil {
		t.Fatalf("corrupt doc must fail")
	}
	// Nil-map normalization on read.
	raw := []byte(`{"version":1,"source_url":"file:///a.git","source_kind":"file","imported_at":"2026-09-04T00:00:00Z","importer":"x","format":"sha1"}`)
	if _, err := store.PutBytes(ctx, st, importKey("acme", "bare"), raw, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	doc, _, err = readImportDoc(ctx, st, "acme", "bare")
	if err != nil || doc.RequestedRefs == nil || doc.HeadSHAs == nil {
		t.Fatalf("read must non-nil maps: %+v %v", doc, err)
	}
}

func TestEnsureImporterAdminUnits(t *testing.T) {
	ctx := context.Background()
	// Nil roles → backstop no-op.
	if err := ensureImporterAdmin(ctx, nil, "a", "b", "x@y.z"); err != nil {
		t.Fatalf("nil roles: %v", err)
	}
	importerBackstop(ctx, nil, "a", "b")
	// Non-email importer → skip (bootstrap covers).
	roles := &FakeRoles{}
	if err := ensureImporterAdmin(ctx, roles, "acme", "r", "anon"); err != nil {
		t.Fatalf("non-email: %v", err)
	}
	if len(roles.Bindings) != 0 {
		t.Fatalf("non-email must not bind: %v", roles.Bindings)
	}
	// Email importer → bound; second call is a no-op.
	if err := ensureImporterAdmin(ctx, roles, "acme", "r", "Zed@Example.COM"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	got := roles.Bindings["acme/r"]
	if len(got) != 1 { // non-email owner ⇒ no owner binding; importer only
		t.Fatalf("bindings = %+v", got)
	}
	found := false
	for _, b := range got {
		if b.Subject == "user:zed@example.com" && b.Role == identity.RoleAdmin {
			found = true
		}
	}
	if !found {
		t.Fatalf("normalized importer binding missing: %+v", got)
	}
	if err := ensureImporterAdmin(ctx, roles, "acme", "r", "zed@example.com"); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if n := len(roles.Bindings["acme/r"]); n != 1 {
		t.Fatalf("re-run duplicated bindings: %d", n)
	}
	// GetAccess error → 500.
	if err := ensureImporterAdmin(ctx, &errRoles{err: errors.New("boom")}, "a", "b", "x@y.z"); err == nil {
		t.Fatalf("backend error must fail")
	}
}

type errRoles struct {
	FakeRoles
	err error
}

func (e *errRoles) GetAccess(_ context.Context, _, _ string) (*identity.AccessDoc, store.Version, error) {
	return nil, "", e.err
}

// --- http units ----------------------------------------------------------------------------------

type noFlushWriter struct {
	h http.Header
	b []byte
	c int
}

func (n *noFlushWriter) Header() http.Header {
	if n.h == nil {
		n.h = http.Header{}
	}
	return n.h
}
func (n *noFlushWriter) Write(p []byte) (int, error) { n.b = append(n.b, p...); return len(p), nil }
func (n *noFlushWriter) WriteHeader(c int)           { n.c = c }

func TestWriteAuthErrMatrix(t *testing.T) {
	h := &Handler{}
	mkReq := func() *http.Request { return httptest.NewRequest(http.MethodPost, "/api/v1/repos/imports", nil) }
	for _, tc := range []struct {
		kind auth.AuthErrorKind
		want int
		hdr  string
	}{
		{auth.ErrInvalid, 401, `Bearer realm="walgit"`},
		{auth.ErrUnauthorized, 401, `Bearer realm="walgit"`},
		{auth.ErrForbidden, 403, ""},
		{auth.ErrUnavailable, 503, ""},
	} {
		t.Run("", func(t *testing.T) {
			h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
				return auth.Principal{}, &auth.AuthError{Kind: tc.kind, Why: "nope"}
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, mkReq())
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			if tc.hdr != "" && w.Header().Get("WWW-Authenticate") != tc.hdr {
				t.Fatalf("auth header = %q", w.Header().Get("WWW-Authenticate"))
			}
			if tc.want == 503 && w.Header().Get("Retry-After") != "15" {
				t.Fatalf("503 must carry Retry-After")
			}
		})
	}
}

func TestAttachFallbacks(t *testing.T) {
	svc, _ := testService(t, nil, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	// No-Flusher writer → JSON fallback (covers newSSE !ok).
	st := newStream()
	st.target = "acme/w"
	rec := wal.TaskRecord{ID: "i-x", Kind: KindRepoImport, Repo: "acme/w", LogTail: []string{"hi"}}
	st.setRecord(rec)
	svc.mu.Lock()
	svc.streams["i-x"] = st
	svc.mu.Unlock()
	nf := &noFlushWriter{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/imports/i-x", nil)
	req.Header.Set("Accept", "text/event-stream")
	h.ServeHTTP(nf, req)
	if nf.c != 200 || !strings.Contains(string(nf.b), "i-x") {
		t.Fatalf("no-flush fallback = %d %q", nf.c, nf.b)
	}
	// Canceled ctx mid-replay → attach gives up (covers event-false).
	st.send(wal.Progress{Kind: "notice", Text: "n1"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/repos/imports/i-x", nil).WithContext(ctx)
	req2.Header.Set("Accept", "text/event-stream")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("canceled attach = %d", w2.Code)
	}
}

func TestSSEWriterUnits(t *testing.T) {
	// terminal(nil) → error frame.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s, ok := newSSE(w, req)
	if !ok {
		t.Fatalf("recorder must flush")
	}
	s.terminal(nil)
	s.close()
	if !strings.Contains(w.Body.String(), `"status":500`) {
		t.Fatalf("nil terminal = %q", w.Body.String())
	}
	// result with nil maps → {} not null.
	w2 := httptest.NewRecorder()
	s2, _ := newSSE(w2, req)
	s2.terminal(&Outcome{Repo: "a/b"})
	s2.close()
	if !strings.Contains(w2.Body.String(), `"head_shas":{}`) {
		t.Fatalf("nil heads = %q", w2.Body.String())
	}
	// error outcome → error frame.
	w3 := httptest.NewRecorder()
	s3, _ := newSSE(w3, req)
	s3.terminal(&Outcome{Repo: "a/b", Err: &StatusError{Status: 409, Message: "taken"}})
	s3.close()
	body := w3.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, `"status":409`) {
		t.Fatalf("error terminal = %q", body)
	}
	// terminal-once: second event dropped.
	if s3.event("notice", "{}") {
		t.Fatalf("post-terminal event must drop")
	}
	// unknown packet kind skipped.
	w4 := httptest.NewRecorder()
	s4, _ := newSSE(w4, req)
	if !s4.packet(wal.Progress{Kind: "weird"}) {
		t.Fatalf("unknown kind must skip-true")
	}
	if !s4.packet(wal.Progress{Kind: "task"}) {
		t.Fatalf("nil task packet must skip-true")
	}
	s4.close()
	// mustJSON failure → {}.
	if got := mustJSON(func() {}); got != `{}` {
		t.Fatalf("mustJSON = %q", got)
	}
	// writeJSON failure → 500.
	w5 := httptest.NewRecorder()
	writeJSON(w5, 200, func() {})
	if w5.Code != 500 {
		t.Fatalf("bad json = %d", w5.Code)
	}
	// writeStatusErr passthrough.
	w6 := httptest.NewRecorder()
	writeStatusErr(w6, errors.New("plain"))
	if w6.Code != 500 || !strings.Contains(w6.Body.String(), "plain") {
		t.Fatalf("plain err = %d %q", w6.Code, w6.Body.String())
	}
	// comment on canceled ctx → false.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w7 := httptest.NewRecorder()
	req7 := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	s7, _ := newSSE(w7, req7)
	if s7.comment(": keepalive") {
		t.Fatalf("canceled comment must fail")
	}
	s7.close()
}

func TestWireShapes(t *testing.T) {
	ok := true
	full := wal.TaskRecord{
		ID: "i-1", Kind: KindRepoImport, Repo: "a/b", Hostname: "h",
		Started: "2026-09-04T00:00:00Z", Finished: "2026-09-04T00:00:01Z",
		ElapsedMS: 1000, OK: &ok, Summary: "done",
		Progress: &wal.Progress{Kind: "progress", Label: "clone", Done: 1, Unit: "objects"},
		Params:   map[string]string{"a": "b"},
	}
	m := taskJSON(full)
	if m["finished"] != full.Finished || m["ok"] != true || m["elapsed_ms"] != int64(1000) {
		t.Fatalf("task = %v", m)
	}
	pm := m["progress"].(map[string]any)
	if pm["label"] != "clone" || pm["done"] != uint64(1) {
		t.Fatalf("progress = %v", pm)
	}
	// Progress with total/percent/task nests.
	total := uint64(4)
	pct := 25.0
	inner := full
	p2 := progressJSON(wal.Progress{Kind: "progress", Label: "l", Done: 1, Total: &total, Percent: &pct, Unit: "u", Task: &inner})
	if p2["total"] != total || p2["percent"] != 25.0 || p2["task"] == nil {
		t.Fatalf("rich progress = %v", p2)
	}
	// Text-only progress.
	p3 := progressJSON(wal.Progress{Kind: "notice", Text: "hi"})
	if p3["text"] != "hi" {
		t.Fatalf("text progress = %v", p3)
	}
	// noOpDoc nil maps.
	d := noOpDoc(&ImportDoc{Version: 1, SourceURL: "u"})
	raw, _ := json.Marshal(d)
	if !strings.Contains(string(raw), `"requested_refs":[]`) || !strings.Contains(string(raw), `"head_shas":{}`) {
		t.Fatalf("noop = %s", raw)
	}
}

// --- service units -----------------------------------------------------------------------------------

func TestCheckGatesMatrix(t *testing.T) {
	ctx := context.Background()
	svc, _ := testService(t, nil, nil)
	// Nil roles: anonymous → 401 on both gates.
	if err := svc.checkCreate(ctx, auth.Anonymous(), "a", "b"); statusCode(err) != 401 {
		t.Fatalf("nil-roles anon create = %v", err)
	}
	if err := svc.checkRead(ctx, auth.Anonymous(), "a", "b"); statusCode(err) != 401 {
		t.Fatalf("nil-roles anon read = %v", err)
	}
	// Nil roles: writer passes both.
	if err := svc.checkCreate(ctx, writerPrincipal(), "a", "b"); err != nil {
		t.Fatalf("nil-roles writer create = %v", err)
	}
	if err := svc.checkRead(ctx, writerPrincipal(), "a", "b"); err != nil {
		t.Fatalf("nil-roles writer read = %v", err)
	}
	// Stub roles surfacing 401/403 through both gates.
	stub := &stubRoles{create: &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "sso"}, read: &auth.AuthError{Kind: auth.ErrForbidden, Why: "priv"}}
	svc2, _ := testService(t, nil, stub)
	if err := svc2.checkCreate(ctx, writerPrincipal(), "a", "b"); statusCode(err) != 401 {
		t.Fatalf("stub create = %v", err)
	}
	if err := svc2.checkRead(ctx, writerPrincipal(), "a", "b"); statusCode(err) != 403 {
		t.Fatalf("stub read = %v", err)
	}
}

func statusCode(err error) int {
	if se, ok := err.(*StatusError); ok {
		return se.Status
	}
	return 0
}

type stubRoles struct {
	FakeRoles
	create *auth.AuthError
	read   *auth.AuthError
}

func (s *stubRoles) CheckRole(_ context.Context, _, _ string, _ auth.Principal, _ identity.Role) *auth.AuthError {
	return s.create
}
func (s *stubRoles) CheckRead(_ context.Context, _, _ string, _ auth.Principal) *auth.AuthError {
	return s.read
}

func TestProbeError(t *testing.T) {
	svc, _ := testService(t, nil, &FakeRoles{})
	svc.store = &errHeadStore{ObjectStore: svc.store}
	p := fileParams("acme", "probe", "file:///x")
	if _, _, err := svc.probe(context.Background(), p); statusCode(err) != 500 {
		t.Fatalf("probe = %v, want 500", err)
	}
	// Begin surfaces the probe failure (no task started).
	if _, _, err := svc.Begin(context.Background(), adminPrincipal(), p, ""); statusCode(err) != 500 {
		t.Fatalf("begin = %v, want 500", err)
	}
	// Lookup on a reg-less service → unknown, never panic.
	bare := &Service{running: map[string]*running{}, streams: map[string]*stream{}}
	if _, _, ok := bare.Lookup("nope"); ok {
		t.Fatalf("bare lookup must miss")
	}
}

// errHeadStore fails HEAD (probe path); everything else delegates.
type errHeadStore struct {
	store.ObjectStore
}

func (f *errHeadStore) Head(_ context.Context, _ string) (*store.ObjectMeta, error) {
	return nil, errors.New("disk gone")
}

// errCasStore fails CAS puts (manifest arbitration); creates delegate.
type errCasStore struct {
	store.ObjectStore
}

func (f *errCasStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if opts.Mode == store.PutUpdate {
		return store.ObjectMeta{}, errors.New("cas lost")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

// errIdxStore fails idx uploads only (pack + manifest succeed).
type errIdxStore struct {
	store.ObjectStore
}

func (f *errIdxStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if opts.Mode == store.PutCreate && strings.HasSuffix(key, ".idx") {
		return store.ObjectMeta{}, errors.New("idx store down")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func TestParseRequestNilCfg(t *testing.T) {
	// Nil cfg falls back to compiled-in defaults (file:// refused there).
	_, _, err := ParseRequest([]byte(`{"source_url":"file:///x","owner":"a","name":"b","token":"t"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "allow_file_urls") {
		t.Fatalf("nil cfg = %v, want defaults-applied refusal", err)
	}
	p, tok, err := ParseRequest([]byte(`{"source_url":"file:///x","owner":"a","name":"b","token":"t"}`), func() *config.Config {
		c := config.Defaults()
		c.Import.AllowFileURLs = true
		return c
	}())
	if err != nil || tok != "t" || !p.TokenSet {
		t.Fatalf("explicit cfg = %+v %q %v", p, tok, err)
	}
	if len(p.Refs) != 0 {
		t.Fatalf("refs must default []")
	}
}

func TestPrinterUnits(t *testing.T) {
	p := &Printer{}
	if p.Ctx() == nil {
		t.Fatalf("nil ctx must fall back")
	}
	p.Notice("hi")
	p.Progress("l", 0, 0, "u")   // no total → quiet
	p.Progress("l", 5, 4, "u")   // done > total → quiet
	p.Progress("l", 1, 10, "u")  // 10% → prints
	p.Progress("l", 10, 10, "u") // done → prints
	if got := statusOf(errTestPlain("x")); got.Status != 500 {
		t.Fatalf("statusOf = %+v", got)
	}
	if got := statusOf(&StatusError{Status: 409, Message: "m"}); got.Status != 409 {
		t.Fatalf("statusOf passthrough = %+v", got)
	}
}

type errTestPlain string

func (e errTestPlain) Error() string { return string(e) }

func TestEmptySource422(t *testing.T) {
	cfg := testConfig(t)
	svc, _ := testService(t, cfg, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	// Empty repo: no commits → no refs after clone → 422.
	empty := t.TempDir() + "/empty"
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := "git"
	run := func(args ...string) {
		c := exec.Command(cmd, args...)
		c.Dir = empty
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main", ".")
	w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":"file://`+empty+`","owner":"acme","name":"empty"}`, "")
	if w.Code != 202 {
		t.Fatalf("POST = %d (%q)", w.Code, w.Body.String())
	}
	var started struct {
		Task map[string]any `json:"task"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &started)
	id, _ := started.Task["id"].(string)
	o := awaitDone(t, svc, id, 60*time.Second)
	if o.Err == nil || o.Err.Status != 422 {
		t.Fatalf("empty source = %+v, want 422", o)
	}
}

func TestBodyAdoptAndConflict(t *testing.T) {
	// Body-level Create race: manifest won by another import of the SAME
	// source → adopt; of a DIFFERENT source → 409.
	cfg := testConfig(t)
	svc, _ := testService(t, cfg, &FakeRoles{})
	ctx := context.Background()
	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 2, 0, 0)
	repackSingle(t, remote)
	// Same-source winner.
	if _, err := svc.reg.Create(ctx, "acme/adopt", 0); err != nil {
		t.Fatal(err)
	}
	doc := &ImportDoc{Version: 1, SourceURL: srcURL, SourceKind: "file", RequestedRefs: []string{}, HeadSHAs: map[string]string{"refs/heads/main": strings.Repeat("b", 40)}, Importer: "w", Format: "sha1", ImportedAt: nowRFC3339()}
	if err := writeImportDoc(ctx, svc.store, "acme", "adopt", doc); err != nil {
		t.Fatal(err)
	}
	in := &importNarr{print: &Printer{Context: ctx}}
	if err := svc.runImport(ctx, in, "i-adopt", fileParams("acme", "adopt", srcURL), ""); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if in.result == nil || in.result.HeadSHAs["refs/heads/main"] != strings.Repeat("b", 40) {
		t.Fatalf("adopted result = %+v", in.result)
	}
	// Different-source winner → 409.
	if _, err := svc.reg.Create(ctx, "acme/clash", 0); err != nil {
		t.Fatal(err)
	}
	foreign := &ImportDoc{Version: 1, SourceURL: "file:///other.git", SourceKind: "file", RequestedRefs: []string{}, HeadSHAs: map[string]string{}, Importer: "w", Format: "sha1", ImportedAt: nowRFC3339()}
	if err := writeImportDoc(ctx, svc.store, "acme", "clash", foreign); err != nil {
		t.Fatal(err)
	}
	in2 := &importNarr{print: &Printer{Context: ctx}}
	if err := svc.runImport(ctx, in2, "i-clash", fileParams("acme", "clash", srcURL), ""); statusCode(err) != 409 {
		t.Fatalf("clash = %v, want 409", err)
	}
	// Corrupt winner doc → 409 (never adopt the unreadable).
	if _, err := svc.reg.Create(ctx, "acme/corrupt", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBytes(ctx, svc.store, importKey("acme", "corrupt"), []byte("{bad"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	in3 := &importNarr{print: &Printer{Context: ctx}}
	if err := svc.runImport(ctx, in3, "i-corrupt", fileParams("acme", "corrupt", srcURL), ""); statusCode(err) != 409 {
		t.Fatalf("corrupt = %v, want 409", err)
	}
}

func TestPublishPackErrors(t *testing.T) {
	cfg := testConfig(t)
	mem := store.NewMemory()
	svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, &errCasStore{ObjectStore: mem})
	ctx := context.Background()
	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 2, 0, 0)
	repackSingle(t, remote)
	_ = srcURL
	packs, _ := filepath.Glob(filepath.Join(remote, ".git", "objects", "pack", "*.pack"))
	h, err := svc.reg.Create(ctx, "acme/p", 0)
	if err != nil {
		t.Fatal(err)
	}
	n := &importNarr{print: &Printer{Context: ctx}}
	p := fileParams("acme", "p", "file:///x")
	// EnsurePackIdx failure (missing pack).
	if err := svc.publishPack(ctx, n, h, p, filepath.Join(t.TempDir(), "missing.pack"), strings.Repeat("0", 40), 0, nil); err == nil {
		t.Fatalf("missing pack must fail")
	}
	// Manifest CAS failure inside AddPack (create-if-absent pack upload
	// succeeds; the manifest PutUpdate loses) → 500.
	if err := svc.publishPack(ctx, n, h, p, packs[0], trailerOf(t, packs[0]), 0, nil); statusCode(err) != 500 {
		t.Fatalf("cas failure = %v, want 500", err)
	}
	// Idx upload failure: pack + manifest succeed, the idx PUT fails.
	svc2, _ := testServiceOnStore(t, cfg, &FakeRoles{}, &errIdxStore{ObjectStore: store.NewMemory()})
	h2, err := svc2.reg.Create(ctx, "acme/p2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc2.publishPack(ctx, n, h2, p, packs[0], trailerOf(t, packs[0]), 0, nil); statusCode(err) != 500 {
		t.Fatalf("idx failure = %v, want 500", err)
	}
}

func trailerOf(t *testing.T, pack string) string {
	t.Helper()
	sum, err := packTrailerChecksum(pack, "sha1")
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func TestNoOpDocAndTaskFallbacks(t *testing.T) {
	// GET on a stream with no record yet → {"id"} (covers get nil-rec).
	svc, _ := testService(t, nil, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	st := newStream()
	st.target = "acme/w"
	svc.mu.Lock()
	svc.streams["i-bare"] = st
	svc.mu.Unlock()
	g := doGet(t, h, "/api/v1/repos/imports/i-bare", "")
	if g.Code != 200 || !strings.Contains(g.Body.String(), "i-bare") {
		t.Fatalf("bare stream GET = %d %q", g.Code, g.Body.String())
	}
	// namespaceOf via table record with empty repo (covers empty branch).
	rec, err := svc.reg.Tasks().Run(context.Background(), "", "sync", nil, func(ctx context.Context, task *wal.Task) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	g2 := doGet(t, h, "/api/v1/repos/imports/"+rec.ID, "")
	if g2.Code != 200 {
		t.Fatalf("empty-repo record GET = %d", g2.Code)
	}
	// Joined HTTP POST carries the live record (covers post joined-rec).
	st2 := newStream()
	st2.target = "acme/j"
	live := wal.TaskRecord{ID: "i-j", Kind: KindRepoImport, Repo: "acme/j", LogTail: []string{"x"}}
	st2.setRecord(live)
	done := make(chan struct{})
	want, _, err := ParseRequest([]byte(`{"source_url":"file:///srv/j.git","owner":"acme","name":"j"}`), svc.cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	svc.running["acme/j"] = &running{id: "i-j", params: want.scrubbedMap(), rec: &live, done: done}
	svc.streams["i-j"] = st2
	svc.mu.Unlock()
	defer close(done)
	w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":"file:///srv/j.git","owner":"acme","name":"j"}`, "")
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"joined":true`) || !strings.Contains(w.Body.String(), `"log_tail":["x"]`) {
		t.Fatalf("joined POST = %d %s", w.Code, w.Body.String())
	}
}
