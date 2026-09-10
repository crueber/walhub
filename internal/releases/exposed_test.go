package releases

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// TestExposedTemplatesExact pins the discovery list for the releases
// surface (Forgejo #272): one entry per distinct path shape, in handler
// order. Any new route must extend this list in the same change (law 12).
func TestExposedTemplatesExact(t *testing.T) {
	want := []string{
		"/{owner}/{repo}/api/releases",
		"/{owner}/{repo}/api/releases/latest",
		"/{owner}/{repo}/api/releases/autodraft",
		"/{owner}/{repo}/api/releases/{tag}",
		"/{owner}/{repo}/api/releases/{tag}/assets/{name}",
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

// TestExposedCoversRoutes audits the full releases surface both ways:
// every route Handler serves (both lanes, every method — including 405s,
// which are recognized routes) is covered by its canonical
// ExposedTemplates entry, and every template entry covers at least one
// served route (no phantoms, nothing missing — Forgejo #272).
//
// The asset byte route (HandleRepo, outside the api lanes) is the static
// contract, not the JSON API, so it is deliberately not a template.
func TestExposedCoversRoutes(t *testing.T) {
	x := newHarness(t)
	x.handler.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "root", Admin: true}, nil
	}
	h := x.handler

	served := []struct {
		name      string
		method    string
		path      string
		want      bool
		canonical string
	}{
		{"list", "GET", "/o/r/api/releases", true, "/{owner}/{repo}/api/releases"},
		{"list browser lane", "GET", "/o/r/api-browser/releases", true, "/{owner}/{repo}/api/releases"},
		{"list git suffix", "GET", "/o/r.git/api/releases", true, "/{owner}/{repo}/api/releases"},
		{"list wrong method recognized", "POST", "/o/r/api/releases", true, "/{owner}/{repo}/api/releases"},
		{"latest", "GET", "/o/r/api/releases/latest", true, "/{owner}/{repo}/api/releases/latest"},
		{"autodraft", "GET", "/o/r/api/releases/autodraft?tag=v1", true, "/{owner}/{repo}/api/releases/autodraft"},
		{"get", "GET", "/o/r/api/releases/v1", true, "/{owner}/{repo}/api/releases/{tag}"},
		{"put", "PUT", "/o/r/api/releases/v1", true, "/{owner}/{repo}/api/releases/{tag}"},
		{"delete", "DELETE", "/o/r/api/releases/v1", true, "/{owner}/{repo}/api/releases/{tag}"},
		{"single wrong method recognized", "POST", "/o/r/api/releases/v1", true, "/{owner}/{repo}/api/releases/{tag}"},
		{"asset upload", "POST", "/o/r/api/releases/v1/assets/a.zip", true, "/{owner}/{repo}/api/releases/{tag}/assets/{name}"},
		{"asset delete", "DELETE", "/o/r/api/releases/v1/assets/a.zip", true, "/{owner}/{repo}/api/releases/{tag}/assets/{name}"},
		{"asset wrong method recognized", "GET", "/o/r/api/releases/v1/assets/a.zip", true, "/{owner}/{repo}/api/releases/{tag}/assets/{name}"},
		// Anything else must NOT claim the releases surface (falls through
		// to the core mux; asset bytes ride HandleRepo): uncovered paths
		// need no template. (Unknown shapes UNDER releases/ are claimed
		// 404s by the router — recognized but not endpoints, so they take
		// no discovery entry, same as 405s take none beyond their shape.)
		{"other family", "GET", "/o/r/api/pulls", false, ""},
		{"top level", "GET", "/api/v1/repos", false, ""},
		{"non repo", "GET", "/o/r/releases", false, ""},
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

// exposedLane normalizes a request path to the /{owner}/{repo}/api lane
// spelling the templates use (both lanes serve the same shapes; the
// .git suffix is core dispatch, not a releases route).
func exposedLane(path string) string {
	p := strings.Replace(path, "/api-browser/", "/api/", 1)
	p = strings.Replace(p, ".git/api/", "/api/", 1)
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	return p
}
