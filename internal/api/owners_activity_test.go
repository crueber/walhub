// owners_activity_test.go — Forgejo #283 owners listing activity ordering.
//
// Rules pinned here:
//   - GET /api/v1/owners default (no sort) is byte-identical to the legacy
//     store order (no catalog read, no shape change).
//   - ?sort=activity&order=desc|asc ranks by the derived per-owner max
//     (known in direction, unknowns always last either way, ties
//     name-ascending); bogus sort/order degrade to the defaults, never 400.
//   - GET /api/v1/owners/detailed serves {owners:[{name,
//     last_commit_sha|null, last_commit_time|null}]} with the same sort;
//     absent catalog degrades to name order + null rows (never 404/500);
//     corrupt catalog is a 503.
//   - All three twins (/api/v1 + /api-browser/v1 + /services/api) serve both
//     the param and the field; discovery lists the new template.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"git.packden.us/crueber/walhub/internal/sizecatalog"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// errFakeOwners injects a registry failure (→ 503 via mapViewErr).
var errFakeOwners = errors.New("registry down")

func putOwnersCatalog(t *testing.T, f *fixture, entries []*proto.RepoCatalogEntry) {
	t.Helper()
	cat := &proto.RepoCatalog{UpdatedAt: &proto.Timestamp{Seconds: 1700000000}, Entries: entries}
	for _, e := range entries {
		cat.Repos = append(cat.Repos, e.Repo)
	}
	if _, err := f.env.Store.Put(context.Background(), sizecatalog.CatalogKey,
		store.PutBody{Bytes: cat.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
}

func ownerEntry(repo, sha string, secs int64) *proto.RepoCatalogEntry {
	e := &proto.RepoCatalogEntry{Repo: repo, SizeBytes: 10, ObjectCount: 1, HeadSeq: 1}
	if sha != "" {
		e.LastCommitSHA = sha
	}
	if secs != 0 {
		e.LastCommitTime = &proto.Timestamp{Seconds: secs}
	}
	return e
}

// multiOwnerFixture seeds three owners: alice (newest), bob (older), zed
// (unknown — catalog row without activity), plus ghost (no catalog row at
// all). Registry order is store-sorted: alice, bob, ghost, zed.
func multiOwnerFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.reg.repos = map[string][]string{
		"alice": {"a"},
		"bob":   {"b"},
		"ghost": {"g"},
		"zed":   {"z"},
	}
	putOwnersCatalog(t, f, []*proto.RepoCatalogEntry{
		ownerEntry("alice/a", "aaa", 1700000300),
		ownerEntry("bob/b", "bbb", 1700000100),
		ownerEntry("zed/z", "", 0),
	})
	return f
}

func decodeOwners(t *testing.T, body []byte) []string {
	t.Helper()
	var names []string
	if err := json.Unmarshal(body, &names); err != nil {
		t.Fatal(err)
	}
	if names == nil {
		t.Fatal("owners must be [], never null")
	}
	return names
}

func decodeOwnersDetailed(t *testing.T, body []byte) []OwnerActivityRow {
	t.Helper()
	var doc struct {
		Owners []OwnerActivityRow `json:"owners"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Owners == nil {
		t.Fatal("owners must be [], never null")
	}
	return doc.Owners
}

func TestOwnersSortActivity(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"default is store order", "", []string{"alice", "bob", "ghost", "zed"}},
		{"explicit name sort", "?sort=name", []string{"alice", "bob", "ghost", "zed"}},
		{"bogus sort degrades", "?sort=size", []string{"alice", "bob", "ghost", "zed"}},
		{"activity desc", "?sort=activity&order=desc", []string{"alice", "bob", "ghost", "zed"}},
		{"activity asc", "?sort=activity&order=asc", []string{"bob", "alice", "ghost", "zed"}},
		{"activity default order is asc", "?sort=activity", []string{"bob", "alice", "ghost", "zed"}},
		{"bogus order degrades to asc", "?sort=activity&order=sideways", []string{"bob", "alice", "ghost", "zed"}},
		{"unknowns last in both directions", "?sort=activity&order=desc", []string{"alice", "bob", "ghost", "zed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := multiOwnerFixture(t)
			// All three twins serve the param identically.
			for _, lane := range []string{"/api/v1/owners", "/api-browser/v1/owners", "/services/api/owners"} {
				w := f.req("GET", lane+tc.query)
				if w.Code != 200 {
					t.Fatalf("%s%s → %d", lane, tc.query, w.Code)
				}
				if cc := w.Header().Get("Cache-Control"); cc != ccSWR {
					t.Fatalf("owners cache class = %q", cc)
				}
				got := decodeOwners(t, w.Body.Bytes())
				if len(got) != len(tc.want) {
					t.Fatalf("%s: owners = %v, want %v", lane, got, tc.want)
				}
				for i := range tc.want {
					if got[i] != tc.want[i] {
						t.Fatalf("%s: owners = %v, want %v", lane, got, tc.want)
					}
				}
			}
		})
	}
}

func TestOwnersSortActivityTies(t *testing.T) {
	f := newFixture(t)
	f.reg.repos = map[string][]string{"zed": {"z"}, "amy": {"a"}, "bob": {"b"}}
	putOwnersCatalog(t, f, []*proto.RepoCatalogEntry{
		ownerEntry("zed/z", "zzz", 1700000300),
		ownerEntry("amy/a", "aaa", 1700000300), // tie with zed → name wins
		ownerEntry("bob/b", "bbb", 1700000100),
	})
	w := f.req("GET", "/api/v1/owners?sort=activity&order=desc")
	got := decodeOwners(t, w.Body.Bytes())
	want := []string{"amy", "zed", "bob"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("owners = %v, want %v", got, want)
		}
	}
}

func TestOwnersSortDegradesWithoutCatalog(t *testing.T) {
	f := newFixture(t)
	f.reg.repos = map[string][]string{"zed": {"z"}, "amy": {"a"}}
	// No catalog object at all: activity sort degrades to name order, 200.
	w := f.req("GET", "/api/v1/owners?sort=activity&order=desc")
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	got := decodeOwners(t, w.Body.Bytes())
	if len(got) != 2 || got[0] != "amy" || got[1] != "zed" {
		t.Fatalf("owners = %v", got)
	}
}

func TestOwnersSortCorruptCatalogIs503(t *testing.T) {
	f := newFixture(t)
	if _, err := f.env.Store.Put(context.Background(), sizecatalog.CatalogKey,
		store.PutBody{Bytes: []byte("}{ not protobuf")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	w := f.req("GET", "/api/v1/owners?sort=activity&order=desc")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d, want 503", w.Code)
	}
	// ...while the default listing (no catalog read) still answers 200.
	w = f.req("GET", "/api/v1/owners")
	if w.Code != 200 {
		t.Fatalf("default code=%d, want 200", w.Code)
	}
}

func TestOwnersDetailedShapeAndSort(t *testing.T) {
	f := multiOwnerFixture(t)
	w := f.req("GET", "/api/v1/owners/detailed?sort=activity&order=desc")
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != ccSWR {
		t.Fatalf("cache class = %q", cc)
	}
	rows := decodeOwnersDetailed(t, w.Body.Bytes())
	if len(rows) != 4 {
		t.Fatalf("rows = %+v", rows)
	}
	// Order: alice, bob, then unknowns (ghost, zed) name-ascending.
	for i, want := range []string{"alice", "bob", "ghost", "zed"} {
		if rows[i].Name != want {
			t.Fatalf("rows[%d].Name = %q, want %q (%+v)", i, rows[i].Name, want, rows)
		}
	}
	// alice carries the per-owner max field (max over her repos).
	if rows[0].LastCommitTime == nil || *rows[0].LastCommitTime == "" {
		t.Fatalf("alice time: %+v", rows[0])
	}
	if rows[0].LastCommitSHA == nil || *rows[0].LastCommitSHA != "aaa" {
		t.Fatalf("alice sha: %+v", rows[0])
	}
	// Unknown owners carry explicit nulls (never "" or a fake epoch).
	for _, r := range rows[2:] {
		if r.LastCommitTime != nil || r.LastCommitSHA != nil {
			t.Fatalf("%s must be null: %+v", r.Name, r)
		}
	}
	// repo_count rides every row (Forgejo #307 — one live repo each here);
	// null-activity rows still carry their count (unknown activity ≠ no repos).
	for _, r := range rows {
		if r.RepoCount != 1 {
			t.Fatalf("%s repo_count = %d, want 1 (%+v)", r.Name, r.RepoCount, r)
		}
	}
	// Asc: bob before alice, unknowns still last.
	w = f.req("GET", "/api/v1/owners/detailed?sort=activity&order=asc")
	rows = decodeOwnersDetailed(t, w.Body.Bytes())
	for i, want := range []string{"bob", "alice", "ghost", "zed"} {
		if rows[i].Name != want {
			t.Fatalf("asc rows[%d].Name = %q, want %q", i, rows[i].Name, want)
		}
	}
	// Default (no sort): store name order with the same field shape.
	w = f.req("GET", "/api/v1/owners/detailed")
	rows = decodeOwnersDetailed(t, w.Body.Bytes())
	for i, want := range []string{"alice", "bob", "ghost", "zed"} {
		if rows[i].Name != want {
			t.Fatalf("default rows[%d].Name = %q, want %q", i, rows[i].Name, want)
		}
	}
}

func TestOwnersDetailedTwinsAndDiscovery(t *testing.T) {
	f := multiOwnerFixture(t)
	for _, p := range []string{
		"/api/v1/owners/detailed",
		"/api-browser/v1/owners/detailed",
		"/services/api/owners/detailed",
	} {
		w := f.req("GET", p)
		if w.Code != 200 {
			t.Fatalf("%s → %d", p, w.Code)
		}
		rows := decodeOwnersDetailed(t, w.Body.Bytes())
		if len(rows) != 4 || rows[0].Name != "alice" {
			t.Fatalf("%s rows: %+v", p, rows)
		}
	}
	// With sort on the twins too.
	for _, p := range []string{
		"/api/v1/owners/detailed?sort=activity&order=desc",
		"/api-browser/v1/owners/detailed?sort=activity&order=desc",
		"/services/api/owners/detailed?sort=activity&order=desc",
	} {
		w := f.req("GET", p)
		if w.Code != 200 {
			t.Fatalf("%s → %d", p, w.Code)
		}
	}
	// Discovery lists the new template (derived from the route table).
	w := f.req("GET", "/api/v1")
	var doc struct {
		Endpoints []string `json:"endpoints"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range doc.Endpoints {
		if e == "/api/v1/owners/detailed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("discovery missing owners/detailed template: %v", doc.Endpoints)
	}
}

func TestOwnersDetailedDegradesWithoutCatalog(t *testing.T) {
	f := newFixture(t)
	f.reg.repos = map[string][]string{"zed": {"z"}, "amy": {"a"}}
	w := f.req("GET", "/api/v1/owners/detailed?sort=activity&order=desc")
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	rows := decodeOwnersDetailed(t, w.Body.Bytes())
	if len(rows) != 2 || rows[0].Name != "amy" || rows[1].Name != "zed" {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		if r.LastCommitSHA != nil || r.LastCommitTime != nil {
			t.Fatalf("absent catalog must be null: %+v", r)
		}
	}
	// Empty registry → 200 {owners:[]} equivalent ([] never null).
	f2 := newFixture(t)
	f2.reg.repos = map[string][]string{}
	w = f2.req("GET", "/api/v1/owners/detailed")
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	if rows := decodeOwnersDetailed(t, w.Body.Bytes()); len(rows) != 0 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestOwnersActivityErrorBranches(t *testing.T) {
	// Nil registry → 503 on both surfaces (never a nil-pointer 500).
	f := newFixture(t)
	f.env.Repos = nil
	for _, p := range []string{"/api/v1/owners?sort=activity&order=desc", "/api/v1/owners/detailed"} {
		if w := f.req("GET", p); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s → %d, want 503", p, w.Code)
		}
	}
	// Registry failure → 503 on both surfaces.
	f2 := newFixture(t)
	f2.reg.fail = errFakeOwners
	for _, p := range []string{"/api/v1/owners?sort=activity&order=desc", "/api/v1/owners/detailed"} {
		if w := f2.req("GET", p); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s → %d, want 503", p, w.Code)
		}
	}
	// Nil store → no catalog read: activity sort degrades to name order,
	// detailed degrades to null rows (never 500).
	f3 := newFixture(t)
	f3.reg.repos = map[string][]string{"zed": {"z"}, "amy": {"a"}}
	f3.env.Store = nil
	w := f3.req("GET", "/api/v1/owners?sort=activity&order=desc")
	if w.Code != 200 {
		t.Fatalf("nil-store owners code=%d", w.Code)
	}
	if got := decodeOwners(t, w.Body.Bytes()); len(got) != 2 || got[0] != "amy" {
		t.Fatalf("nil-store owners = %v", got)
	}
	w = f3.req("GET", "/api/v1/owners/detailed")
	if w.Code != 200 {
		t.Fatalf("nil-store detailed code=%d", w.Code)
	}
	for _, r := range decodeOwnersDetailed(t, w.Body.Bytes()) {
		if r.LastCommitSHA != nil || r.LastCommitTime != nil {
			t.Fatalf("nil-store detailed must be null: %+v", r)
		}
	}
	// Corrupt catalog → 503 on the detailed surface too.
	f4 := newFixture(t)
	if _, err := f4.env.Store.Put(context.Background(), sizecatalog.CatalogKey,
		store.PutBody{Bytes: []byte("}{ not protobuf")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if w := f4.req("GET", "/api/v1/owners/detailed"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("corrupt detailed → %d, want 503", w.Code)
	}
}

// TestOwnersDetailedRepoCounts pins the Forgejo #307 rail: every row carries
// its manifest-gated live-repo count, and the instance repo total is the sum
// over the uncapped payload (never a capped slice, never a per-owner walk).
func TestOwnersDetailedRepoCounts(t *testing.T) {
	cases := []struct {
		name   string
		repos  map[string][]string
		query  string
		counts map[string]int
		total  int
	}{
		{
			name:   "varied counts sum to the instance total",
			repos:  map[string][]string{"alice": {"a1", "a2", "a3"}, "bob": {"b"}, "zed": {"z1", "z2"}},
			query:  "",
			counts: map[string]int{"alice": 3, "bob": 1, "zed": 2},
			total:  6,
		},
		{
			name:   "counts ride the activity sort too",
			repos:  map[string][]string{"alice": {"a1", "a2", "a3"}, "bob": {"b"}},
			query:  "?sort=activity&order=desc",
			counts: map[string]int{"alice": 3, "bob": 1},
			total:  4,
		},
		{
			name:   "zero-live owners are absent, never zero-valued",
			repos:  map[string][]string{"alice": {"a"}, "empty": {}},
			query:  "",
			counts: map[string]int{"alice": 1},
			total:  1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.reg.repos = tc.repos
			// All three twins serve the field identically.
			for _, lane := range []string{"/api/v1/owners/detailed", "/api-browser/v1/owners/detailed", "/services/api/owners/detailed"} {
				w := f.req("GET", lane+tc.query)
				if w.Code != 200 {
					t.Fatalf("%s%s → %d", lane, tc.query, w.Code)
				}
				rows := decodeOwnersDetailed(t, w.Body.Bytes())
				if len(rows) != len(tc.counts) {
					t.Fatalf("%s: %d rows, want %d (%+v)", lane, len(rows), len(tc.counts), rows)
				}
				sum := 0
				for _, r := range rows {
					want, ok := tc.counts[r.Name]
					if !ok {
						t.Fatalf("%s: unexpected owner %q (%+v)", lane, r.Name, rows)
					}
					if r.RepoCount != want {
						t.Fatalf("%s: %q repo_count = %d, want %d", lane, r.Name, r.RepoCount, want)
					}
					sum += r.RepoCount
				}
				if sum != tc.total {
					t.Fatalf("%s: instance total = %d, want %d", lane, sum, tc.total)
				}
			}
		})
	}
}

// TestOwnersDetailedRepoCountPresent decodes one payload raw: repo_count is
// always present (never null, never omitted — membership implies ≥1 live
// repo), so a zero count on the wire is a bug, not "unknown".
func TestOwnersDetailedRepoCountPresent(t *testing.T) {
	f := multiOwnerFixture(t)
	w := f.req("GET", "/api/v1/owners/detailed")
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	var doc struct {
		Owners []map[string]any `json:"owners"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Owners) != 4 {
		t.Fatalf("rows = %v", doc.Owners)
	}
	for _, r := range doc.Owners {
		v, ok := r["repo_count"]
		if !ok {
			t.Fatalf("repo_count missing: %v", r)
		}
		if n, isNum := v.(float64); !isNum || n < 1 {
			t.Fatalf("repo_count = %v, want a positive number (%v)", v, r)
		}
	}
}

// TestOwnersDetailedCountsError pins the failure mapping: a registry failure
// behind OwnerRepoCounts is a 503 (never a nil-pointer 500, never a 200 with
// zeroed rows).
func TestOwnersDetailedCountsError(t *testing.T) {
	f := newFixture(t)
	f.reg.fail = errFakeOwners
	if w := f.req("GET", "/api/v1/owners/detailed"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("counts failure → %d, want 503", w.Code)
	}
}
