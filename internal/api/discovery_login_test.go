package api

import (
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/config"
)

// TestDiscoveryBrowserLoginAdvertisement pins the #344 discovery half: the
// auth block reports browser_login + login_url exactly when the OIDC browser
// flow can start (mode=oidc + the session/client trio), so the SPA renders
// the "Log in with OIDC" button only when it works.
func TestDiscoveryBrowserLoginAdvertisement(t *testing.T) {
	trio := func(c *config.Config) {
		c.Server.Auth.Mode = "oidc"
		c.Server.Auth.SessionSecret = strings.Repeat("x", 32)
		c.Server.Auth.OAuthClientID = "id"
		c.Server.Auth.OAuthClientSecret = "secret"
	}
	cases := []struct {
		name      string
		mutate    func(*config.Config)
		wantLogin bool
		wantURL   string
	}{
		{"token mode disabled", func(c *config.Config) {}, false, ""},
		{"oidc trio enabled", trio, true, "/_auth/login"},
		{"oidc missing session_secret disabled", func(c *config.Config) {
			trio(c)
			c.Server.Auth.SessionSecret = ""
		}, false, ""},
		{"oidc missing oauth_client_id disabled", func(c *config.Config) {
			trio(c)
			c.Server.Auth.OAuthClientID = ""
		}, false, ""},
		{"oidc missing oauth_client_secret disabled", func(c *config.Config) {
			trio(c)
			c.Server.Auth.OAuthClientSecret = ""
		}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.mutate(f.env.Cfg)
			w := f.do("GET", "/api/v1", nil, nil, nil)
			if w.Code != 200 {
				t.Fatalf("status = %d", w.Code)
			}
			var doc struct {
				Auth struct {
					Bearer       bool   `json:"bearer"`
					Setup        string `json:"setup"`
					Browser      string `json:"browser"`
					Authenticate string `json:"authenticate"`
					BrowserLogin bool   `json:"browser_login"`
					LoginURL     string `json:"login_url"`
				} `json:"auth"`
			}
			decodeJSON(t, w, &doc)
			if doc.Auth.BrowserLogin != tc.wantLogin {
				t.Fatalf("browser_login = %v, want %v", doc.Auth.BrowserLogin, tc.wantLogin)
			}
			if doc.Auth.LoginURL != tc.wantURL {
				t.Fatalf("login_url = %q, want %q", doc.Auth.LoginURL, tc.wantURL)
			}
			// The pre-existing members never move (contract stability).
			if !doc.Auth.Bearer || doc.Auth.Setup != "/services/setup.json" ||
				doc.Auth.Browser != "/api-browser/v1" || doc.Auth.Authenticate != "/api/v1/authenticate" {
				t.Fatalf("auth block regressed: %+v", doc.Auth)
			}
		})
	}
}
