// x_gaps10_test.go — Forgejo #289 (internal/server coverage gate), part 2:
// handler/engine/misc branches (serveSPA 304, asset MIME types, ChainExtra,
// request scheme/CORS tables, session refresh, atomic-write/listener utils,
// pkt-ERR writer, broker fallback, LFS/upstream negatives, routing edges,
// SSH transport errors, metrics exposition, oid tables). Behavioral pins
// only — each case asserts a status, wire shape, or error contract.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/sshd"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
	"git.packden.us/crueber/walhub/web"
)

// --- health.go: serveSPA 304, CSS MIME, webAsset guards -----------------------

func TestServeSPAConditional(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "http://x/", nil)
	rec := httptest.NewRecorder()
	s.serveSPA(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("shell = %d", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("shell must carry an ETag")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("shell content-type = %q", ct)
	}
	// Matching validator → 304 with an empty body (no-cache revalidation).
	req = httptest.NewRequest("GET", "http://x/", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	s.serveSPA(rec, req)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("shell 304 = %d len=%d", rec.Code, rec.Body.Len())
	}
	// HEAD → headers, no body.
	req = httptest.NewRequest("HEAD", "http://x/", nil)
	rec = httptest.NewRecorder()
	s.serveSPA(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("shell head = %d len=%d", rec.Code, rec.Body.Len())
	}
}

func TestServeUIAssetsCSS(t *testing.T) {
	s, _ := newTestServer(t, nil)
	entries, err := web.Files.ReadDir("dist/assets")
	if err != nil {
		t.Fatal(err)
	}
	css := ""
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".css") {
			css = e.Name()
		}
	}
	if css == "" {
		t.Fatal("built UI must ship a stylesheet (run make web)")
	}
	req := httptest.NewRequest("GET", "http://x/_ui/assets/"+css, nil)
	rec := httptest.NewRecorder()
	s.serveUIAssets(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("css asset = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Fatalf("css content-type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("hashed bundle must be immutable, got %q", cc)
	}
}

func TestWebAssetTable(t *testing.T) {
	if b, ok := webAsset("dist/index.html"); !ok || len(b) == 0 {
		t.Fatal("built shell must resolve")
	}
	for _, name := range []string{"", "../go.mod", "/dist/index.html", "dist/no-such-file", "dist/assets/../../go.mod"} {
		if _, ok := webAsset(name); ok {
			t.Fatalf("webAsset(%q) must miss", name)
		}
	}
}

func TestEqualFoldLastTable(t *testing.T) {
	cases := []struct {
		s, suf string
		want   bool
	}{
		{"app.js", ".js", true},
		{"APP.JS", ".js", true},   // upper input vs lower suffix
		{"app.js", ".JS", true},   // lower input vs upper suffix
		{"app.js", ".css", false}, // real mismatch
		{"a", "abc", false},       // suffix longer than input
	}
	for _, tc := range cases {
		if got := equalFoldLast(tc.s, tc.suf); got != tc.want {
			t.Errorf("equalFoldLast(%q, %q) = %v, want %v", tc.s, tc.suf, got, tc.want)
		}
	}
}

// --- WalEngine: Publish/Revision/PublishSettings -------------------------------

func TestWalEnginePublishUnknownRepo(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	defer reg.Close()
	e := NewWalEngine(reg, cfg)
	req := &git.PushRequest{Commands: []git.PushCommand{{
		Old: strings.Repeat("0", 40), New: strings.Repeat("a", 40), Ref: "refs/heads/main"}}}
	// Pushing a repo with no manifest fails cleanly as not-found (the HTTP
	// layer maps it to 404 / auto-create sees a clean miss).
	if _, err := e.Publish(ctx, mustRepoID(t, "o/ghost"), req, "alice", wal.ObjectAccess{}); err == nil || !isNotFound(err) {
		t.Fatalf("publish ghost = %v, want not-found", err)
	}
	if err := e.Sync(ctx, mustRepoID(t, "o/ghost"), wal.LevelRefs); err == nil || !isNotFound(err) {
		t.Fatalf("sync ghost = %v, want not-found", err)
	}
}

