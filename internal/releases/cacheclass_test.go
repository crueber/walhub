package releases

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/cachepolicy"
	"git.packden.us/crueber/walhub/internal/git"
)

// TestCacheClassContract is the Forgejo #382 systemic guard for the
// releases surface: every served cacheable GET is pinned to its §4 cache
// class, and every pinned pair passes the shared mutability rule (a
// version-keyed ETag must never ride a stale-serve window). Coverage:
// every ExposedTemplates entry appears as an exact row — all five serve
// GET (mutations share the template). The asset byte route rides
// HandleRepo outside the api lanes (no discovery entry, same rule as the
// issues attachment bytes) and is pinned as an extra immutable row. A new
// template without a row fails here; a new GET serving SWR with a version
// ETag fails cachepolicy.Check.
func TestCacheClassContract(t *testing.T) {
	x := newHarness(t)
	grantWrite(x)
	x.git.tags["v1"] = strings.Repeat("a", 40)
	x.git.tags["v2"] = strings.Repeat("b", 40)
	putJSON(t, x, "v1", map[string]any{"name": "R1"}, asWriter())

	rows := []struct {
		name     string
		template string
		path     string
		wantCC   string
		wantETag bool
	}{
		{"single", "/{owner}/{repo}/api/releases/{tag}", "/o/r/api/releases/v1", ccMutable, true},
		{"latest", "/{owner}/{repo}/api/releases/latest", "/o/r/api/releases/latest", ccMutable, true},
		{"list", "/{owner}/{repo}/api/releases", "/o/r/api/releases", ccMutable, true},
		{"autodraft", "/{owner}/{repo}/api/releases/autodraft", "/o/r/api/releases/autodraft?tag=v2", ccMutable, false},
	}
	covered := map[string]bool{}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			covered[row.template] = true
			rec := do(t, x, "GET", row.path, nil, asReader("bob"))
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
	for _, e := range ExposedTemplates {
		// The asset template serves POST/DELETE on the api lane (405
		// otherwise); its GET rides HandleRepo outside the lanes and is
		// pinned by the "asset bytes" row below.
		if e == "/{owner}/{repo}/api/releases/{tag}/assets/{name}" {
			continue
		}
		if !covered[e] {
			t.Errorf("template %q serves a cacheable GET with no pinned class row", e)
		}
	}

	// Asset bytes: content-addressed immutable (the static contract —
	// immutable must never carry a stale window regardless of ETag).
	t.Run("asset bytes", func(t *testing.T) {
		body := []byte("0123456789abcdef")
		up := do(t, x, "POST", "/o/r/api/releases/v1/assets/tool",
			body, mergeHeaders(asWriter(), map[string]string{"X-Walgit-Asset-Sha256": shaOf(body)}))
		if up.Code != http.StatusCreated {
			t.Fatalf("upload: %d %q", up.Code, up.Body.String())
		}
		id := git.RepoId{Owner: "o", Name: "r"}
		get := httptest.NewRequest("GET", "/o/r/releases/v1/assets/tool", nil)
		get.Header.Set("X-Test-Principal", "bob")
		grec := httptest.NewRecorder()
		if !x.handler.HandleRepo(grec, get, id, []string{"releases", "v1", "assets", "tool"}) {
			t.Fatal("byte route not claimed")
		}
		cc := grec.Header().Get("Cache-Control")
		if !strings.Contains(cc, "immutable") || strings.Contains(cc, "stale-while-revalidate") {
			t.Fatalf("asset class = %q, want immutable without a stale window", cc)
		}
		if err := cachepolicy.Check(cc, grec.Header().Get("ETag")); err != nil {
			t.Fatalf("mutability rule: %v", err)
		}
	})
}
