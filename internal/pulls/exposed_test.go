package pulls

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/server/auth"
)

// TestExposedTemplatesExact pins the discovery list for the pulls surface
// (Forgejo #272): one entry per distinct path shape, in handler order.
// Any new route must extend this list in the same change (law 12).
func TestExposedTemplatesExact(t *testing.T) {
	want := []string{
		"/api/v1/repos/{owner}/{repo}/forks",
		"/{owner}/{repo}/api/pulls",
		"/{owner}/{repo}/api/pulls/{num}",
		"/{owner}/{repo}/api/pulls/{num}/diff",
		"/{owner}/{repo}/api/pulls/{num}/commits",
		"/{owner}/{repo}/api/pulls/{num}/comments",
		"/{owner}/{repo}/api/pulls/{num}/merge",
		"/{owner}/{repo}/api/pulls/{num}/merge/task",
		"/{owner}/{repo}/api/pulls/{num}/update-branch",
		"/{owner}/{repo}/api/pulls/{num}/head",
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

// TestExposedCoversRoutes audits the full pulls surface both ways: every
// route Handler serves (both lanes, every method — including 405s, which
// are recognized routes) is covered by its canonical ExposedTemplates
// entry, and every template entry covers at least one served route (no
// phantoms, nothing missing — Forgejo #272).
func TestExposedCoversRoutes(t *testing.T) {
	e := newTestEnv()
	e.h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{Name: "root", Admin: true}, nil
	}
	h := e.h

	served := []struct {
		name      string
		method    string
		path      string
		want      bool
		canonical string
	}{
		{"fork", "POST", "/api/v1/repos/o/r/forks", true, "/api/v1/repos/{owner}/{repo}/forks"},
		{"fork browser lane", "POST", "/api-browser/v1/repos/o/r/forks", true, "/api/v1/repos/{owner}/{repo}/forks"},
		{"fork wrong method recognized", "GET", "/api/v1/repos/o/r/forks", true, "/api/v1/repos/{owner}/{repo}/forks"},
		{"list", "GET", "/o/r/api/pulls", true, "/{owner}/{repo}/api/pulls"},
		{"open", "POST", "/o/r/api/pulls", true, "/{owner}/{repo}/api/pulls"},
		{"list browser lane", "GET", "/o/r/api-browser/pulls", true, "/{owner}/{repo}/api/pulls"},
		{"list git suffix", "GET", "/o/r.git/api/pulls", true, "/{owner}/{repo}/api/pulls"},
		{"get", "GET", "/o/r/api/pulls/1", true, "/{owner}/{repo}/api/pulls/{num}"},
		{"update", "PUT", "/o/r/api/pulls/1", true, "/{owner}/{repo}/api/pulls/{num}"},
		{"get wrong method recognized", "POST", "/o/r/api/pulls/1", true, "/{owner}/{repo}/api/pulls/{num}"},
		{"diff", "GET", "/o/r/api/pulls/1/diff", true, "/{owner}/{repo}/api/pulls/{num}/diff"},
		{"diff wrong method recognized", "POST", "/o/r/api/pulls/1/diff", true, "/{owner}/{repo}/api/pulls/{num}/diff"},
		{"commits", "GET", "/o/r/api/pulls/1/commits", true, "/{owner}/{repo}/api/pulls/{num}/commits"},
		{"comment", "POST", "/o/r/api/pulls/1/comments", true, "/{owner}/{repo}/api/pulls/{num}/comments"},
		{"comment wrong method recognized", "GET", "/o/r/api/pulls/1/comments", true, "/{owner}/{repo}/api/pulls/{num}/comments"},
		{"merge", "POST", "/o/r/api/pulls/1/merge", true, "/{owner}/{repo}/api/pulls/{num}/merge"},
		{"merge task", "GET", "/o/r/api/pulls/1/merge/task", true, "/{owner}/{repo}/api/pulls/{num}/merge/task"},
		{"merge task wrong method recognized", "POST", "/o/r/api/pulls/1/merge/task", true, "/{owner}/{repo}/api/pulls/{num}/merge/task"},
		{"update-branch", "POST", "/o/r/api/pulls/1/update-branch", true, "/{owner}/{repo}/api/pulls/{num}/update-branch"},
		{"head delete", "DELETE", "/o/r/api/pulls/1/head", true, "/{owner}/{repo}/api/pulls/{num}/head"},
		{"head wrong method recognized", "GET", "/o/r/api/pulls/1/head", true, "/{owner}/{repo}/api/pulls/{num}/head"},
		// Anything else must NOT claim the pulls surface (falls through
		// to the core mux or the review surface): uncovered paths need no
		// template.
		{"deep merge", "GET", "/o/r/api/pulls/1/merge/task/extra", false, ""},
		{"unknown leaf", "GET", "/o/r/api/pulls/1/nope", false, ""},
		{"other family", "GET", "/o/r/api/issues", false, ""},
		{"top level", "GET", "/api/v1/repos", false, ""},
		{"non repo", "GET", "/o/r/pulls", false, ""},
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
// not a pulls route).
func exposedLane(path string) string {
	p := strings.Replace(path, "/api-browser/", "/api/", 1)
	p = strings.Replace(p, ".git/api/", "/api/", 1)
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	return p
}