func TestWalEnginePublishSettingsRevision(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	st := store.NewMemory()
	reg := wal.NewRegistry(ctx, st, cfg)
	defer reg.Close()
	e := NewWalEngine(reg, cfg)
	id := mustRepoID(t, "o/props")
	if _, err := reg.Create(ctx, id.String(), git.Sha1); err != nil {
		t.Fatal(err)
	}
	// Revision reports the manifest revision (1 at create — the manifest
	// exists before any settings payload does).
	if rev, err := e.Revision(ctx, id); err != nil || rev != 1 {
		t.Fatalf("fresh revision = %d %v, want manifest rev 1", rev, err)
	}
	// Malformed TOML is rejected before any publish (D24 payload guard).
	if _, err := e.PublishSettings(ctx, id, []byte("[[["), "m", "a@x.test"); err == nil ||
		!strings.Contains(err.Error(), "settings.toml") {
		t.Fatalf("bad toml = %v, want settings.toml error", err)
	}
	// The settings revision starts at 1 and bumps exactly once per
	// publish (D24: the returned revision is the new settings revision).
	rev, err := e.PublishSettings(ctx, id, []byte("[repo]\ndescription = \"hi\""), "first", "alice@example.com")
	if err != nil || rev != 1 {
		t.Fatalf("publish settings = %d %v, want rev 1", rev, err)
	}
	rev, err = e.PublishSettings(ctx, id, []byte("[repo]\ndescription = \"yo\""), "second", "alice@example.com")
	if err != nil || rev != 2 {
		t.Fatalf("republish settings = %d %v, want rev 2", rev, err)
	}
}

// --- adoptPlaceholder (fire-and-forget marker adoption) -------------------------

type deleteStubStore struct {
	store.ObjectStore
	mu      sync.Mutex
	deleted []string
	delErr  error
	done    chan string
}

func (d *deleteStubStore) Delete(ctx context.Context, key string, _ store.Version) error {
	d.mu.Lock()
	d.deleted = append(d.deleted, key)
	err := d.delErr // read under mutex: the adopting goroutine is fire-and-forget
	d.mu.Unlock()
	select {
	case d.done <- key:
	default:
	}
	return err
}

func (d *deleteStubStore) setErr(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.delErr = err
}

func (d *deleteStubStore) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.deleted)
}

