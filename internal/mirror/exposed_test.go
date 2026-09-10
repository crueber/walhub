package mirror

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExposedTemplatesExact pins the discovery list for the mirror surface
// (Forgejo #272 extends the #271-era top-level-only list with the repo
// lanes): one entry per distinct path shape. Any new route must extend
// this list in the same change (law 12).
func TestExposedTemplatesExact(t *testing.T) {
	want := []string{
		"/api/v1/repos/mirrors",
		"/{owner}/{repo}/api/mirror",
		"/{owner}/{repo}/api/mirror/sync",
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

// TestExposedCoversRoutes audits the full mirror surface both ways: every
// route Handler serves (both lanes, every method — including 405s, which
// are recognized routes) is covered by its canonical ExposedTemplates
// entry, and every template entry covers at least one served route (no
// phantoms, nothing missing — Forgejo #272).
func TestExposedCoversRoutes(t *testing.T) {
	reg, st := testRegistry(t)
	svc := testService(t, st, reg)
	h := testHandler(t, svc, reg, adminP)

	served := []struct {
		name      string
		method    string
		path      string
		want      bool
		canonical string
	}{
		{"create from URL", "POST", "/api/v1/repos/mirrors", true, "/api/v1/repos/mirrors"},
		{"create browser lane", "POST", "/api-browser/v1/repos/mirrors", true, "/api/v1/repos/mirrors"},
		{"create wrong method recognized", "GET", "/api/v1/repos/mirrors", true, "/api/v1/repos/mirrors"},
		{"mirror get", "GET", "/o/r/api/mirror", true, "/{owner}/{repo}/api/mirror"},
		{"mirror put", "PUT", "/o/r/api/mirror", true, "/{owner}/{repo}/api/mirror"},
		{"mirror delete", "DELETE", "/o/r/api/mirror", true, "/{owner}/{repo}/api/mirror"},
		{"mirror browser lane", "GET", "/o/r/api-browser/mirror", true, "/{owner}/{repo}/api/mirror"},
		{"mirror wrong method recognized", "POST", "/o/r/api/mirror", true, "/{owner}/{repo}/api/mirror"},
		{"sync now", "POST", "/o/r/api/mirror/sync", true, "/{owner}/{repo}/api/mirror/sync"},
		{"sync status", "GET", "/o/r/api/mirror/sync", true, "/{owner}/{repo}/api/mirror/sync"},
		{"sync wrong method recognized", "PUT", "/o/r/api/mirror/sync", true, "/{owner}/{repo}/api/mirror/sync"},
		// Anything else must NOT claim the mirror surface (falls through
		// to the core mux): uncovered paths need no template.
		{"deep mirror", "GET", "/o/r/api/mirror/sync/extra", false, ""},
		{"other family", "GET", "/o/r/api/pulls", false, ""},
		{"top level", "GET", "/api/v1/repos", false, ""},
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
// (both lanes serve the same shapes).
func exposedLane(path string) string {
	p := strings.Replace(path, "/api-browser/", "/api/", 1)
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	return p
}
