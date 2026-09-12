package identity

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExposedTemplatesExact pins the discovery list for the identity
// surface (Forgejo #272): one entry per distinct path shape, in handler
// order. Any new route must extend this list in the same change (law 12).
func TestExposedTemplatesExact(t *testing.T) {
	want := []string{
		"/api/v1/users/{principal}",
		"/api/v1/orgs",
		"/api/v1/orgs/{org}",
		"/api/v1/orgs/{org}/avatar",
		"/api/v1/orgs/{org}/members",
		"/api/v1/orgs/{org}/members/{principal}",
		"/api/v1/orgs/{org}/teams",
		"/api/v1/orgs/{org}/teams/{slug}",
		"/api/v1/orgs/{org}/teams/{slug}/members/{principal}",
		"/api/v1/orgs/{org}/invitations",
		"/api/v1/orgs/{org}/invitations/{id}",
		"/api/v1/invitations",
		"/api/v1/invitations/{id}",
		"/api/v1/invitations/{id}/accept",
		"/{owner}/{repo}/api/access",
		"/{owner}/{repo}/api/permissions",
		"/{owner}/{repo}/api/collaborators",
		"/{owner}/{repo}/api/assignables",
		"/{owner}/{repo}/api/invitations",
		"/{owner}/{repo}/api/invitations/{id}",
		"/{owner}/{repo}/api/transfer",
	}
	if len(ExposedTemplates) != len(want) {
		t.Fatalf("ExposedTemplates = %v, want %v", ExposedTemplates, want)
	}
	for i := range want {
		if ExposedTemplates[i] != want[i] {
			t.Fatalf("ExposedTemplates[%d] = %q, want %q", i, ExposedTemplates[i], want[i])
		}
	}
}

// exposedMatch reports whether path fits template: literals must match,
// {name} matches any single non-empty segment.
func exposedMatch(template, path string) bool {
	toks := strings.Split(strings.TrimPrefix(template, "/"), "/")
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(toks) != len(segs) {
		return false
	}
	for i, tok := range toks {
		if strings.HasPrefix(tok, "{") && strings.HasSuffix(tok, "}") {
			if segs[i] == "" {
				return false
			}
			continue
		}
		if tok != segs[i] {
			return false
		}
	}
	return true
}

