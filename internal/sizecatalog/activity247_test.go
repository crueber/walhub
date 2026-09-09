// activity247_test.go — Forgejo #247 activity merge contract (the
// load-bearing #247/#248 interaction): the blind push-path write and the
// reading sweep must never clobber each other's fields on the shared
// meta/stats.json sidecar + catalog row.
//
// Rules pinned here:
//   - EncodeStats with nil activity records nulls (hint-less blind write).
//   - statsEqual gates PUT-if-changed on size + head + activity together.
//   - foldOne preserves sidecar activity when the hook is absent or fails,
//     heals it when the hook finds fresher state, and carries PushAt across
//     a derivation that only refreshes tip/time.
//   - The catalog fold never regresses a same-head row it cannot refresh.
//   - FilterSort sort=activity: time-desc/asc, unknowns always last, ties
//     on (owner, name).
package sizecatalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

var actTime = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

func putManifestHead(t *testing.T, ctx context.Context, st store.ObjectStore, id string, head uint64) {
	t.Helper()
	m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: id, HeadSeq: head, Revision: 1,
		Packs: []*proto.PackRef{{Checksum: "p1", PackSize: 6, IdxSize: 4, ObjectCount: 2}}}
	if _, err := st.Put(ctx, "repos/"+id+"/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
}

