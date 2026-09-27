package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- F1: nonce binding -------------------------------------------------------

func TestVerifyNonceMismatch(t *testing.T) {
	iss := newStubIssuer(t)
	tSrv, _ := newTestServer(t, nil)
	tSrv.cfg.Server.Auth.Mode = "oidc"
	tSrv.cfg.Server.Auth.Issuer = iss.srvURL()
	tSrv.cfg.Server.Auth.OAuthClientID = "walhub"
	tSrv.cfg.Server.Auth.AllowedDomains = []string{"example.com"}
	a := &tSrv.cfg.Server.Auth
	j := NewJWKS(iss.srvURL())
	mint := func(nonce string) string {
		tv := true
		return iss.mint(t, map[string]any{
			"aud": "walhub", "exp": time.Now().Add(time.Hour).Unix(),
			"email": "alice@example.com", "email_verified": tv, "nonce": nonce,
		})
	}
	if _, aerr := j.Verify(context.Background(), mint("n1"), a, true, "n1"); aerr != nil {
		t.Fatalf("matching nonce = %v", aerr)
	}
	if _, aerr := j.Verify(context.Background(), mint("n1"), a, true, "n2"); aerr == nil ||
		aerr.Why != "nonce mismatch" {
		t.Fatalf("wrong nonce = %v, want nonce mismatch", aerr)
	}
	if _, aerr := j.Verify(context.Background(), mint(""), a, true, "n2"); aerr == nil {
		t.Fatal("missing token nonce must fail a bound verification")
	}
	// Empty expected nonce skips the check (direct bearer ID tokens).
	if _, aerr := j.Verify(context.Background(), mint("anything"), a, false, ""); aerr != nil {
		t.Fatalf("unbound verify = %v", aerr)
	}
}

// TestOIDCCallbackNonceMismatch drives a callback whose ID token carries the
// wrong nonce: login must not complete.
func TestOIDCCallbackNonceMismatch(t *testing.T) {
	_, h, iss := oidcFull(t)
	req := httptest.NewRequest("GET", "http://x/_auth/login?next=/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("login = %d", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	state := loc.Query().Get("state")
	var pkce *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "walgit_pkce" {
			c := c
			pkce = c
		}
	}
	if pkce == nil {
		t.Fatal("login must set the walgit_pkce cookie")
	}
	tv := true
	iss.idTokens <- iss.mint(t, map[string]any{
		"aud": "walhub", "exp": time.Now().Add(time.Hour).Unix(),
		"email": "alice@example.com", "email_verified": tv, "nonce": "wrong-nonce",
	})
	cb := httptest.NewRequest("GET", "http://x/_auth/callback?code=good&state="+url.QueryEscape(state), nil)
	cb.AddCookie(pkce)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, cb)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "nonce mismatch") {
		t.Fatalf("nonce mismatch callback = %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "walgit_session" && c.Value != "" {
			t.Fatal("no session may be minted on nonce mismatch")
		}
	}
}

// --- F5: wgt kind confusion --------------------------------------------------

func TestWgtSessionKindRejected(t *testing.T) {
	s, _ := newTestServer(t, nil)
	sess, err := s.authSvc.MintSession("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// A session wire with a forged wgt_ prefix must not authenticate (F5).
	if _, aerr := s.authSvc.wgtPrincipal(tokenPrefix + sess.Wire); aerr == nil {
		t.Fatal("session wire with wgt_ prefix must fail")
	}
	// A real access token still authenticates.
	tok, err := s.authSvc.MintToken("alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, aerr := s.authSvc.wgtPrincipal(tok.Wire); aerr != nil {
		t.Fatalf("wgt_ token = %v", aerr)
	}
}

// --- F6: exchange retry + verifier -------------------------------------------

func TestExchangeCodeRetryPolicy(t *testing.T) {
	s, _ := newTestServer(t, nil)
	ctx := context.Background()
	// 4xx fails fast: exactly one attempt (F6).
	var hits int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer bad.Close()
	if got := s.exchangeCode(ctx, bad.URL+"/token", "c", "http://localhost/cb", ""); got != "" {
		t.Fatalf("400 endpoint = %q, want empty", got)
	}
	if n := atomic.LoadInt64(&hits); n != 1 {
		t.Fatalf("400 attempts = %d, want exactly 1", n)
	}
	// 5xx retried once, then a healthy retry succeeds.
	var flaky int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&flaky, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id_token":"tok-retry"}`))
	}))
	defer srv.Close()
	if got := s.exchangeCode(ctx, srv.URL+"/token", "c", "http://localhost/cb", ""); got != "tok-retry" {
		t.Fatalf("flaky exchange = %q, want tok-retry", got)
	}
	if n := atomic.LoadInt64(&flaky); n != 2 {
		t.Fatalf("flaky attempts = %d, want 2", n)
	}
	// The verifier is forwarded as code_verifier (F1).
	var seen string
	ver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		seen = r.Form.Get("code_verifier")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id_token":"tok-v"}`))
	}))
	defer ver.Close()
	if got := s.exchangeCode(ctx, ver.URL+"/token", "c", "http://localhost/cb", "ver-123"); got != "tok-v" {
		t.Fatalf("verifier exchange = %q", got)
	}
	if seen != "ver-123" {
		t.Fatalf("code_verifier = %q, want ver-123", seen)
	}
}

// --- F8: claimed-ticket 60 s window ------------------------------------------

func TestClaimedTicket60sWindow(t *testing.T) {
	s, _ := oidcServer(t)
	body := "alice@example.com|wire"
	fresh, _ := s.signStateTTL(body, s.Now(), 60*time.Second)
	if _, ok := s.verifyStateTicket(fresh); !ok {
		t.Fatal("fresh 60 s ticket must verify")
	}
	aged, _ := s.signStateTTL(body, s.Now().Add(-61*time.Second), 60*time.Second)
	if _, ok := s.verifyStateTicket(aged); ok {
		t.Fatal("61 s old ticket must not verify (F8: 60 s window)")
	}
	// The login state window is unchanged at 600 s.
	login, _ := s.signStateTTL("/settings", s.Now().Add(-70*time.Second), 600*time.Second)
	if _, ok := s.verifyState(login); !ok {
		t.Fatal("70 s old login state must still verify (600 s window)")
	}
}
