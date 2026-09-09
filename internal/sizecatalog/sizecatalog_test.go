package sizecatalog

import (
	"context"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func TestSizeOf(t *testing.T) {
	if s, o := SizeOf(nil); s != 0 || o != 0 {
		t.Fatalf("nil packs → (%d,%d), want (0,0)", s, o)
	}
	if s, o := SizeOf([]*proto.PackRef{}); s != 0 || o != 0 {
		t.Fatalf("empty packs → (%d,%d), want (0,0)", s, o)
	}
	packs := []*proto.PackRef{
		{Checksum: "a", PackSize: 100, IdxSize: 10, ObjectCount: 5},
		{Checksum: "b", PackSize: 200, IdxSize: 20, ObjectCount: 7},
		nil,
	}
	if s, o := SizeOf(packs); s != 330 || o != 12 {
		t.Fatalf("sum → (%d,%d), want (330,12)", s, o)
	}
	// Saturation, never wrap.
	maxed := []*proto.PackRef{{Checksum: "m", PackSize: ^uint64(0), IdxSize: 1}}
	if s, _ := SizeOf(maxed); s != ^uint64(0) {
		t.Fatalf("overflow → %d, want max uint64", s)
	}
	if _, _, ok := ManifestSize(nil); ok {
		t.Fatal("nil manifest must report unknown, not empty")
	}
	m := &proto.Manifest{Packs: packs}
	if s, o, ok := ManifestSize(m); !ok || s != 330 || o != 12 {
		t.Fatalf("manifest → (%d,%d,%v)", s, o, ok)
	}
}

func TestStatsRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	b := EncodeStats(330, 12, 4, now)
	s, ok, err := DecodeStats(b)
	if err != nil || !ok {
		t.Fatalf("decode: %v %v", ok, err)
	}
	if s.Version != StatsVersion || s.SizeBytes != 330 || s.ObjectCount != 12 || s.HeadSeq != 4 {
		t.Fatalf("shape: %+v", s)
	}
	if _, ok, _ := DecodeStats(nil); ok {
		t.Fatal("empty body must be unknown")
	}
	if _, _, err := DecodeStats([]byte(`{"version":999}`)); err == nil {
		t.Fatal("unknown version must fail closed")
	}
	if _, _, err := DecodeStats([]byte(`{bad`)); err == nil {
		t.Fatal("corrupt body must error")
	}
}

func u64p(n uint64) *uint64 { return &n }

func TestFilterSort(t *testing.T) {
	strp := func(s string) *string { return &s }
	_ = strp
	rows := []Row{
		{Owner: "o", Name: "b", SizeBytes: u64p(200)},
		{Owner: "o", Name: "a", SizeBytes: u64p(100)},
		{Owner: "o", Name: "c"},                       // unknown
		{Owner: "o", Name: "d", SizeBytes: u64p(100)}, // tie with a
		{Owner: "o", Name: "e"},                       // second unknown: tie → (owner,name)
	}
	got := FilterSort(rows, "size", "asc", nil, nil)
	want := []string{"a", "d", "b", "c", "e"} // ties → (owner,name); unknowns last
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("asc order[%d]=%s, want %s (%v)", i, got[i].Name, w, names(got))
		}
	}
	got = FilterSort(rows, "size", "desc", nil, nil)
	want = []string{"b", "a", "d", "c", "e"} // unknowns STILL last in desc
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("desc order[%d]=%s, want %s", i, got[i].Name, w)
		}
	}
	got = FilterSort(rows, "name", "asc", nil, nil)
	want = []string{"a", "b", "c", "d", "e"}
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("name order[%d]=%s, want %s", i, got[i].Name, w)
		}
	}
	// Cross-owner tie on size breaks on owner first.
	mixed := []Row{
		{Owner: "zeta", Name: "a", SizeBytes: u64p(50)},
		{Owner: "alpha", Name: "z", SizeBytes: u64p(50)},
	}
	got = FilterSort(mixed, "size", "asc", nil, nil)
	if got[0].Owner != "alpha" || got[1].Owner != "zeta" {
		t.Fatalf("owner tiebreak: %+v", got)
	}
	min := uint64(150)
	got = FilterSort(rows, "size", "asc", &min, nil)
	if len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("min_bytes filter: %v", names(got))
	}
	max := uint64(150)
	got = FilterSort(rows, "size", "asc", nil, &max)
	if len(got) != 2 {
		t.Fatalf("max_bytes filter drops unknowns: %v", names(got))
	}
}

func names(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out
}

