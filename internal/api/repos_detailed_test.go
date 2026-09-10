package api

import (
	"context"
	"encoding/json"
	"testing"

	"git.packden.us/crueber/walhub/internal/sizecatalog"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func putTestCatalog(t *testing.T, f *fixture) {
	t.Helper()
	ctx := context.Background()
	cat := &proto.RepoCatalog{
		Repos:     []string{"demo/hello", "demo/walgit"},
		UpdatedAt: &proto.Timestamp{Seconds: 1700000000},
		Entries: []*proto.RepoCatalogEntry{
			{Repo: "demo/hello", SizeBytes: 300, ObjectCount: 3, HeadSeq: 2,
				LastCommitSHA:  "aaa",
				LastCommitTime: &proto.Timestamp{Seconds: 1700000100},
				LastPushAt:     &proto.Timestamp{Seconds: 1700000200}},
			{Repo: "demo/walgit", SizeBytes: 100, ObjectCount: 1, HeadSeq: 1,
				LastCommitSHA:  "bbb",
				LastCommitTime: &proto.Timestamp{Seconds: 1700000300}},
		},
	}
	if _, err := f.env.Store.Put(ctx, sizecatalog.CatalogKey, store.PutBody{Bytes: cat.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
}

func decodeDetailed(t *testing.T, body []byte) []RepoSizeRow {
	t.Helper()
	var doc struct {
		Repos []RepoSizeRow `json:"repos"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Repos == nil {
		t.Fatal("repos must be [], never null")
	}
	return doc.Repos
}

func TestOwnerReposDetailedSortFilter(t *testing.T) {
	f := newFixture(t)
	putTestCatalog(t, f)

	// Default: name order.
	w := f.req("GET", "/api/v1/owners/demo/repos/detailed")
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	rows := decodeDetailed(t, w.Body.Bytes())
	if len(rows) != 2 || rows[0].Name != "hello" || rows[1].Name != "walgit" {
		t.Fatalf("name order: %+v", rows)
	}
	if rows[0].SizeBytes == nil || *rows[0].SizeBytes != 300 {
		t.Fatalf("hello size: %+v", rows[0])
	}

	// sort=size desc → hello (300) first.
	w = f.req("GET", "/api/v1/owners/demo/repos/detailed?sort=size&order=desc")
	rows = decodeDetailed(t, w.Body.Bytes())
	if rows[0].Name != "hello" || rows[1].Name != "walgit" {
		t.Fatalf("size desc: %+v", rows)
	}
	// sort=size asc → walgit first.
	w = f.req("GET", "/api/v1/owners/demo/repos/detailed?sort=size&order=asc")
	rows = decodeDetailed(t, w.Body.Bytes())
	if rows[0].Name != "walgit" || rows[1].Name != "hello" {
		t.Fatalf("size asc: %+v", rows)
	}
	// sort=activity desc → walgit (newer commit) first; activity fields ride.
	w = f.req("GET", "/api/v1/owners/demo/repos/detailed?sort=activity&order=desc")
	rows = decodeDetailed(t, w.Body.Bytes())
	if rows[0].Name != "walgit" || rows[1].Name != "hello" {
		t.Fatalf("activity desc: %+v", rows)
	}
	if rows[0].LastCommitSHA == nil || *rows[0].LastCommitSHA != "bbb" {
		t.Fatalf("walgit sha: %+v", rows[0])
	}
	if rows[0].LastCommitTime == nil || *rows[0].LastCommitTime == "" {
		t.Fatalf("walgit time: %+v", rows[0])
	}
	if rows[1].LastPushAt == nil || *rows[1].LastPushAt == "" {
		t.Fatalf("hello push clock: %+v", rows[1])
	}
	// sort=activity asc → hello first (unknowns would still sort last).
	w = f.req("GET", "/api/v1/owners/demo/repos/detailed?sort=activity&order=asc")
	rows = decodeDetailed(t, w.Body.Bytes())
	if rows[0].Name != "hello" || rows[1].Name != "walgit" {
		t.Fatalf("activity asc: %+v", rows)
	}
	// min_bytes filters small repos (large/small findability).
	w = f.req("GET", "/api/v1/owners/demo/repos/detailed?sort=size&min_bytes=200")
	rows = decodeDetailed(t, w.Body.Bytes())
	if len(rows) != 1 || rows[0].Name != "hello" {
		t.Fatalf("min_bytes: %+v", rows)
	}
	// max_bytes filters large repos.
	w = f.req("GET", "/api/v1/owners/demo/repos/detailed?max_bytes=150")
	rows = decodeDetailed(t, w.Body.Bytes())
	if len(rows) != 1 || rows[0].Name != "walgit" {
		t.Fatalf("max_bytes: %+v", rows)
	}
	// Bad params are 400, not 500.
	for _, p := range []string{"?min_bytes=abc", "?max_bytes=-1", "?min_bytes=5&max_bytes=2"} {
		w = f.req("GET", "/api/v1/owners/demo/repos/detailed"+p)
		if w.Code != 400 {
			t.Fatalf("%s → %d, want 400", p, w.Code)
		}
	}
	// Unknown owner → 200 [] (same as v1, never 404).
	w = f.req("GET", "/api/v1/owners/ghost/repos/detailed")
	if w.Code != 200 {
		t.Fatalf("ghost code=%d", w.Code)
	}
	if rows := decodeDetailed(t, w.Body.Bytes()); len(rows) != 0 {
		t.Fatalf("ghost rows: %+v", rows)
	}
}

func TestOwnerReposDetailedDegradesWithoutCatalog(t *testing.T) {
	f := newFixture(t)
	// No catalog object at all → null sizes, still 200.
	w := f.req("GET", "/api/v1/owners/demo/repos/detailed?sort=size")
	if w.Code != 200 {
		t.Fatalf("code=%d", w.Code)
	}
	rows := decodeDetailed(t, w.Body.Bytes())
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	for _, r := range rows {
		if r.SizeBytes != nil {
			t.Fatalf("absent catalog must be null, got %+v", r)
		}
		if r.LastCommitSHA != nil || r.LastCommitTime != nil || r.LastPushAt != nil {
			t.Fatalf("absent catalog must be activity-null, got %+v", r)
		}
	}
	// v1 string list is untouched by the new endpoint.
	w = f.req("GET", "/api/v1/owners/demo/repos")
	if w.Code != 200 {
		t.Fatalf("v1 code=%d", w.Code)
	}
}

func TestOwnerReposDetailedTwinsAndDiscovery(t *testing.T) {
	f := newFixture(t)
	putTestCatalog(t, f)
	// Triple-lane twins (14 §14.12 two-lane rule + services twin).
	for _, p := range []string{
		"/api/v1/owners/demo/repos/detailed",
		"/api-browser/v1/owners/demo/repos/detailed",
		"/services/api/owners/demo/repos/detailed",
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
		if e == "/api/v1/owners/{owner}/repos/detailed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("discovery missing detailed template: %v", doc.Endpoints)
	}
}

// TestOwnerReposDetailedMirrorFlag (Forgejo #281): rows carry the
// mirror flag (+ upstream when the sidecar parses) with no per-row
// summary fetch — the listing itself is the source. Absent sidecar →
// false; valid sidecar → true + upstream; corrupt-but-present → true
// with no upstream (fail closed).
func TestOwnerReposDetailedMirrorFlag(t *testing.T) {
	f := newFixture(t)
	putTestCatalog(t, f)
	ctx := context.Background()
	put := func(owner, name, body string) {
		t.Helper()
		if _, err := f.env.Store.Put(ctx, store.MirrorKey(owner, name),
			store.PutBody{Bytes: []byte(body)}, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	byName := func(rows []RepoSizeRow) map[string]RepoSizeRow {
		m := map[string]RepoSizeRow{}
		for _, r := range rows {
			m[r.Name] = r
		}
		return m
	}

	// No sidecars → every row is a non-mirror (flag present, false).
	w := f.req("GET", "/api/v1/owners/demo/repos/detailed")
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	rows := byName(decodeDetailed(t, w.Body.Bytes()))
	if rows["hello"].Mirror || rows["walgit"].Mirror {
		t.Fatalf("no sidecars, want mirror=false: %+v", rows)
	}
	if rows["hello"].MirrorUpstream != "" {
		t.Fatalf("no upstream without sidecar: %+v", rows["hello"])
	}

	// Valid sidecar on hello; corrupt-but-present on walgit.
	put("demo", "hello", `{"version":1,"upstream_url":"https://example.com/up.git","schedule":"daily"}`)
	put("demo", "walgit", `}{ not json`)
	w = f.req("GET", "/api/v1/owners/demo/repos/detailed")
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	rows = byName(decodeDetailed(t, w.Body.Bytes()))
	if !rows["hello"].Mirror {
		t.Fatalf("hello sidecar present, want mirror=true: %+v", rows["hello"])
	}
	if rows["hello"].MirrorUpstream != "https://example.com/up.git" {
		t.Fatalf("hello upstream: %+v", rows["hello"])
	}
	if !rows["walgit"].Mirror {
		t.Fatalf("corrupt sidecar still counts as mirror (fail closed): %+v", rows["walgit"])
	}
	if rows["walgit"].MirrorUpstream != "" {
		t.Fatalf("corrupt sidecar carries no upstream: %+v", rows["walgit"])
	}
}