func TestAdoptPlaceholderBranches(t *testing.T) {
	id := mustRepoID(t, "o/hint")
	marker := id.StorePrefix() + "meta/placeholder.json"

	// No hint set → no store op at all (push budget: zero marker round
	// trips for auto-created and pre-existing repos).
	s, _ := newTestServer(t, nil)
	st := &deleteStubStore{ObjectStore: newFakeStore(), done: make(chan string, 8)}
	s.store = st
	s.adoptPlaceholder(id)
	time.Sleep(200 * time.Millisecond)
	if n := st.count(); n != 0 {
		t.Fatalf("hintless adopt issued %d deletes", n)
	}

	// A hint with no store → consumed and dropped, never a panic.
	s.placeholderHints = &api.PlaceholderHints{}
	s.placeholderHints.Add(id)
	s.store = nil
	s.adoptPlaceholder(id)
	s.store = st
	s.adoptPlaceholder(id) // hint already consumed → still nothing
	time.Sleep(200 * time.Millisecond)
	if n := st.count(); n != 0 {
		t.Fatalf("consumed hint re-adopted %d deletes", n)
	}

	// A delete failure is logged and dropped (stale markers are harmless).
	st.setErr(errors.New("bucket down"))
	s.placeholderHints.Add(id)
	s.adoptPlaceholder(id)
	select {
	case got := <-st.done:
		if got != marker {
			t.Fatalf("marker key = %q, want %q", got, marker)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adopt never issued the marker delete")
	}
	time.Sleep(200 * time.Millisecond) // the error path must not resurface

	// Success deletes exactly the marker key.
	st.setErr(nil)
	s.placeholderHints.Add(id)
	s.adoptPlaceholder(id)
	select {
	case got := <-st.done:
		if got != marker {
			t.Fatalf("marker key = %q, want %q", got, marker)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("adopt never issued the marker delete")
	}
}

// --- ChainExtra seam order -------------------------------------------------------

type stubProvider struct {
	served int
	owners []string
}

func (p *stubProvider) Serve(w http.ResponseWriter, r *http.Request) {
	p.served++
	plainStatus(w, http.StatusOK, "primary")
}

func (p *stubProvider) Owners(r *http.Request) ([]string, error) { return p.owners, nil }

type stubChainExtra struct {
	handled int
	claim   bool
}

func (x *stubChainExtra) Handle(w http.ResponseWriter, r *http.Request) bool {
	if !x.claim {
		return false
	}
	x.handled++
	plainStatus(w, http.StatusOK, "extra")
	return true
}

func TestChainExtraWiresExtras(t *testing.T) {
	s, _ := newTestServer(t, nil)
	prim := &stubProvider{owners: []string{"octo"}}
	s.api = prim
	x1 := &stubChainExtra{claim: true}
	x2 := &stubChainExtra{}
	s.ChainExtra(x1, x2)
	req := httptest.NewRequest("GET", "http://x/api", nil)
	rec := httptest.NewRecorder()
	s.api.Serve(rec, req)
	if rec.Body.String() != "extra\n" || prim.served != 0 || x1.handled != 1 {
		t.Fatalf("claimed serve = %q primary=%d extra=%d", rec.Body.String(), prim.served, x1.handled)
	}
	// Unclaimed → falls through to the primary, untouched core routes.
	x1.claim = false
	rec = httptest.NewRecorder()
	s.api.Serve(rec, req)
	if rec.Body.String() != "primary\n" || prim.served != 1 {
		t.Fatalf("unclaimed serve = %q primary=%d", rec.Body.String(), prim.served)
	}
	owners, err := s.api.Owners(req)
	if err != nil || len(owners) != 1 || owners[0] != "octo" {
		t.Fatalf("owners delegate = %v %v", owners, err)
	}
	// Nil api → ChainExtra is a silent no-op (setup-only boots).
	s.api = nil
	s.ChainExtra(x1)
}

// --- middleware: ReqLog fallback, scheme, CORS ------------------------------------

func TestReqLogFallback(t *testing.T) {
	if l := ReqLog(httptest.NewRequest("GET", "http://x/", nil)); l == nil {
		t.Fatal("request without a scoped logger must fall back to Default")
	}
	// A scoped logger round-trips back out (the http.request record).
	scoped := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := httptest.NewRequest("GET", "http://x/", nil).
		WithContext(context.WithValue(context.Background(), ctxLogKey{}, scoped))
	if l := ReqLog(req); l != scoped {
		t.Fatal("scoped logger must be returned as-is")
	}
}

func TestRequestSchemeTable(t *testing.T) {
	if got := requestScheme(nil); got != "http" {
		t.Fatalf("nil scheme = %q", got)
	}
	cases := []struct {
		name  string
		setup func(*http.Request)
		want  string
	}{
		{"plain", func(r *http.Request) {}, "http"},
		{"forwarded https", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }, "https"},
		{"forwarded list", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https, http") }, "https"},
		{"forwarded case", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "HTTPS") }, "https"},
		{"forwarded http", func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") }, "http"},
		{"tls connection", func(r *http.Request) { r.TLS = &tls.ConnectionState{} }, "https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://x/", nil)
			tc.setup(r)
			if got := requestScheme(r); got != tc.want {
				t.Fatalf("scheme = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOriginAllowedTable(t *testing.T) {
	allowed := []string{"https://app.example.com", "*.example.com"}
	cases := []struct {
		origin string
		want   bool
	}{
		{"https://app.example.com", true}, // exact
		{"https://sub.example.com", true}, // wildcard subdomain
		{"https://example.com", false},    // wildcard needs a label (§2.3)
		{"https://evil.com", false},       // foreign
		// Path confusion: the match runs on the stripped host, so a legit
		// subdomain host with a confusing path still matches.
		{"https://sub.example.com/.example.com", true},
		// Query-string smuggling: the ".example.com" suffix lives in the
		// query, the host does not match → refused (wildcard bypass).
		{"https://a.b/?u=.example.com", false},
	}
	for _, tc := range cases {
		if got := originAllowed(allowed, tc.origin); got != tc.want {
			t.Errorf("originAllowed(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}

// --- session sliding refresh -------------------------------------------------------

func TestMaybeRefreshSessionAged(t *testing.T) {
	s, _ := newTestServer(t, nil)
	sess, err := s.authSvc.MintSession("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	withCookie := func(path string) (*httptest.ResponseRecorder, *http.Request) {
		r := httptest.NewRequest("GET", "http://x"+path, nil)
		r.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
		return httptest.NewRecorder(), r
	}
	// Fresh session → no re-issue (no Set-Cookie churn on every request).
	rec, req := withCookie("/o/r")
	s.maybeRefreshSession(rec, req, auth.Principal{Name: "alice", Write: true})
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("fresh session must not be re-issued")
	}
	// Anonymous callers never refresh, even with a cookie present.
	rec, req = withCookie("/o/r")
	s.maybeRefreshSession(rec, req, auth.Anonymous())
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("anonymous must not refresh")
	}
	// Aged past ttl/4 (30d/4) → the cookie is re-issued deterministically.
	s.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	rec, req = withCookie("/o/r")
	s.maybeRefreshSession(rec, req, auth.Principal{Name: "alice", Write: true})
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "walgit_session" || cookies[0].Value != sess.Wire {
		t.Fatalf("aged session must re-issue the same wire: %v", cookies)
	}
	// The /_auth/ lane is exempt (it manages its own cookies).
	rec, req = withCookie("/_auth/check")
	s.maybeRefreshSession(rec, req, auth.Principal{Name: "alice", Write: true})
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("_auth lane must not refresh")
	}
}

// --- listener utils ------------------------------------------------------------------

func TestWriteFileAtomicBranches(t *testing.T) {
	dir := t.TempDir()
	// Happy path: exact bytes, no stray tmp left behind.
	p := filepath.Join(dir, "walhub.toml")
	if err := writeFileAtomic(p, []byte("x = 1\n")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "x = 1\n" {
		t.Fatalf("atomic write = %q %v", b, err)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp file must not survive the rename")
	}
	// Missing parent → the write error surfaces.
	if err := writeFileAtomic(filepath.Join(dir, "nope", "f"), []byte("x")); err == nil {
		t.Fatal("missing parent must error")
	}
	// Renaming onto a directory fails (never clobber one).
	if err := writeFileAtomic(dir, []byte("x")); err == nil {
		t.Fatal("write onto a directory must error")
	}
}

type ctxer struct{ c context.Context }

func (x ctxer) Context() context.Context { return x.c }

func TestNewHTTPServerBaseContext(t *testing.T) {
	s, _ := newTestServer(t, nil)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if srv := s.NewHTTPServer(h, nil); srv.Handler == nil || srv.BaseContext != nil {
		t.Fatal("nil app context must leave BaseContext unset")
	}
	type ctxVal struct{}
	ctx := context.WithValue(context.Background(), ctxVal{}, "v")
	srv := s.NewHTTPServer(h, ctxer{c: ctx})
	if srv.BaseContext == nil {
		t.Fatal("app context must wire BaseContext")
	}
	if got := srv.BaseContext(nil); got.Value(ctxVal{}) != "v" {
		t.Fatal("BaseContext must carry the app context")
	}
}

// --- pkt-ERR writer + placement ---------------------------------------------------------

func TestPktErrForBranches(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// Browser/curl callers get no pkt writer (plain statuses instead).
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("User-Agent", "curl/8.0")
	if fn := s.pktErrFor(httptest.NewRecorder(), req); fn != nil {
		t.Fatal("non-git client must get no pkt writer")
	}
	// Git callers get a 200 + pkt-line ERR naming the reason.
	req = httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	rec := httptest.NewRecorder()
	fn := s.pktErrFor(rec, req)
	if fn == nil {
		t.Fatal("git client must get a pkt writer")
	}
	fn("drained for test")
	if rec.Code != http.StatusOK {
		t.Fatalf("pkt status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "ERR") || !strings.Contains(body, "drained for test") {
		t.Fatalf("pkt body = %q", body)
	}
}

func TestPlacementOKNilEngine(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.engine = nil // setup-only shape: no engine, everything serves
	if !s.placementOK(httptest.NewRecorder(), httptest.NewRequest("GET", "http://x/", nil), mustRepoID(t, "o/r"), git.ServiceUploadPack) {
		t.Fatal("nil engine must serve")
	}
}

// --- broker forwarding --------------------------------------------------------------------

func TestForwardToBrokerStripsHopHeaders(t *testing.T) {
	var gotH http.Header
	var gotBody []byte
	var gotHost, gotPath string
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotH = r.Header.Clone()
		gotHost, gotPath = r.Host, r.URL.RequestURI()
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte("broker-ok"))
	}))
	defer broker.Close()
	s, _ := newTestServer(t, nil)
	s.cfg.WAL.PushBrokerURL = broker.URL
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", nil)
	req.Header.Set("Host", "evil.test")
	req.Header.Set("Content-Length", "9999")
	req.Header.Set("X-Keep", "yes")
	resp, err := s.forwardToBroker(context.Background(), req, []byte("payload"), "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotHost == "evil.test" {
		t.Fatal("broker must see its own Host, never the client's")
	}
	if gotH.Get("Content-Length") == "9999" {
		t.Fatal("client Content-Length must not be forwarded")
	}
	if gotH.Get("X-Keep") != "yes" || gotH.Get("X-Walgit-Principal") != "alice" || gotH.Get("X-Walgit-Forwarded") != "1" {
		t.Fatalf("forwarded headers = %v", gotH)
	}
	if string(gotBody) != "payload" || gotPath != "/o/r.git/git-receive-pack" {
		t.Fatalf("forwarded body/path = %q %q", gotBody, gotPath)
	}
}

func TestReceivePackBroker502FallsBack(t *testing.T) {
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer broker.Close()
	root := t.TempDir()
	fe := &fakeEngine{placement: Placement{Serve: false, Maintain: false}, exists: true}
	s, _ := newTestServer(t, func(o *Options) {
		o.Engine = fe
		o.Config.WAL.PushBrokerURL = broker.URL
		o.Config.WAL.PushBrokerBufferBytes = 1 << 20
	})
	ctx := context.WithValue(context.Background(), repoRootKey{}, root)
	if _, err := fe.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1); err != nil {
		t.Fatal(err)
	}
	var b bytes2Buffer
	b.Write(git.Pkt(strings.Repeat("0", 40) + " " + strings.Repeat("0", 40) + " refs/heads/none\n"))
	b.Write(git.Flush())
	req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", b.reader())
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	// A broker 502 (not a transport error) still falls back to local: the
	// push lands instead of failing the client.
	s.receivePackForward(rec, req, mustRepoID(t, "o/r"), principalAlice)
	if rec.Code != http.StatusOK || fe.published != 1 {
		t.Fatalf("502 fallback = %d published=%d", rec.Code, fe.published)
	}
}

// --- errorsAs / serveStatic / etag ----------------------------------------------------------

func TestErrorsAsTable(t *testing.T) {
	var we *wal.WalError
	if errorsAs(errors.New("plain"), &we) {
		t.Fatal("plain error must not resolve")
	}
	direct := &wal.WalError{Kind: wal.WalErrNotFound}
	if !errorsAs(direct, &we) || we != direct {
		t.Fatal("direct WalError must resolve with its target")
	}
	wrapped := fmt.Errorf("push: %w", &wal.WalError{Kind: wal.WalErrRefConflict})
	if !errorsAs(wrapped, &we) || we.Kind != wal.WalErrRefConflict {
		t.Fatalf("wrapped WalError must resolve: %+v", we)
	}
	// A nil error resolves to nothing (loop skipped, terminal false).
	we = nil
	if errorsAs(nil, &we) || we != nil {
		t.Fatal("nil error must not resolve")
	}
}

func TestServeStaticHeadError(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// A genuine miss is a 404.
	rec := httptest.NewRecorder()
	s.serveStatic(rec, httptest.NewRequest("GET", "http://x/gone", nil), "lfs/objects/ab/gone", "image/gif")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("static miss = %d, want 404", rec.Code)
	}
	// A store outage behind a static lane is a 503 (never 404/500).
	s.store = failingStore{ObjectStore: newFakeStore(), headErr: errors.New("disk gone")}
	rec = httptest.NewRecorder()
	s.serveStatic(rec, httptest.NewRequest("GET", "http://x/concepts/push.gif", nil), "lfs/objects/ab/push.gif", "image/gif")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("head outage = %d, want 503", rec.Code)
	}
}