func putSidecar(t *testing.T, ctx context.Context, st store.ObjectStore, owner, name string, size, objs, head uint64, act *Activity) {
	t.Helper()
	if _, err := st.Put(ctx, store.StatsKey(owner, name),
		store.PutBody{Bytes: EncodeStats(size, objs, head, actTime, act)},
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
}

func readStats(t *testing.T, ctx context.Context, st store.ObjectStore, owner, name string) *Stats {
	t.Helper()
	body, _, err := store.GetBytes(ctx, st, store.StatsKey(owner, name), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, ok, err := DecodeStats(body)
	if err != nil || !ok {
		t.Fatalf("sidecar decode: ok=%v err=%v", ok, err)
	}
	return s
}

func TestEncodeStatsActivityNulls(t *testing.T) {
	// Nil activity (hint-less blind write) records nulls, not zero values.
	b := EncodeStats(10, 2, 5, actTime, nil)
	s, ok, err := DecodeStats(b)
	if err != nil || !ok {
		t.Fatalf("decode: %v %v", ok, err)
	}
	if s.LastCommitSHA != nil || s.LastCommitTime != nil || s.LastPushAt != nil {
		t.Fatalf("nil activity must record nulls: %+v", s)
	}
	// A full activity stamps all three fields (RFC 3339 UTC).
	b = EncodeStats(10, 2, 5, actTime, &Activity{TipSHA: "abc", CommitTime: actTime, PushAt: actTime})
	s, _, _ = DecodeStats(b)
	if s.LastCommitSHA == nil || *s.LastCommitSHA != "abc" {
		t.Fatalf("sha: %+v", s)
	}
	if s.LastCommitTime == nil || *s.LastCommitTime != "2026-09-09T12:00:00Z" {
		t.Fatalf("time: %+v", s)
	}
	if s.LastPushAt == nil || *s.LastPushAt != "2026-09-09T12:00:00Z" {
		t.Fatalf("push: %+v", s)
	}
	// Zero times omit their fields (the sweep never fabricates push times).
	b = EncodeStats(10, 2, 5, actTime, &Activity{TipSHA: "abc"})
	s, _, _ = DecodeStats(b)
	if s.LastCommitTime != nil || s.LastPushAt != nil {
		t.Fatalf("zero times must omit: %+v", s)
	}
}

func TestStatsEqualCoversActivity(t *testing.T) {
	act := &Activity{TipSHA: "abc", CommitTime: actTime, PushAt: actTime}
	b := EncodeStats(10, 2, 5, actTime, act)
	s, _, _ := DecodeStats(b)
	if !statsEqual(s, 10, 2, 5, act) {
		t.Fatal("identical state must compare equal")
	}
	if statsEqual(s, 11, 2, 5, act) {
		t.Fatal("size change must compare different")
	}
	if statsEqual(s, 10, 2, 6, act) {
		t.Fatal("head change must compare different")
	}
	if statsEqual(s, 10, 2, 5, &Activity{TipSHA: "abd", CommitTime: actTime, PushAt: actTime}) {
		t.Fatal("sha change must compare different")
	}
	if statsEqual(s, 10, 2, 5, nil) {
		t.Fatal("null activity must compare different from known")
	}
	// nil-vs-nil is equal (two unknowns agree).
	s2, _, _ := DecodeStats(EncodeStats(10, 2, 5, actTime, nil))
	if !statsEqual(s2, 10, 2, 5, nil) {
		t.Fatal("two null activities must compare equal")
	}
}

func TestFoldOnePreservesActivityWithoutHook(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putManifestHead(t, ctx, st, "o/a", 7)
	putSidecar(t, ctx, st, "o", "a", 10, 2, 7,
		&Activity{TipSHA: "abc", CommitTime: actTime, PushAt: actTime})
	// Sweep without a hook folds sizes only: the sidecar is already current
	// (same head, same size) → no write, activity intact.
	_, _, _, act, changed, ok := foldOne(ctx, st, "o/a", actTime, nil, nil)
	if !ok || changed {
		t.Fatalf("fresh sidecar must skip the write: changed=%v ok=%v", changed, ok)
	}
	if act == nil || act.TipSHA != "abc" {
		t.Fatalf("fold must return the preserved activity: %+v", act)
	}
	s := readStats(t, ctx, st, "o", "a")
	if s.LastCommitSHA == nil || *s.LastCommitSHA != "abc" {
		t.Fatalf("sidecar activity clobbered: %+v", s)
	}
}

func TestFoldOneHealsStaleActivityViaHook(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putManifestHead(t, ctx, st, "o/a", 8) // head moved past the sidecar's 7
	putSidecar(t, ctx, st, "o", "a", 10, 2, 7,
		&Activity{TipSHA: "abc", CommitTime: actTime, PushAt: actTime.Add(-time.Hour)})
	hook := func(ctx context.Context, id string) (string, time.Time, bool, error) {
		if id != "o/a" {
			t.Fatalf("hook id = %q", id)
		}
		return "def", actTime, true, nil
	}
	_, _, head, act, changed, ok := foldOne(ctx, st, "o/a", actTime, hook, nil)
	if !ok || !changed || head != 8 {
		t.Fatalf("stale sidecar must rewrite: changed=%v ok=%v head=%d", changed, ok, head)
	}
	if act == nil || act.TipSHA != "def" || !act.CommitTime.Equal(actTime) {
		t.Fatalf("fold must return healed activity: %+v", act)
	}
	// PushAt carries across a tip/time-only refresh (never fabricated,
	// never dropped).
	if act.PushAt.IsZero() || !act.PushAt.Equal(actTime.Add(-time.Hour)) {
		t.Fatalf("PushAt must carry: %+v", act)
	}
	s := readStats(t, ctx, st, "o", "a")
	if s.LastCommitSHA == nil || *s.LastCommitSHA != "def" || s.HeadSeq != 8 {
		t.Fatalf("sidecar after heal: %+v", s)
	}
}

func TestFoldOneHookFailurePreserves(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putManifestHead(t, ctx, st, "o/a", 8)
	putSidecar(t, ctx, st, "o", "a", 10, 2, 7,
		&Activity{TipSHA: "abc", CommitTime: actTime, PushAt: actTime})
	boom := errors.New("git down")
	hook := func(ctx context.Context, id string) (string, time.Time, bool, error) {
		return "", time.Time{}, false, boom
	}
	_, _, _, act, changed, ok := foldOne(ctx, st, "o/a", actTime, hook, nil)
	if !ok {
		t.Fatal("hook failure must isolate to the activity (fold still ok)")
	}
	if !changed {
		t.Fatal("head moved 7→8, size fold must still write")
	}
	if act == nil || act.TipSHA != "abc" {
		t.Fatalf("failed derivation must preserve: %+v", act)
	}
	s := readStats(t, ctx, st, "o", "a")
	if s.HeadSeq != 8 || s.LastCommitSHA == nil || *s.LastCommitSHA != "abc" {
		t.Fatalf("size refreshed, activity preserved: %+v", s)
	}
}

func TestFoldOneMissingSidecarHookMissWritesNulls(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putManifestHead(t, ctx, st, "o/a", 3)
	hook := func(ctx context.Context, id string) (string, time.Time, bool, error) {
		return "", time.Time{}, false, nil // empty repo: no tip
	}
	_, _, _, act, changed, ok := foldOne(ctx, st, "o/a", actTime, hook, nil)
	if !ok || !changed {
		t.Fatalf("missing sidecar must be created: %v %v", changed, ok)
	}
	if act == nil || act.TipSHA != "" || !act.CommitTime.IsZero() {
		t.Fatalf("hook miss must fold nulls: %+v", act)
	}
	s := readStats(t, ctx, st, "o", "a")
	if s.LastCommitSHA != nil || s.LastCommitTime != nil || s.LastPushAt != nil {
		t.Fatalf("hook miss must record nulls: %+v", s)
	}
}

func TestFoldOneHealsNullActivityOnFreshHead(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putManifestHead(t, ctx, st, "o/a", 5)
	// A hint-less push recorded nulls at the current head; the sweep heals.
	putSidecar(t, ctx, st, "o", "a", 10, 2, 5, nil)
	calls := 0
	hook := func(ctx context.Context, id string) (string, time.Time, bool, error) {
		calls++
		return "abc", actTime, true, nil
	}
	_, _, _, act, changed, ok := foldOne(ctx, st, "o/a", actTime, hook, nil)
	if !ok || !changed {
		t.Fatalf("null activity on fresh head must heal: %v %v", changed, ok)
	}
	if calls != 1 {
		t.Fatalf("hook calls = %d, want 1", calls)
	}
	if act == nil || act.TipSHA != "abc" {
		t.Fatalf("healed: %+v", act)
	}
}

func TestFoldOneSkipsGitWhenFresh(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putManifestHead(t, ctx, st, "o/a", 5)
	putSidecar(t, ctx, st, "o", "a", 10, 2, 5,
		&Activity{TipSHA: "abc", CommitTime: actTime})
	hook := func(ctx context.Context, id string) (string, time.Time, bool, error) {
		t.Fatal("fresh sidecar must cost zero git")
		return "", time.Time{}, false, nil
	}
	_, _, _, _, changed, ok := foldOne(ctx, st, "o/a", actTime, hook, nil)
	if !ok || changed {
		t.Fatalf("fresh sidecar: changed=%v ok=%v", changed, ok)
	}
}

func TestSweepFoldsActivityIntoCatalog(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putManifestHead(t, ctx, st, "o/a", 5)
	putManifestHead(t, ctx, st, "o/b", 5)
	putSidecar(t, ctx, st, "o", "a", 10, 2, 5,
		&Activity{TipSHA: "aaa", CommitTime: actTime})
	hook := func(ctx context.Context, id string) (string, time.Time, bool, error) {
		if id == "o/b" {
			return "bbb", actTime.Add(time.Hour), true, nil
		}
		t.Fatalf("fresh repo must not derive: %s", id)
		return "", time.Time{}, false, nil
	}
	res, err := Sweep(ctx, st, SweepOptions{
		Now:      func() time.Time { return actTime },
		Activity: hook,
		ListRepos: func(context.Context) ([]string, error) {
			return []string{"o/a", "o/b"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Folded != 1 || res.Unchanged != 1 {
		t.Fatalf("folded=%d unchanged=%d, want 1/1", res.Folded, res.Unchanged)
	}
	cat, err := ReadCatalog(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	byRepo := map[string]*proto.RepoCatalogEntry{}
	for _, e := range cat.Entries {
		byRepo[e.Repo] = e
	}
	a := byRepo["o/a"]
	if a == nil || a.LastCommitSHA != "aaa" || a.LastCommitTime == nil ||
		a.LastCommitTime.Seconds != actTime.Unix() {
		t.Fatalf("preserved row: %+v", a)
	}
	b := byRepo["o/b"]
	if b == nil || b.LastCommitSHA != "bbb" || b.LastCommitTime == nil ||
		b.LastCommitTime.Seconds != actTime.Add(time.Hour).Unix() {
		t.Fatalf("healed row: %+v", b)
	}
	// RowsForOwner projects the activity onto listing rows.
	rows := RowsForOwner("o", []string{"a", "b", "ghost"}, cat)
	if len(rows) != 3 {
		t.Fatalf("rows: %d", len(rows))
	}
	if rows[0].LastCommitSHA == nil || *rows[0].LastCommitSHA != "aaa" {
		t.Fatalf("row a: %+v", rows[0])
	}
	if rows[1].LastCommitTime == nil || *rows[1].LastCommitTime != "2026-09-09T13:00:00Z" {
		t.Fatalf("row b: %+v", rows[1])
	}
	if rows[2].LastCommitSHA != nil || rows[2].LastCommitTime != nil || rows[2].LastPushAt != nil {
		t.Fatalf("missing entry must project nulls: %+v", rows[2])
	}
}

func TestSweepCatalogNeverRegressesSameHeadRow(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putManifestHead(t, ctx, st, "o/a", 5)
	putSidecar(t, ctx, st, "o", "a", 10, 2, 5,
		&Activity{TipSHA: "aaa", CommitTime: actTime, PushAt: actTime})
	// Seed the catalog with a KNOWN row, then wipe the sidecar's activity
	// knowledge: derivation fails, head unmoved → the stored row survives.
	if _, err := Sweep(ctx, st, SweepOptions{
		Now: func() time.Time { return actTime },
		ListRepos: func(context.Context) ([]string, error) {
			return []string{"o/a"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	cat, _ := ReadCatalog(ctx, st)
	if len(cat.Entries) != 1 || cat.Entries[0].LastCommitSHA != "aaa" {
		t.Fatalf("seed: %+v", cat.Entries)
	}
	// Now regress the sidecar to nulls at the SAME head (a hint-less push
	// that raced the sweep) and re-sweep with a failing hook.
	if _, err := st.Put(ctx, store.StatsKey("o", "a"),
		store.PutBody{Bytes: EncodeStats(10, 2, 5, actTime, nil)},
		store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("git down")
	if _, err := Sweep(ctx, st, SweepOptions{
		Now: func() time.Time { return actTime },
		Activity: func(ctx context.Context, id string) (string, time.Time, bool, error) {
			return "", time.Time{}, false, boom
		},
		ListRepos: func(context.Context) ([]string, error) {
			return []string{"o/a"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	cat, _ = ReadCatalog(ctx, st)
	if len(cat.Entries) != 1 || cat.Entries[0].LastCommitSHA != "aaa" {
		t.Fatalf("same-head row must survive a failed derivation: %+v", cat.Entries)
	}
}

func TestFilterSortActivity(t *testing.T) {
	strp := func(s string) *string { return &s }
	rows := []Row{
		{Owner: "o", Name: "b", LastCommitTime: strp("2026-09-09T12:00:00Z")},
		{Owner: "o", Name: "a", LastCommitTime: strp("2026-09-10T12:00:00Z")},
		{Owner: "o", Name: "c"}, // unknown: always last
		{Owner: "o", Name: "d", LastCommitTime: strp("2026-09-10T12:00:00Z")}, // tie with a
		{Owner: "o", Name: "e"}, // second unknown: tie → (owner,name)
	}
	got := FilterSort(rows, "activity", "desc", nil, nil)
	want := []string{"a", "d", "b", "c", "e"} // newest first; ties → name; unknowns last
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("desc order[%d]=%s, want %s", i, got[i].Name, w)
		}
	}
	got = FilterSort(rows, "activity", "asc", nil, nil)
	want = []string{"b", "a", "d", "c", "e"} // unknowns STILL last in asc
	for i, w := range want {
		if got[i].Name != w {
			t.Fatalf("asc order[%d]=%s, want %s", i, got[i].Name, w)
		}
	}
	// Unknown sort key falls back to name order (never activity).
	got = FilterSort(rows, "bogus", "asc", nil, nil)
	for i, w := range []string{"a", "b", "c", "d", "e"} {
		if got[i].Name != w {
			t.Fatalf("fallback order[%d]=%s, want %s", i, got[i].Name, w)
		}
	}
}
