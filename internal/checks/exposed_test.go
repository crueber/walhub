package checks

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// TestExposedTemplatesExact pins the discovery list for the checks surface
// (Forgejo #271): one entry per distinct path shape, in handler order.
// Any new route must extend this list in the same change (law 12).
func TestExposedTemplatesExact(t *testing.T) {
	want := []string{
		"/{owner}/{repo}/api/checks",
		"/{owner}/{repo}/api/checks/{sha}",
		"/{owner}/{repo}/api/checks/statuses/{sha}",
		"/{owner}/{repo}/api/checks/tokens",
		"/{owner}/{repo}/api/checks/tokens/{id}",
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

// templateMatch reports whether path ("/o/r/api/checks/<sha>") fits template
// ("/{owner}/{repo}/api/checks/{sha}"): literals must match, {name} matches
// any single non-empty segment.
func templateMatch(template, path string) bool {
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

// TestExposedCoversRoutes audits the full checks surface both ways: every
// route Handler serves (both lanes, every method — including 405s, which
// are recognized routes) is covered by its canonical ExposedTemplates
// entry, and every template entry covers at least one served route (no
// phantoms, nothing missing — Forgejo #271).
//
// Note the one deliberate overlap: /checks/tokens also shape-matches
// /checks/{sha}. The router disambiguates (handleRepo matches the "tokens"
// literal before the {sha} fallthrough), so the canonical template for the
// token roots is /checks/tokens — the assertion pins that, not uniqueness.
func TestExposedCoversRoutes(t *testing.T) {
	e, h := testHandler()
	sha := hexSHA(52)
	e.knowSHA(sha)
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) { return admin(), nil }

	served := []struct {
		name      string
		method    string
		path      string
		want      bool
		canonical string
	}{
		{"list", "GET", "/o/r/api/checks", true, "/{owner}/{repo}/api/checks"},
		{"list browser lane", "GET", "/o/r/api-browser/checks", true, "/{owner}/{repo}/api/checks"},
		{"list git suffix", "GET", "/o/r.git/api/checks", true, "/{owner}/{repo}/api/checks"},
		{"list wrong method recognized", "POST", "/o/r/api/checks", true, "/{owner}/{repo}/api/checks"},
		{"combined", "GET", "/o/r/api/checks/" + sha, true, "/{owner}/{repo}/api/checks/{sha}"},
		{"combined browser lane", "GET", "/o/r/api-browser/checks/" + sha, true, "/{owner}/{repo}/api/checks/{sha}"},
		{"combined wrong method recognized", "POST", "/o/r/api/checks/" + sha, true, "/{owner}/{repo}/api/checks/{sha}"},
		{"statuses", "GET", "/o/r/api/checks/statuses/" + sha, true, "/{owner}/{repo}/api/checks/statuses/{sha}"},
		{"statuses browser lane", "GET", "/o/r/api-browser/checks/statuses/" + sha, true, "/{owner}/{repo}/api/checks/statuses/{sha}"},
		{"report", "POST", "/o/r/api/checks/statuses/" + sha, true, "/{owner}/{repo}/api/checks/statuses/{sha}"},
		{"statuses wrong method recognized", "PUT", "/o/r/api/checks/statuses/" + sha, true, "/{owner}/{repo}/api/checks/statuses/{sha}"},
		{"tokens list", "GET", "/o/r/api/checks/tokens", true, "/{owner}/{repo}/api/checks/tokens"},
		{"tokens create", "POST", "/o/r/api/checks/tokens", true, "/{owner}/{repo}/api/checks/tokens"},
		{"tokens wrong method recognized", "PUT", "/o/r/api/checks/tokens", true, "/{owner}/{repo}/api/checks/tokens"},
		{"token revoke", "DELETE", "/o/r/api/checks/tokens/abcd1234", true, "/{owner}/{repo}/api/checks/tokens/{id}"},
		{"token revoke wrong method recognized", "GET", "/o/r/api/checks/tokens/abcd1234", true, "/{owner}/{repo}/api/checks/tokens/{id}"},
		// Anything else must NOT claim the checks surface (falls through
		// to the core mux): uncovered paths need no template.
		{"extra segment", "GET", "/o/r/api/checks/" + sha + "/extra", false, ""},
		{"deep statuses", "GET", "/o/r/api/checks/statuses/" + sha + "/extra", false, ""},
		{"deep tokens", "GET", "/o/r/api/checks/tokens/a/b", false, ""},
		{"other family", "GET", "/o/r/api/pulls", false, ""},
		{"top level", "GET", "/api/v1/repos", false, ""},
		{"non repo", "GET", "/o/r/checks", false, ""},
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
			if templateMatch(tmpl, laneStrip(tc.path)) {
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

// laneStrip normalizes a request path to the /{owner}/{repo}/api lane
// spelling the templates use (both lanes serve the same shapes; the
// .git suffix is core dispatch, not a checks route).
func laneStrip(path string) string {
	p := strings.Replace(path, "/api-browser/", "/api/", 1)
	p = strings.Replace(p, ".git/api/", "/api/", 1)
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	return p
}