func TestETagMatchHeaderTable(t *testing.T) {
	cases := []struct {
		header, version string
		want            bool
	}{
		{"*", "v", true},          // star matches anything
		{`"v"`, "v", true},        // strong
		{`W/"v"`, "v", true},      // weak prefix tolerated
		{`"a", "v"`, "v", true},   // list member
		{`"a"`, "v", false},       // list miss
		{"", "v", false},          // absent
		{`W/"other"`, "v", false}, // weak miss
	}
	for _, tc := range cases {
		if got := etagMatchHeader(tc.header, tc.version); got != tc.want {
			t.Errorf("etagMatchHeader(%q, %q) = %v, want %v", tc.header, tc.version, got, tc.want)
		}
	}
}

// --- LFS negatives ----------------------------------------------------------------------------

func TestLFSVerifyAuthGate(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// The verify lane authenticates before touching the body (write gate).
	req := httptest.NewRequest("POST", "http://x/o/r.git/info/lfs/objects/verify", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
	rec := httptest.NewRecorder()
	s.lfsVerify(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous verify = %d, want 401", rec.Code)
	}
}

func TestIsHexOidLFSTable(t *testing.T) {
	ok64 := strings.Repeat("ab12", 16)
	ok40 := strings.Repeat("ab12", 10)
	cases := []struct {
		in   string
		want bool
	}{
		{ok64, true},
		{ok40, true},
		{ok64[:63], false},
		{ok64[:63] + "G", false}, // non-hex nibble
		{"", false},
	}
	for _, tc := range cases {
		if got := isHexOidLFS(tc.in); got != tc.want {
			t.Errorf("isHexOidLFS(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestLFSUpstreamNegatives(t *testing.T) {
	s, _ := newTestServer(t, nil)
	ctx := context.Background()
	id := mustRepoID(t, "o/r")
	oid := strings.Repeat("a", 64)
	// Unparseable upstream → fail closed (no size, no panic).
	s.cfg.Upstream.Lfs = "://bad-upstream"
	if _, ok := s.lfsUpstreamResolve(ctx, id, oid); ok {
		t.Fatal("bad upstream URL must not resolve")
	}
	// Torn upstream batch → no resolution (never a half-parsed action).
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{torn`))
	}))
	defer up.Close()
	s.cfg.Upstream.Lfs = up.URL
	if _, ok := s.lfsUpstreamResolve(ctx, id, oid); ok {
		t.Fatal("torn upstream batch must not resolve")
	}
	// Read-through against a dead/broken upstream → 404 (no 500 leak).
	s.cfg.Upstream.Lfs = "http://127.0.0.1:1"
	rec := httptest.NewRecorder()
	s.lfsReadThrough(rec, httptest.NewRequest("GET", "http://x/o.wav", nil), id, oid, "lfs/objects/aa/"+oid)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("dead upstream read-through = %d, want 404", rec.Code)
	}
	s.cfg.Upstream.Lfs = "://bad-upstream"
	rec = httptest.NewRecorder()
	s.lfsReadThrough(rec, httptest.NewRequest("GET", "http://x/o.wav", nil), id, oid, "lfs/objects/aa/"+oid)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bad upstream read-through = %d, want 404", rec.Code)
	}
}

// --- oid/glob/error tables ----------------------------------------------------------------------

func TestOidAndGlobTables(t *testing.T) {
	for in, want := range map[string]bool{
		"": true, strings.Repeat("0", 40): true, strings.Repeat("0", 64): true,
		"abc": false, strings.Repeat("0", 39) + "1": false,
	} {
		if got := isZeroOid(in); got != want {
			t.Errorf("isZeroOid(%q) = %v, want %v", in, got, want)
		}
	}
	for _, tc := range []struct {
		globs []string
		id    string
		want  bool
	}{
		{[]string{"*"}, "o/r", true},
		{[]string{"o/*"}, "o/r", true},
		{[]string{"o/r"}, "o/r", true},
		{[]string{"x/*"}, "o/r", false},
		{nil, "o/r", false},
	} {
		if got := matchAnyGlob(tc.globs, tc.id); got != tc.want {
			t.Errorf("matchAnyGlob(%v, %q) = %v, want %v", tc.globs, tc.id, got, tc.want)
		}
	}
	var we *wal.WalError
	if errAsWalNotFound(errors.New("boom"), &we) {
		t.Fatal("plain error is not a wal not-found")
	}
	nf := &wal.WalError{Kind: wal.WalErrNotFound}
	if !errAsWalNotFound(nf, &we) || we != nf {
		t.Fatal("direct WalErrNotFound must resolve")
	}
	if errAsWalNotFound(fmt.Errorf("open: %w", &wal.WalError{Kind: wal.WalErrRefConflict}), &we) {
		t.Fatal("wrapped non-notfound must not resolve")
	}
	if !errAsWalNotFound(fmt.Errorf("open: %w", nf), &we) {
		t.Fatal("wrapped not-found must resolve")
	}
	if errAsWalNotFound(nil, &we) {
		t.Fatal("nil error is not a wal not-found")
	}
}

// --- routing edges ---------------------------------------------------------------------------------

func TestRouterAPIUnwired(t *testing.T) {
	s, h := newTestServer(t, nil)
	s.api = nil // no API seam: the lanes fail closed with 503
	for _, path := range []string{"http://x/api", "http://x/api-browser/v1/repos"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "api not wired") {
			t.Fatalf("unwired %s = %d %q", path, rec.Code, rec.Body.String())
		}
	}
}

func TestRepoDispatchEncodingEdges(t *testing.T) {
	_, h := newTestServer(t, nil)
	// An owner outside the charset → 404 before any gating or sync work.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/a%20b", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("spaced owner = %d, want 404", rec.Code)
	}
	// A request with no path at all → the paranoia guard 404s.
	s, _ := newTestServer(t, nil)
	rec = httptest.NewRecorder()
	s.repoDispatch(rec, &http.Request{Method: "GET", URL: &url.URL{Path: ""}, Host: "x", Header: make(http.Header)})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty path = %d, want 404", rec.Code)
	}
}

type claimZzz struct{ claimed int }

func (c *claimZzz) HandleRepo(w http.ResponseWriter, r *http.Request, id git.RepoId, sub []string) bool {
	if len(sub) > 0 && sub[0] == "zzz" {
		c.claimed++
		w.WriteHeader(http.StatusTeapot)
		return true
	}
	return false
}

func TestRepoDispatchDefaultExtraClaimed(t *testing.T) {
	s, h := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	fe.placement = Placement{Serve: true}
	stub := &claimZzz{}
	s.ChainRepo(stub)
	// A claimed non-UI family is answered by the extra on the default
	// branch (core untouched, no SPA shell for byte shapes).
	req := httptest.NewRequest("GET", "http://x/o/r/zzz/v1/download", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot || stub.claimed != 1 {
		t.Fatalf("default extra = %d claimed=%d", rec.Code, stub.claimed)
	}
}

// --- SSH transport errors ---------------------------------------------------------------------------

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }

type brokeRepoEngine struct {
	*fakeEngine
	t    *testing.T
	root string
}

func (b *brokeRepoEngine) Repo(ctx context.Context, id git.RepoId, create bool, format git.ObjectFormat) (*git.LocalRepo, error) {
	// A serving copy whose packed-refs is a directory: every ref read
	// fails, so the advertisement cannot be built (git edge2 pattern).
	repo, err := git.InitLocalRepo(b.root, id, format)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(repo.Path, "packed-refs"), 0o755); err != nil {
		return nil, err
	}
	return repo, nil
}

func brokeSSHServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.SSH.Listen = "127.0.0.1:0"
	eng := &brokeRepoEngine{fakeEngine: &fakeEngine{exists: true, placement: Placement{Serve: true}}, t: t, root: t.TempDir()}
	return New(Options{Config: cfg, Store: newFakeStore(), Engine: eng, DataDir: t.TempDir(), Log: testLogger(t)})
}

func TestSSHReceivePackClientWentAway(t *testing.T) {
	s := sshGateServer(t, &fakeEngine{exists: true, placement: Placement{Serve: true}}, nil)
	ctx := context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
	// The client disconnects mid-advertisement → a clean transport error
	// naming the disconnect (no hang, no panic).
	err := s.SSHReceivePack(ctx, mustRepoID(t, "o/r"), "ada", strings.NewReader(""), errWriter{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "client went away") {
		t.Fatalf("went-away = %v", err)
	}
}

func TestSSHReceivePackBadAdvertisement(t *testing.T) {
	s := brokeSSHServer(t)
	ctx := context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
	// A broken serving copy fails the advertisement → unavailable (the
	// client retries elsewhere; nothing half-written).
	err := s.SSHReceivePack(ctx, mustRepoID(t, "o/r"), "ada", strings.NewReader(""), io.Discard, io.Discard)
	if !errors.Is(err, sshd.ErrUnavailable) {
		t.Fatalf("bad advertisement = %v, want ErrUnavailable", err)
	}
}

// --- metrics exposition -------------------------------------------------------------------------------

func TestRenderLabelsAndHistogram(t *testing.T) {
	if got := renderLabels("method=GET,code=200,"); got != `{method="GET",code="200"}` {
		t.Fatalf("labels = %q", got)
	}
	if got := renderLabels(`m=a"b\\c,`); got != `{m="a\"b\\\\c"}` {
		t.Fatalf("label escaping = %q", got)
	}
	r := newRegistry()
	r.Histogram("t_hist", "test histogram", []float64{10}).Observe(5, "m", "get")
	r.Histogram("t_hist", "test histogram", []float64{10}).Observe(50, "m", "get")
	out := r.Render()
	if !strings.Contains(out, `t_hist_count{m="get"} 2`) || !strings.Contains(out, "t_hist_sum") {
		t.Fatalf("histogram render = %q", out)
	}
}

func TestGitInfoRefsAdvertiseFailure(t *testing.T) {
	s := brokeSSHServer(t)
	// The serving copy is unreadable → the advertisement cannot be built:
	// 503 + Retry-After (never a hang, never a half-written advert).
	req := httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	rec := httptest.NewRecorder()
	s.gitInfoRefs(rec, req, mustRepoID(t, "o/r"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("advertise failure = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("unavailable advertisement must carry Retry-After")
	}
}

// upstreamBatchServer answers the LFS batch endpoint with a downloadable
// object of ln bytes while failing every download GET with status.
func upstreamBatchServer(t *testing.T, oid string, ln int64, dlStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/batch") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"objects":[{"oid":%q,"size":%d}]}`, oid, ln)
			return
		}
		w.WriteHeader(dlStatus)
	}))
}

