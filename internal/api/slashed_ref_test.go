package api

// slashed_ref_test.go pins issue #251: blob/tree (and ?raw) URLs whose rev
// contains a slash must resolve via Resolve's longest-prefix match, exactly
// like the resolve endpoint already does. The engine fixture runs the full
// HTTP stack over a real bare git serving copy, so the ref/path split under
// test is the production one in bind_wal.go — not a fake.

import (
	"context"
	"net/http"
	"testing"

	"git.packden.us/crueber/walhub/internal/wal"
)

func TestRefPartOf(t *testing.T) {
	for _, tc := range []struct{ rest, path, want string }{
		{"main", "", "main"},
		{"main/src/a.txt", "src/a.txt", "main"},
		{"feat/identity", "", "feat/identity"},
		{"feat/identity/README.md", "README.md", "feat/identity"},
		{fakeSHA, "", fakeSHA},
		{fakeSHA + "/hi.txt", "hi.txt", fakeSHA},
	} {
		if got := refPartOf(tc.rest, tc.path); got != tc.want {
			t.Fatalf("refPartOf(%q, %q) = %q, want %q", tc.rest, tc.path, got, tc.want)
		}
		if tc.want == fakeSHA && !revIsFullSHA(refPartOf(tc.rest, tc.path)) {
			t.Fatalf("refPartOf(%q, %q) must test as a full sha", tc.rest, tc.path)
		}
	}
}

