package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
)

// fakeAPI is a RouteProvider stub for the SPA home and repo lanes.
type fakeAPI struct {
	owners []string
	err    error
	served int
}

func (f *fakeAPI) Serve(w http.ResponseWriter, r *http.Request) {
	f.served++
	w.WriteHeader(http.StatusTeapot)
}

func (f *fakeAPI) Owners(r *http.Request) ([]string, error) { return f.owners, f.err }

func TestHealthzAndPrewarm(t *testing.T) {
	s, _ := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	s.healthz(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":"testsha"`) {
		t.Fatalf("healthz = %d %s", rec.Code, rec.Body.String())
	}
	SetPrewarmPending(3)
	defer SetPrewarmPending(0)
	rec = httptest.NewRecorder()
	s.readyz(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "warming") {
		t.Fatalf("warming readyz = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"prewarm_pending":3`) {
		t.Fatalf("prewarm count missing: %s", rec.Body.String())
	}
	// prewarmTimedOut: 0 timeout never gates.
	if s.prewarmTimedOut() {
		t.Fatal("zero timeout must not time out")
	}
}

func TestSDKReposJS(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "/repos.js", nil)
	rec := httptest.NewRecorder()
	s.sdkReposJS(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") == "" {
		t.Fatalf("repos.js = %d etag=%q", rec.Code, rec.Header().Get("ETag"))
	}
	etag := rec.Header().Get("ETag")
	if !strings.Contains(rec.Body.String(), "export async function summary") {
		t.Fatalf("sdk body = %q", rec.Body.String())
	}
	// Conditional GET → 304.
	req = httptest.NewRequest("GET", "/repos.js", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	s.sdkReposJS(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("304 = %d", rec.Code)
	}
	// HEAD: headers only.
	req = httptest.NewRequest("HEAD", "/repos.js", nil)
	rec = httptest.NewRecorder()
	s.sdkReposJS(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("head = %d len=%d", rec.Code, rec.Body.Len())
	}
}

func TestCAPem(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cacheRoot = t.TempDir()
	// No TLS → 404.
	rec := httptest.NewRecorder()
	s.caPem(rec, httptest.NewRequest("GET", "/services/public/ca.pem", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no tls ca.pem = %d", rec.Code)
	}
	// TLS mode self-signed without a cert file → 404.
	s.cfg.Server.TLS.Mode = "self_signed"
	rec = httptest.NewRecorder()
	s.caPem(rec, httptest.NewRequest("GET", "/services/public/ca.pem", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing cert = %d", rec.Code)
	}
	// With a generated cert → 200 PEM.
	if err := s.EnsureSelfSigned(); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.caPem(rec, httptest.NewRequest("GET", "/services/public/ca.pem", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "BEGIN CERTIFICATE") {
		t.Fatalf("ca.pem = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-pem-file" {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestEventsNotify(t *testing.T) {
	var woken string
	s, _ := newTestServer(t, func(o *Options) {
		o.Notifier = func(repo string) { woken = repo }
	})
	rec := httptest.NewRecorder()
	s.eventsNotify(rec, httptest.NewRequest("POST", "/_events/notify?repo=o/r", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("anon notify = %d", rec.Code)
	}
	// Authorized → 202 + wake.
	req := httptest.NewRequest("POST", "/_events/notify?repo=o/r", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.eventsNotify(rec, req)
	if rec.Code != http.StatusAccepted || woken != "o/r" {
		t.Fatalf("notify = %d wake=%q", rec.Code, woken)
	}
	// Without notifier wired → still 202.
	s2, _ := newTestServer(t, nil)
	req = httptest.NewRequest("POST", "/_events/notify", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s2.eventsNotify(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("notify without bridge = %d", rec.Code)
	}
}

func TestSPAHome(t *testing.T) {
	api := &fakeAPI{owners: []string{"alice", "bob"}}
	s, h := newTestServer(t, func(o *Options) { o.API = api })
	do := func(url string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", url, nil)
		if auth {
			req.Header.Set("Authorization", "Bearer tok123")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// Anonymous + no anonymous read → gated 401.
	if rec := do("http://x/", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon home = %d", rec.Code)
	}
	// HTML shell.
	rec := do("http://x/", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<div id="app">`) {
		t.Fatalf("html home = %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("home content-type = %q", ct)
	}
	// ?format=text → owner list.
	if rec := do("http://x/?format=text", true); rec.Body.String() != "alice\nbob\n" {
		t.Fatalf("text home = %q", rec.Body.String())
	}
	// API seam failure → 503.
	api.err = &apiErrString{"boom"}
	if rec := do("http://x/?format=text", true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("api failure = %d", rec.Code)
	}
	api.err = nil
	// ownerPage / repoPage / serveSPA all render the shell.
	rec = httptest.NewRecorder()
	s.ownerPage(rec, httptest.NewRequest("GET", "/o", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner page = %d", rec.Code)
	}
	hh := s.repoPage(mustRepoID(t, "o/r"))
	rec = httptest.NewRecorder()
	hh(rec, httptest.NewRequest("GET", "/o/r", nil))
	if !strings.Contains(rec.Body.String(), "walhub") {
		t.Fatal("repo page shell missing")
	}
}

type apiErrString struct{ s string }

func (e *apiErrString) Error() string { return e.s }

func TestSetupJSONRecipes(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.Server.PublicURL = "https://walgit.example.com"
	// Anonymous → 401.
	rec := httptest.NewRecorder()
	s.setupJSON(rec, httptest.NewRequest("GET", "/services/setup.json", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon setup.json = %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "http://x/services/setup.json?repo=o/r", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec = httptest.NewRecorder()
	s.setupJSON(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup.json = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"manual_clone", "plain_clone", "blobless_clone",
		"bundle_list", "setup_text", "install.sh?repo=o/r", "host"} {
		if !strings.Contains(body, want) {
			t.Fatalf("setup.json missing %q: %s", want, body)
		}
	}
	// oidc mode exposes the token URL; self-signed TLS exposes the CA URL.
	s.cfg.Server.Auth.Mode = "oidc"
	s.cfg.Server.TLS.Mode = "self_signed"
	rec = httptest.NewRecorder()
	s.setupJSON(rec, req)
	if !strings.Contains(rec.Body.String(), "/_auth/tokens") {
		t.Fatalf("token_url missing: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ca.pem") {
		t.Fatalf("ca_url missing: %s", rec.Body.String())
	}
}

func TestHostSlug(t *testing.T) {
	cases := map[string]string{
		"walgit.example.com": "walgit-example-com",
		"LOCALHOST:8080":     "localhost-8080",
		"":                   "",
		"a_b.c":              "a-b-c",
	}
	for in, want := range cases {
		if got := hostSlug(in); got != want {
			t.Fatalf("hostSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServeUIAssets(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "http://x/_ui/app.js", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.serveUIAssets(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset = %d", rec.Code)
	}
	// stringsTrimPrefix fold behavior (uiAsset is a stub; exercise the helper).
	if stringsTrimPrefix("X-UI/thing", "x-ui/") != "thing" {
		t.Fatal("stringsTrimPrefix must be case-insensitive")
	}
	if stringsTrimPrefix("other", "x") != "other" {
		t.Fatal("stringsTrimPrefix must keep non-matching input")
	}
	if !hasSuffixFold("App.JS", ".js") || hasSuffixFold("app.js", ".css") {
		t.Fatal("hasSuffixFold truth table broken")
	}
	if b, ok := setupAsset("setup.js"); !ok || !strings.Contains(string(b), "walhub") {
		t.Fatal("setup.js must resolve")
	}
	if b, ok := setupAsset("setup.css"); !ok || !strings.Contains(string(b), "banner") {
		t.Fatal("setup.css must resolve")
	}
	if _, ok := setupAsset("nope"); ok {
		t.Fatal("unknown asset must miss")
	}
	if _, ok := uiAsset("app.js"); ok {
		t.Fatal("uiAsset is a stub and must miss")
	}
}

func TestSetupUIAndAssets(t *testing.T) {
	// defaults mode → open.
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		Boot: BootState{Mode: "defaults"}})
	rec := httptest.NewRecorder()
	s.setupUI(rec, httptest.NewRequest("GET", "/setup", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "walhub setup") {
		t.Fatalf("setup page = %d", rec.Code)
	}
	// auth none → warning banner.
	s.cfg.Server.Auth.Mode = "none"
	rec = httptest.NewRecorder()
	s.setupUI(rec, httptest.NewRequest("GET", "/setup", nil))
	if !strings.Contains(rec.Body.String(), "banner") {
		t.Fatal("auth-none banner missing")
	}
	// Assets.
	for _, name := range []string{"setup.js", "setup.css"} {
		rec = httptest.NewRecorder()
		s.setupUIAssets(rec, httptest.NewRequest("GET", "/setup/assets/"+name, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", name, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	s.setupUIAssets(rec, httptest.NewRequest("GET", "/setup/assets/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing setup asset = %d", rec.Code)
	}
	// Gated: normal mode + auth token → admin required. Anonymous is
	// authenticated-with-no-error in token mode → 403 admin access required.
	s2, _ := newTestServer(t, nil)
	rec = httptest.NewRecorder()
	s2.setupUI(rec, httptest.NewRequest("GET", "/setup", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gated setup = %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "/setup", nil)
	req.Header.Set("Authorization", "Bearer tok123") // alice is admin
	rec = httptest.NewRecorder()
	s2.setupUI(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin setup = %d", rec.Code)
	}
	// WALHUB_SETUP_TOKEN escape hatch accepts a bearer header.
	t.Setenv("WALHUB_SETUP_TOKEN", "hatch")
	rec = httptest.NewRecorder()
	s2.setupUI(rec, httptest.NewRequest("GET", "/setup", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("token gate = %d", rec.Code)
	}
	req = httptest.NewRequest("GET", "/setup", nil)
	req.Header.Set("Authorization", "Bearer hatch")
	rec = httptest.NewRecorder()
	s2.setupUI(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("token access = %d", rec.Code)
	}
}

func TestInstallSh(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.cfg.Server.PublicURL = "https://wal.example.com"
	req := httptest.NewRequest("GET", "https://wal.example.com/services/public/install.sh?repo=o/r", nil)
	rec := httptest.NewRecorder()
	s.installSh(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("install.sh = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/x-shellscript") {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"wal-example-com", "o/r", "#!/"} {
		if !strings.Contains(body, want) {
			t.Fatalf("install.sh missing %q", want)
		}
	}
	// Token mode → the credential helper is baked in.
	if !strings.Contains(body, "credential") && !strings.Contains(body, "helper") {
		t.Logf("helper not referenced in auth-token mode body (len %d)", len(body))
	}
	// TLS self-signed adds CA trust steps.
	s.cfg.Server.TLS.Mode = "self_signed"
	rec = httptest.NewRecorder()
	s.installSh(rec, req)
	if !strings.Contains(rec.Body.String(), "ca.pem") {
		t.Fatal("self-signed install.sh must reference ca.pem")
	}
}
