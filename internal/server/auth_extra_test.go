package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// ExtraCredential (Seam 2 hook, first consumer: checks' wct_ CI tokens):
// claimed credentials resolve through the hook in token and oidc modes;
// unclaimed credentials fall through to the mode's normal resolution;
// nil hook preserves legacy behavior.

func hookServer(t *testing.T, mode string) (*Server, http.Handler) {
	s, h := newTestServer(t, nil)
	s.cfg.Server.Auth.Mode = mode
	svc := NewAuthService(&s.cfg.Server.Auth, s.Now)
	s.authSvc = svc
	return s, h
}

func TestExtraCredentialHook(t *testing.T) {
	for _, mode := range []string{"token", "oidc"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := hookServer(t, mode)
			s.authSvc.ExtraCredential = func(token string) (auth.Principal, *auth.AuthError, bool) {
				if token == "wct_abcd1234.secret" {
					return auth.Principal{Name: "ci:abcd1234"}, nil, true
				}
				if token == "wct_bad" {
					return auth.Principal{}, &auth.AuthError{Kind: auth.ErrInvalid, Why: "bad ci"}, true
				}
				return auth.Principal{}, nil, false
			}
			// Claimed + valid ⇒ hook principal (unprivileged).
			req := httptest.NewRequest("GET", "http://x/api/v1/me", nil)
			req.Header.Set("Authorization", "Bearer wct_abcd1234.secret")
			p, aerr := s.authSvc.Authenticate(req, s.cfg)
			if aerr != nil || p.Name != "ci:abcd1234" || p.Write || p.Admin {
				t.Fatalf("hook: %+v %v", p, aerr)
			}
			// Claimed + rejected ⇒ hook's 401 wins.
			req = httptest.NewRequest("GET", "http://x/api/v1/me", nil)
			req.Header.Set("Authorization", "Bearer wct_bad")
			if _, aerr := s.authSvc.Authenticate(req, s.cfg); aerr == nil {
				t.Fatal("hook rejection swallowed")
			}
			// Unclaimed ⇒ normal resolution (unknown static token 401s).
			req = httptest.NewRequest("GET", "http://x/api/v1/me", nil)
			req.Header.Set("Authorization", "Bearer nope")
			if _, aerr := s.authSvc.Authenticate(req, s.cfg); aerr == nil {
				t.Fatal("fall-through swallowed")
			}
			// Basic password form reaches the hook too.
			req = httptest.NewRequest("GET", "http://x/api/v1/me", nil)
			req.SetBasicAuth("ci", "wct_abcd1234.secret")
			p, aerr = s.authSvc.Authenticate(req, s.cfg)
			if aerr != nil || p.Name != "ci:abcd1234" {
				t.Fatalf("basic hook: %+v %v", p, aerr)
			}
		})
	}
}

func TestExtraCredentialNil(t *testing.T) {
	s, _ := hookServer(t, "token")
	// Nil hook: legacy behavior — unknown token 401s, empty is anonymous.
	req := httptest.NewRequest("GET", "http://x/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer wct_abcd1234.secret")
	if _, aerr := s.authSvc.Authenticate(req, s.cfg); aerr == nil {
		t.Fatal("nil hook claimed")
	}
	req = httptest.NewRequest("GET", "http://x/api/v1/me", nil)
	p, aerr := s.authSvc.Authenticate(req, s.cfg)
	if aerr != nil || !p.Anonymous {
		t.Fatalf("nil hook anon: %+v %v", p, aerr)
	}
}