// TestSlashedBranchBlobTreeRaw replays the issue's live repro table: every
// URL that 404d with "not found: feat" must now answer like its resolve
// twin does.
func TestSlashedBranchBlobTreeRaw(t *testing.T) {
	fix := newGitFix(t)
	runGit(t, fix.bare.Path, "branch", "feat/identity", fix.main)
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 7}
	f := newEngineFixture(t, eng)
	ctx := context.Background()
	id := repoID()

	// Sanity: the resolve twin already worked before the fix.
	w := f.do("GET", "/demo/walgit/api/resolve/feat/identity")
	if w.Code != http.StatusOK {
		t.Fatalf("resolve = %d (%s), want 200", w.Code, w.Body.String())
	}
	var resolved struct {
		Ref, SHA, Path, Kind string
	}
	decodeJSON(t, w, &resolved)
	if resolved.Ref != "refs/heads/feat/identity" || resolved.SHA != fix.main ||
		resolved.Path != "" || resolved.Kind != "branch" {
		t.Fatalf("resolve = %+v", resolved)
	}

	// The Resolve-level split the handlers now share.
	res, err := f.view().Resolve(ctx, id, "feat/identity/hello.txt")
	if err != nil || res.Ref != "refs/heads/feat/identity" || res.SHA != fix.main ||
		res.Path != "hello.txt" || res.Kind != "branch" {
		t.Fatalf("Resolve split = %+v, %v", res, err)
	}

	// ?raw on the slashed branch: the exact URL from the issue.
	w = f.do("GET", "/demo/walgit/api/blob/feat/identity/hello.txt?raw")
	if w.Code != http.StatusOK {
		t.Fatalf("raw = %d (%s), want 200", w.Code, w.Body.String())
	}
	if w.Body.String() != "hello again\n" {
		t.Fatalf("raw body = %q", w.Body.String())
	}
	if etag := w.Header().Get("ETag"); etag != `"`+fix.main+`"` {
		t.Fatalf("raw etag = %q (must key on the resolved sha)", etag)
	}
	if cc := w.Header().Get("Cache-Control"); cc != ccSWR {
		t.Fatalf("named raw cache = %q, want SWR", cc)
	}

	// JSON blob on the slashed branch.
	w = f.do("GET", "/demo/walgit/api/blob/feat/identity/hello.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("blob = %d (%s), want 200", w.Code, w.Body.String())
	}
	var blob blobBody
	decodeJSON(t, w, &blob)
	if blob.Ref != "refs/heads/feat/identity" || blob.SHA != fix.main ||
		blob.Path != "hello.txt" || blob.Name != "hello.txt" || blob.Contents != "hello again\n" {
		t.Fatalf("blob = %+v", blob)
	}

	// Tree root on the slashed branch.
	w = f.do("GET", "/demo/walgit/api/tree/feat/identity")
	if w.Code != http.StatusOK {
		t.Fatalf("tree = %d (%s), want 200", w.Code, w.Body.String())
	}
	var tree struct {
		Ref, SHA, Path string
		Entries        []TreeEntry `json:"entries"`
	}
	decodeJSON(t, w, &tree)
	if tree.Ref != "refs/heads/feat/identity" || tree.SHA != fix.main || tree.Path != "" {
		t.Fatalf("tree = %+v", tree)
	}
	if len(tree.Entries) == 0 {
		t.Fatal("tree root must list entries")
	}

	// Tree subdir on the slashed branch.
	w = f.do("GET", "/demo/walgit/api/tree/feat/identity/docs")
	if w.Code != http.StatusOK {
		t.Fatalf("tree subdir = %d (%s), want 200", w.Code, w.Body.String())
	}
	decodeJSON(t, w, &tree)
	if tree.Path != "docs" || tree.Ref != "refs/heads/feat/identity" {
		t.Fatalf("tree subdir = %+v", tree)
	}

	// Sha-addressed raw through a path stays immutable (the ?raw cache
	// path keys on the resolved sha, not the raw rev).
	w = f.do("GET", "/demo/walgit/api/blob/"+fix.main+"/hello.txt?raw")
	if w.Code != http.StatusOK {
		t.Fatalf("sha raw = %d (%s), want 200", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != ccImmutable {
		t.Fatalf("sha raw cache = %q, want immutable", cc)
	}

	// Blob with no path still 404s with the guard shape.
	w = f.do("GET", "/demo/walgit/api/blob/feat/identity")
	if w.Code != http.StatusNotFound || w.Body.String() != "blob requires a path" {
		t.Fatalf("pathless blob = %d %q", w.Code, w.Body.String())
	}

	// The api-browser lane twin shares the greedy route (lane-agnostic).
	w = f.do("GET", "/demo/walgit/api-browser/blob/feat/identity/hello.txt?raw")
	if w.Code != http.StatusOK || w.Body.String() != "hello again\n" {
		t.Fatalf("browser-lane raw = %d %q", w.Code, w.Body.String())
	}

	// Bare blob (no tail at all) still 404s at the route table.
	w = f.do("GET", "/demo/walgit/api/blob")
	if w.Code != http.StatusNotFound {
		t.Fatalf("bare blob = %d, want 404", w.Code)
	}

	// No match anywhere: the same 404 shape as before, naming the first
	// non-matching segment.
	w = f.do("GET", "/demo/walgit/api/blob/feat/nonexistent/x.txt")
	if w.Code != http.StatusNotFound || w.Body.String() != "not found: feat" {
		t.Fatalf("unresolvable = %d %q, want 404 not found: feat", w.Code, w.Body.String())
	}
}

// TestSlashedRefFirstDisambiguation pins the ref-first rule: branch "docs"
// exists, so …/blob/docs/hello.txt resolves the BRANCH (base commit), not
// the docs/ path on main — even though main has a docs/ directory too. The
// ETag AND the bytes discriminate: base sha + "hello world\n" proves
// ref-first; path-first would 404 (main has no docs/hello.txt).
func TestSlashedRefFirstDisambiguation(t *testing.T) {
	fix := newGitFix(t)
	runGit(t, fix.bare.Path, "branch", "docs", fix.base)
	eng := &fakeEngine{obj: wal.ObjectAccess{Local: fix.bare}, rev: 7}
	f := newEngineFixture(t, eng)

	w := f.do("GET", "/demo/walgit/api/resolve/docs/hello.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("resolve = %d (%s), want 200", w.Code, w.Body.String())
	}
	var resolved struct {
		Ref, SHA, Path, Kind string
	}
	decodeJSON(t, w, &resolved)
	if resolved.Ref != "refs/heads/docs" || resolved.SHA != fix.base || resolved.Path != "hello.txt" {
		t.Fatalf("resolve must split ref-first: %+v", resolved)
	}

	w = f.do("GET", "/demo/walgit/api/blob/docs/hello.txt?raw")
	if w.Code != http.StatusOK {
		t.Fatalf("raw = %d (%s), want 200", w.Code, w.Body.String())
	}
	if etag := w.Header().Get("ETag"); etag != `"`+fix.base+`"` {
		t.Fatalf("raw etag = %q, want the docs-branch tip %q (ref-first)", etag, fix.base)
	}
	if w.Body.String() != "hello world\n" {
		t.Fatalf("raw body = %q, want the base bytes (ref-first)", w.Body.String())
	}
}
