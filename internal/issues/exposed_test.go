package issues

import (
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// TestExposedTemplatesExact pins the discovery list for the issues surface
// (Forgejo #272): one entry per distinct path shape, in handler order.
// Any new route must extend this list in the same change (law 12).
func TestExposedTemplatesExact(t *testing.T) {
	want := []string{
		"/{owner}/{repo}/api/issues",
		"/{owner}/{repo}/api/issues/{num}",
		"/{owner}/{repo}/api/issues/{num}/events",
		"/{owner}/{repo}/api/issues/{num}/comments",
		"/{owner}/{repo}/api/issues/{num}/reactions",
		"/{owner}/{repo}/api/issues/{num}/reactions/{seq}/{content}",
		"/{owner}/{repo}/api/labels",
		"/{owner}/{repo}/api/labels/{name}",
		"/{owner}/{repo}/api/milestones",
		"/{owner}/{repo}/api/milestones/{id}",
		"/{owner}/{repo}/api/attachments",
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

// exposedMatch reports whether path ("/o/r/api/issues/1") fits template
// ("/{owner}/{repo}/api/issues/{num}"): literals must match, {name}
// matches any single non-empty segment.
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

// TestExposedCoversRoutes audits the full issues surface both ways: every
// route Handler serves (both lanes, every method — including 405s, which
// are recognized routes) is covered by its canonical ExposedTemplates
// entry, and every template entry covers at least one served route (no
// phantoms, nothing missing — Forgejo #272).
func TestExposedCoversRoutes(t *testing.T) {
	roles := newFakeRoles()
	svc := testService(roles)
	h := testHandler(svc, auth.Principal{Name: "root", Admin: true})

	served := []struct {
		name      string
		method    string
		path      string
		want      bool
		canonical string
	}{
		{"list", "GET", "/o/r/api/issues", true, "/{owner}/{repo}/api/issues"},
		{"create", "POST", "/o/r/api/issues", true, "/{owner}/{repo}/api/issues"},
		{"list browser lane", "GET", "/o/r/api-browser/issues", true, "/{owner}/{repo}/api/issues"},
		{"list git suffix", "GET", "/o/r.git/api/issues", true, "/{owner}/{repo}/api/issues"},
		{"list wrong method recognized", "DELETE", "/o/r/api/issues", true, "/{owner}/{repo}/api/issues"},
		{"get", "GET", "/o/r/api/issues/1", true, "/{owner}/{repo}/api/issues/{num}"},
		{"patch", "PATCH", "/o/r/api/issues/1", true, "/{owner}/{repo}/api/issues/{num}"},
		{"get wrong method recognized", "POST", "/o/r/api/issues/1", true, "/{owner}/{repo}/api/issues/{num}"},
		{"events", "GET", "/o/r/api/issues/1/events", true, "/{owner}/{repo}/api/issues/{num}/events"},
		{"events wrong method recognized", "POST", "/o/r/api/issues/1/events", true, "/{owner}/{repo}/api/issues/{num}/events"},
		{"comment", "POST", "/o/r/api/issues/1/comments", true, "/{owner}/{repo}/api/issues/{num}/comments"},
		{"comment wrong method recognized", "GET", "/o/r/api/issues/1/comments", true, "/{owner}/{repo}/api/issues/{num}/comments"},
		{"react", "POST", "/o/r/api/issues/1/reactions", true, "/{owner}/{repo}/api/issues/{num}/reactions"},
		{"unreact", "DELETE", "/o/r/api/issues/1/reactions/3/+1", true, "/{owner}/{repo}/api/issues/{num}/reactions/{seq}/{content}"},
		{"labels list", "GET", "/o/r/api/labels", true, "/{owner}/{repo}/api/labels"},
		{"labels create", "POST", "/o/r/api/labels", true, "/{owner}/{repo}/api/labels"},
		{"label update", "PATCH", "/o/r/api/labels/bug", true, "/{owner}/{repo}/api/labels/{name}"},
		{"label delete", "DELETE", "/o/r/api/labels/bug", true, "/{owner}/{repo}/api/labels/{name}"},
		{"label wrong method recognized", "GET", "/o/r/api/labels/bug", true, "/{owner}/{repo}/api/labels/{name}"},
		{"milestones list", "GET", "/o/r/api/milestones", true, "/{owner}/{repo}/api/milestones"},
		{"milestones create", "POST", "/o/r/api/milestones", true, "/{owner}/{repo}/api/milestones"},
		{"milestone get", "GET", "/o/r/api/milestones/abcdef", true, "/{owner}/{repo}/api/milestones/{id}"},
		{"milestone patch", "PATCH", "/o/r/api/milestones/abcdef", true, "/{owner}/{repo}/api/milestones/{id}"},
		{"milestone delete", "DELETE", "/o/r/api/milestones/abcdef", true, "/{owner}/{repo}/api/milestones/{id}"},
		{"attachments upload", "POST", "/o/r/api/attachments", true, "/{owner}/{repo}/api/attachments"},
		{"attachments browser lane", "POST", "/o/r/api-browser/attachments", true, "/{owner}/{repo}/api/attachments"},
		{"attachments wrong method recognized", "GET", "/o/r/api/attachments", true, "/{owner}/{repo}/api/attachments"},
		// Anything else must NOT claim the issues surface (falls through
		// to the core mux or the repo-subpath byte route): uncovered paths
		// need no template.
		{"deep labels", "GET", "/o/r/api/labels/a/b", false, ""},
		{"bad milestone id", "GET", "/o/r/api/milestones/nothex", false, ""},
		{"deep reactions", "DELETE", "/o/r/api/issues/1/reactions/3/x/y", false, ""},
		{"other family", "GET", "/o/r/api/pulls", false, ""},
		{"top level", "GET", "/api/v1/repos", false, ""},
		{"non repo", "GET", "/o/r/issues", false, ""},
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
// .git suffix is core dispatch, not an issues route).
func exposedLane(path string) string {
	p := strings.Replace(path, "/api-browser/", "/api/", 1)
	p = strings.Replace(p, ".git/api/", "/api/", 1)
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	return p
}
