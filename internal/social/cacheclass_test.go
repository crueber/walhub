package social

import (
	"net/http"
	"testing"

	"git.packden.us/crueber/walhub/internal/cachepolicy"
)

// TestCacheClassContract is the Forgejo #382 systemic guard for the social
// surface: every served cacheable GET is pinned to its §4 cache class, and
// every pinned pair passes the shared mutability rule (a version-keyed
// ETag must never ride a stale-serve window). Coverage: every
// ExposedTemplates entry appears either as an exact row (cacheable GET)
// or in noGET (mutation-only). A new template without a row fails here; a
// new GET serving SWR with a version ETag fails cachepolicy.Check.
func TestCacheClassContract(t *testing.T) {
	x := newHarness(t)
	seedRepo(t, x, "o", "r")
	if rec := do(t, x, "PUT", "/o/r/api/star", nil, asUser("jane")); rec.Code != http.StatusOK {
		t.Fatalf("seed star = %d: %s", rec.Code, rec.Body.String())
	}
	seedWatchRecord(t, x, "jane", "o", "r")

	rows := []struct {
		name     string
		template string
		path     string
		wantCC   string
		wantETag bool
	}{
		{"social", "/{owner}/{repo}/api/social", "/o/r/api/social", ccMutable, true},
		{"me starred", "/api/v1/me/starred", "/api/v1/me/starred", ccNoStore, false},
		{"user starred", "/api/v1/users/{principal}/starred", "/api/v1/users/jane/starred", ccNoStore, false},
	}
	covered := map[string]bool{}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			covered[row.template] = true
			rec := do(t, x, "GET", row.path, nil, asUser("jane"))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", row.path, rec.Code, rec.Body.String())
			}
			cc := rec.Header().Get("Cache-Control")
			if cc != row.wantCC {
				t.Fatalf("GET %s class = %q, want %q", row.path, cc, row.wantCC)
			}
			etag := rec.Header().Get("ETag")
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
	// Mutation-only: PUT/DELETE star serves no 200-GET.
	noGET := map[string]bool{
		"/{owner}/{repo}/api/star": true,
	}
	for _, e := range ExposedTemplates {
		if !covered[e] && !noGET[e] {
			t.Errorf("template %q serves a cacheable GET with no pinned class row", e)
		}
	}
}
