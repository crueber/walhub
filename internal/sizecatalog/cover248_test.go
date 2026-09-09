// cover248_test.go — Forgejo #248 coverage for the error/isolation paths:
// WriteCatalog abort + cancel, ReadCatalog corrupt, foldOne bad inputs,
// store-enumerated listing, EncodeStats zero-clock, Row.FullName.
package sizecatalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func TestRowFullNameAndZeroClock(t *testing.T) {
	r := Row{Owner: "o", Name: "r"}
	if r.FullName() != "o/r" {
		t.Fatalf("fullname: %s", r.FullName())
	}
	b := EncodeStats(1, 2, 3, time.Time{})
	s, ok, err := DecodeStats(b)
	if err != nil || !ok || s.SizeBytes != 1 {
		t.Fatalf("zero-clock encode: %+v %v %v", s, ok, err)
	}
	if s.UpdatedAt == "" {
		t.Fatal("updated_at must be stamped")
	}
}

func TestWriteCatalogAbortAndCancel(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	got, err := WriteCatalog(ctx, st, func(*proto.RepoCatalog) (*proto.RepoCatalog, error) { return nil, nil })
	if err != nil || got != nil {
		t.Fatalf("abort: %v %v", got, err)
	}
	boom := errors.New("boom")
	if _, err := WriteCatalog(ctx, st, func(*proto.RepoCatalog) (*proto.RepoCatalog, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("f error: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := WriteCatalog(cctx, st, func(*proto.RepoCatalog) (*proto.RepoCatalog, error) {
		t.Fatal("f must not run on cancelled ctx")
		return nil, nil
	}); err == nil {
		t.Fatal("cancelled ctx must error")
	}
	if err := sleepBackoff(cctx, 0); err == nil {
		t.Fatal("cancelled backoff must error")
	}
	// Normal write then overwrite (Create → Update path).
	if _, err := WriteCatalog(ctx, st, func(cur *proto.RepoCatalog) (*proto.RepoCatalog, error) {
		return &proto.RepoCatalog{Repos: []string{"o/a"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteCatalog(ctx, st, func(cur *proto.RepoCatalog) (*proto.RepoCatalog, error) {
		if len(cur.Repos) != 1 {
			t.Fatalf("re-read: %+v", cur)
		}
		cur.Repos = append(cur.Repos, "o/b")
		return cur, nil
	}); err != nil {
		t.Fatal(err)
	}
	cat, _ := ReadCatalog(ctx, st)
	if len(cat.Repos) != 2 {
		t.Fatalf("repos: %+v", cat.Repos)
	}
}

func TestReadCatalogCorrupt(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if _, err := st.Put(ctx, CatalogKey, store.PutBody{Bytes: []byte{0xff, 0xff}}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCatalog(ctx, st); err == nil {
		t.Fatal("corrupt catalog must error (bucket is wrong)")
	}
}

func TestFoldOneBadInputs(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"", "noslash", "/x", "x/", "a/b/c"} {
		if _, _, _, _, ok := foldOne(ctx, st, id, now); ok {
			t.Fatalf("%q must fail", id)
		}
	}
	// Missing manifest → unknown.
	if _, _, _, _, ok := foldOne(ctx, st, "o/ghost", now); ok {
		t.Fatal("missing manifest must fail")
	}
	// Corrupt manifest → unknown.
	if _, err := st.Put(ctx, "repos/o/bad/manifest.pb", store.PutBody{Bytes: []byte{0xff}}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, ok := foldOne(ctx, st, "o/bad", now); ok {
		t.Fatal("corrupt manifest must fail")
	}
	// Corrupt sidecar is tolerated (treated as changed, rewritten).
	m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "o/c", HeadSeq: 1, Revision: 1}
	if _, err := st.Put(ctx, "repos/o/c/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.StatsKey("o", "c"), store.PutBody{Bytes: []byte("{bad")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, changed, ok := foldOne(ctx, st, "o/c", now); !ok || !changed {
		t.Fatalf("corrupt sidecar must rewrite: %v %v", changed, ok)
	}
}

func TestListIDsViaStore(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	mk := func(id string) {
		t.Helper()
		m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: id, Revision: 1}
		if _, err := st.Put(ctx, "repos/"+id+"/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
	}
	mk("o/a")
	mk("o/b")
	// Ghost prefix without a manifest must not list.
	if _, err := st.Put(ctx, "repos/o/ghost/junk", store.PutBody{Bytes: []byte("x")}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	ids, err := listIDs(ctx, st, SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "o/a" || ids[1] != "o/b" {
		t.Fatalf("ids: %v", ids)
	}
	// Sweep over the store-enumerated list (nil ListRepos exercises LIST).
	fixed := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	res, err := Sweep(ctx, st, SweepOptions{Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Folded != 2 {
		t.Fatalf("sweep: %+v", res)
	}
}

func TestRowsForOwnerUpdatedAt(t *testing.T) {
	cat := &proto.RepoCatalog{Entries: []*proto.RepoCatalogEntry{{
		Repo: "o/a", SizeBytes: 5, ObjectCount: 1, HeadSeq: 2,
		UpdatedAt: &proto.Timestamp{Seconds: 1700000000},
	}}}
	rows := RowsForOwner("o", []string{"a"}, cat)
	if rows[0].UpdatedAt == nil || rows[0].ObjectCount == nil || rows[0].HeadSeq == nil {
		t.Fatalf("row: %+v", rows[0])
	}
}
