package bundle

import (
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store/proto"
)

// TestGateMath is §8.7: gates apply to incrementals only; evaluation order is
// no-state → unchanged → min_commits; closed slots (> 120 s past) record
// verdicts; the open slot is re-measured each pass and never recorded.
func TestGateMath(t *testing.T) {
	now := at("2026-09-01T12:00:00Z")
	sun := at("2026-08-30T00:00:00Z")
	mon := at("2026-08-31T00:00:00Z")

	strategies := DefaultStrategies()
	byName := ByName(strategies)
	daily := byName["daily"]

	oid1 := oidFor("tips-1")
	oid2 := oidFor("tips-2")

	// The weekly exists; the Monday daily cut on it; the daily's tips moved.
	W := entry("weekly", "weekly/2026-08-30T00:00:00Z", KindFull, uint64(sun.Unix()), uint64(sun.Unix()), 40)
	monDaily := entry("daily", "daily/2026-08-31T00:00:00Z", KindIncremental, uint64(mon.Unix()), uint64(mon.Unix()), 60)
	list := protoBundleList(W, monDaily)

	// The Tuesday slot (after the Monday daily and the Aug-30 weekly) has a
	// resolvable base: ownPrev = Monday daily, chain → cut on it.
	tue := at("2026-09-01T00:00:00Z")
	tueTips := Refs{{Name: "HEAD", Oid: oid1}, {Name: "refs/heads/main", Oid: oidFor("tue-main")}}
	wal := &fakeWal{
		asOf: map[time.Time]fold{tue: {refs: tueTips, seq: 70}},
	}

	t.Run("unchanged gate: same tips as newest same-strategy entry", func(t *testing.T) {
		// monDaily carries oid2 tips; feed identical tips at the Tue slot.
		sameTips := Refs{{Name: "HEAD", Oid: monDaily.Tips[0].Oid}, {Name: "refs/heads/main", Oid: monDaily.Tips[1].Oid}}
		wal.asOf[tue] = fold{refs: sameTips, seq: 70}
		prim := &fakePrim{counts: []int{999}}
		d := &Deps{Wal: wal, Prim: prim, St: newMemStore(t), Now: func() time.Time { return now.Add(2 * time.Hour) },
			RepoDir: "/repo", List: list, Verdicts: dVerdicts()}
		err := BuildSlot(t.Context(), d, "acme/repo", strategies, daily, tue)
		if err != nil {
			t.Fatalf("BuildSlot: %v", err)
		}
		if len(d.Verdicts) != 1 {
			t.Fatalf("verdicts = %d, want 1", len(d.Verdicts))
		}
		v := d.Verdicts[0]
		wantReason := "unchanged since " + monDaily.ID
		if v.Reason != wantReason {
			t.Fatalf("reason = %q, want %q", v.Reason, wantReason)
		}
		if v.Strategy != "daily" || v.Slot != uint64(tue.Unix()) {
			t.Fatalf("verdict = %+v", v)
		}
	})

	t.Run("too-small gate: below min_commits records the exact reason", func(t *testing.T) {
		moved := Refs{{Name: "HEAD", Oid: oid2}, {Name: "refs/heads/main", Oid: oidFor("moved-main")}}
		wal.asOf[tue] = fold{refs: moved, seq: 70}
		prim := &fakePrim{counts: []int{3}}
		d := &Deps{Wal: wal, Prim: prim, St: newMemStore(t), Now: func() time.Time { return now.Add(2 * time.Hour) },
			RepoDir: "/repo", List: list, Verdicts: dVerdicts()}
		err := BuildSlot(t.Context(), d, "acme/repo", strategies, daily, tue)
		if err != nil {
			t.Fatalf("BuildSlot: %v", err)
		}
		if len(d.Verdicts) != 1 {
			t.Fatalf("verdicts = %d, want 1", len(d.Verdicts))
		}
		want := "too-small: 3 commits (min 25)"
		if d.Verdicts[0].Reason != want {
			t.Fatalf("reason = %q, want %q", d.Verdicts[0].Reason, want)
		}
		if d.Verdicts[0].BaseID != monDaily.ID {
			t.Fatalf("base_id = %q, want %q (Monday daily is the newest base link)", d.Verdicts[0].BaseID, monDaily.ID)
		}
		if d.Verdicts[0].AsOfSeq != 70 {
			t.Fatalf("as_of_seq = %d, want 70", d.Verdicts[0].AsOfSeq)
		}
	})

	t.Run("too-small is per-strategy overridable and >=0", func(t *testing.T) {
		s := *daily
		s.MinCommits = 2
		moved := Refs{{Name: "HEAD", Oid: oid2}, {Name: "refs/heads/main", Oid: oidFor("moved-main")}}
		wal.asOf[tue] = fold{refs: moved, seq: 70}
		prim := &fakePrim{counts: []int{3}}
		d := &Deps{Wal: wal, Prim: prim, St: newMemStore(t), Now: func() time.Time { return now.Add(2 * time.Hour) },
			RepoDir: "/repo", List: list, Verdicts: dVerdicts()}
		if err := BuildSlot(t.Context(), d, "acme/repo", strategies, &s, tue); err != nil {
			t.Fatalf("BuildSlot: %v", err)
		}
		if len(d.Verdicts) != 0 {
			t.Fatalf("3 commits ≥ min 2 must build, got verdicts %v", d.Verdicts)
		}
	})

	t.Run("no-state: unresolvable cut is a verdict with seq 0", func(t *testing.T) {
		broken := &fakeWal{asOf: map[time.Time]fold{}}
		d := &Deps{Wal: broken, Prim: &fakePrim{}, St: newMemStore(t),
			Now:     func() time.Time { return now.Add(2 * time.Hour) },
			RepoDir: "/repo", List: list, Verdicts: dVerdicts()}
		if err := BuildSlot(t.Context(), d, "acme/repo", strategies, daily, tue); err != nil {
			t.Fatalf("BuildSlot: %v", err)
		}
		if len(d.Verdicts) != 1 {
			t.Fatalf("verdicts = %d, want 1", len(d.Verdicts))
		}
		v := d.Verdicts[0]
		if v.Reason != "no state as of the slot" || v.AsOfSeq != 0 {
			t.Fatalf("verdict = %+v", v)
		}
	})

	t.Run("open slot within the 120 s grace is never recorded", func(t *testing.T) {
		moved := Refs{{Name: "HEAD", Oid: oid2}, {Name: "refs/heads/main", Oid: oidFor("moved-main")}}
		wal.asOf[tue] = fold{refs: moved, seq: 70}
		// now is 30 s after the slot → open.
		d := &Deps{Wal: wal, Prim: &fakePrim{counts: []int{0}}, St: newMemStore(t),
			Now:     func() time.Time { return tue.Add(30 * time.Second) },
			RepoDir: "/repo", List: list, Verdicts: dVerdicts()}
		if err := BuildSlot(t.Context(), d, "acme/repo", strategies, daily, tue); err != nil {
			t.Fatalf("BuildSlot: %v", err)
		}
		if len(d.Verdicts) != 0 {
			t.Fatalf("open slot recorded %v", d.Verdicts)
		}
	})

	t.Run("exactly 120 s is still open; 121 s is closed", func(t *testing.T) {
		moved := Refs{{Name: "HEAD", Oid: oid2}, {Name: "refs/heads/main", Oid: oidFor("moved-main")}}
		wal.asOf[tue] = fold{refs: moved, seq: 70}
		d := &Deps{Wal: wal, Prim: &fakePrim{counts: []int{0}}, St: newMemStore(t),
			Now:     func() time.Time { return tue.Add(120 * time.Second) },
			RepoDir: "/repo", List: list, Verdicts: dVerdicts()}
		if err := BuildSlot(t.Context(), d, "acme/repo", strategies, daily, tue); err != nil {
			t.Fatalf("BuildSlot: %v", err)
		}
		if len(d.Verdicts) != 0 {
			t.Fatalf("120 s slot must stay open, got %v", d.Verdicts)
		}
		d = &Deps{Wal: wal, Prim: &fakePrim{counts: []int{0}}, St: newMemStore(t),
			Now:     func() time.Time { return tue.Add(121 * time.Second) },
			RepoDir: "/repo", List: list, Verdicts: dVerdicts()}
		if err := BuildSlot(t.Context(), d, "acme/repo", strategies, daily, tue); err != nil {
			t.Fatalf("BuildSlot: %v", err)
		}
		if len(d.Verdicts) != 1 || !strings.HasPrefix(d.Verdicts[0].Reason, "too-small") {
			t.Fatalf("121 s slot must record too-small, got %+v", d.Verdicts)
		}
	})

	t.Run("fulls are never gated", func(t *testing.T) {
		// Same tips as the existing weekly + a CountCommits of 0: still builds.
		same := Refs{{Name: "HEAD", Oid: W.Tips[0].Oid}, {Name: "refs/heads/main", Oid: W.Tips[1].Oid}}
		sunAsOf := sun
		wal2 := &fakeWal{asOf: map[time.Time]fold{sunAsOf: {refs: same, seq: 99}}, noHead: true}
		prim := &fakePrim{counts: []int{0}}
		d := &Deps{Wal: wal2, Prim: prim, St: newMemStore(t),
			Now:     func() time.Time { return now.Add(2 * time.Hour) },
			RepoDir: "/repo", List: list, Verdicts: dVerdicts()}
		weekly := byName["weekly"]
		if err := BuildSlot(t.Context(), d, "acme/repo", strategies, weekly, sunAsOf); err != nil {
			t.Fatalf("BuildSlot: %v", err)
		}
		if len(d.Verdicts) != 0 {
			t.Fatalf("fulls must never be gated, got %v", d.Verdicts)
		}
		l, _, err := storeGetList(t, d.St)
		if err != nil {
			t.Fatalf("get list: %v", err)
		}
		if e := entryAt(l, "weekly", uint64(sun.Unix())); e == nil {
			t.Fatalf("weekly entry missing after build: %+v", l)
		}
	})

	t.Run("skipped slot produces no entry so the next slot re-picks the same base", func(t *testing.T) {
		// The too-small Tue slot produced no entry, so Wed's ownPrev is still
		// the Monday daily — the same base Tue would have cut on (§8.7:
		// nothing lost).
		listAfter := protoBundleList(W, monDaily)
		tueBase := BaseFor(daily, uint64(tue.Unix()), listAfter, byName)
		wed := uint64(at("2026-09-02T00:00:00Z").Unix())
		wedBase := BaseFor(daily, wed, listAfter, byName)
		if tueBase.ID != monDaily.ID || wedBase.ID != monDaily.ID {
			t.Fatalf("tue base %q / wed base %q, want both %q (same base re-picked)", tueBase.ID, wedBase.ID, monDaily.ID)
		}
	})
}

func dVerdicts() []*proto.SkippedSlot { return nil }
