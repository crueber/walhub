package api

import (
	"net/http"
	"testing"

	"git.packden.us/crueber/walhub/internal/cachepolicy"
)

// TestCacheClassContract is the Forgejo #382 systemic guard for the core
// api surface: every cacheable GET is pinned to its §4 cache class, and
// every pinned pair passes the shared mutability rule (a version-keyed
// ETag must never ride a stale-serve window).
//
// The table encodes the whole §4 amendment in one place:
//   - sha-addressed git content → immutable;
//   - name-addressed git views (resolve, refs, tree, commits, commit) →
//     SWR with the resolved-sha ETag (staleness bounded by ref movement);
//   - user-mutable projections (summary, repos/detailed, owner profile)
//     → no-cache with version/content ETags;
//   - unversioned membership/activity listings (owners, ownerRepos,
//     owners/detailed) → SWR with NO ETag, pinned deliberately: they carry
//     no user-mutable projections and no sibling endpoint serves the same
//     data under a different class (the §4 listings boundary — gaining a
//     version ETag moves them to Mutable, and cachepolicy.Check fails the
//     SWR+version combination the moment it appears).
func TestCacheClassContract(t *testing.T) {
	f := newFixture(t)
	seedTree(f)
	putTestCatalog(t, f)
	c1 := Commit{SHA: fakeTag, Parents: []string{}, Subject: "s1", Trailers: []Trailer{}}
	f.view.commitPg["demo/walgit|"+fakeSHA+"||0|35"] = CommitPage{Commits: []Commit{c1}}
	f.view.commits["demo/walgit|"+fakeSHA] = CommitDetail{Commit: Commit{SHA: fakeSHA}, Patch: "p"}
	f.view.resolves["demo/walgit/"+fakeSHA+"/src"] = Resolution{Ref: "", SHA: fakeSHA, Path: "src", Kind: "commit", Revision: 7}

	rows := []struct {
		name     string
		path     string
		wantCC   string
		wantETag bool
	}{
		{"summary", "/demo/walgit/api", ccMutable, true},
		{"repos detailed", "/api/v1/owners/demo/repos/detailed", ccMutable, true},
		{"owner profile", "/api/v1/owners/ghost/profile", ccMutable, true},
		{"refs head", "/demo/walgit/api/refs", ccSWR, true},
		{"refs list", "/demo/walgit/api/refs/branches", ccSWR, false},
		{"resolve", "/demo/walgit/api/resolve/main/src", ccSWR, true},
		{"tree named", "/demo/walgit/api/tree/main/src", ccSWR, true},
		{"tree sha", "/demo/walgit/api/tree/" + fakeSHA + "/src", ccImmutable, true},
		{"commits named", "/demo/walgit/api/commits?ref=main", ccSWR, true},
		{"commits sha", "/demo/walgit/api/commits?ref=" + fakeSHA, ccImmutable, true},
		{"commit sha", "/demo/walgit/api/commit/" + fakeSHA, ccImmutable, true},
		{"owners", "/api/v1/owners", ccSWR, false},
		{"owner repos", "/api/v1/owners/demo/repos", ccSWR, false},
		{"owners detailed", "/api/v1/owners/detailed", ccSWR, false},
		{"discovery", "/api/v1", ccNoCache, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			w := f.req("GET", row.path)
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
}
