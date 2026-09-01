package bundle

import (
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// TestPlanWindows pins §8.10 plan windows: fulls = keep newest slots ≤ now;
// chain incrementals = fire slots ≥ the newest base-strategy bundle's slot
// (includes the tie slot); non-chain = 2 newest fire slots ≤ now.
func TestPlanWindows(t *testing.T) {
	now := at("2026-09-01T12:00:00Z")
	strategies := DefaultStrategies()
	byName := ByName(strategies)

	sun := uint64(at("2026-08-30T00:00:00Z").Unix())
	W := goldEntry("weekly/2026-08-30T00:00:00Z", "weekly", KindFull, sun, "")
	W.Key = "bundles/weekly/a.bundle"
	list := protoBundleList(W)

	windows := PlanWindows(strategies, byName, list, now)

	// Weekly (keep 2): the 2 newest Sunday slots ≤ now.
	if got := windows["weekly"]; len(got) != 2 || got[1] != sun {
		t.Fatalf("weekly window = %v, want the 2 newest Sundays ending %d", got, sun)
	}

	// Daily (chain): fire slots ≥ the weekly's slot — includes the tie.
	if got := windows["daily"]; len(got) == 0 || got[0] != sun {
		t.Fatalf("daily window must start at the weekly's slot (tie included), got %v", got)
	} else if last := got[len(got)-1]; last != uint64(at("2026-09-01T00:00:00Z").Unix()) {
		t.Fatalf("daily window must end at today's fire slot, got last=%d", last)
	}

	// Hourly (non-chain): the 2 newest hourly slots ≤ now.
	if got := windows["hourly"]; len(got) != 2 || got[1] != uint64(at("2026-09-01T12:00:00Z").Unix()) {
		t.Fatalf("hourly window = %v, want the 2 newest hours ending 12:00", got)
	}

	// No base bundle → chain window empty (all slots blocked).
	empty := PlanWindows(strategies, byName, protoBundleList(), now)
	if got := empty["daily"]; len(got) != 0 {
		t.Fatalf("daily window without a base = %v, want empty (blocked)", got)
	}
}

// TestPlanStates pins §8.14 classifications on a small timeline.
func TestPlanStates(t *testing.T) {
	now := at("2026-09-01T12:00:00Z")
	strategies := DefaultStrategies()
	sun := uint64(at("2026-08-30T00:00:00Z").Unix())
	mon := uint64(at("2026-08-31T00:00:00Z").Unix())
	tue := uint64(at("2026-09-01T00:00:00Z").Unix())

	W := goldEntry("weekly/2026-08-30T00:00:00Z", "weekly", KindFull, sun, "")
	list := protoBundleList(W)
	// A closed too-small verdict on the Monday daily; no verdict for Tuesday.
	list.Skipped = append(list.Skipped, &proto.SkippedSlot{
		Strategy: "daily", Slot: mon, BaseID: W.ID, AsOfSeq: 70,
		Reason: "too-small: 3 commits (min 25)",
	})

	states := PlanStates("acme/repo", strategies, list, now, nil, nil)
	byState := map[string]PlanSlot{}
	for _, ps := range states {
		if ps.Strategy == "daily" {
			byState[ps.Detail] = ps
		}
	}
	_ = byState

	got := map[uint64]string{}
	for _, ps := range states {
		if ps.Strategy == "daily" {
			got[ps.Slot] = ps.State
		}
	}
	if got[mon] != "too-small" {
		t.Fatalf("Monday daily state = %q, want too-small", got[mon])
	}
	if got[sun] != "missing" {
		t.Fatalf("Sunday tie slot = %q, want missing (in window, no entry, no verdict)", got[sun])
	}
	if got[tue] != "missing" {
		t.Fatalf("Tuesday slot = %q, want missing", got[tue])
	}
	// The built weekly reports built with its id as detail.
	for _, ps := range states {
		if ps.Strategy == "weekly" && ps.Slot == sun {
			if ps.State != "built" || ps.Detail != W.ID {
				t.Fatalf("weekly slot state = %q detail %q, want built/%s", ps.State, ps.Detail, W.ID)
			}
		}
	}
}

// TestBackfillOrder pins §8.10 backfill: missing slots oldest-first within a
// strategy, strategies in config order, ≤ backfill_max per strategy.
func TestBackfillOrder(t *testing.T) {
	now := at("2026-09-01T12:00:00Z")
	strategies := DefaultStrategies() // weekly bf 1, daily bf 7, hourly bf 48
	sun := uint64(at("2026-08-30T00:00:00Z").Unix())
	prevSun := uint64(at("2026-08-23T00:00:00Z").Unix())

	W := goldEntry("weekly/2026-08-30T00:00:00Z", "weekly", KindFull, sun, "")
	list := protoBundleList(W)

	plan := BackfillPlan(strategies, list, now)
	// Weekly: the older missing Sunday (Aug 23) is the only backfill candidate
	// (keep 2 = Aug 30 + Aug 23; Aug 30 has an entry).
	var weeklyCands []uint64
	var dailyCands []uint64
	for _, p := range plan {
		switch p.Strategy {
		case "weekly":
			weeklyCands = append(weeklyCands, p.Slot)
		case "daily":
			dailyCands = append(dailyCands, p.Slot)
		}
	}
	if len(weeklyCands) != 1 || weeklyCands[0] != prevSun {
		t.Fatalf("weekly backfill = %v, want [prevSunday]", weeklyCands)
	}
	// Daily: oldest-first ascending.
	for i := 1; i < len(dailyCands); i++ {
		if dailyCands[i] <= dailyCands[i-1] {
			t.Fatalf("daily backfill not oldest-first: %v", dailyCands)
		}
	}
	if len(dailyCands) == 0 || dailyCands[0] != sun {
		t.Fatalf("daily backfill must start at the tie slot %d, got %v", sun, dailyCands)
	}

	// backfill_max bounds per strategy per pass.
	strategies[1].BackfillMax = 2
	plan = BackfillPlan(strategies, list, now)
	n := 0
	for _, p := range plan {
		if p.Strategy == "daily" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("daily backfill = %d slots, want 2 (backfill_max)", n)
	}

	// backfill_max 0 = unlimited: the non-chain hourly window holds at most
	// the 2 newest slots (§8.10 plan windows), and with BackfillMax 0 both are
	// candidates — the cap is the window, not a per-pass limit.
	strategies[2].BackfillMax = 0
	plan = BackfillPlan(strategies, list, now)
	hourly := 0
	for _, p := range plan {
		if p.Strategy == "hourly" {
			hourly++
		}
	}
	// The 12:00 slot is the OPEN slot (within the 120 s grace) → never a
	// backfill candidate; the closed 11:00 slot is.
	strategies[2].BackfillMax = 1
	plan = BackfillPlan(strategies, list, now)
	var hourlySlots []uint64
	for _, p := range plan {
		if p.Strategy == "hourly" {
			hourlySlots = append(hourlySlots, p.Slot)
		}
	}
	if len(hourlySlots) != 1 || hourlySlots[0] != uint64(at("2026-09-01T11:00:00Z").Unix()) {
		t.Fatalf("hourly backfill = %v, want exactly the closed 11:00 slot", hourlySlots)
	}
}

// TestRetention pins §8.10 retention: keep = fulls listed (keep, default 2) +
// the chain under every kept full + non-chain incrementals (2 newest whose
// base is kept); everything else pruned with its keys reported for deletion.
func TestRetention(t *testing.T) {
	st := newMemStore(t)
	now := at("2026-09-01T12:00:00Z")
	strategies := DefaultStrategies()

	keyed := func(id, strategy, kind string, token uint64) *proto.BundleEntry {
		x := goldEntry(id, strategy, kind, token, "")
		x.Key = "bundles/" + strategy + "/" + FormatSlot(time.Unix(int64(token), 0).UTC()) + "-" + id + ".bundle"
		return x
	}
	w1 := keyed("weekly/2026-08-09T00:00:00Z", "weekly", KindFull, 1786233600) // Aug 9
	w2 := keyed("weekly/2026-08-16T00:00:00Z", "weekly", KindFull, 1786838400) // Aug 16
	w3 := keyed("weekly/2026-08-23T00:00:00Z", "weekly", KindFull, 1787443200) // Aug 23
	w4 := keyed("weekly/2026-08-30T00:00:00Z", "weekly", KindFull, 1788048000) // Aug 30 (newest)
	d1 := keyed("daily/2026-08-31T00:00:00Z", "daily", KindIncremental, 1788134400)
	d1.BaseID = w4.ID
	d2 := keyed("daily/2026-09-01T00:00:00Z", "daily", KindIncremental, 1788220800)
	d2.BaseID = d1.ID
	// An incremental under a PRUNED weekly → orphaned.
	d3 := keyed("daily/2026-08-10T00:00:00Z", "daily", KindIncremental, 1786320000)
	d3.BaseID = w1.ID
	// A non-chain incremental (hourly, chain=false) whose base is kept → keep
	// 2 newest; older ones pruned.
	h1 := keyed("hourly/2026-09-01T10:00:00Z", "hourly", KindIncremental, 1788256800)
	h1.BaseID = d2.ID
	h2 := keyed("hourly/2026-09-01T11:00:00Z", "hourly", KindIncremental, 1788260400)
	h2.BaseID = d2.ID
	h3 := keyed("hourly/2026-09-01T12:00:00Z", "hourly", KindIncremental, 1788264000)
	h3.BaseID = d2.ID

	list := protoBundleList(w1, w2, w3, w4, d1, d2, d3, h1, h2, h3)
	if err := UpsertEntriesForTest(t, st, list); err != nil {
		t.Fatal(err)
	}

	keys, err := SettleAndPrune(t.Context(), st, strategies, now)
	if err != nil {
		t.Fatalf("SettleAndPrune: %v", err)
	}

	got, _, err := storeGetList(t, st)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, x := range got.Bundles {
		kept[x.ID] = true
	}
	// Fulls: newest keep=2 (w4, w3); w1/w2 pruned.
	if !kept[w4.ID] || !kept[w3.ID] || kept[w2.ID] || kept[w1.ID] {
		t.Fatalf("full retention wrong: kept w3=%v w4=%v w1=%v w2=%v", kept[w3.ID], kept[w4.ID], kept[w1.ID], kept[w2.ID])
	}
	// The chain under kept fulls: d1 (base w4) and d2 (base d1) survive; d3
	// (base w1, pruned) is orphaned out.
	if !kept[d1.ID] || !kept[d2.ID] || kept[d3.ID] {
		t.Fatalf("chain retention wrong: d1=%v d2=%v d3=%v", kept[d1.ID], kept[d2.ID], kept[d3.ID])
	}
	// Non-chain hourly: 2 newest whose base is kept → h3, h2.
	if !kept[h3.ID] || !kept[h2.ID] || kept[h1.ID] {
		t.Fatalf("non-chain retention wrong: h3=%v h2=%v h1=%v", kept[h3.ID], kept[h2.ID], kept[h1.ID])
	}
	// Keys reported for deletion are exactly the pruned entries' keys.
	wantKeys := map[string]bool{w1.Key: true, w2.Key: true, d3.Key: true, h1.Key: true}
	if len(keys) != len(wantKeys) {
		t.Fatalf("removed keys = %v, want %v", keys, wantKeys)
	}
	for _, k := range keys {
		if !wantKeys[k] {
			t.Fatalf("unexpected removal %q (want subset of %v)", k, wantKeys)
		}
	}
}

// UpsertEntriesForTest persists a fixture list entry by entry.
func UpsertEntriesForTest(t *testing.T, st store.ObjectStore, list *proto.BundleList) error {
	t.Helper()
	for _, e := range list.Bundles {
		if err := UpsertEntry(t.Context(), st, e); err != nil {
			return err
		}
	}
	return nil
}
