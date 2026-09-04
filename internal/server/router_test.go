package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
	"os"
)

func TestSetupOnlyMode(t *testing.T) {
	cfg := config.Defaults()
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		Boot: BootState{Mode: "setup_only", Errors: []string{"store.backend: unknown backend \"tape\""}}})
	h := s.Handler()

	// The §3.4 subset answers.
	allowed := map[string]int{
		"/healthz":                    http.StatusOK,
		"/readyz":                     http.StatusServiceUnavailable, // setup_required
		"/setup":                      http.StatusOK,
		"/api/v1/setup":               http.StatusOK,
		"/services/public/install.sh": http.StatusOK,
	}
	for path, want := range allowed {
		req := httptest.NewRequest("GET", "http://x"+path, nil)
		req.Header.Set("User-Agent", "curl/8")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s = %d, want %d (body %q)", path, rec.Code, want, rec.Body.String())
		}
	}

	// Everything else → 503 plain text with a pointer to /setup.
	others := []string{"/", "/metrics", "/api/v1/me", "/services/setup.json",
		"/o/r.git/info/refs?service=git-upload-pack", "/repos.js", "/_auth/login"}
	for _, path := range others {
		req := httptest.NewRequest("GET", "http://x"+path, nil)
		req.Header.Set("User-Agent", "curl/8")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d, want 503", path, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "/setup") {
			t.Fatalf("%s body missing /setup pointer: %q", path, body)
		}
	}

	// readyz carries the exact config validation errors.
	req := httptest.NewRequest("GET", "http://x/readyz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "setup_required") ||
		!strings.Contains(rec.Body.String(), "tape") {
		t.Fatalf("readyz body = %q", rec.Body.String())
	}
}

func TestSetupOnlySetupAPI(t *testing.T) {
	cfg := config.Defaults()
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		Boot: BootState{Mode: "setup_only", Errors: []string{"boom"}}})
	h := s.Handler()
	req := httptest.NewRequest("GET", "http://x/api/v1/setup", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"file_state", "invalid", "groups", "server.listen"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
}

func TestSetupAPISaveRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dataDir
	cfg.Server.Auth.Mode = "none" // open access rule
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		DataDir: dataDir, Boot: BootState{Mode: "defaults"}})
	h := s.Handler()

	// POST test: valid config → 200 {errors: []}
	body := `{"overrides": {"server.listen": "127.0.0.1:9099"}}`
	req := httptest.NewRequest("POST", "http://x/api/v1/setup/test", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"errors":[]`) {
		t.Fatalf("test status = %d body = %s", rec.Code, rec.Body.String())
	}

	// POST test: unknown key → 422.
	req = httptest.NewRequest("POST", "http://x/api/v1/setup/test",
		strings.NewReader(`{"overrides": {"server.bogus": "1"}}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid test status = %d body = %s", rec.Code, rec.Body.String())
	}

	// PUT: atomic save via config.SaveSetup.
	req = httptest.NewRequest("PUT", "http://x/api/v1/setup", strings.NewReader(body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"saved":true`) {
		t.Fatalf("put status = %d body = %s", rec.Code, rec.Body.String())
	}
	if _, err := osStat(dataDir + "/walhub.toml"); err != nil {
		t.Fatalf("walhub.toml not written: %v", err)
	}
}

func TestSetupTokenEscapeHatch(t *testing.T) {
	t.Setenv("WALHUB_SETUP_TOKEN", "sekrit")
	cfg := config.Defaults()
	cfg.Server.Auth.Mode = "token" // normally admin-required
	s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
		Boot: BootState{Mode: "normal"}})
	h := s.Handler()

	req := httptest.NewRequest("GET", "http://x/api/v1/setup", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without token status = %d, want 401", rec.Code)
	}
	req = httptest.NewRequest("GET", "http://x/api/v1/setup?token=sekrit", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with token status = %d", rec.Code)
	}
}

func TestWildcardDispatch(t *testing.T) {
	s, h := newTestServer(t, nil)
	s.engine.(*fakeEngine).exists = true
	s.engine.(*fakeEngine).bundles = BundleList{
		Fulls: []BundleEntry{{Strategy: "full", Name: "b1.bundle"}},
	}

	t.Run("junk is deliberate 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/nonsense/junk/xyz", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("404 must be plain text, got %q", ct)
		}
	})
	t.Run("bad repo id is 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/o/../evil.git/info/refs?service=git-upload-pack", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("bundle object serves with static contract", func(t *testing.T) {
		st := s.store.(store.ObjectStore)
		if _, err := st.Put(nil, "repos/o/r/bundles/full/b1.bundle",
			store.PutBody{Bytes: []byte("BUNDLE-DATA")},
			store.PutOptions{ContentType: "application/x-git-bundle", Immutable: true}); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", "http://x/o/r.git/bundles/full/b1.bundle", nil)
		req.Header.Set("Authorization", "Bearer tok123")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/x-git-bundle" {
			t.Fatalf("content-type = %q", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Fatalf("cache-control = %q", cc)
		}
		if body := rec.Body.String(); body != "BUNDLE-DATA" {
			t.Fatalf("body = %q", body)
		}
	})
	t.Run("bundle list 400 on unknown filter", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://x/o/r.git/bundles/list?filter=weird", nil)
		req.Header.Set("Authorization", "Bearer tok123")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

func TestMethodNotAllowed(t *testing.T) {
	_, h := newTestServer(t, nil)
	req := httptest.NewRequest("POST", "http://x/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Allow") == "" {
		t.Fatal("Allow header missing")
	}
}

func TestMetricsExposition(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.metrics.Counter("walgit_push_refused_total", "pushes refused").Inc("reason", "busy")
	s.metrics.Counter("walgit_push_refused_total", "pushes refused").Inc("reason", "busy")
	s.metrics.Gauge("walgit_http_inflight", "in-flight HTTP requests").Set(3)
	body := s.metrics.Render()
	if !strings.Contains(body, "# TYPE walgit_push_refused_total counter") {
		t.Fatalf("family header missing: %s", body)
	}
	if !strings.Contains(body, `walgit_push_refused_total{reason="busy"} 2`) {
		t.Fatalf("labeled sample missing: %s", body)
	}
	if !strings.Contains(body, "walgit_http_inflight 3") {
		t.Fatalf("gauge missing: %s", body)
	}
	// Lexicographic family order.
	names := []string{}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# TYPE ") {
			names = append(names, strings.Fields(line)[2])
		}
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("families not sorted: %v", names)
		}
	}
}

func TestReadyzStates(t *testing.T) {
	t.Run("auth_none warning", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Server.Auth.Mode = "none"
		s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
			Boot: BootState{Mode: "normal"}})
		rec := httptest.NewRecorder()
		s.readyz(rec, httptest.NewRequest("GET", "/readyz", nil))
		if !strings.Contains(rec.Body.String(), "auth_none") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("defaults mode config flag", func(t *testing.T) {
		cfg := config.Defaults()
		s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
			Boot: BootState{Mode: "defaults"}})
		rec := httptest.NewRecorder()
		s.readyz(rec, httptest.NewRequest("GET", "/readyz", nil))
		if !strings.Contains(rec.Body.String(), `"config":"defaults"`) {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
	t.Run("draining", func(t *testing.T) {
		cfg := config.Defaults()
		s := New(Options{Config: cfg, Store: newFakeStore(), Engine: &fakeEngine{},
			Boot: BootState{Mode: "normal"}})
		s.drain.Begin2()
		rec := httptest.NewRecorder()
		s.readyz(rec, httptest.NewRequest("GET", "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "draining") {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Retry-After") != "15" {
			t.Fatal("Retry-After missing on draining readyz")
		}
	})
}

func TestHealthzAlwaysOK(t *testing.T) {
	cfg := config.Defaults()
	s := New(Options{Config: cfg, Store: newFakeStore(),
		Boot: BootState{Mode: "setup_only"}})
	rec := httptest.NewRecorder()
	s.healthz(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// osStat wraps os.Stat for the save test.
func osStat(p string) (interface{ Size() int64 }, error) {
	fi, err := os.Stat(p)
	return fi, err
}

// TestTeamPageRoute pins the 08 §1 org team page: "/{owner}/teams/{slug}"
// serves the SPA shell (gated, GET/HEAD only); other methods 405; a repo
// literally named "teams" keeps its 2-segment root.
func TestTeamPageRoute(t *testing.T) {
	_, h := newTestServer(t, nil)
	rows := []struct {
		name   string
		method string
		target string
		want   int
		shell  bool
	}{
		{"team page shell", "GET", "http://x/acme/teams/dev", http.StatusOK, true},
		{"team page head", "HEAD", "http://x/acme/teams/dev", http.StatusOK, false},
		{"team page post rejected", "POST", "http://x/acme/teams/dev", http.StatusMethodNotAllowed, false},
		{"empty slug 404", "GET", "http://x/acme/teams/", http.StatusNotFound, false},
		{"repo named teams keeps root", "GET", "http://x/acme/teams", http.StatusOK, true},
		{"bad owner 404", "GET", "http://x/Bad%20Owner/teams/dev", http.StatusNotFound, false},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, nil)
			req.Header.Set("Authorization", "Bearer tok123")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s %s = %d, want %d (%s)", tc.method, tc.target, rec.Code, tc.want, rec.Body.String())
			}
			if tc.shell && !strings.Contains(rec.Body.String(), "walhub") {
				t.Fatalf("%s %s missing shell: %q", tc.method, tc.target, rec.Body.String())
			}
		})
	}
}
