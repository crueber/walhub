package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestOIDCDiscoveryFailure covers the 503 branches when the issuer is down.
func TestOIDCDiscoveryFailure(t *testing.T) {
	s, h, iss := oidcFull(t)
	iss.srv.Close() // issuer gone: discovery fails
	time.Sleep(10 * time.Millisecond)

	// login → 503 issuer discovery failed.
	req := httptest.NewRequest("GET", "http://x/_auth/login?next=/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "issuer discovery failed") {
		t.Fatalf("login discovery = %d %s", rec.Code, rec.Body.String())
	}
	// callback → 503 issuer discovery failed.
	state := s.signState("/x", s.Now())
	req = httptest.NewRequest("GET", "http://x/_auth/callback?code=c&state="+state, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "issuer discovery failed") {
		t.Fatalf("callback discovery = %d %s", rec.Code, rec.Body.String())
	}
}

// TestOIDCTokenMintInvalidAuth covers the mint Authenticate-error branch.
func TestOIDCTokenMintInvalidAuth(t *testing.T) {
	s, _, _ := oidcFull(t)
	req := httptest.NewRequest("POST", "http://x/_auth/tokens", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Authorization", "Bearer dead")
	rec := httptest.NewRecorder()
	s.authTokensMint(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("mint invalid = %d", rec.Code)
	}
}

// TestOIDCCallbackBadIDToken covers the verifyIDToken-failure branch.
func TestOIDCCallbackBadIDToken(t *testing.T) {
	s, _, iss := oidcFull(t)
	tv := true
	iss.idTokens <- iss.mint(t, map[string]any{
		"aud": "other-app", "exp": time.Now().Add(time.Hour).Unix(),
		"email": "alice@example.com", "email_verified": tv,
	})
	state := s.signState("/x", s.Now())
	req := httptest.NewRequest("GET", "http://x/_auth/callback?code=c&state="+state, nil)
	rec := httptest.NewRecorder()
	s.authCallback(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad id token = %d %s", rec.Code, rec.Body.String())
	}
}

// TestAuthCheckInvalidToken covers the authCheck mapAuthStatus branch.
func TestAuthCheckInvalidToken(t *testing.T) {
	s, _ := newTestServer(t, nil)
	req := httptest.NewRequest("GET", "/_auth/check", nil)
	req.Header.Set("Authorization", "Bearer dead")
	rec := httptest.NewRecorder()
	s.authCheck(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("check invalid = %d", rec.Code)
	}
}

// TestMetricsGaugeWithLabels covers the labeled gauge set branch.
func TestMetricsGaugeWithLabels(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.metrics.Gauge("walgit_tasks_running", "tasks running").Set(2, "kind", "repack")
	body := s.metrics.Render()
	if !strings.Contains(body, `walgit_tasks_running{kind="repack"} 2`) {
		t.Fatalf("labeled gauge missing: %s", body)
	}
}

// TestFirstErrFirstBranch covers firstErr's non-nil first argument.
func TestFirstErrFirstBranch(t *testing.T) {
	boom := context.Canceled
	if firstErr(boom, nil) != boom {
		t.Fatal("firstErr must prefer the first error")
	}
}

// TestParseRangeNoDash covers the missing-dash branch.
func TestParseRangeNoDash(t *testing.T) {
	if _, _, ok := parseRange("bytes=", 10); ok {
		t.Fatal("missing dash must be unsatisfiable")
	}
}

// TestNoDelayListenerAcceptError covers the Accept-error path.
func TestNoDelayListenerAcceptError(t *testing.T) {
	s, _ := newTestServer(t, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := s.BuildListener(ln)
	ln.Close() // force Accept to fail
	if _, err := wrapped.Accept(); err == nil {
		t.Fatal("closed listener must surface an accept error")
	}
}
