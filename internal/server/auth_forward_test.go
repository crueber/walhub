package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
)

// TestAuthenticateForwarded pins the §8.6 broker-forwarding rule at the ONE
// entry point feature (Seam 1) surfaces must resolve through: Authenticate
// decides, identityForward rewrites, and authentication failures never
// forward (fail closed).
func TestAuthenticateForwarded(t *testing.T) {
	s, _ := newTestServer(t, func(o *Options) {
		o.Config.Server.Auth.Tokens = []config.StaticToken{
			{Principal: "broker", Token: "brokertok", Write: true},
			{Principal: "alice", Token: "alicetok", Write: true},
			{Principal: "edge", Token: "edgetok"}, // read-only, but trusted
		}
		o.Config.Server.Auth.TrustedForwarders = []string{"broker", "edge"}
	})

	mkreq := func(token, forwarded string) *http.Request {
		r := httptest.NewRequest("GET", "/x", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if forwarded != "" {
			r.Header.Set("X-Walgit-Principal", forwarded)
		}
		return r
	}

	cases := []struct {
		name      string
		token     string
		forwarded string
		wantName  string
		wantWrite bool
		wantErr   bool
	}{
		// Trusted broker's forwarded identity is honored, write kept.
		{name: "broker forwards", token: "brokertok", forwarded: "carol", wantName: "carol", wantWrite: true},
		// Direct auth without a forwarding header is unchanged.
		{name: "broker direct", token: "brokertok", wantName: "broker", wantWrite: true},
		// Untrusted writer's forwarding header is ignored (no forwarding).
		{name: "untrusted forwarder kept", token: "alicetok", forwarded: "carol", wantName: "alice", wantWrite: true},
		// Trusted name without write cannot forward (fail closed).
		{name: "read-only trusted kept", token: "edgetok", forwarded: "carol", wantName: "edge", wantWrite: false},
		// Anonymous callers never forward.
		{name: "anonymous kept", forwarded: "carol", wantName: "anonymous", wantWrite: false},
		// Invalid credentials fail before forwarding is considered.
		{name: "invalid token errors", token: "bogus", forwarded: "carol", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, aerr := s.authSvc.AuthenticateForwarded(mkreq(tc.token, tc.forwarded), s.cfg)
			if tc.wantErr {
				if aerr == nil {
					t.Fatalf("expected auth error, got principal %+v", p)
				}
				return
			}
			if aerr != nil {
				t.Fatalf("unexpected auth error: %v", aerr)
			}
			if p.Name != tc.wantName || p.Write != tc.wantWrite {
				t.Fatalf("principal = %+v, want name=%q write=%v", p, tc.wantName, tc.wantWrite)
			}
		})
	}
}
