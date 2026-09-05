// forward_test.go — Forgejo issue #71 ([codex blocking] auth forwarding
// bypass on collaboration routes): every collab surface chained by
// chainCollab must honor the §8.6 broker-forwarding rule instead of
// re-authenticating the broker. Table-driven httptest over the SHIPPED
// composition (chainCollab wires the Auth closures production serves):
// a trusted forwarding broker's X-Walgit-Principal resolves to the
// forwarded principal on every family (identity, issues, pulls, review,
// checks, notify, releases, social, repoimport); direct auth is unchanged;
// an untrusted forwarder gets no forwarding; invalid credentials fail
// closed before forwarding is considered.
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"git.packden.us/crueber/walhub/internal/checks"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/issues"
	"git.packden.us/crueber/walhub/internal/notify"
	"git.packden.us/crueber/walhub/internal/pulls"
	"git.packden.us/crueber/walhub/internal/releases"
	"git.packden.us/crueber/walhub/internal/repoimport"
	"git.packden.us/crueber/walhub/internal/review"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/social"
)

type authFunc func(r *http.Request) (auth.Principal, *auth.AuthError)

// TestChainCollabHonorsForwardedIdentity wires the production chainCollab
// composition over bare handlers and asserts the forwarding contract on
// every collab route family.
func TestChainCollabHonorsForwardedIdentity(t *testing.T) {
	cfg := config.Defaults()
	cfg.Server.Auth.Mode = "token"
	cfg.Server.Auth.Tokens = []config.StaticToken{
		{Principal: "broker", Token: "brokertok", Write: true},
		{Principal: "alice", Token: "alicetok", Write: true},
	}
	cfg.Server.Auth.TrustedForwarders = []string{"broker"}
	srv := server.New(server.Options{Config: cfg})

	c := &collabWiring{
		identHandler:    &identity.Handler{},
		issuesHandler:   &issues.Handler{},
		pullsHandler:    &pulls.Handler{},
		reviewHandler:   &review.Handler{},
		checksHandler:   &checks.Handler{},
		releasesHandler: &releases.Handler{},
		socialHandler:   &social.Handler{},
		notifyHandler:   &notify.Handler{},
		importHandler:   &repoimport.Handler{},
	}
	chainCollab(srv, c)

	families := map[string]authFunc{
		"identity":   func(r *http.Request) (auth.Principal, *auth.AuthError) { return c.identHandler.Auth(r) },
		"issues":     func(r *http.Request) (auth.Principal, *auth.AuthError) { return c.issuesHandler.Auth(r) },
		"pulls":      func(r *http.Request) (auth.Principal, *auth.AuthError) { return c.pullsHandler.Auth(r) },
		"review":     func(r *http.Request) (auth.Principal, *auth.AuthError) { return c.reviewHandler.Auth(r) },
		"checks":     func(r *http.Request) (auth.Principal, *auth.AuthError) { return c.checksHandler.Auth(r) },
		"notify":     func(r *http.Request) (auth.Principal, *auth.AuthError) { return c.notifyHandler.Auth(r) },
		"releases":   func(r *http.Request) (auth.Principal, *auth.AuthError) { return c.releasesHandler.Auth(r) },
		"social":     func(r *http.Request) (auth.Principal, *auth.AuthError) { return c.socialHandler.Auth(r) },
		"repoimport": func(r *http.Request) (auth.Principal, *auth.AuthError) { return c.importHandler.Auth(r) },
	}
	if len(families) != 9 {
		t.Fatalf("families = %d, want 9 (every collab route family covered)", len(families))
	}

	mkreq := func(token, forwarded string) *http.Request {
		r := httptest.NewRequest("GET", "/o/r/api/v1/me", nil)
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
		// Trusted broker's forwarded identity honored, broker write kept.
		{name: "forwarded honored", token: "brokertok", forwarded: "carol", wantName: "carol", wantWrite: true},
		// Direct broker auth without a forwarding header is unchanged.
		{name: "direct unchanged", token: "brokertok", wantName: "broker", wantWrite: true},
		// Non-broker direct auth is unchanged.
		{name: "non-broker direct", token: "alicetok", wantName: "alice", wantWrite: true},
		// Untrusted forwarder (write, but no broker rights): no forwarding.
		{name: "untrusted no forwarding", token: "alicetok", forwarded: "carol", wantName: "alice", wantWrite: true},
		// Invalid credentials fail closed before forwarding is considered.
		{name: "invalid fails closed", token: "bogus", forwarded: "carol", wantErr: true},
	}

	for family, authFn := range families {
		if authFn == nil {
			t.Fatalf("family %s: Auth not wired by chainCollab", family)
		}
		for _, tc := range cases {
			t.Run(family+"/"+tc.name, func(t *testing.T) {
				p, aerr := authFn(mkreq(tc.token, tc.forwarded))
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
}
