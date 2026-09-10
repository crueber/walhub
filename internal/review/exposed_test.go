package review

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExposedTemplatesExact pins the discovery list for the review surface
// (Forgejo #272): one entry per distinct path shape, in handler order.
// Any new route must extend this list in the same change (law 12).
func TestExposedTemplatesExact(t *testing.T) {
	want := []string{
		"/{owner}/{repo}/api/pulls/{num}/reviews",
		"/{owner}/{repo}/api/pulls/{num}/reviews/{seq}",
		"/{owner}/{repo}/api/pulls/{num}/reviews/{seq}/dismiss",
		"/{owner}/{repo}/api/pulls/{num}/threads",
		"/{owner}/{repo}/api/pulls/{num}/threads/{id}",
		"/{owner}/{repo}/api/pulls/{num}/threads/{id}/comments",
		"/{owner}/{repo}/api/pulls/{num}/threads/{id}/resolve",
		"/{owner}/{repo}/api/pulls/{num}/threads/{id}/unresolve",
		"/{owner}/{repo}/api/pulls/{num}/review-requests",
		"/{owner}/{repo}/api/pulls/{num}/review-suggest",
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

// TestExposedCoversRoutes audits the full review surface both ways: every
// route Handler serves (both lanes, every method — including 405s, which
// are recognized routes) is covered by its canonical ExposedTemplates
// entry, and every template entry covers at least one served route (no
// phantoms, nothing missing — Forgejo #272).
func TestExposedCoversRoutes(t *testing.T) {
	h, _ := testHandler(t)
	// Thread-leaf routes claim before the service lookup, so an unseeded
	// tid exercises the same claim path (via 404/400) without fixtures.
	const tid = "00000001"

	served := []struct {
		name      string
		method    string
		path      string
		want      bool
		canonical string
	}{
		{"reviews list", "GET", "/o/r/api/pulls/7/reviews", true, "/{owner}/{repo}/api/pulls/{num}/reviews"},
		{"reviews submit", "POST", "/o/r/api/pulls/7/reviews", true, "/{owner}/{repo}/api/pulls/{num}/reviews"},
		{"reviews browser lane", "GET", "/o/r/api-browser/pulls/7/reviews", true, "/{owner}/{repo}/api/pulls/{num}/reviews"},
		{"reviews wrong method recognized", "PUT", "/o/r/api/pulls/7/reviews", true, "/{owner}/{repo}/api/pulls/{num}/reviews"},
		{"review get", "GET", "/o/r/api/pulls/7/reviews/0", true, "/{owner}/{repo}/api/pulls/{num}/reviews/{seq}"},
		{"review get wrong method recognized", "POST", "/o/r/api/pulls/7/reviews/0", true, "/{owner}/{repo}/api/pulls/{num}/reviews/{seq}"},
		{"review dismiss", "POST", "/o/r/api/pulls/7/reviews/0/dismiss", true, "/{owner}/{repo}/api/pulls/{num}/reviews/{seq}/dismiss"},
		{"threads list", "GET", "/o/r/api/pulls/7/threads", true, "/{owner}/{repo}/api/pulls/{num}/threads"},
		{"threads open", "POST", "/o/r/api/pulls/7/threads", true, "/{owner}/{repo}/api/pulls/{num}/threads"},
		{"thread get", "GET", "/o/r/api/pulls/7/threads/t1", true, "/{owner}/{repo}/api/pulls/{num}/threads/{id}"},
		{"thread comment", "POST", "/o/r/api/pulls/7/threads/00000001/comments", true, "/{owner}/{repo}/api/pulls/{num}/threads/{id}/comments"},
		{"thread resolve", "POST", "/o/r/api/pulls/7/threads/00000001/resolve", true, "/{owner}/{repo}/api/pulls/{num}/threads/{id}/resolve"},
		{"thread unresolve", "POST", "/o/r/api/pulls/7/threads/00000001/unresolve", true, "/{owner}/{repo}/api/pulls/{num}/threads/{id}/unresolve"},
		{"thread leaf wrong method recognized", "GET", "/o/r/api/pulls/7/threads/00000001/comments", true, "/{owner}/{repo}/api/pulls/{num}/threads/{id}/comments"},
		{"review-requests", "GET", "/o/r/api/pulls/7/review-requests", true, "/{owner}/{repo}/api/pulls/{num}/review-requests"},
		{"review-requests add", "POST", "/o/r/api/pulls/7/review-requests", true, "/{owner}/{repo}/api/pulls/{num}/review-requests"},
		{"review-requests remove", "DELETE", "/o/r/api/pulls/7/review-requests", true, "/{owner}/{repo}/api/pulls/{num}/review-requests"},
		{"review-suggest", "GET", "/o/r/api/pulls/7/review-suggest", true, "/{owner}/{repo}/api/pulls/{num}/review-suggest"},
		{"review-suggest wrong method recognized", "POST", "/o/r/api/pulls/7/review-suggest", true, "/{owner}/{repo}/api/pulls/{num}/review-suggest"},
		// Anything else must NOT claim the review surface (falls through
		// to pulls or the core mux): uncovered paths need no template.
		{"pulls root is pulls surface", "GET", "/o/r/api/pulls", false, ""},
		{"unknown leaf", "GET", "/o/r/api/pulls/7/nope", false, ""},
		{"other family", "GET", "/o/r/api/issues", false, ""},
		{"top level", "GET", "/api/v1/repos", false, ""},
	}

	covered := make([]bool, len(ExposedTemplates))
	for _, tc := range served {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer admin:erin")
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
// spelling the templates use (both lanes serve the same shapes).
func exposedLane(path string) string {
	p := strings.Replace(path, "/api-browser/", "/api/", 1)
	if i := strings.Index(p, "?"); i >= 0 {
		p = p[:i]
	}
	return p
}
