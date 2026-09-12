package issues

import (
	"encoding/json"
	"net/http"
	"testing"

	"git.packden.us/crueber/walhub/internal/cachepolicy"
)

// TestCacheClassContract is the Forgejo #382 systemic guard for the issues
// surface: every served cacheable GET is pinned to its §4 cache class, and
// every pinned pair passes the shared mutability rule (a version-keyed
// ETag must never ride a stale-serve window). Coverage: every
// ExposedTemplates entry appears either as an exact row (cacheable GET)
// or in noGET (mutation-only: no 200-GET exists on that template, so no
// class applies). A new template without a row fails here; a new GET
// serving SWR with a version ETag fails cachepolicy.Check.
func TestCacheClassContract(t *testing.T) {
	roles := newFakeRoles()
	grantTriage(roles, "acme", "repo")
	s := testService(roles)
	h := testHandler(s, janeP)
	mustCreate(t, s, "acme", "repo", janeP, "t", "b")
	if _, err := s.AddComment(reqCtx(), "acme", "repo", 1, janeP, "c1"); err != nil {
		t.Fatal(err)
	}
	// Milestone singleton row needs an id: create one first (milestones
	// need triage — aliceP has it via grantTriage above).
	ha := testHandler(s, aliceP)
	w := doReq(ha, "POST", "/acme/repo/api/milestones", `{"title":"v1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed milestone = %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		Milestone *Milestone `json:"milestone"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	rows := []struct {
		name     string
		template string
		path     string
		wantCC   string
		wantETag bool
	}{
		{"thread", "/{owner}/{repo}/api/issues/{num}", "/acme/repo/api/issues/1", ccThread, true},
		{"list", "/{owner}/{repo}/api/issues", "/acme/repo/api/issues", ccNoStore, false},
		{"events", "/{owner}/{repo}/api/issues/{num}/events", "/acme/repo/api/issues/1/events", ccNoStore, false},
		{"labels", "/{owner}/{repo}/api/labels", "/acme/repo/api/labels", ccNoStore, false},
		{"milestones", "/{owner}/{repo}/api/milestones", "/acme/repo/api/milestones", ccNoStore, false},
		{"milestone", "/{owner}/{repo}/api/milestones/{id}", "/acme/repo/api/milestones/" + created.Milestone.ID, ccNoStore, false},
	}
	covered := map[string]bool{}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			covered[row.template] = true
			w := doReq(h, "GET", row.path, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", row.path, w.Code, w.Body.String())
			}
			cc := w.Header().Get("Cache-Control")
			if cc != row.wantCC {
				t.Fatalf("GET %s class = %q, want %q", row.path, cc, row.wantCC)
			}
			etag := w.Header().Get("ETag")
			if row.wantETag && etag == "" {
				t.Fatalf("GET %s must carry an ETag", row.path)
			}
			if !row.wantETag && etag != "" {
				t.Fatalf("GET %s carries unexpected ETag %q", row.path, etag)
			}
			if err := cachepolicy.Check(cc, etag); err != nil {
				t.Fatalf("mutability rule: %v", err)
			}
		})
	}
	// Mutation-only templates serve no 200-GET (POST/DELETE/PATCH-only per
	// routeIssues/routeLabels/routeMilestones/handleRepo): no class applies.
	noGET := map[string]bool{
		"/{owner}/{repo}/api/issues/{num}/comments":                  true, // POST only
		"/{owner}/{repo}/api/issues/{num}/reactions":                 true, // POST only
		"/{owner}/{repo}/api/issues/{num}/reactions/{seq}/{content}": true, // DELETE only
		"/{owner}/{repo}/api/labels/{name}":                          true, // PATCH/DELETE only
		"/{owner}/{repo}/api/attachments":                            true, // POST upload only
	}
	for _, e := range ExposedTemplates {
		if !covered[e] && !noGET[e] {
			t.Errorf("template %q serves a cacheable GET with no pinned class row", e)
		}
	}
}