func TestLFSReadThroughDownloadFailures(t *testing.T) {
	s, _ := newTestServer(t, nil)
	ctx := context.Background()
	id := mustRepoID(t, "o/r")
	oid := strings.Repeat("b", 64)
	// Resolve succeeds but the object download is a 500 → 404 (the
	// resolve-fail 404 above is a different statement; both map fail-closed).
	up := upstreamBatchServer(t, oid, 4, http.StatusInternalServerError)
	defer up.Close()
	s.cfg.Upstream.Lfs = up.URL
	rec := httptest.NewRecorder()
	s.lfsReadThrough(rec, httptest.NewRequest("GET", "http://x/o.wav", nil).WithContext(ctx), id, oid, "lfs/objects/bb/"+oid)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("500 download = %d, want 404", rec.Code)
	}
	// An oid that cannot form a download URL (control byte in the path)
	// → 404 before any GET issues.
	spaceOid := "zz\x7ftop"
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in lfsBatchReq
		_ = json.NewDecoder(r.Body).Decode(&in)
		oidBack := spaceOid
		if len(in.Objects) > 0 {
			oidBack = in.Objects[0].OID
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"objects":[{"oid":%q,"size":4}]}`, oidBack)
	}))
	defer up2.Close()
	s.cfg.Upstream.Lfs = up2.URL
	rec = httptest.NewRecorder()
	s.lfsReadThrough(rec, httptest.NewRequest("GET", "http://x/o.wav", nil).WithContext(ctx), id, spaceOid, "lfs/objects/zz/"+spaceOid)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unbuildable download = %d, want 404", rec.Code)
	}
}

// --- SSH key registry store faults ---------------------------------------------------

// faultSeqStore fails Get always (when getErr set) and fails the Nth Put
// onwards (failAfter <= 0 with putErr set fails every Put).
type faultSeqStore struct {
	store.ObjectStore
	getErr    error
	putErr    error
	failAfter int
	putCalls  int
}

func (p *faultSeqStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if p.getErr != nil {
		return nil, p.getErr
	}
	return p.ObjectStore.Get(ctx, key, opts)
}

func (p *faultSeqStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	p.putCalls++
	if p.putErr != nil && p.putCalls > p.failAfter {
		return store.ObjectMeta{}, p.putErr
	}
	return p.ObjectStore.Put(ctx, key, body, opts)
}

func TestRegistryStoreFaults(t *testing.T) {
	r, ctx := registryFor(t, nil)
	keyID := fpID(fingerprintOf(registryTestKey))

	// A torn k-doc surfaces its decode error on direct load (List skips
	// torn entries instead — the k-doc is the truth).
	if _, err := r.st.Put(ctx, keyDocKey(keyID), store.PutBody{Bytes: []byte("{not json")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.get(ctx, keyID); err == nil {
		t.Fatal("torn k-doc must fail the load")
	}

	// A dead store fails the add (the k-doc write is the commit point).
	dead := &faultSeqStore{ObjectStore: store.NewMemory(), putErr: errors.New("bucket down")}
	rDead := &SSHKeyRegistry{st: dead, auth: r.auth, log: r.log}
	if _, err := rDead.Add(ctx, "ada", registryTestKey, "one"); err == nil ||
		!strings.Contains(err.Error(), "bucket down") {
		t.Fatalf("dead-store add = %v", err)
	}

	// Index write fails after the k-doc landed → the k-doc rolls back so
	// a failed add leaves nothing behind.
	flaky := &faultSeqStore{ObjectStore: store.NewMemory(), putErr: errors.New("index down"), failAfter: 1}
	rFlaky := &SSHKeyRegistry{st: flaky, auth: r.auth, log: r.log}
	if _, err := rFlaky.Add(ctx, "ada", registryTestKey, "one"); err == nil ||
		!strings.Contains(err.Error(), "index down") {
		t.Fatalf("flaky add = %v", err)
	}
	if _, err := flaky.ObjectStore.Get(ctx, keyDocKey(keyID), store.GetOptions{}); !store.IsNotFound(err) {
		t.Fatalf("rolled-back k-doc must be gone: %v", err)
	}

	// A listed key that can no longer be read surfaces (never a partial
	// list passed off as complete).
	r2, ctx2 := registryFor(t, nil)
	if _, err := r2.Add(ctx2, "ada", registryTestKey, "one"); err != nil {
		t.Fatal(err)
	}
	blind := &faultSeqStore{ObjectStore: r2.st, getErr: errors.New("read down")}
	rBlind := &SSHKeyRegistry{st: blind, auth: r2.auth, log: r2.log}
	if _, err := rBlind.List(ctx2, "ada"); err == nil || !strings.Contains(err.Error(), "read down") {
		t.Fatalf("blind list = %v", err)
	}
}
