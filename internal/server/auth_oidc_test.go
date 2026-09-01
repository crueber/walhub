package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// oidcServer is a full §8.6 fixture: issuer + browser login enabled.
func oidcServer(t *testing.T) (*Server, http.Handler) {
	s, h := newTestServer(t, nil)
	s.cfg.Server.Auth.Mode = "oidc"
	s.cfg.Server.Auth.Issuer = "https://issuer.test"
	s.cfg.Server.Auth.OAuthClientID = "walhub"
	s.cfg.Server.Auth.OAuthClientSecret = "sekrit"
	s.cfg.Server.Auth.SessionSecret = "0123456789abcdef0123456789abcdef"
	s.cfg.Server.Auth.AllowedDomains = []string{"example.com"}
	svc := NewAuthService(&s.cfg.Server.Auth, s.Now)
	s.authSvc = svc
	// Pre-seed discovery so the flow never leaves the test process.
	svc.jwks.disc = oidcDiscovery{AuthEndpoint: "https://issuer.test/auth", TokenEndpoint: "https://issuer.test/token"}
	svc.jwks.discHas = true
	return s, h
}

func TestOIDCFlowLoginRedirect(t *testing.T) {
	s, h := oidcServer(t)
	req := httptest.NewRequest("GET", "http://x/_auth/login?next=/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if q.Get("response_type") != "code" || q.Get("scope") != "openid email" ||
		q.Get("prompt") != "select_account" || q.Get("client_id") != "walhub" {
		t.Fatalf("authorization params = %v", q)
	}
	if q.Get("hd") != "example.com" {
		t.Fatalf("hd = %q, want first allowed domain", q.Get("hd"))
	}
	if q.Get("code_challenge") != "" {
		t.Fatal("no PKCE (§8.6)")
	}
	if !strings.HasSuffix(q.Get("redirect_uri"), "/_auth/callback") {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	// State is verifiable and carries the sanitized next.
	next, ok := oidcStateNext(s, rec.Header().Get("Location"))
	if !ok || next != "/settings" {
		t.Fatalf("state next = %q ok=%v", next, ok)
	}
}

// oidcStateNext extracts the next path from the state of a login redirect.
func oidcStateNext(s *Server, loc string) (string, bool) {
	u, err := url.Parse(loc)
	if err != nil {
		return "", false
	}
	return s.verifyState(u.Query().Get("state"))
}

func TestOIDCFlowDisabled501(t *testing.T) {
	_, h := newTestServer(t, nil) // token mode: browser login disabled
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/_auth/login", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestOIDCCallbackBadState(t *testing.T) {
	_, h := oidcServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://x/_auth/callback?code=c&state=forged", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestOIDCClaimedTicketSetsCookie(t *testing.T) {
	s, h := oidcServer(t)
	sess, _ := s.authSvc.MintSession("alice@example.com")
	ticket := s.signState("alice@example.com|"+sess.Wire, s.Now())
	req := httptest.NewRequest("GET", "http://walgit.localhost:8080/_auth/claimed?ticket="+
		url.QueryEscape(ticket)+"&next=/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == "walgit_session" && c.Value == sess.Wire {
			found = true
		}
	}
	if !found {
		t.Fatalf("session cookie missing: %v", cookies)
	}
	if loc := rec.Header().Get("Location"); loc != "/settings" {
		t.Fatalf("next = %q", loc)
	}
}

func TestTokensMintCSRFGuard(t *testing.T) {
	s, h := oidcServer(t)
	sess, _ := s.authSvc.MintSession("alice@example.com")
	req := httptest.NewRequest("POST", "http://x/_auth/tokens", nil)
	req.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site mint status = %d, want 403", rec.Code)
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin mint status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "wgt_") {
		t.Fatalf("token body = %s", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("token mint must be no-store")
	}
	_ = s
}