// TestExposedCoversRoutes audits the full identity surface both ways:
// every route Handler serves (both lanes, every method — including 405s,
// which are recognized routes) is covered by its canonical
// ExposedTemplates entry, and every template entry covers at least one
// served route (no phantoms, nothing missing — Forgejo #272).
func TestExposedCoversRoutes(t *testing.T) {
	s := testService()
	h := testHandler(s, admin)

	served := []struct {
		name      string
		method    string
		path      string
		want      bool
		canonical string
	}{
		{"user get", "GET", "/api/v1/users/alice@example.com", true, "/api/v1/users/{principal}"},
		{"user put", "PUT", "/api/v1/users/alice@example.com", true, "/api/v1/users/{principal}"},
		{"user browser lane", "GET", "/api-browser/v1/users/alice@example.com", true, "/api/v1/users/{principal}"},
		{"orgs list", "GET", "/api/v1/orgs", true, "/api/v1/orgs"},
		{"orgs create", "POST", "/api/v1/orgs", true, "/api/v1/orgs"},
		{"org get", "GET", "/api/v1/orgs/acme", true, "/api/v1/orgs/{org}"},
		{"org put", "PUT", "/api/v1/orgs/acme", true, "/api/v1/orgs/{org}"},
		{"org delete", "DELETE", "/api/v1/orgs/acme", true, "/api/v1/orgs/{org}"},
		{"org avatar get", "GET", "/api/v1/orgs/acme/avatar", true, "/api/v1/orgs/{org}/avatar"},
		{"org avatar put", "PUT", "/api/v1/orgs/acme/avatar", true, "/api/v1/orgs/{org}/avatar"},
		{"org avatar delete", "DELETE", "/api/v1/orgs/acme/avatar", true, "/api/v1/orgs/{org}/avatar"},
		{"org avatar browser lane", "GET", "/api-browser/v1/orgs/acme/avatar", true, "/api/v1/orgs/{org}/avatar"},
		{"members list", "GET", "/api/v1/orgs/acme/members", true, "/api/v1/orgs/{org}/members"},
		{"member get", "GET", "/api/v1/orgs/acme/members/alice@example.com", true, "/api/v1/orgs/{org}/members/{principal}"},
		{"member put", "PUT", "/api/v1/orgs/acme/members/alice@example.com", true, "/api/v1/orgs/{org}/members/{principal}"},
		{"member delete", "DELETE", "/api/v1/orgs/acme/members/alice@example.com", true, "/api/v1/orgs/{org}/members/{principal}"},
		{"teams list", "GET", "/api/v1/orgs/acme/teams", true, "/api/v1/orgs/{org}/teams"},
		{"teams create", "POST", "/api/v1/orgs/acme/teams", true, "/api/v1/orgs/{org}/teams"},
		{"team get", "GET", "/api/v1/orgs/acme/teams/core", true, "/api/v1/orgs/{org}/teams/{slug}"},
		{"team put", "PUT", "/api/v1/orgs/acme/teams/core", true, "/api/v1/orgs/{org}/teams/{slug}"},
		{"team delete", "DELETE", "/api/v1/orgs/acme/teams/core", true, "/api/v1/orgs/{org}/teams/{slug}"},
		{"team member put", "PUT", "/api/v1/orgs/acme/teams/core/members/alice@example.com", true, "/api/v1/orgs/{org}/teams/{slug}/members/{principal}"},
		{"team member delete", "DELETE", "/api/v1/orgs/acme/teams/core/members/alice@example.com", true, "/api/v1/orgs/{org}/teams/{slug}/members/{principal}"},
		{"org invites list", "GET", "/api/v1/orgs/acme/invitations", true, "/api/v1/orgs/{org}/invitations"},
		{"org invites create", "POST", "/api/v1/orgs/acme/invitations", true, "/api/v1/orgs/{org}/invitations"},
		{"org invite cancel", "DELETE", "/api/v1/orgs/acme/invitations/abc123", true, "/api/v1/orgs/{org}/invitations/{id}"},
		{"invites mine", "GET", "/api/v1/invitations", true, "/api/v1/invitations"},
		{"invite preview", "GET", "/api/v1/invitations/abc123", true, "/api/v1/invitations/{id}"},
		{"invite decline", "DELETE", "/api/v1/invitations/abc123", true, "/api/v1/invitations/{id}"},
		{"invite accept", "POST", "/api/v1/invitations/abc123/accept", true, "/api/v1/invitations/{id}/accept"},
		{"access get", "GET", "/o/r/api/access", true, "/{owner}/{repo}/api/access"},
		{"access put", "PUT", "/o/r/api/access", true, "/{owner}/{repo}/api/access"},
		{"access browser lane", "GET", "/o/r/api-browser/access", true, "/{owner}/{repo}/api/access"},
		{"permissions", "GET", "/o/r/api/permissions", true, "/{owner}/{repo}/api/permissions"},
		{"collaborators", "GET", "/o/r/api/collaborators", true, "/{owner}/{repo}/api/collaborators"},
		{"assignables", "GET", "/o/r/api/assignables", true, "/{owner}/{repo}/api/assignables"},
		{"repo invites list", "GET", "/o/r/api/invitations", true, "/{owner}/{repo}/api/invitations"},
		{"repo invites create", "POST", "/o/r/api/invitations", true, "/{owner}/{repo}/api/invitations"},
		{"repo invite cancel", "DELETE", "/o/r/api/invitations/abc123", true, "/{owner}/{repo}/api/invitations/{id}"},
		{"repo transfer", "POST", "/o/r/api/transfer", true, "/{owner}/{repo}/api/transfer"},
		{"repo transfer browser lane", "POST", "/o/r/api-browser/transfer", true, "/{owner}/{repo}/api/transfer"},
		// Anything else must NOT claim the identity surface (falls through
		// to the core mux): uncovered paths need no template.
		{"unknown top", "GET", "/api/v1/nope", false, ""},
		{"unknown org leaf", "GET", "/api/v1/orgs/acme/nope", false, ""},
		{"deep access", "GET", "/o/r/api/access/extra", false, ""},
		{"other family", "GET", "/o/r/api/issues", false, ""},
		{"non repo", "GET", "/o/r/access", false, ""},
	}

	covered := make([]bool, len(ExposedTemplates))
	for _, tc := range served {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		if got := h.Handle(rec, req); got != tc.want {
			t.Errorf("%s: Handle(%s %s) = %v, want %v (status %d)",
				tc.name, tc.method, tc.path, got, tc.want, rec.Code)
			continue
		}
		if !tc.want {
			continue
		}
		hits := 0
		canonical := false
		for i, tmpl := range ExposedTemplates {
			if exposedMatch(tmpl, exposedLane(tc.path)) {
				hits++
				covered[i] = true
				if tmpl == tc.canonical {
					canonical = true
				}
			}
		}
		if hits == 0 {
			t.Errorf("%s: %s matches no template (missing discovery entry)", tc.name, tc.path)
		}
		if !canonical {
			t.Errorf("%s: %s not covered by canonical template %q",
				tc.name, tc.path, tc.canonical)
		}
	}
	for i, tmpl := range ExposedTemplates {
		if !covered[i] {
			t.Errorf("template %q covers no served route (phantom entry)", tmpl)
		}
	}
}

// exposedLane normalizes a request path to the spelling the templates use
// (both lanes serve the same shapes; the .git suffix is core dispatch,
// not an identity route).
func exposedLane(path string) string {
	p := strings.Replace(path, "/api-browser/", "/api/", 1)
	p = strings.Replace(p, ".git/api/", "/api/", 1)
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	return p
}
