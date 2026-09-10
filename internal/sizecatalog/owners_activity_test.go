// owners_activity_test.go — Forgejo #283 derived per-owner rollup.
//
// Rules pinned here:
//   - OwnerRollups folds max(LastCommitTime) per owner over the catalog
//     entries (nil catalog/entries/times skipped, malformed repo ids
//     skipped); ties keep the smaller sha (deterministic).
//   - SortOwners sort=activity: known in the requested direction, unknowns
//     always last in EITHER direction, ties name-ascending; any other sort
//     is legacy ascending name order (rollup untouched).
//   - Incremental: healing one repo's entry (the sweep's per-repo fold)
//     moves its owner's max without a rescan — the rollup is a pure fold,
//     so the #247 per-repo incrementality carries the owner level.
package sizecatalog

import (
	"context"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func ownerCatEntry(repo, sha string, secs int64) *proto.RepoCatalogEntry {
	e := &proto.RepoCatalogEntry{Repo: repo, SizeBytes: 10, ObjectCount: 1, HeadSeq: 1}
	if sha != "" {
		e.LastCommitSHA = sha
	}
	if secs != 0 {
		e.LastCommitTime = &proto.Timestamp{Seconds: secs}
	}
	return e
}

func TestOwnerRollupsValues(t *testing.T) {
	cases := []struct {
		name    string
		entries []*proto.RepoCatalogEntry
		want    map[string]OwnerRollup
	}{
		{"nil catalog", nil, map[string]OwnerRollup{}},
		{"no entries", []*proto.RepoCatalogEntry{}, map[string]OwnerRollup{}},
		{"nil entries skipped", []*proto.RepoCatalogEntry{nil}, map[string]OwnerRollup{}},
		{"single repo",
			[]*proto.RepoCatalogEntry{ownerCatEntry("alice/a", "aaa", 100)},
			map[string]OwnerRollup{"alice": {LastCommitTime: time.Unix(100, 0).UTC().Format(time.RFC3339), LastCommitSHA: "aaa"}}},
		{"max over owner repos",
			[]*proto.RepoCatalogEntry{
				ownerCatEntry("alice/old", "aaa", 100),
				ownerCatEntry("alice/new", "bbb", 300),
				ownerCatEntry("alice/mid", "ccc", 200),
			},
			map[string]OwnerRollup{"alice": {LastCommitTime: time.Unix(300, 0).UTC().Format(time.RFC3339), LastCommitSHA: "bbb"}}},
		{"unknowns skipped, owner without times absent",
			[]*proto.RepoCatalogEntry{
				ownerCatEntry("alice/a", "aaa", 100),
				ownerCatEntry("bob/b", "", 0),      // no time at all
				ownerCatEntry("carol/c", "ccc", 0), // sha but no time: still unknown
			},
			map[string]OwnerRollup{"alice": {LastCommitTime: time.Unix(100, 0).UTC().Format(time.RFC3339), LastCommitSHA: "aaa"}}},
		{"malformed repo ids skipped",
			[]*proto.RepoCatalogEntry{
				ownerCatEntry("noslash", "aaa", 100),
				ownerCatEntry("/lead", "bbb", 200),
				ownerCatEntry("trail/", "ccc", 300),
				ownerCatEntry("a/b/c", "ddd", 400),
				ownerCatEntry("ok/r", "eee", 50),
			},
			map[string]OwnerRollup{"ok": {LastCommitTime: time.Unix(50, 0).UTC().Format(time.RFC3339), LastCommitSHA: "eee"}}},
		{"tie keeps smaller sha",
			[]*proto.RepoCatalogEntry{
				ownerCatEntry("alice/z", "zzz", 100),
				ownerCatEntry("alice/a", "aaa", 100),
			},
			map[string]OwnerRollup{"alice": {LastCommitTime: time.Unix(100, 0).UTC().Format(time.RFC3339), LastCommitSHA: "aaa"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cat *proto.RepoCatalog
			if tc.entries != nil || tc.name == "no entries" || tc.name == "nil entries skipped" {
				cat = &proto.RepoCatalog{Entries: tc.entries}
			}
			got := OwnerRollups(cat)
			if len(got) != len(tc.want) {
				t.Fatalf("rollups = %v, want %v", got, tc.want)
			}
			for o, w := range tc.want {
				if got[o] != w {
					t.Fatalf("owner %q = %+v, want %+v", o, got[o], w)
				}
			}
		})
	}
}

func TestSortOwnersActivity(t *testing.T) {
	roll := func(times map[string]string) map[string]OwnerRollup {
		out := map[string]OwnerRollup{}
		for o, ts := range times {
			out[o] = OwnerRollup{LastCommitTime: ts, LastCommitSHA: "sha-" + o}
		}
		return out
	}
	cases := []struct {
		name    string
		names   []string
		rollups map[string]OwnerRollup
		sortKey string
		order   string
		want    []string
	}{
		{"nil input", nil, nil, "activity", "desc", []string{}},
		{"empty", []string{}, nil, "activity", "desc", []string{}},
		{"desc known first",
			[]string{"alice", "bob", "carol"},
			roll(map[string]string{"alice": "2026-09-08T12:00:00Z", "bob": "2026-09-10T12:00:00Z", "carol": "2026-09-09T12:00:00Z"}),
			"activity", "desc", []string{"bob", "carol", "alice"}},
		{"asc known first, unknowns still last",
			[]string{"alice", "bob", "carol"},
			roll(map[string]string{"alice": "2026-09-10T12:00:00Z", "bob": "2026-09-08T12:00:00Z"}),
			"activity", "asc", []string{"bob", "alice", "carol"}},
		{"desc unknowns last",
			[]string{"zed", "amy", "bob"},
			roll(map[string]string{"bob": "2026-09-10T12:00:00Z"}),
			"activity", "desc", []string{"bob", "amy", "zed"}},
		{"all unknown degrades to name order",
			[]string{"zed", "amy", "bob"}, nil, "activity", "desc",
			[]string{"amy", "bob", "zed"}},
		{"empty-time rollup entry counts as unknown",
			[]string{"zed", "amy"},
			map[string]OwnerRollup{"zed": {}},
			"activity", "desc", []string{"amy", "zed"}},
		{"ties break name-ascending",
			[]string{"zed", "amy", "bob"},
			roll(map[string]string{"zed": "2026-09-10T12:00:00Z", "amy": "2026-09-10T12:00:00Z", "bob": "2026-09-09T12:00:00Z"}),
			"activity", "desc", []string{"amy", "zed", "bob"}},
		{"single owner", []string{"solo"},
			roll(map[string]string{"solo": "2026-09-10T12:00:00Z"}),
			"activity", "desc", []string{"solo"}},
		{"default sort ignores rollup",
			[]string{"zed", "amy"},
			roll(map[string]string{"zed": "2026-09-10T12:00:00Z"}),
			"", "", []string{"amy", "zed"}},
		{"bogus sort is name order",
			[]string{"zed", "amy"}, nil, "size", "desc",
			[]string{"amy", "zed"}},
		{"bogus order is asc",
			[]string{"alice", "bob"},
			roll(map[string]string{"alice": "2026-09-10T12:00:00Z", "bob": "2026-09-08T12:00:00Z"}),
			"activity", "sideways", []string{"bob", "alice"}},
		{"case-insensitive params",
			[]string{"alice", "bob"},
			roll(map[string]string{"alice": "2026-09-10T12:00:00Z", "bob": "2026-09-08T12:00:00Z"}),
			"Activity", "DESC", []string{"alice", "bob"}},
		{"input not mutated",
			[]string{"zed", "amy"},
			roll(map[string]string{"zed": "2026-09-10T12:00:00Z"}),
			"activity", "desc", []string{"zed", "amy"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "input not mutated" {
				before := append([]string{}, tc.names...)
				_ = SortOwners(tc.names, tc.rollups, tc.sortKey, tc.order)
				for i := range before {
					if tc.names[i] != before[i] {
						t.Fatalf("input mutated: %v", tc.names)
					}
				}
				return
			}
			got := SortOwners(tc.names, tc.rollups, tc.sortKey, tc.order)
			if len(got) != len(tc.want) {
				t.Fatalf("sorted = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("sorted = %v, want %v", got, tc.want)
				}
			}
			if got == nil {
				t.Fatal("must return non-nil (wire null-safety)")
			}
		})
	}
}

// TestOwnerRollupHealsIncrementally proves the backfill/incremental claim:
// healing ONE repo's catalog entry (exactly what the sweep's per-repo fold
// does — no rescan) moves its owner's max on the next fold.
func TestOwnerRollupHealsIncrementally(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	putManifestHead(t, ctx, st, "alice/old", 5)
	putManifestHead(t, ctx, st, "alice/new", 5)
	// Both sidecars start hint-less (null activity, as a crashed push leaves).
	putSidecar(t, ctx, st, "alice", "old", 10, 2, 5, nil)
	putSidecar(t, ctx, st, "alice", "new", 10, 2, 5, nil)
	hook := func(ctx context.Context, id string) (string, time.Time, bool, error) {
		if id == "alice/new" {
			return "bbb", actTime, true, nil
		}
		return "", time.Time{}, false, nil // old stays unknown
	}
	if _, err := Sweep(ctx, st, SweepOptions{
		Now:      func() time.Time { return actTime },
		Activity: hook,
		ListRepos: func(context.Context) ([]string, error) {
			return []string{"alice/new", "alice/old"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	cat, err := ReadCatalog(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	got := OwnerRollups(cat)
	rl, ok := got["alice"]
	if !ok || rl.LastCommitSHA != "bbb" || rl.LastCommitTime != "2026-09-09T12:00:00Z" {
		t.Fatalf("owner max after one-repo heal: %v", got)
	}
	// A newer push on the OTHER repo moves the max without touching "new".
	if _, err := st.Put(ctx, store.StatsKey("alice", "old"),
		store.PutBody{Bytes: EncodeStats(10, 2, 5, actTime.Add(2*time.Hour),
			&Activity{TipSHA: "aaa", CommitTime: actTime.Add(2 * time.Hour)})},
		store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatal(err)
	}
	if _, err := Sweep(ctx, st, SweepOptions{
		Now: func() time.Time { return actTime.Add(2 * time.Hour) },
		ListRepos: func(context.Context) ([]string, error) {
			return []string{"alice/new", "alice/old"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	cat, _ = ReadCatalog(ctx, st)
	got = OwnerRollups(cat)
	rl = got["alice"]
	if rl.LastCommitSHA != "aaa" || rl.LastCommitTime != "2026-09-09T14:00:00Z" {
		t.Fatalf("owner max after second heal: %v", got)
	}
}
