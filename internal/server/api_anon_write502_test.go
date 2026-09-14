package server

// api_anon_write502_test.go — Forgejo #502: the oidc anonymous write-assert
// in apiServe (router.go). A state-changing request from an anonymous
// principal in oidc mode 401s WITHOUT invoking the handler — the catch-all
// holds even past a hypothetically-ungated handler. Reads pass through;
// other modes keep their per-route gates.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type countingProvider502 struct{ served int }

func (p *countingProvider502) Serve(w http.ResponseWriter, r *http.Request) {
	p.served++
	plainStatus(w, http.StatusOK, "reached")
}

func (p *countingProvider502) Owners(r *http.Request) ([]string, error) { return nil, nil }

// oidcServer502 builds a Server in oidc mode with anonymous_read on and a
// counting provider behind apiServe.
func oidcServer502(t *testing.T) (*Server, *countingProvider502) {
	t.Helper()
	s, _ := newTestServer(t, func(o *Options) {
		o.Config.Server.Auth.Mode = "oidc"
		o.Config.Server.Auth.AnonymousRead = true
	})
	prim := &countingProvider502{}
	s.api = prim
	return s, prim
}

func TestAPIServeOIDCAnonWriteAssert502(t *testing.T) {
	s, prim := oidcServer502(t)
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		prim.served = 0
		req := httptest.NewRequest(method, "/api/v1/ssh-keys", nil)
		rec := httptest.NewRecorder()
		s.apiServe(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", method, rec.Code)
		}
		if h := rec.Header().Get("WWW-Authenticate"); h != `Bearer realm="walgit"` {
			t.Errorf("%s: www-authenticate = %q", method, h)
		}
		if prim.served != 0 {
			t.Errorf("%s: handler ran for anonymous write", method)
		}
	}
}

func TestAPIServeOIDCAnonReadsPass502(t *testing.T) {
	s, prim := oidcServer502(t)
	for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
		prim.served = 0
		req := httptest.NewRequest(method, "/api/v1/owners", nil)
		rec := httptest.NewRecorder()
		s.apiServe(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want passthrough 200", method, rec.Code)
		}
		if prim.served != 1 {
			t.Errorf("%s: handler must run for anonymous read", method)
		}
	}
}

func TestAPIServeOIDCAuthedWritePasses502(t *testing.T) {
	s, prim := oidcServer502(t)
	// Static tokens work in the oidc tree — an authenticated write reaches
	// the handler (per-route gates decide from there).
	req := httptest.NewRequest("POST", "/api/v1/ssh-keys", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	s.apiServe(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("authed POST: status = %d, want passthrough 200", rec.Code)
	}
	if prim.served != 1 {
		t.Error("authed POST: handler must run")
	}
}

func TestAPIServeTokenModeKeepsPerRouteGates502(t *testing.T) {
	// Outside oidc mode the assert does not fire: anonymous writes reach
	// the seam, where the per-route gates refuse them (401 via gate()).
	s, _ := newTestServer(t, nil) // token mode
	prim := &countingProvider502{}
	s.api = prim
	req := httptest.NewRequest("POST", "/api/v1/ssh-keys", nil)
	rec := httptest.NewRecorder()
	s.apiServe(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("token-mode anonymous POST: status = %d, want seam passthrough 200", rec.Code)
	}
	if prim.served != 1 {
		t.Error("token-mode anonymous POST: seam must run (its own gates refuse)")
	}
}
