package pulls

import (
	"net/http"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/cachepolicy"
)

// TestCacheClassContract is the Forgejo #382 systemic guard for the pulls
// surface: every served cacheable GET is pinned to its §4 cache class, and
// every pinned pair passes the shared mutability rule (a version-keyed
// ETag must never ride a stale-serve window). The pull view is mutable
// (comments/patches move the folded ETag); the diff is the one SWR route
// left here — a ref-derived patch body whose staleness is bounded by ref
// movement. Coverage: every ExposedTemplates entry appears either as an
// exact row (cacheable GET) or in noGET (mutation-only). A new template
// without a row fails here; a new GET serving SWR with a version ETag
// fails cachepolicy.Check.
func TestCacheClassContract(t *testing.T) {
	e := newTestEnv()
	seedOpened(t, e)

	rows := []struct {
		name     string
		template string
		path     string
		wantCC   string
		wantETag bool
	}{
		{"view", "/{owner}/{repo}/api/pulls/{num}", "/o/r/api/pulls/1", ccMutable, true},
		{"list", "/{owner}/{repo}/api/pulls", "/o/r/api/pulls", ccNoStore, false},
		{"diff", "/{owner}/{repo}/api/pulls/{num}/diff", "/o/r/api/pulls/1/diff", ccSWR, false},
		{"commits", "/{owner}/{repo}/api/pulls/{num}/commits", "/o/r/api/pulls/1/commits", ccNoStore, false},
	}
	covered := map[string]bool{}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			covered[row.template] = true
			w := doReq(t, e.h, "GET", "api", row.path, "", writer())
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

	// Merge-task poll: no-store task stream (200 once a merge runs; the
	// TestHTTPMergeFlow poll pattern — deadline-bounded, no silent wait).
	t.Run("merge task", func(t *testing.T) {
		covered["/{owner}/{repo}/api/pulls/{num}/merge/task"] = true
		w := doReq(t, e.h, "POST", "api", "/o/r/api/pulls/1/merge", `{"strategy":"squash"}`, maintainer())
		if w.Code != http.StatusAccepted {
			t.Fatalf("seed merge = %d: %s", w.Code, w.Body.String())
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			w2 := doReq(t, e.h, "GET", "api", "/o/r/api/pulls/1/merge/task", "", maintainer())
			if w2.Code == http.StatusOK {
				cc := w2.Header().Get("Cache-Control")
				if cc != ccNoStore {
					t.Fatalf("task class = %q, want %q", cc, ccNoStore)
				}
				if err := cachepolicy.Check(cc, w2.Header().Get("ETag")); err != nil {
					t.Fatalf("mutability rule: %v", err)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("merge task never 200 (last = %d: %s)", w2.Code, w2.Body.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	})

	// Mutation-only templates serve no 200-GET (routePulls/handleTop
	// method gates): no class applies.
	noGET := map[string]bool{
		"/api/v1/repos/{owner}/{repo}/forks":            true, // POST only
		"/{owner}/{repo}/api/pulls/{num}/comments":      true, // POST only
		"/{owner}/{repo}/api/pulls/{num}/merge":         true, // POST only
		"/{owner}/{repo}/api/pulls/{num}/update-branch": true, // POST only
		"/{owner}/{repo}/api/pulls/{num}/head":          true, // DELETE only
	}
	for _, e := range ExposedTemplates {
		if !covered[e] && !noGET[e] {
			t.Errorf("template %q serves a cacheable GET with no pinned class row", e)
		}
	}
}
