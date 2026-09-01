package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
)

// newTestServer builds a Server with a fake engine and a memory store.
func newTestServer(t *testing.T, mutate func(*Options)) (*Server, http.Handler) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.Auth.Mode = "token"
	cfg.Server.Auth.Tokens = []config.StaticToken{{Principal: "alice", Token: "tok123", Write: true, Admin: true}}
	cfg.Server.CorsOrigins = []string{"https://app.example.com", "*.localhost"}
	cfg.Server.Auth.AnonymousRead = false
	cfg.Server.Auth.SessionSecret = "0123456789abcdef0123456789abcdef"
	cfg.Server.Auth.AllowedDomains = []string{"example.com"}
	cfg.Server.Auth.WriteDomains = []string{"example.com"}
	o := Options{Config: cfg, Engine: &fakeEngine{placement: Placement{Serve: true}}, Store: newFakeStore(), Version: "testsha", Kind: KindDev}
	o.Engine.(*fakeEngine).exists = true
	if mutate != nil {
		mutate(&o)
	}
	s := New(o)
	return s, s.Handler()
}

func TestRequestIDMintedAndEchoed(t *testing.T) {
	_, h := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	id := rec.Header().Get("X-Request-Id")
	if len(id) != 32 {
		t.Fatalf("minted request id %q, want 32 hex chars", id)
	}
	req.Header.Set("X-Request-Id", "my-id-123")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); got != "my-id-123" {
		t.Fatalf("inbound request id = %q, want my-id-123", got)
	}
}

func TestServerHeadersOnEveryResponse(t *testing.T) {
	_, h := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	for _, k := range []string{"Server", "X-Walgit-Server"} {
		if v := rec.Header().Get(k); !strings.Contains(v, "walgit/") || !strings.Contains(v, "dev") {
			t.Fatalf("%s = %q, want walgit/<version> (<kind>; …)", k, v)
		}
	}
}

func TestRecoverPanicReturns500Plain(t *testing.T) {
	s, _ := newTestServer(t, nil)
	boom := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	w := s.recoverPanic(boom)
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "internal error") {
		t.Fatalf("body = %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want plain text", ct)
	}
}

func TestCanonicalBrowserHostRedirect(t *testing.T) {
	_, h := newTestServer(t, nil)
	cases := []struct {
		name   string
		req    func() *http.Request
		status int
	}{
		{"browser GET redirects", func() *http.Request {
			r := httptest.NewRequest("GET", "http://localhost:8080/some/path?q=1", nil)
			r.Host = "localhost:8080"
			r.Header.Set("Accept", "text/html")
			return r
		}, http.StatusFound},
		{"git client not redirected", func() *http.Request {
			r := httptest.NewRequest("GET", "http://localhost:8080/a/b.git/info/refs?service=git-upload-pack", nil)
			r.Host = "localhost:8080"
			r.Header.Set("User-Agent", "git/2.46.0")
			return r
		}, http.StatusUnauthorized},
		{"_auth skipped", func() *http.Request {
			r := httptest.NewRequest("GET", "http://localhost:8080/_auth/login", nil)
			r.Host = "localhost:8080"
			r.Header.Set("Accept", "text/html")
			return r
		}, http.StatusNotImplemented}, // flow disabled → 501, not 302
		{"healthz skipped", func() *http.Request {
			r := httptest.NewRequest("GET", "http://localhost:8080/healthz", nil)
			r.Host = "localhost:8080"
			r.Header.Set("Accept", "text/html")
			return r
		}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tc.req())
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (loc=%q)", rec.Code, tc.status, rec.Header().Get("Location"))
			}
			if loc := rec.Header().Get("Location"); tc.status == http.StatusFound {
				if !strings.HasPrefix(loc, "http://walgit.localhost:8080/") || !strings.Contains(loc, "q=1") {
					t.Fatalf("location = %q", loc)
				}
			}
		})
	}
}

