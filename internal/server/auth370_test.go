// auth370_test.go — Forgejo #370: the OIDC principal is a username.
//
// principalFromEmail resolves email → username (immutable, stable,
// collision-uniquified through the wired registry); the email never
// becomes a principal name, owner segment, or path component, and
// every auth surface (me/check/tokens/session/wgt_) renders the
// username. The email rides on Principal.Email for alias matching and
// session/token mints only.
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOIDCUsernamePrincipal pins the core rewrite: Name is the
// username (no @), stable across calls, carrying the verified email.
func TestOIDCUsernamePrincipal(t *testing.T) {
	s, _ := oidcServer(t)
	p, aerr := s.authSvc.principalFromEmail("Crueber@Gmail.COM")
	_ = s // allowed-domains fixture is example.com; use it below
	if aerr == nil {
		t.Fatalf("gmail must be outside the example.com allowlist")
	}
	p, aerr = s.authSvc.principalFromEmail("crueber@example.com")
	if aerr != nil {
		t.Fatal(aerr)
	}
	if p.Name != "crueber" {
		t.Fatalf("principal name = %q, want username", p.Name)
	}
	if strings.Contains(p.Name, "@") {
		t.Fatalf("principal name leaks email: %q", p.Name)
	}
	if p.Email != "crueber@example.com" {
		t.Fatalf("principal email = %q", p.Email)
	}
	// Stable across sessions (same derivation, no resolver wired).
	q, _ := s.authSvc.principalFromEmail("crueber@example.com")
	if q.Name != p.Name {
		t.Fatalf("unstable username: %q vs %q", p.Name, q.Name)
	}
}

// TestOIDCUsernameResolverHook pins the store seam: the wired registry
// uniquifies (crueber → crueber2) and the principal follows it.
func TestOIDCUsernameResolverHook(t *testing.T) {
	s, _ := oidcServer(t)
	s.authSvc.UsernameResolver = func(email string) string { return "crueber2" }
	p, aerr := s.authSvc.principalFromEmail("crueber@example.com")
	if aerr != nil || p.Name != "crueber2" || p.Email != "crueber@example.com" {
		t.Fatalf("resolved principal = %+v %v", p, aerr)
	}
	// An invalid hook return falls back to the pure base (never an email).
	s.authSvc.UsernameResolver = func(email string) string { return "not a name" }
	p, aerr = s.authSvc.principalFromEmail("crueber@example.com")
	if aerr != nil || p.Name != "crueber" {
		t.Fatalf("fallback principal = %+v %v", p, aerr)
	}
}

// TestOIDCUsernameAuthSurfaces pins the leak audit at the auth layer:
// session cookies, wgt_ tokens, /_auth/me, and /_auth/check all carry
// the username; the email appears in none of them.
func TestOIDCUsernameAuthSurfaces(t *testing.T) {
	s, h := oidcServer(t)
	const email = "crueber@example.com"
	// Session-cookie path.
	sess, err := s.authSvc.MintSession(email)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sess.Wire, "@") {
		t.Fatalf("session wire must not carry a raw email: %q", sess.Wire)
	}
	req := httptest.NewRequest("GET", "http://x/_auth/me", nil)
	req.AddCookie(&http.Cookie{Name: "walgit_session", Value: sess.Wire})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "crueber") || strings.Contains(body, email) {
		t.Fatalf("me body leaks: %s", body)
	}
	// wgt_ path.
	tok, err := s.authSvc.MintToken(email)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "http://x/_auth/check", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Wire)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("check = %d", rec.Code)
	}
	if got := rec.Header().Get("X-Walgit-Principal"); got != "crueber" {
		t.Fatalf("X-Walgit-Principal = %q, want username", got)
	}
}

// TestPrincipalForNameUsername pins SSH-side resolution: a username
// resolves through the wired EmailLookup to the email's admission;
// without the lookup it fails closed.
func TestPrincipalForNameUsername(t *testing.T) {
	s, _ := oidcServer(t)
	if _, err := s.authSvc.PrincipalForName("crueber"); err == nil {
		t.Fatal("username without EmailLookup must fail closed")
	}
	s.authSvc.EmailLookup = func(u string) (string, bool) {
		if u == "crueber" {
			return "crueber@example.com", true
		}
		return "", false
	}
	p, err := s.authSvc.PrincipalForName("crueber")
	if err != nil || p.Name != "crueber" || p.Email != "crueber@example.com" || !p.Write {
		t.Fatalf("username principal = %+v %v", p, err)
	}
	if _, err := s.authSvc.PrincipalForName("ghost"); err == nil {
		t.Fatal("unknown username must fail closed")
	}
}