func TestRowsForOwner(t *testing.T) {
	cat := &proto.RepoCatalog{
		Repos: []string{"o/a", "o/b"},
		Entries: []*proto.RepoCatalogEntry{
			{Repo: "o/a", SizeBytes: 10, ObjectCount: 1, HeadSeq: 2},
		},
	}
	rows := RowsForOwner("o", []string{"a", "b"}, cat)
	if len(rows) != 2 {
		t.Fatalf("rows: %d", len(rows))
	}
	if rows[0].SizeBytes == nil || *rows[0].SizeBytes != 10 {
		t.Fatalf("known row: %+v", rows[0])
	}
	if rows[1].SizeBytes != nil {
		t.Fatalf("missing entry must be null (unknown), got %+v", rows[1])
	}
	// Empty-repo verified zero rides a present entry with 0.
	cat.Entries = append(cat.Entries, &proto.RepoCatalogEntry{Repo: "o/b"})
	rows = RowsForOwner("o", []string{"b"}, cat)
	if rows[0].SizeBytes == nil || *rows[0].SizeBytes != 0 {
		t.Fatalf("zero entry must be non-nil 0: %+v", rows[0])
	}
}

// putManifest writes a minimal manifest.pb for "owner/name".
func putManifest(t *testing.T, ctx context.Context, st store.ObjectStore, id string, packs []*proto.PackRef, headSeq uint64) {
	t.Helper()
	slash := -1
	for i := 0; i < len(id); i++ {
		if id[i] == '/' {
			slash = i
		}
	}
	m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: id, ObjectFormat: "sha1", HeadSeq: headSeq, Revision: 1, Packs: packs}
	if _, err := st.Put(ctx, "repos/"+id+"/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	_ = slash
}

func TestSweepFold(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putManifest(t, ctx, st, "o/small", []*proto.PackRef{{Checksum: "a", PackSize: 100, IdxSize: 10, ObjectCount: 3}}, 2)
	putManifest(t, ctx, st, "o/big", []*proto.PackRef{{Checksum: "b", PackSize: 500, IdxSize: 50, ObjectCount: 9}}, 5)
	putManifest(t, ctx, st, "o/empty", nil, 0)

	fixed := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	res, err := Sweep(ctx, st, SweepOptions{Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Folded != 3 || res.Failed != 0 {
		t.Fatalf("sweep: %+v", res)
	}
	// Sidecars carry the sums.
	for id, want := range map[string]uint64{"o/small": 110, "o/big": 550, "o/empty": 0} {
		body, _, err := store.GetBytes(ctx, st, "repos/"+id+"/meta/stats.json", store.GetOptions{})
		if err != nil || body == nil {
			t.Fatalf("%s sidecar: %v", id, err)
		}
		s, ok, err := DecodeStats(body)
		if err != nil || !ok || s.SizeBytes != want {
			t.Fatalf("%s sidecar: %+v %v %v", id, s, ok, err)
		}
	}
	cat, err := ReadCatalog(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Entries) != 3 {
		t.Fatalf("catalog entries: %d", len(cat.Entries))
	}
	byRepo := map[string]*proto.RepoCatalogEntry{}
	for _, e := range cat.Entries {
		byRepo[e.Repo] = e
	}
	if byRepo["o/big"].SizeBytes != 550 || byRepo["o/big"].ObjectCount != 9 || byRepo["o/big"].HeadSeq != 5 {
		t.Fatalf("big row: %+v", byRepo["o/big"])
	}
	// Second sweep: everything unchanged (no rewrite storm).
	res2, err := Sweep(ctx, st, SweepOptions{Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Unchanged != 3 || res2.Folded != 0 {
		t.Fatalf("re-sweep: %+v", res2)
	}
}

func TestSweepResumeAndIsolation(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putManifest(t, ctx, st, "o/a", nil, 0)
	putManifest(t, ctx, st, "o/b", nil, 0)
	putManifest(t, ctx, st, "o/c", nil, 0)
	fixed := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	r1, err := Sweep(ctx, st, SweepOptions{Now: func() time.Time { return fixed }, MaxRepos: 2})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Folded != 2 || r1.NextCursor == "" {
		t.Fatalf("bounded pass: %+v", r1)
	}
	r2, err := Sweep(ctx, st, SweepOptions{Now: func() time.Time { return fixed }, MaxRepos: 2, Cursor: r1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Folded != 1 || r2.NextCursor != "" {
		t.Fatalf("resume pass: %+v", r2)
	}
	// Catalog deleted → optional/rebuildable: reads degrade, sweep restores.
	if err := st.Delete(ctx, CatalogKey, ""); err != nil {
		t.Fatal(err)
	}
	cat, err := ReadCatalog(ctx, st)
	if err != nil || len(cat.Entries) != 0 {
		t.Fatalf("absent catalog must read empty: %v %+v", err, cat)
	}
	rows := RowsForOwner("o", []string{"a"}, cat)
	if rows[0].SizeBytes != nil {
		t.Fatal("absent catalog → null rows, never zero")
	}
	if _, err := Sweep(ctx, st, SweepOptions{Now: func() time.Time { return fixed }}); err != nil {
		t.Fatal(err)
	}
	cat, _ = ReadCatalog(ctx, st)
	if len(cat.Entries) != 3 {
		t.Fatalf("rebuilt catalog: %d", len(cat.Entries))
	}
}

func TestReadCatalogAbsent(t *testing.T) {
	cat, err := ReadCatalog(context.Background(), store.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Repos) != 0 || len(cat.Entries) != 0 {
		t.Fatalf("absent → empty: %+v", cat)
	}
}
