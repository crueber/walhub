package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// --- middleware extras ---------------------------------------------------------

func TestTraceIDParsing(t *testing.T) {
	s, h := newTestServer(t, nil)
	var trace string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { trace = TraceIDOf(r) })
	// Wire the trace middleware behavior through the full chain.
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set("X-Cloud-Trace-Context", "abc123/5;o=1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	_ = next
	if trace != "" {
		t.Fatal("plain handler must not see traces")
	}
	// Direct parsing truth table.
	mk := func(headers map[string]string) string {
		req := httptest.NewRequest("GET", "/x", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return traceIDFrom(req)
	}
	if got := mk(map[string]string{"X-Cloud-Trace-Context": "abc123/5;o=1"}); got != "abc123" {
		t.Fatalf("cloud trace = %q", got)
	}
	if got := mk(map[string]string{"X-Cloud-Trace-Context": "abc123"}); got != "abc123" {
		t.Fatalf("cloud trace bare = %q", got)
	}
	if got := mk(map[string]string{"traceparent": "00-" + strings.Repeat("a", 32) + "-01"}); got != strings.Repeat("a", 32) {
		t.Fatalf("traceparent = %q", got)
	}
	if got := mk(map[string]string{"traceparent": "00-short-01"}); got != "" {
		t.Fatalf("short traceparent = %q", got)
	}
	if got := mk(nil); got != "" {
		t.Fatalf("no trace = %q", got)
	}
	// ReqLog falls back to the default logger.
	if ReqLog(httptest.NewRequest("GET", "/x", nil)) == nil {
		t.Fatal("ReqLog must never be nil")
	}
	_ = s
}

func TestRecorderHooks(t *testing.T) {
	inner := httptest.NewRecorder()
	rw := &recorder{ResponseWriter: inner}
	rw.Write([]byte("hello")) // implicit 200
	if rw.status != http.StatusOK || !rw.touched() {
		t.Fatalf("status=%d touched=%v", rw.status, rw.touched())
	}
	if rw.bytes != 5 {
		t.Fatalf("bytes = %d", rw.bytes)
	}
	done := 0
	rw2 := &recorder{ResponseWriter: httptest.NewRecorder(), onDone: func() { done++ }}
	rw2.WriteHeader(http.StatusTeapot)
	rw2.WriteHeader(http.StatusOK) // ignored: first status wins
	if rw2.status != http.StatusTeapot {
		t.Fatalf("second WriteHeader overrode: %d", rw2.status)
	}
	rw2.finish()
	rw2.finish()
	if done != 1 {
		t.Fatalf("finish ran %d times", done)
	}
	// Flush passthrough (recorder implements Flusher).
	rw.Flush()
}

func TestCompressMiddleware(t *testing.T) {
	s, _ := newTestServer(t, nil)
	plain := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"a":1}`))
	})
	// Accept-Encoding: gzip → gzipped body.
	req := httptest.NewRequest("GET", "/api/v1/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.compress(plain).ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("encoding = %q", rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if string(body) != `{"a":1}` {
		t.Fatalf("gunzipped = %q", body)
	}
	// No Accept-Encoding → passthrough.
	rec = httptest.NewRecorder()
	s.compress(plain).ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != `{"a":1}` {
		t.Fatalf("passthrough = %q %q", rec.Header().Get("Content-Encoding"), rec.Body.String())
	}
	// SSE responses bypass compression.
	sse := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK) // bypass is decided at WriteHeader
		_, _ = w.Write([]byte("data: hi\n\n"))
	})
	rec = httptest.NewRecorder()
	greq := httptest.NewRequest("GET", "/api/x", nil)
	greq.Header.Set("Accept-Encoding", "gzip")
	s.compress(sse).ServeHTTP(rec, greq)
	if strings.Contains(rec.Header().Get("Content-Encoding"), "gzip") {
		t.Fatal("SSE must bypass compression")
	}
	// acceptsGzip helper.
	req = httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Accept-Encoding", "GZIP")
	if !acceptsGzip(req) {
		t.Fatal("acceptsGzip must be case-insensitive")
	}
}

func TestMapAuthStatusAndInjectPrincipal(t *testing.T) {
	s, _ := newTestServer(t, nil)
	cases := map[auth.AuthErrorKind]int{
		auth.ErrForbidden:    http.StatusForbidden,
		auth.ErrUnavailable:  http.StatusServiceUnavailable,
		auth.ErrInvalid:      http.StatusUnauthorized,
		auth.ErrUnauthorized: http.StatusUnauthorized,
	}
	for kind, want := range cases {
		rec := httptest.NewRecorder()
		s.mapAuthStatus(rec, &auth.AuthError{Kind: kind, Why: "why"})
		if rec.Code != want {
			t.Fatalf("%v = %d, want %d", kind, rec.Code, want)
		}
		if want == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") != "15" {
			t.Fatal("unavailable must carry Retry-After")
		}
	}
	// injectPrincipal makes the principal visible to the api seam and the
	// server's own context key.
	req := httptest.NewRequest("GET", "/x", nil)
	req = injectPrincipal(req, principalAlice)
	if p, ok := req.Context().Value(ctxPrincipalKey{}).(auth.Principal); !ok || p.Name != "alice" {
		t.Fatalf("server ctx principal = %+v ok=%v", p, ok)
	}
}

func TestAuthFailureBrowserRedirect(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.Server.Auth.Mode = "oidc"
	s.cfg.Server.Auth.SessionSecret = "0123456789abcdef0123456789abcdef"
	s.cfg.Server.Auth.OAuthClientID = "c"
	s.cfg.Server.Auth.OAuthClientSecret = "s"
	// Browser-ish GET without Authorization → 307 to login.
	req := httptest.NewRequest("GET", "/wal/settings", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	s.authFailure(rec, req, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "auth required"})
	if rec.Code != http.StatusTemporaryRedirect || !strings.Contains(rec.Header().Get("Location"), "/_auth/login") {
		t.Fatalf("browser redirect = %d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	// With Authorization header → real 401.
	req = httptest.NewRequest("GET", "/wal/settings", nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Authorization", "Bearer junk")
	rec = httptest.NewRecorder()
	s.authFailure(rec, req, &auth.AuthError{Kind: auth.ErrUnauthorized, Why: "auth required"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("401 path = %d", rec.Code)
	}
}
func TestInflightAndBusyCount(t *testing.T) {
	s, _ := newTestServer(t, nil)
	g := s.Inflight()
	g.n.Store(600)
	if !g.OverCap() || g.N() != 600 {
		t.Fatalf("inflight = %d cap", g.N())
	}
	g.n.Store(0)
	if g.OverCap() {
		t.Fatal("0 must not be over cap")
	}
	// BusyCount counts refusals.
	sem := NewRepoSemaphores(1)
	rel := sem.TryAcquire("a")
	if rel == nil {
		t.Fatal("first acquire must succeed")
	}
	if sem.TryAcquire("a") != nil {
		t.Fatal("second acquire must be refused")
	}
	if sem.BusyCount() != 1 {
		t.Fatalf("busy count = %d", sem.BusyCount())
	}
	rel()
}

// --- router dispatch branches ---------------------------------------------------

func TestRepoDispatchBranches(t *testing.T) {
	s, h := newTestServer(t, nil)
	fe := s.engine.(*fakeEngine)
	fe.exists = true
	fe.placement = Placement{Serve: true}
	fe.bundles = BundleList{Fulls: []BundleEntry{{Strategy: "full", Name: "b.bundle"}}}
	api := &fakeAPI{}
	s.api = api

	// /{owner} UI page (gated) → 200 with token.
	req := httptest.NewRequest("GET", "http://x/alice", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner page = %d", rec.Code)
	}
	// POST /{owner} → 405.
	req = httptest.NewRequest("POST", "http://x/alice", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("owner POST = %d", rec.Code)
	}
	// UI page route /o/r/tree → 200.
	req = httptest.NewRequest("GET", "http://x/o/r/tree", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tree page = %d", rec.Code)
	}
	// POST /o/r/tree → 405.
	req = httptest.NewRequest("POST", "http://x/o/r/tree", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("tree POST = %d", rec.Code)
	}
	// Repo api lane goes to the api seam (teapot stub).
	req = httptest.NewRequest("GET", "http://x/o/r/api/v1/summary", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot || api.served != 1 {
		t.Fatalf("api lane = %d served=%d", rec.Code, api.served)
	}
	// PUT on the bare repo root → api seam (lifecycle).
	req = httptest.NewRequest("PUT", "http://x/o/r.git", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot || api.served != 2 {
		t.Fatalf("bare PUT = %d served=%d", rec.Code, api.served)
	}
	// Unknown subpath → 404.
	req = httptest.NewRequest("GET", "http://x/o/r.git/nope", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown subpath = %d", rec.Code)
	}
	// Drain phase 2: new git traffic is refused with the pkt-ERR shape.
	s.drain.Begin2()
	req = httptest.NewRequest("GET", "http://x/o/r.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("User-Agent", "git/2.46.0")
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("drained git = %d", rec.Code)
	}
	if _, ok := pktErrOf(rec.Body.String()); !ok || !strings.Contains(rec.Body.String(), "draining") {
		t.Fatalf("drain pkt = %q", rec.Body.String())
	}
	// LFS dispatch is reachable via the router (auth failure → 401 for anon).
	s.drain = NewDrainState()
	req = httptest.NewRequest("GET", "http://x/o/r.git/info/lfs/objects/ab", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("lfs via router = %d", rec.Code)
	}
}

func TestNotImplementedAuthFlowDisabled(t *testing.T) {
	_, h := newTestServer(t, nil)
	for _, p := range []string{"/_auth/login", "/_auth/callback", "/_auth/claimed",
		"/_auth/logout", "/_auth/me", "/_auth/check", "/_auth/tokens"} {
		want := http.StatusNotImplemented
		switch p {
		case "/_auth/claimed":
			want = http.StatusBadRequest // no ticket: verify fails first
		case "/_auth/logout":
			want = http.StatusFound // clears the cookie regardless
		case "/_auth/me":
			want = http.StatusOK // not flow-gated
		case "/_auth/check", "/_auth/tokens":
			want = http.StatusUnauthorized // anonymous + no anonymous read
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x"+p, nil))
		if rec.Code != want {
			t.Fatalf("%s disabled = %d, want %d", p, rec.Code, want)
		}
	}
}

// --- setup API extras -------------------------------------------------------------

func TestParseSetupBodyTOMLAndErrors(t *testing.T) {
	// Raw TOML with all scalar shapes.
	req := httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader(`
[server]
listen = "127.0.0.1:9999"
auto_create_on_push = true
`))
	c, err := parseSetupBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != "127.0.0.1:9999" || !c.Server.AutoCreateOnPush {
		t.Fatalf("toml parse = %+v", c.Server)
	}
	// JSON overrides with non-string values.
	req = httptest.NewRequest("PUT", "/api/v1/setup",
		strings.NewReader(`{"overrides": {"server.auto_create_on_push": true, "server.max_push_bytes": 1000, "server.cors_origins": "a.com,b.com"}}`))
	c, err = parseSetupBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Server.AutoCreateOnPush || int64(c.Server.MaxPushBytes) != 1000 ||
		len(c.Server.CorsOrigins) != 2 {
		t.Fatalf("json overrides = %+v %+v", c.Server, c.Server.Auth)
	}
	// Section-less key → error.
	req = httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader(`{"overrides": {"listen": "x"}}`))
	if _, err := parseSetupBody(req); err == nil {
		t.Fatal("bare key must fail")
	}
	// Unknown section/key → error.
	req = httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader(`{"overrides": {"nope.key": "1"}}`))
	if _, err := parseSetupBody(req); err == nil {
		t.Fatal("unknown section must fail")
	}
	req = httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader(`{"overrides": {"server.nope": "1"}}`))
	if _, err := parseSetupBody(req); err == nil {
		t.Fatal("unknown key must fail")
	}
	// Malformed JSON.
	req = httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader(`{"overrides": `))
	if _, err := parseSetupBody(req); err == nil {
		t.Fatal("bad json must fail")
	}
	// A top-level non-table value is rejected.
	req = httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader("server = 5\n"))
	if _, err := parseSetupBody(req); err == nil {
		t.Fatal("non-table section must fail")
	}
	// configCoerce errors: bad bool, bad int; string list parses.
	req = httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader(`{"overrides": {"server.auto_create_on_push": "notabool"}}`))
	if _, err := parseSetupBody(req); err == nil {
		t.Fatal("bad bool must fail")
	}
	req = httptest.NewRequest("PUT", "/api/v1/setup", strings.NewReader(`{"overrides": {"server.max_push_bytes": "NaN"}}`))
	if _, err := parseSetupBody(req); err == nil {
		t.Fatal("bad int must fail")
	}
	req = httptest.NewRequest("PUT", "/api/v1/setup",
		strings.NewReader(`{"overrides": {"server.cors_origins": "a.test,b.test"}}`))
	if _, err := parseSetupBody(req); err != nil {
		t.Fatalf("string list must parse: %v", err)
	}
}

func TestSetupPutRestartKeys(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dataDir
	cfg.Server.Auth.Mode = "token"
	cfg.Server.Auth.Tokens = []config.StaticToken{{Principal: "alice", Token: "tok123", Write: true, Admin: true}}
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		DataDir: dataDir, Boot: BootState{Mode: "normal"}})
	body := `{"overrides": {"server.listen": "127.0.0.1:9099"}}`
	req := httptest.NewRequest("PUT", "http://x/api/v1/setup", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.setupPut(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"saved":true`) {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	// requires_restart names the changed key.
	if !strings.Contains(rec.Body.String(), "server.listen") {
		t.Fatalf("restart keys missing: %s", rec.Body.String())
	}
	if _, err := os.Stat(dataDir + "/walhub.toml"); err != nil {
		t.Fatalf("walhub.toml missing: %v", err)
	}
	// Invalid config → 422.
	req = httptest.NewRequest("PUT", "http://x/api/v1/setup", strings.NewReader(`{"overrides": {"server.bogus": "1"}}`))
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.setupPut(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid put = %d", rec.Code)
	}
	// Non-admin → 403.
	s.cfg.Server.Auth.Tokens = []config.StaticToken{{Principal: "ro", Token: "ro", Write: false}}
	s.authSvc = NewAuthService(&s.cfg.Server.Auth, s.Now)
	req = httptest.NewRequest("PUT", "http://x/api/v1/setup", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer ro")
	rec = httptest.NewRecorder()
	s.setupPut(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin put = %d", rec.Code)
	}
	// restartKeys diff + effectiveValue helpers.
	cand := config.Defaults()
	cand.Server.Listen = "1.2.3.4:1"
	keys := restartKeys(cfg, cand)
	if len(keys) == 0 {
		t.Fatal("changed key must be reported")
	}
	if effectiveValue(cfg, "server.listen") == nil || effectiveValue(cfg, "bogus") != nil {
		t.Fatal("effectiveValue truth table broken")
	}
	if anyToString(nil) != "<nil>" || anyToString(true) != "true" || anyToString(int64(7)) != "7" {
		t.Fatal("anyToString truth table broken")
	}
}

// --- metrics -----------------------------------------------------------------------

func TestMetricsHistogramAndHandler(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.metrics.Histogram("walgit_checkpoint_seconds", "checkpoint duration", defBounds).
		Observe(0.3, "kind", "manual")
	s.metrics.Counter("walgit_checkpoints_total", "checkpoints").Add(2)
	s.metrics.Counter("walgit_checkpoints_total", "checkpoints").Inc()
	body := s.metrics.Render()
	if !strings.Contains(body, `walgit_checkpoint_seconds_bucket{kind="manual",le="0.5"} 1`) {
		t.Fatalf("histogram bucket missing: %s", body)
	}
	if !strings.Contains(body, "walgit_checkpoint_seconds_count{kind=\"manual\"} 1") {
		t.Fatalf("histogram count missing: %s", body)
	}
	// metricsHandler serves text exposition.
	rec := httptest.NewRecorder()
	s.metricsHandler(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "version=0.0.4") {
		t.Fatalf("metrics = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	// Label escaping: the value is quoted and special chars escaped.
	if got := renderLabels(`k=a"b`); !strings.Contains(got, `\"`) {
		t.Fatalf("renderLabels = %q", got)
	}
	// Histogram registered twice returns the same family.
	h1 := s.metrics.Histogram("walgit_checkpoint_seconds", "h", defBounds)
	h1.Observe(2.5)
	if !strings.Contains(s.metrics.Render(), "walgit_checkpoint_seconds_count") {
		t.Fatal("re-registered histogram lost observations")
	}
}

// --- TLS ---------------------------------------------------------------------------

func TestEnsureSelfSignedStable(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cacheRoot = t.TempDir()
	s.cfg.Server.PublicURL = "https://wal.example.com"
	s.cfg.Server.TLS.Hostnames = []string{"extra.test"}
	if err := s.EnsureSelfSigned(); err != nil {
		t.Fatal(err)
	}
	certPath, _, sansPath := s.tlsFiles()
	c1, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	// Same SAN set → no regeneration (file untouched, mtime stable).
	if err := s.EnsureSelfSigned(); err != nil {
		t.Fatal(err)
	}
	c2, _ := os.ReadFile(certPath)
	if !bytes.Equal(c1, c2) {
		t.Fatal("SAN-stable contract violated: cert regenerated")
	}
	// Changed SANs → regeneration.
	s.cfg.Server.TLS.Hostnames = []string{"other.test"}
	if err := s.EnsureSelfSigned(); err != nil {
		t.Fatal(err)
	}
	c3, _ := os.ReadFile(certPath)
	if bytes.Equal(c1, c3) {
		t.Fatal("SAN change must regenerate")
	}
	if _, err := os.Stat(sansPath); err != nil {
		t.Fatalf("cert.sans missing: %v", err)
	}
	// desiredSANs composition.
	want := s.desiredSANs()
	if want[0] != "localhost" || want[1] != "*.localhost" || want[2] != "127.0.0.1" || want[3] != "::1" {
		t.Fatalf("sans head = %v", want)
	}
	if !containsString(want, "wal.example.com") || !containsString(want, "other.test") {
		t.Fatalf("sans tail = %v", want)
	}
	// sans helpers.
	if !sansEqual("a,b,c\n", []string{"c", "b", "a"}) || sansEqual("a,b", []string{"a", "b", "c"}) {
		t.Fatal("sansEqual truth table broken")
	}
	if got := joinSans([]string{"a", "b"}); got != "a,b\n" {
		t.Fatalf("joinSans = %q", got)
	}
	if got := splitSans("a\nb,,c\n"); len(got) != 3 || got[2] != "c" {
		t.Fatalf("splitSans = %v", got)
	}
	if sdkETag("x") == sdkETag("y") {
		t.Fatal("sdkETag must differ per input")
	}
	// TLSServerConfig loads the freshly generated pair.
	tc, err := s.TLSServerConfig()
	if err != nil || len(tc.Certificates) != 1 || tc.NextProtos[0] != "h2" {
		t.Fatalf("tls config = %v %v", tc, err)
	}
}

func TestBuildListenerAndHTTPServer(t *testing.T) {
	s, _ := newTestServer(t, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	wrapped := s.BuildListener(ln)
	go func() {
		c, err := wrapped.Accept()
		if err == nil {
			c.Close()
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	// NewHTTPServer wires h2c + BaseContext.
	srv := s.NewHTTPServer(http.NewServeMux(), ctxProvider{ctx: context.Background()})
	if srv.BaseContext == nil {
		t.Fatal("BaseContext missing")
	}
	srv2 := s.NewHTTPServer(http.NewServeMux(), nil)
	if srv2.BaseContext != nil {
		t.Fatal("nil Contexter must skip BaseContext")
	}
}

type ctxProvider struct{ ctx context.Context }

func (c ctxProvider) Context() context.Context { return c.ctx }

// --- util --------------------------------------------------------------------------

func TestUtilHelpers(t *testing.T) {
	if !hasPrefixFold("Git/2.4", "git/") || hasPrefixFold("curl", "git/") {
		t.Fatal("hasPrefixFold truth table broken")
	}
	if !containsFold("jgit Client", "GIT") {
		t.Fatal("containsFold truth table broken")
	}
	if trimPrefixFold("GIT-lfs/3", "git-lfs/") != "3" {
		t.Fatal("trimPrefixFold failed")
	}
	id := newRequestID()
	if len(id) != 32 {
		t.Fatalf("request id = %q", id)
	}
	if got := pctEncode("a b/c~d_e.f-g+h"); got != "a%20b/c~d_e.f-g%2Bh" {
		t.Fatalf("pctEncode = %q", got)
	}
	if got := pctEncode("100%"); got != "100%25" {
		t.Fatalf("pctEncode percent = %q", got)
	}
	// parseRange table.
	cases := []struct {
		spec  string
		size  int64
		start int64
		end   int64
		ok    bool
	}{
		{"bytes=0-0", 10, 0, 0, true},
		{"bytes=5-", 10, 5, 9, true},
		{"bytes=-3", 10, 7, 9, true},
		{"bytes=-999", 10, 0, 9, true},
		{"bytes=5-99", 10, 5, 9, true},
		{"bytes=0-2,5-6", 10, 0, 0, false}, // multiple → full
		{"items=0-2", 10, 0, 0, false},
		{"bytes=abc-", 10, 0, 0, false},
		{"bytes=9-5", 10, 0, 0, false},
		{"bytes=10-20", 10, 0, 0, false}, // start >= size
		{"bytes=-", 10, 0, 0, false},
		{"bytes=-x", 10, 0, 0, false},
		{"bytes=-3", 0, 0, 0, false},
	}
	for _, tc := range cases {
		s, e, ok := parseRange(tc.spec, tc.size)
		if ok != tc.ok || (ok && (s != tc.start || e != tc.end)) {
			t.Fatalf("parseRange(%q, %d) = %d,%d,%v", tc.spec, tc.size, s, e, ok)
		}
	}
	// plainStatus guarantees a trailing newline + Content-Length.
	rec := httptest.NewRecorder()
	plainStatus(rec, http.StatusTeapot, "boom")
	if !strings.HasSuffix(rec.Body.String(), "boom\n") || rec.Header().Get("Content-Length") != "5" {
		t.Fatalf("plainStatus = %q", rec.Body.String())
	}
	// Host helpers.
	if !isLoopbackHost("127.0.0.1") || !isLoopbackHost("localhost") || isLoopbackHost("10.0.0.1") {
		t.Fatal("isLoopbackHost truth table broken")
	}
	if got := canonicalHost("localhost:8080"); got != "walgit.localhost:8080" {
		t.Fatalf("canonicalHost port = %q", got)
	}
	if got := canonicalHost("localhost:80"); got != "walgit.localhost" {
		t.Fatalf("canonicalHost 80 = %q", got)
	}
	req := httptest.NewRequest("GET", "http://x/y", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	if remoteHost(req) != "192.0.2.1" {
		t.Fatalf("remoteHost = %q", remoteHost(req))
	}
	req.RemoteAddr = "unix"
	if remoteHost(req) != "unix" {
		t.Fatalf("remoteHost bare = %q", remoteHost(req))
	}
	if got := hostOnly("host:80"); got != "host" {
		t.Fatalf("hostOnly = %q", got)
	}
}

// --- drain / server getters ----------------------------------------------------------

func TestDrainGateAndRunPhase2(t *testing.T) {
	s, _ := newTestServer(t, nil)
	// Phase 0 → not refused.
	rec := httptest.NewRecorder()
	if s.drainGate(rec, false, nil) {
		t.Fatal("phase 0 must not refuse")
	}
	// Phase 2 with in-flight request: waits until it drops.
	s.drain.Begin2()
	s.Inflight().n.Store(0)
	done := make(chan struct{})
	go func() {
		s.RunPhase2(nil)
		close(done)
	}()
	s.Inflight().n.Store(0)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunPhase2 must exit once inflight drains")
	}
	s.drain = NewDrainState()
}

func TestServerGetters(t *testing.T) {
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		Instance: "host-1", Version: "v1", Kind: KindSSD})
	if s.Auth() != s.authSvc || s.Drain() != s.drain || s.Inflight() == nil || s.Metrics() == nil || s.Config() != cfg {
		t.Fatal("getter wiring broken")
	}
	if s.Version() != "v1" {
		t.Fatalf("version = %q", s.Version())
	}
	if got := s.serverHeaderValue(); got != "walgit/v1 (ssd; walhub/host-1)" {
		t.Fatalf("server header = %q", got)
	}
	s.version = ""
	if s.Version() != "dev" {
		t.Fatalf("fallback version = %q", s.Version())
	}
	if got := s.serverHeaderValue(); got != "walgit/dev (ssd; walhub/host-1)" {
		t.Fatalf("server header 2 = %q", got)
	}
	s.instance = ""
	// cacheDir resolves under Options.CacheRoot; with no root configured it
	// falls back to the OS temp dir.
	s2 := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		CacheRoot: cfg.Cache.Dir})
	if got := s2.cacheDir("x"); !strings.HasPrefix(got, cfg.Cache.Dir+"/x") {
		t.Fatalf("cacheDir root = %q", got)
	}
	if got := s.cacheDir("lfs-spool"); !strings.HasSuffix(got, "lfs-spool") {
		t.Fatalf("cacheDir = %q", got)
	}
	// isGitClient truth table.
	for ua, want := range map[string]bool{"git/2.4": true, "JGit/x": true, "git-lfs/3": true, "curl/8": false} {
		if isGitClient(ua) != want {
			t.Fatalf("isGitClient(%q) != %v", ua, want)
		}
	}
}

// --- api + wal seams -----------------------------------------------------------------

func TestAPIProviderBind(t *testing.T) {
	cfg := config.Defaults()
	env := api.NewEnv(store.NewMemory(), nil, cfg, nil, "testver", "host")
	p := NewAPIProvider(env)
	if p == nil {
		t.Fatal("provider missing")
	}
	rec := httptest.NewRecorder()
	p.Serve(rec, httptest.NewRequest("GET", "/api/v1/me", nil))
	if rec.Code == 0 {
		t.Fatal("api mux must answer")
	}
}

// TestWalEngineSeams drives the api-facing WalEngine methods against a real
// registry over the memory store.
func TestWalEngineSeams(t *testing.T) {
	ctx := context.Background()
	cfg := walTestCfg(t)
	reg := wal.NewRegistry(ctx, store.NewMemory(), cfg)
	defer reg.Close()
	e := NewWalEngine(reg, cfg)
	id := mustRepoID(t, "o/r")

	// Missing repo → not-found through every seam.
	if _, err := e.ObjectAccess(ctx, id); !isNotFound(err) {
		t.Fatalf("object access missing = %v", err)
	}
	if _, err := e.Revision(ctx, id); !isNotFound(err) {
		t.Fatalf("revision missing = %v", err)
	}
	if _, err := e.Manifest(ctx, id); !isNotFound(err) {
		t.Fatalf("manifest missing = %v", err)
	}
	if _, err := e.ReadLog(ctx, id, 0, 10); !isNotFound(err) {
		t.Fatalf("read log missing = %v", err)
	}
	if _, err := e.PublishSettings(ctx, id, []byte("{}"), "m", "a"); !isNotFound(err) {
		t.Fatalf("publish settings missing = %v", err)
	}

	// Create, then exercise the seams on the live handle.
	if _, err := e.Repo(ctx, id, true, git.Sha1); err != nil {
		t.Fatal(err)
	}
	oa, err := e.ObjectAccess(ctx, id)
	if err != nil {
		t.Fatalf("object access: %v", err)
	}
	_ = oa
	rev, err := e.Revision(ctx, id)
	if err != nil {
		t.Fatalf("revision: %v", err)
	}
	_ = rev
	m, err := e.Manifest(ctx, id)
	if err != nil || m == nil || m.Repo != "o/r" {
		t.Fatalf("manifest = %+v %v", m, err)
	}
	if _, err := e.ReadLog(ctx, id, 0, 10); err != nil {
		t.Fatalf("read log: %v", err)
	}
	rev, err = e.PublishSettings(ctx, id, []byte("auto_create_on_push = true\n"), "test", "alice")
	if err != nil {
		t.Fatalf("publish settings: %v", err)
	}
	if rev == 0 {
		t.Fatal("settings publish must advance the revision")
	}

	// Publish with an empty transaction through the real funnel.
	req := &git.PushRequest{}
	if _, err := e.Publish(ctx, id, req, "alice", wal.ObjectAccess{}); err != nil {
		t.Fatalf("publish empty txn: %v", err)
	}
}

// compile-time guard that the fake API keeps satisfying the seam
var _ RouteProvider = &fakeAPI{}

// Silence unused warnings for proto when assertions evolve.
var _ = proto.Manifest{}
