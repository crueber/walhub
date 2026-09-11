package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
)

// TestGatedBrowserLoginDecisionTable pins the #344 login entry through the
// full handler stack (non-loopback host, so the canonical-host redirect
// never fires): enabled → 307 to the provider; disabled → a usable page,
// never a bare 401; non-browsers and credentialed requests keep the plain
// mapping. Env.gate / smart-HTTP / LFS behavior is untouched — this table
// covers the gated-shell login path only.
func TestGatedBrowserLoginDecisionTable(t *testing.T) {
	browserGET := func(target string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "http://x"+target, nil)
		req.Header.Set("Accept", "text/html")
		return req
	}
	t.Run("enabled browser GET 307s to the provider", func(t *testing.T) {
		_, h := oidcServer(t)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, browserGET("/acme/widgets"))
		if rec.Code != http.StatusTemporaryRedirect {
			t.Fatalf("status = %d, want 307", rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "/_auth/login?next=") {
			t.Fatalf("location = %q, want the login entry with next", loc)
		}
	})
	t.Run("disabled browser GET renders the login page, not a bare 401", func(t *testing.T) {
		_, h := newTestServer(t, nil) // token mode: browser login disabled
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, browserGET("/acme/widgets"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
			t.Fatalf("WWW-Authenticate = %q, want the Bearer challenge", got)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("content-type = %q, want the rendered page", ct)
		}
		body := rec.Body.String()
		for _, want := range []string{"Log in with OIDC", "/_auth/login?next=", "Browser login is not enabled"} {
			if !strings.Contains(body, want) {
				t.Fatalf("page missing %q:\n%s", want, body)
			}
		}
	})
	t.Run("disabled plain GET keeps the bare 401", func(t *testing.T) {
		_, h := newTestServer(t, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/acme/widgets", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("content-type = %q, want plain text for API clients", ct)
		}
		if strings.Contains(rec.Body.String(), "Log in with OIDC") {
			t.Fatalf("API 401 must stay plain: %q", rec.Body.String())
		}
	})
	t.Run("credentialed browser GET is never redirected", func(t *testing.T) {
		_, h := newTestServer(t, nil)
		req := browserGET("/")
		req.Header.Set("Authorization", "Bearer junk")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Fatalf("location = %q, want no redirect for credentialed requests", loc)
		}
	})
	t.Run("enabled non-browser GET keeps the bare 401", func(t *testing.T) {
		_, h := oidcServer(t)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/acme/widgets", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Fatalf("location = %q, want no redirect without the browser signal", loc)
		}
	})
}

// TestAuthLoginDisabledRendersPage pins the /_auth/login half of #344: a
// browser gets the rendered page (501, with a button that round-trips next);
// API clients keep the plain 501 string.
func TestAuthLoginDisabledRendersPage(t *testing.T) {
	_, h := newTestServer(t, nil) // token mode: browser login disabled
	t.Run("browser gets the page", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://x/_auth/login?next=/acme/widgets", nil)
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("content-type = %q, want the rendered page", ct)
		}
		body := rec.Body.String()
		for _, want := range []string{"Log in with OIDC", "Browser login is not enabled"} {
			if !strings.Contains(body, want) {
				t.Fatalf("page missing %q:\n%s", want, body)
			}
		}
		if !strings.Contains(body, "/_auth/login?next=%2Facme%2Fwidgets") {
			t.Fatalf("button must round-trip next:\n%s", body)
		}
	})
	t.Run("API client keeps the plain string", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/_auth/login", nil))
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "browser login is not enabled") {
			t.Fatalf("body = %q", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "<html>") {
			t.Fatalf("API 501 must stay plain: %q", rec.Body.String())
		}
	})
}

// TestLoginNextRoundtrip pins the flow entry: /_auth/login signs next into
// the state, and hostile targets sanitize to "/" before signing.
func TestLoginNextRoundtrip(t *testing.T) {
	s, h := oidcServer(t)
	for _, tc := range []struct{ in, want string }{
		{"/acme/widgets/settings", "/acme/widgets/settings"},
		{"/", "/"},
		{"", "/"},
		{"//evil.test/x", "/"},
		{"https://evil.test/x", "/"},
	} {
		t.Run("next="+tc.in, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				"http://x/_auth/login?next="+url.QueryEscape(tc.in), nil))
			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302", rec.Code)
			}
			next, ok := oidcStateNext(s, rec.Header().Get("Location"))
			if !ok || next != tc.want {
				t.Fatalf("state next = %q ok=%v, want %q", next, ok, tc.want)
			}
		})
	}
}

// TestLoginUnavailablePageDefaultsNext pins the empty-next edge: the button
// still targets the flow entry with next=/.
func TestLoginUnavailablePageDefaultsNext(t *testing.T) {
	body := loginUnavailableHTML("")
	for _, want := range []string{"Log in with OIDC", "/_auth/login?next=%2F"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q:\n%s", want, body)
		}
	}
}

// TestStaticTokensWorkInOIDCMode pins the #344 acceptance line that
// bearer/token paths are unaffected: static tokens still authenticate when
// mode=oidc (the browser-login trio requirement changes nothing here).
func TestStaticTokensWorkInOIDCMode(t *testing.T) {
	s, _ := oidcServer(t)
	s.cfg.Server.Auth.Tokens = []config.StaticToken{{Principal: "ci", Token: "tok-ci", Write: true}}
	s.authSvc = NewAuthService(&s.cfg.Server.Auth, s.Now)
	req := httptest.NewRequest(http.MethodGet, "http://x/_auth/me", nil)
	req.Header.Set("Authorization", "Bearer tok-ci")
	p, aerr := s.authSvc.Authenticate(req, s.cfg)
	if aerr != nil {
		t.Fatalf("static token in oidc mode: %v", aerr)
	}
	if p.Name != "ci" || !p.Write {
		t.Fatalf("principal = %+v, want ci+write", p)
	}
}

// TestSetupJSONBrowserLogin pins the setup.json half of the #344
// advertisement: browser_login + login_url track the gate.
func TestSetupJSONBrowserLogin(t *testing.T) {
	read := func(s *Server, h http.Handler) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://x/services/setup.json", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("setup.json = %d (%s)", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	t.Run("enabled advertises the entry", func(t *testing.T) {
		s, h := oidcServer(t)
		s.cfg.Server.Auth.AnonymousRead = true // let the anonymous probe read
		body := read(s, h)
		if body["browser_login"] != true {
			t.Fatalf("browser_login = %v, want true", body["browser_login"])
		}
		if lu, _ := body["login_url"].(string); !strings.HasSuffix(lu, "/_auth/login") {
			t.Fatalf("login_url = %v, want the flow entry", body["login_url"])
		}
	})
	t.Run("disabled reports false with no URL", func(t *testing.T) {
		s, h := newTestServer(t, nil)
		s.cfg.Server.Auth.AnonymousRead = true
		body := read(s, h)
		if body["browser_login"] != false {
			t.Fatalf("browser_login = %v, want false", body["browser_login"])
		}
		if _, ok := body["login_url"]; ok {
			t.Fatalf("login_url must be absent when disabled: %v", body)
		}
	})
}
