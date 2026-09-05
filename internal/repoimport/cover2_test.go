// cover2_test.go — second gap-closing wave: retry loops, store-failure
// injection, attach races, sha256, and constructor branches.
package repoimport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// --- flaky roles: CAS-retry loop paths -------------------------------------------------

type flakyRoles struct {
	FakeRoles
	putFails     int
	getFailsNext bool
	nilDoc       bool
}

func (f *flakyRoles) PutAccess(ctx context.Context, owner, repo string, ver store.Version, vis identity.Visibility, bindings []identity.AccessBinding) (*identity.AccessDoc, error) {
	if f.putFails > 0 {
		f.putFails--
		return nil, errors.New("cas lost")
	}
	return f.FakeRoles.PutAccess(ctx, owner, repo, ver, vis, bindings)
}

func (f *flakyRoles) GetAccess(ctx context.Context, owner, repo string) (*identity.AccessDoc, store.Version, error) {
	if f.nilDoc {
		return nil, "", nil
	}
	if f.getFailsNext {
		f.getFailsNext = false
		return nil, "", errors.New("store down")
	}
	return f.FakeRoles.GetAccess(ctx, owner, repo)
}

func TestEnsureImporterAdminRetries(t *testing.T) {
	ctx := context.Background()
	// Fail once → retry converges.
	r := &flakyRoles{}
	r.putFails = 1
	if err := ensureImporterAdmin(ctx, r, "acme", "r", "zed@example.com"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(r.Bindings["acme/r"]) != 1 {
		t.Fatalf("bindings = %+v", r.Bindings)
	}
	// Always fail → bounded 500.
	r2 := &flakyRoles{}
	r2.putFails = 100
	if err := ensureImporterAdmin(ctx, r2, "acme", "r", "zed@example.com"); statusCode(err) != 500 {
		t.Fatalf("always-fail = %v, want 500", err)
	}
	// Re-read fails mid-loop → 500.
	r3 := &flakyRoles{}
	if _, _, err := r3.GetAccess(ctx, "acme", "r2"); err != nil {
		t.Fatal(err)
	}
	r3.putFails = 1
	r3.getFailsNext = true
	if err := ensureImporterAdmin(ctx, r3, "acme", "r2", "zed@example.com"); statusCode(err) != 500 {
		t.Fatalf("re-read fail = %v, want 500", err)
	}
	// Nil doc (defensive) → synthesized default path.
	r4 := &flakyRoles{nilDoc: true}
	if err := ensureImporterAdmin(ctx, r4, "acme", "r", "zed@example.com"); err != nil {
		t.Fatalf("nil doc: %v", err)
	}
}

// --- store-failure injection -----------------------------------------------------------------

type errCreateStore struct {
	store.ObjectStore
}

func (f *errCreateStore) Put(_ context.Context, _ string, _ store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if opts.Mode == store.PutCreate {
		return store.ObjectMeta{}, errors.New("create down")
	}
	return store.ObjectMeta{}, errors.New("unreachable")
}

type errGetStore struct {
	store.ObjectStore
}

func (f *errGetStore) Get(_ context.Context, _ string, _ store.GetOptions) (store.GetResult, error) {
	return nil, errors.New("get down")
}

func TestWriteImportDocStoreFailures(t *testing.T) {
	ctx := context.Background()
	claim := &ImportDoc{Version: 1, SourceURL: "u"}
	// Create failure (non-412) → 500.
	if _, _, _, err := claimImportDoc(ctx, &errCreateStore{ObjectStore: store.NewMemory()}, "a", "b", claim); statusCode(err) != 500 {
		t.Fatalf("create failure = %v, want 500", err)
	}
	// 412 then readback failure → the read error (500 here).
	mem := store.NewMemory()
	if _, _, _, err := claimImportDoc(ctx, mem, "a", "b", &ImportDoc{Version: 1, SourceURL: "u"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := claimImportDoc(ctx, &errGetStore{ObjectStore: mem}, "a", "b",
		&ImportDoc{Version: 1, SourceURL: "u"}); statusCode(err) != 500 {
		t.Fatalf("readback failure = %v, want 500", err)
	}
	// Completion against a failing store → 500.
	if _, err := completeImportDoc(ctx, &errCreateStore{ObjectStore: store.NewMemory()}, "a", "b",
		&ImportDoc{Version: 1, SourceURL: "u"}, "v1"); statusCode(err) != 500 {
		t.Fatalf("complete failure = %v, want 500", err)
	}
	// Non-NotFound read error → 500.
	if _, _, err := readImportDoc(ctx, &errGetStore{ObjectStore: mem}, "a", "b"); statusCode(err) != 500 {
		t.Fatalf("read failure = %v, want 500", err)
	}
}

func TestRunImportCreateFailure(t *testing.T) {
	cfg := testConfig(t)
	svc, _ := testServiceOnStore(t, cfg, &FakeRoles{}, &errCreateStore{ObjectStore: store.NewMemory()})
	remote := t.TempDir() + "/src"
	srcURL := fixtureRepo(t, remote, 1, 0, 0)
	in := &importNarr{print: &Printer{Context: context.Background()}}
	if err := svc.runImport(context.Background(), in, "i-x", fileParams("acme", "cfail", srcURL), ""); statusCode(err) != 500 {
		t.Fatalf("create failure = %v, want 500", err)
	}
}

// --- joinOrConflictLocked direct ------------------------------------------------------------------

func TestJoinOrConflictDirect(t *testing.T) {
	svc, _ := testService(t, nil, &FakeRoles{})
	want := map[string]string{"source_url": "file:///a"}
	if res, _, err := svc.joinOrConflictLocked("t", want); err != nil || res != nil {
		t.Fatalf("fresh = %v %v, want start", res, err)
	}
	done := make(chan struct{})
	defer close(done)
	svc.mu.Lock()
	svc.running["t"] = &running{id: "i-1", params: want, done: done}
	svc.mu.Unlock()
	svc.mu.Lock()
	res, _, err := svc.joinOrConflictLocked("t", want)
	svc.mu.Unlock()
	if err != nil || res == nil || !res.Joined || res.TaskID != "i-1" {
		t.Fatalf("match = %+v %v", res, err)
	}
	svc.mu.Lock()
	_, _, err = svc.joinOrConflictLocked("t", map[string]string{"source_url": "file:///b"})
	svc.mu.Unlock()
	if statusCode(err) != 409 {
		t.Fatalf("mismatch = %v, want 409", err)
	}
}

// --- scrubbedMap all-flags ----------------------------------------------------------------------------

func TestScrubbedMapFlags(t *testing.T) {
	n, _ := NormalizeSource("acme/r")
	p := Params{
		Owner: "acme", Name: "r", Source: n,
		Refs: []string{"refs/heads/main"}, DefaultBranchOnly: true,
		IncludePullHeads: true, IncludeNotes: true,
		Format: "sha1", Dangerous: true, TokenSet: true,
	}
	m := p.scrubbedMap()
	for _, k := range []string{"source_url", "source_kind", "target", "format", "refs", "default_branch_only", "include_pull_heads", "include_notes", "secret_set"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("scrubbed map lacks %q: %v", k, m)
		}
	}
	if strings.Contains(m["refs"], "token") {
		t.Fatalf("refs leak: %v", m)
	}
}

// --- probe/store-nil + corrupt-doc -----------------------------------------------------------------------

func TestProbeStoreNil(t *testing.T) {
	cfg := testConfig(t)
	svc := New(Deps{Roles: &FakeRoles{}, Cfg: cfg, Hostname: "h"})
	if _, _, err := svc.probe(context.Background(), fileParams("a", "b", "file:///x")); statusCode(err) != 503 {
		t.Fatalf("nil store probe = %v, want 503", err)
	}
	if _, _, err := svc.Begin(context.Background(), adminPrincipal(), fileParams("a", "b", "file:///x"), ""); statusCode(err) != 503 {
		t.Fatalf("nil store begin = %v, want 503", err)
	}
}

func TestProbeCorruptDoc409(t *testing.T) {
	cfg := testConfig(t)
	svc, _ := testService(t, cfg, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	ctx := context.Background()
	if _, err := svc.reg.Create(ctx, "acme/bad", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBytes(ctx, svc.store, importKey("acme", "bad"), []byte("{corrupt"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":"file:///srv/x.git","owner":"acme","name":"bad"}`, "")
	if w.Code != 409 {
		t.Fatalf("corrupt doc probe = %d (%q), want 409", w.Code, w.Body.String())
	}
}

// --- nil-service guards (both verbs 503, never a nil-map panic) -------------------------

func TestNilSvc503(t *testing.T) {
	h := testHandler(nil, adminPrincipal())
	w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":"file:///srv/x.git","owner":"a","name":"b"}`, "")
	if w.Code != 503 {
		t.Fatalf("nil-svc POST = %d, want 503", w.Code)
	}
	if g := doGet(t, h, "/api/v1/repos/imports/whatever", ""); g.Code != 503 {
		t.Fatalf("nil-svc GET = %d, want 503", g.Code)
	}
}

// --- checkRead 401 via stub ----------------------------------------------------------------------------------

func TestCheckRead401(t *testing.T) {
	svc, _ := testService(t, nil, &stubRoles{read: &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "sso expired"}})
	if err := svc.checkRead(context.Background(), writerPrincipal(), "a", "b"); statusCode(err) != 401 {
		t.Fatalf("stub read = %v, want 401", err)
	}
}

// --- http: nil auth, short/deep paths, bad escape, err body, nil svc, get auth ------------------------

func TestNilAuthFallsBackAnonymous(t *testing.T) {
	svc, _ := testService(t, nil, &FakeRoles{})
	h := &Handler{Svc: svc} // Auth nil → anonymous → 401 on POST
	w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":"file:///x","owner":"a","name":"b"}`, "")
	if w.Code != 401 {
		t.Fatalf("nil-auth POST = %d, want 401", w.Code)
	}
}

func TestRoutingEdges(t *testing.T) {
	svc, _ := testService(t, nil, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	if h.Handle(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatalf("root must report false")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/imports/a/b", nil)
	if h.Handle(httptest.NewRecorder(), req) {
		t.Fatalf("deep path must report false")
	}
	// httptest.NewRequest rejects bad escapes, so build the request by
	// hand (splitPath must survive an undecodable segment → 404).
	reqBad := &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/api/v1/repos/imports/%zz"}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, reqBad)
	if w.Code != 404 {
		t.Fatalf("bad escape = %d, want 404", w.Code)
	}
}

type errBody struct{}

func (errBody) Read(_ []byte) (int, error) { return 0, errors.New("read down") }
func (errBody) Close() error               { return nil }

func TestPostEdges(t *testing.T) {
	svc, _ := testService(t, nil, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	// Unreadable body.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/imports", errBody{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("unreadable body = %d, want 400", w.Code)
	}
	// Struct-unmarshal failure after map success.
	w2 := doPost(t, h, "/api/v1/repos/imports", `{"source_url":"file:///x","owner":"a","name":"b","refs":"nope"}`, "")
	if w2.Code != 400 {
		t.Fatalf("bad refs shape = %d, want 400", w2.Code)
	}
	// Nil service → 503 (never a panic).
	hn := &Handler{}
	w3 := doPost(t, hn, "/api/v1/repos/imports", `{"source_url":"file:///x","owner":"a","name":"b"}`, "")
	if w3.Code != 503 {
		t.Fatalf("nil svc = %d, want 503", w3.Code)
	}
	// Auth error on GET.
	he := &Handler{Svc: svc, Auth: func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad token"}
	}}
	w4 := doGet(t, he, "/api/v1/repos/imports/whatever", "")
	if w4.Code != 401 {
		t.Fatalf("get auth err = %d, want 401", w4.Code)
	}
}

// --- attach races ------------------------------------------------------------------------------------------------

func TestAttachCancelEmpty(t *testing.T) {
	svc, _ := testService(t, nil, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	st := newStream()
	st.target = "acme/w"
	svc.mu.Lock()
	svc.streams["i-cancel"] = st
	svc.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/imports/i-cancel", nil).WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req) // must return via ctx.Done (covers the select arm)
	if w.Code != 200 {
		t.Fatalf("canceled attach = %d", w.Code)
	}
}

func TestAttachUnsubscribed(t *testing.T) {
	svc, _ := testService(t, nil, &FakeRoles{})
	h := testHandler(svc, adminPrincipal())
	st := newStream()
	st.target = "acme/w"
	svc.mu.Lock()
	svc.streams["i-unsub"] = st
	svc.mu.Unlock()
	done := make(chan int, 1)
	go func() {
		w := doGet(t, h, "/api/v1/repos/imports/i-unsub", "text/event-stream")
		done <- w.Code
	}()
	time.Sleep(200 * time.Millisecond)
	st.unsubscribe(0) // the handler's sub id (first subscriber) → !ok arm
	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("unsubscribed attach = %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("attach did not notice unsubscribe")
	}
}

func TestKeepaliveTick(t *testing.T) {
	w := &syncRecorder{header: http.Header{}}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s, ok := newSSEWithTicker(w, req, 20*time.Millisecond)
	if !ok {
		t.Fatal("recorder must flush")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.mu.Lock()
		body := string(w.body)
		w.mu.Unlock()
		if strings.Contains(body, ": keepalive") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no keepalive in %q", body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.close()
}

// syncRecorder is a race-clean ResponseRecorder+Flusher for keepalive
// tests (httptest.ResponseRecorder is not safe for concurrent
// read-while-write).
type syncRecorder struct {
	mu     sync.Mutex
	header http.Header
	body   []byte
	code   int
}

func (s *syncRecorder) Header() http.Header { return s.header }
func (s *syncRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body = append(s.body, p...)
	return len(p), nil
}
func (s *syncRecorder) WriteHeader(c int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code = c
}
func (s *syncRecorder) Flush() {}

// --- git: bad binary, script binary, parse units, env -----------------------------------------------------

func TestCloneStartError(t *testing.T) {
	r := &Runner{Binary: "/nonexistent/git-binary-xyz", Pool: newPool(1), CloneTimeout: time.Minute, GitTimeout: time.Minute, CacheDir: t.TempDir()}
	err := r.CloneMirror(context.Background(), "file:///x", t.TempDir()+"/d", "file", "", "", "",
		func(string, uint64, uint64, string) {}, func() {})
	if err == nil || !strings.Contains(err.Error(), "clone: start") {
		t.Fatalf("bad binary = %v", err)
	}
}

func writeScriptBinary(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScriptBinaryShapes(t *testing.T) {
	bin := writeScriptBinary(t, `if [ "$1" = "rev-parse" ]; then echo bogus; exit 0; fi
echo "garbage-no-spaces"
echo "only two"
exit 0
`)
	r := &Runner{Binary: bin, Pool: newPool(1), CloneTimeout: time.Minute, GitTimeout: time.Minute, CacheDir: t.TempDir()}
	refs, err := r.ForEachRef(context.Background(), t.TempDir())
	if err != nil || len(refs) != 0 {
		t.Fatalf("garbage for-each-ref = %v %v (malformed lines skipped)", refs, err)
	}
	if _, err := r.ShowObjectFormat(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "unknown object format") {
		t.Fatalf("bogus format = %v", err)
	}
	// index-pack exiting 0 without producing an idx.
	pack := filepath.Join(t.TempDir(), "x.pack")
	writeFile(t, pack, "PACK")
	if _, err := r.EnsurePackIdx(context.Background(), pack); err == nil || !strings.Contains(err.Error(), "no idx") {
		t.Fatalf("missing idx production = %v", err)
	}
}

func TestParseCloneProgressTail(t *testing.T) {
	var bars int
	var tail string
	var errBuf boundedStderr
	parseCloneProgress(strings.NewReader("remote: Enumerating objects: 10%\nReceiving objects: 50% (1/2)"), &errBuf, func(string, uint64, uint64, string) { bars++ })
	tail = errBuf.String()
	if bars != 2 {
		t.Fatalf("bars = %d, want 2 (no-trailing-newline tail parsed)", bars)
	}
	if tail != "" {
		t.Fatalf("tail = %q, want empty (all lines were progress)", tail)
	}
	// Non-progress lines land scrubbed in the tail.
	parseCloneProgress(strings.NewReader("Cloning into 'x'...\npassword=hunter2\ndone\n"), &errBuf, nil)
	if strings.Contains(errBuf.String(), "hunter2") {
		t.Fatalf("tail leaks: %q", errBuf.String())
	}
}

func TestBoundedStderrTruncate(t *testing.T) {
	var b boundedStderr
	if n, _ := b.Write(make([]byte, 9000)); n != 9000 {
		t.Fatalf("write = %d", n)
	}
	if len(b.String()) > 8192 {
		t.Fatalf("unbounded tail: %d", len(b.String()))
	}
}

func TestPathEnvEmpty(t *testing.T) {
	t.Setenv("PATH", "")
	if got := pathEnv(); got != "/usr/bin:/bin" {
		t.Fatalf("pathEnv = %q", got)
	}
}

// --- stream units --------------------------------------------------------------------------------------------------

func (s *stream) snapshotReplay() []wal.Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]wal.Progress, len(s.replay))
	copy(out, s.replay)
	return out
}

func TestStreamAfterFinish(t *testing.T) {
	st := newStream()
	st.send(wal.Progress{Kind: "notice", Text: "a"})
	o := &Outcome{Repo: "a/b", HeadSHAs: map[string]string{}}
	st.finish(o)
	n := len(st.snapshotReplay())
	st.send(wal.Progress{Kind: "notice", Text: "late"})
	if len(st.snapshotReplay()) != n {
		t.Fatalf("post-finish send must drop")
	}
	st.finish(o) // second finish: no-op, never panics
	if st.expired(time.Now()) {
		t.Fatalf("fresh finish must not expire")
	}
	running := newStream()
	if running.expired(time.Now()) {
		t.Fatalf("running stream must not expire")
	}
}

func TestValidateTransportDefault(t *testing.T) {
	if err := ValidateTransport(Normalized{URL: "ftp://x", Scheme: "ftp"}, false); statusCode(err) != 400 {
		t.Fatalf("unknown scheme = %v, want 400", err)
	}
}

// --- runImport: canceled acquire + scratch failure -------------------------------------------------------------------

func TestRunImportAcquireCancel(t *testing.T) {
	cfg := testConfig(t)
	svc, _ := testService(t, cfg, &FakeRoles{})
	svc.clones <- struct{}{}
	svc.clones <- struct{}{}
	defer func() { <-svc.clones; <-svc.clones }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in := &importNarr{print: &Printer{Context: ctx}}
	if err := svc.runImport(context.Background(), in, "i-x", fileParams("a", "b", "file:///x"), ""); statusCode(err) != 503 {
		t.Fatalf("canceled acquire = %v, want 503", err)
	}
}

func TestRunImportScratchFailure(t *testing.T) {
	cfg := testConfig(t)
	cfg.Cache.Dir = filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(cfg.Cache.Dir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc, _ := testService(t, cfg, &FakeRoles{})
	in := &importNarr{print: &Printer{Context: context.Background()}}
	if err := svc.runImport(context.Background(), in, "i-x", fileParams("a", "b", "file:///x"), ""); statusCode(err) != 500 {
		t.Fatalf("scratch failure = %v, want 500", err)
	}
}

// --- sha256 end-to-end (format follows source) --------------------------------------------------------------------------

func fixtureRepoFmt(t *testing.T, dir, format string, n, branches, tags int) string {
	t.Helper()
	args := []string{"init", "-b", "main"}
	if format != "" {
		args = []string{"init", "--object-format=" + format, "-b", "main"}
	}
	run := func(a ...string) {
		cmd := exec.Command("git", a...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(append(args, ".")...)
	for i := 0; i < n; i++ {
		name := filepath.Join(dir, "f.txt")
		f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("x"); err != nil {
			t.Fatal(err)
		}
		f.Close()
		run("add", "f.txt")
		run("commit", "-q", "-m", "c")
	}
	for v := 0; v < tags; v++ {
		run("tag", "-a", "v0", "-m", "r")
	}
	_ = branches
	return "file://" + dir
}

func TestImportSha256(t *testing.T) {
	cfg := testConfig(t)
	svc, st := testService(t, cfg, nil)
	svc.roles = realRoles(st, cfg)
	h := testHandler(svc, adminPrincipal())
	remote := t.TempDir() + "/src256"
	srcURL := fixtureRepoFmt(t, remote, "sha256", 3, 0, 1)
	repackSingle(t, remote)
	w := doPost(t, h, "/api/v1/repos/imports", `{"source_url":`+q(srcURL)+`,"owner":"acme","name":"s256"}`, "")
	if w.Code != 202 {
		t.Fatalf("POST = %d (%q)", w.Code, w.Body.String())
	}
	var started struct {
		Task map[string]any `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	id, _ := started.Task["id"].(string)
	o := awaitDone(t, svc, id, 120*time.Second)
	if o.Err != nil {
		t.Fatalf("sha256 import: %v", o.Err)
	}
	if o.Format != "sha256" {
		t.Fatalf("format = %q", o.Format)
	}
	for ref, sha := range o.HeadSHAs {
		if len(sha) != 64 {
			t.Fatalf("head_shas[%q] = %q (want 64-hex)", ref, sha)
		}
	}
}