func TestHostFromAuthority(t *testing.T) {
	s, _ := newTestServer(t, nil)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "example.test" {
			t.Errorf("host = %q", r.Host)
		}
	})
	r := httptest.NewRequest("GET", "http://example.test/", nil)
	r.ProtoMajor = 2
	r.Host = ""
	r.URL.Host = "example.test"
	s.hostFromAuthority(next).ServeHTTP(httptest.NewRecorder(), r)
}

// TestCORSMatrix covers §2.3 exactly.
func TestCORSMatrix(t *testing.T) {
	_, h := newTestServer(t, nil)
	do := func(method, path, origin string, preflight bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if preflight {
			req.Header.Set("Access-Control-Request-Method", "POST")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	t.Run("preflight allowed origin 204", func(t *testing.T) {
		rec := do("OPTIONS", "/api/v1/me", "https://app.example.com", true)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d", rec.Code)
		}
		h := rec.Header()
		if got := h.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" || got == "*" {
			t.Fatalf("allow-origin = %q", got)
		}
		if h.Get("Access-Control-Allow-Credentials") != "true" {
			t.Fatal("credentials flag missing")
		}
		if h.Get("Access-Control-Allow-Methods") != corsAllowMethods {
			t.Fatalf("methods = %q", h.Get("Access-Control-Allow-Methods"))
		}
		if h.Get("Access-Control-Allow-Headers") != corsAllowHeaders {
			t.Fatalf("headers = %q", h.Get("Access-Control-Allow-Headers"))
		}
		if h.Get("Access-Control-Max-Age") != "600" {
			t.Fatalf("max-age = %q", h.Get("Access-Control-Max-Age"))
		}
	})
	t.Run("preflight foreign origin 403", func(t *testing.T) {
		rec := do("OPTIONS", "/api/v1/me", "https://evil.example.net", true)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("wildcard label matches subdomain not apex", func(t *testing.T) {
		if !originAllowed([]string{"*.localhost"}, "https://walgit.localhost") {
			t.Fatal("*.localhost must match walgit.localhost")
		}
		if originAllowed([]string{"*.example.com"}, "https://example.com") {
			t.Fatal("*.example.com must not match example.com")
		}
	})
	t.Run("state-changing foreign origin 403 before handler", func(t *testing.T) {
		rec := do("POST", "/api/v1/me", "https://evil.example.net", false)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("GET foreign origin passes through without CORS headers", func(t *testing.T) {
		rec := do("GET", "/api/v1/me", "https://evil.example.net", false)
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("foreign GET must not get allow-origin")
		}
	})
	t.Run("allowed origin gets expose headers and vary", func(t *testing.T) {
		rec := do("GET", "/api/v1/me", "https://app.example.com", false)
		if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" {
			t.Fatal("allow-origin missing")
		}
		if rec.Header().Get("Access-Control-Expose-Headers") != corsExpose {
			t.Fatalf("expose = %q", rec.Header().Get("Access-Control-Expose-Headers"))
		}
		if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
			t.Fatal("Vary: Origin missing")
		}
	})
	t.Run("no CORS outside scoped paths", func(t *testing.T) {
		rec := do("GET", "/healthz", "https://app.example.com", false)
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("CORS must be path-scoped")
		}
	})
}

func TestRefreshSessionSlides(t *testing.T) {
	s, h := newTestServer(t, nil)
	svc := s.authSvc
	sess, err := svc.MintSession("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatal("fresh session must not be re-issued")
	}
	// Age past ttl/4 → slide (the server clock drives the sliding check).
	half := time.Duration(30 * 24 * time.Hour / 2)
	s.Now = func() time.Time { return time.Now().Add(half) }
	req = httptest.NewRequest("GET", "/healthz", nil)
	req.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Set-Cookie") == "" {
		t.Fatal("aged session must be re-issued (sliding refresh)")
	}
}
