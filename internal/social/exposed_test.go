package social

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// TestExposedTemplatesExact pins the discovery list for the social surface
// (Forgejo #272): one entry per distinct path shape, in handler order.
// Any new route must extend this list in the same change (law 12).
func TestExposedTemplatesExact(t *testing.T) {
	want := []string{
		"/{owner}/{repo}/api/star",
		"/{owner}/{repo}/api/social",
		"/api/v1/me/starred",
		"/api/v1/users/{principal}/starred",
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

// TestExposedCoversRoutes audits the full social surface both ways: every
// route Handler serves (both lanes, every method — including 405s, which
// are recognized routes) is covered by its canonical ExposedTemplates
// entry, and every template entry covers at least one served route (no
// phantoms, nothing missing — Forgejo #272).
func TestExposedCoversRoutes(t *testing.T) {
	x := newHarness(t)
	x.handler.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "jane"}, nil
	}
	h := x.handler

	served := []struct {
		name      string
		method    string
		path      string
		want      bool
		canonical string
	}{
		{"star", "PUT", "/o/r/api/star", true, "/{owner}/{repo}/api/star"},
		{"unstar", "DELETE", "/o/r/api/star", true, "/{owner}/{repo}/api/star"},
		{"star browser lane", "PUT", "/o/r/api-browser/star", true, "/{owner}/{repo}/api/star"},
		{"star git suffix", "PUT", "/o/r.git/api/star", true, "/{owner}/{repo}/api/star"},
		{"star wrong method recognized", "GET", "/o/r/api/star", true, "/{owner}/{repo}/api/star"},
		{"social", "GET", "/o/r/api/social", true, "/{owner}/{repo}/api/social"},
		{"social browser lane", "GET", "/o/r/api-browser/social", true, "/{owner}/{repo}/api/social"},
		{"social wrong method recognized", "POST", "/o/r/api/social", true, "/{owner}/{repo}/api/social"},
		{"my starred", "GET", "/api/v1/me/starred", true, "/api/v1/me/starred"},
		{"my starred browser lane", "GET", "/api-browser/v1/me/starred", true, "/api/v1/me/starred"},
		{"my starred wrong method recognized", "POST", "/api/v1/me/starred", true, "/api/v1/me/starred"},
		{"user starred", "GET", "/api/v1/users/jane/starred", true, "/api/v1/users/{principal}/starred"},
		{"user starred wrong method recognized", "DELETE", "/api/v1/users/jane/starred", true, "/api/v1/users/{principal}/starred"},
		// Anything else must NOT claim the social surface (falls through
		// to the core mux or notify's watch surface): uncovered paths need
		// no template.
		{"deep star", "GET", "/o/r/api/star/extra", false, ""},
		{"other family", "GET", "/o/r/api/watch", false, ""},
		{"top level", "GET", "/api/v1/repos", false, ""},
		{"non repo", "GET", "/o/r/star", false, ""},
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
// not a social route).
func exposedLane(path string) string {
	p := strings.Replace(path, "/api-browser/", "/api/", 1)
	p = strings.Replace(p, ".git/api/", "/api/", 1)
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	return p
}
