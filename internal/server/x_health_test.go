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
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/javascript") {
		t.Fatalf("repos.js content-type = %q (module MIME is load-bearing)", ct)
	}
	if !strings.Contains(rec.Body.String(), "ReposClient") {
		t.Fatalf("repos.js must be the real SDK bundle (D-WEB-2), got %q", rec.Body.String()[:120])
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

// TestUIAssetResolvers: the embedded SPA must actually be reachable — the
// /repos.js bundle, the /_ui module tree, and the standalone setup page
// (regression: uiAsset/setupAsset were hardcoded stubs, blanking every page).
func TestUIAssetResolvers(t *testing.T) {
	b, ok := webAsset("dist/index.html")
	if !ok || !strings.Contains(string(b), `id="root"`) || !strings.Contains(string(b), `/_ui/assets/`) {
		t.Fatalf("embedded dist/index.html missing or wrong: ok=%v len=%d (run make web)", ok, len(b))
	}
	if b, ok := uiAsset("index.html"); !ok || !strings.Contains(string(b), "root") {
		t.Fatal("uiAsset must serve the built shell")
	}
	if _, ok := uiAsset("../go.mod"); ok {
		t.Fatal("uiAsset must refuse traversal-style names")
	}
	if _, ok := uiAsset("etc/passwd"); ok {
		t.Fatal("uiAsset must refuse paths outside the UI tree")
	}
	if _, ok := uiAsset("src/main.js"); ok {
		t.Fatal("raw source lanes must 404 after the vite cutover (D-WEB-6)")
	}
	if _, ok := uiAsset("assets/../../go.mod"); ok {
		t.Fatal("traversal via assets/ prefix must fail")
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
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `/_ui/assets/`) || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("html home must serve the built SPA shell: %d %.160s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("home content-type = %q", ct)
	}
	// ?format=text moved to /explore (issue #187): / answers the shell
	// unconditionally, even for text requests.
	if rec := do("http://x/?format=text", true); !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("landing home must serve the shell, got %q", rec.Body.String())
	}
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

// TestExplorePage: GET /explore serves the owners shell; ?format=text (or a
// text/plain Accept without text/html) answers the plain owner list (moved
// from / by issue #187 — the machine twin follows the page; pre-1.0
// no-alias: / has no text branch anymore).
func TestExplorePage(t *testing.T) {
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
	if rec := do("http://x/explore", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon explore = %d", rec.Code)
	}
	// HTML shell.
	rec := do("http://x/explore", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `/_ui/assets/`) || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("html explore must serve the built SPA shell: %d %.160s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("explore content-type = %q", ct)
	}
	_ = s
	// ?format=text → owner list.
	if rec := do("http://x/explore?format=text", true); rec.Body.String() != "alice\nbob\n" {
		t.Fatalf("text explore = %q", rec.Body.String())
	}
	if rec := do("http://x/explore?format=text", true); !strings.Contains(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("text explore content-type = %q", rec.Header().Get("Content-Type"))
	}
	// text/plain Accept (without text/html) → owner list too.
	req := httptest.NewRequest("GET", "http://x/explore", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("Accept", "text/plain")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Body.String() != "alice\nbob\n" {
		t.Fatalf("accept-text explore = %q", rec.Body.String())
	}
	// text/html Accept still gets the shell (query param absent).
	req = httptest.NewRequest("GET", "http://x/explore", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("Accept", "text/html, text/plain")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("html-accept explore must serve the shell, got %q", rec.Body.String())
	}
	// API seam failure → 503.
	api.err = &apiErrString{"boom"}
	if rec := do("http://x/explore?format=text", true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("api failure = %d", rec.Code)
	}
	api.err = nil
	// /explore must be the explicit route, not the owner wildcard: an owner
	// literally named "explore" is shadowed (documented reservation).
	if rec := do("http://x/explore", true); rec.Code != http.StatusOK {
		t.Fatalf("explore route = %d", rec.Code)
	}
}

// TestHowItWorksPage: GET /how-it-works serves the deep-dive shell (issue
// #191, R1 B1) — gated, unconditional HTML (no ?format=text branch), and the
// explicit route (an owner literally named "how-it-works" is shadowed).
func TestHowItWorksPage(t *testing.T) {
	api := &fakeAPI{owners: []string{"alice", "bob"}}
	_, h := newTestServer(t, func(o *Options) { o.API = api })
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
	if rec := do("http://x/how-it-works", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon how-it-works = %d", rec.Code)
	}
	// HTML shell, no-cache.
	rec := do("http://x/how-it-works", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `/_ui/assets/`) || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("html how-it-works must serve the built SPA shell: %d %.160s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("how-it-works content-type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Fatalf("how-it-works cache-control = %q", cc)
	}
	// No text twin here (R1 S1): ?format=text still serves the shell.
	if rec := do("http://x/how-it-works?format=text", true); !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("how-it-works must serve the shell unconditionally, got %q", rec.Body.String())
	}
}

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
	// oidc mode exposes the token URL; there is no CA URL anymore (#165:
	// TLS terminates at the reverse proxy, never in-process).
	s.cfg.Server.Auth.Mode = "oidc"
	rec = httptest.NewRecorder()
	s.setupJSON(rec, req)
	if !strings.Contains(rec.Body.String(), "/_auth/tokens") {
		t.Fatalf("token_url missing: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ca.pem") {
		t.Fatalf("ca_url must be gone: %s", rec.Body.String())
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
	if _, ok := uiAsset("app.js"); ok {
		t.Fatal("uiAsset is a stub and must miss")
	}
}

// TestUIAssetConcepts: /_ui/concepts/*.gif (issue #187) resolve + serve as
// image/gif with the no-cache (never immutable) class — stable filenames
// whose bytes change on regeneration. Requires the built UI (run make web;
// same precondition as TestUIAssetResolvers).
func TestUIAssetConcepts(t *testing.T) {
	s, _ := newTestServer(t, nil)
	b, ok := uiAsset("concepts/push.gif")
	if !ok || len(b) == 0 || string(b[:6]) != "GIF89a" {
		t.Fatal("concepts/push.gif must resolve to GIF bytes (run make web)")
	}
	if _, ok := uiAsset("concepts/push.png"); ok {
		t.Fatal("non-gif concept names must miss")
	}
	if _, ok := uiAsset("concepts/../../go.mod"); ok {
		t.Fatal("traversal via concepts/ prefix must fail")
	}
	// MIME + cache class + ETag through the handler.
	req := httptest.NewRequest("GET", "http://x/_ui/concepts/push.gif", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.serveUIAssets(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("concepts gif = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/gif" {
		t.Fatalf("concepts content-type = %q, want image/gif", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("concepts cache-control = %q, want no-cache (never immutable)", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("concepts gif must carry an ETag")
	}
	req = httptest.NewRequest("GET", "http://x/_ui/concepts/push.gif", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	s.serveUIAssets(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("conditional concepts gif = %d, want 304", rec.Code)
	}
}

// TestCompressBypassesImages: the compress middleware must not gzip
// image/* bodies (issue #187 B2 — gzipping LZW GIFs is pure CPU per
// request); SSE + precompressed bodies keep their existing bypass.
func TestCompressBypassesImages(t *testing.T) {
	s, _ := newTestServer(t, nil)
	run := func(ct string) *httptest.ResponseRecorder {
		h := s.compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("bytes"))
		}))
		req := httptest.NewRequest("GET", "http://x/_ui/concepts/push.gif", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := run("image/gif"); rec.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("image/gif must bypass gzip")
	}
	if rec := run("text/event-stream"); rec.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("SSE must bypass gzip")
	}
	if rec := run("text/html; charset=utf-8"); rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("text/html must still gzip")
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
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "id=\"root\"") {
		t.Fatalf("setup page = %d, want the SPA shell (D-WEB-6)", rec.Code)
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
	// No CA trust steps anymore (#165): the proxy terminates TLS.
	rec = httptest.NewRecorder()
	s.installSh(rec, req)
	if strings.Contains(rec.Body.String(), "ca.pem") {
		t.Fatal("install.sh must not reference ca.pem")
	}
}
