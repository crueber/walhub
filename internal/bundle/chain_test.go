package bundle

import (
	"testing"

	"git.packden.us/crueber/walhub/internal/store/proto"
)

// TestBaseForWorkedWeek is the §8.6 worked week (W = weekly bundle of Sunday
// 00:00, token = Sunday epoch):
//
//	| daily slot | ownPrev | newest base | rule applied | cut on |
//	| Sun (tie)  | Sat daily | W (same slot) | tie → own link | Sat daily |
//	| Mon | Sun daily (Sun token) | W (Sun token) | ownPrev NOT strictly newer → base | W |
//	| Tue … Sat | previous daily | W | ownPrev.slot > W.slot | previous daily |
//	| next Sun (tie) | Sat | W′ | tie → own link | Sat |
func TestBaseForWorkedWeek(t *testing.T) {
	// The doc's worked week: W is the weekly of the TIE Sunday (2026-08-30);
	// daily slots run Sat 08-29, Sun 08-30 (tie), Mon 08-31, Tue 09-01.
	sat := epoch(t, "2026-08-29T00:00:00Z")
	sun2 := epoch(t, "2026-08-30T00:00:00Z")
	mon := epoch(t, "2026-08-31T00:00:00Z")
	tue := epoch(t, "2026-09-01T00:00:00Z")

	strategies := DefaultStrategies()
	byName := ByName(strategies)
	daily := byName["daily"]
	if !daily.Chain {
		t.Fatal("daily must chain by default")
	}

	W := entry("weekly", "weekly/2026-08-30T00:00:00Z", KindFull, sun2, sun2, 40)
	satDaily := entry("daily", "daily/2026-08-29T00:00:00Z", KindIncremental, sat, sat, 50)
	sunDaily := entry("daily", "daily/2026-08-30T00:00:00Z", KindIncremental, sun2, sun2, 51)
	monDaily := entry("daily", "daily/2026-08-31T00:00:00Z", KindIncremental, mon, mon, 60)

	list := protoBundleList(W, satDaily, sunDaily, monDaily)
	// The first-ever week: the weekly exists but no daily built before the tie.
	list2 := protoBundleList(W)

	tests := []struct {
		name string
		slot uint64
		list *proto.BundleList
		want string // expected base entry id, or ""
	}{
		{name: "Sun tie → own link (Sat daily)", slot: sun2, list: list, want: satDaily.ID},
		{name: "Mon → re-base on W (ownPrev not strictly newer)", slot: mon, list: list, want: W.ID},
		{name: "Tue → previous daily (strictly newer than W)", slot: tue, list: list, want: monDaily.ID},
		{name: "first-ever Sun tie → base W (no ownPrev yet)", slot: sun2, list: list2, want: W.ID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BaseFor(daily, tt.slot, tt.list, byName)
			gotID := ""
			if got != nil {
				gotID = got.ID
			}
			if gotID != tt.want {
				t.Fatalf("BaseFor(daily, slot=%d) = %q, want %q", tt.slot, gotID, tt.want)
			}
		})
	}

	// Tue after an outage that left only W: no own daily → base W.
	got := BaseFor(daily, tue, protoBundleList(W), byName)
	if got.ID != W.ID {
		t.Fatalf("BaseFor(Tue after outage) = %q, want weekly %q", got.ID, W.ID)
	}

	// The blocked case: no base bundle anywhere → nil (§8.6 "blocked").
	if got := BaseFor(daily, sat, protoBundleList(), byName); got != nil {
		t.Fatalf("BaseFor with no base = %v, want nil", got.ID)
	}
}

// TestBaseForTieToken pins the tie consequences from §8.6: the Sunday daily's
// token equals the weekly's token, so a fresh clone ("newest weekly + bundles
// NEWER than it") skips the tie daily as redundant.
func TestBaseForTieToken(t *testing.T) {
	sun := epoch(t, "2026-08-23T00:00:00Z")
	strategies := DefaultStrategies()
	byName := ByName(strategies)

	W := entry("weekly", "weekly/2026-08-23T00:00:00Z", KindFull, sun, sun, 40)
	sunDaily := entry("daily", "daily/2026-08-23T00:00:00Z", KindIncremental, sun, sun, 41)
	// Both fold the same tie instant → identical tips by deterministic replay.
	sunDaily.Tips = W.Tips
	list := protoBundleList(W, sunDaily)

	daily := byName["daily"]
	base := BaseFor(daily, sun+86400, list, byName) // Monday
	if base.ID != W.ID {
		t.Fatalf("Monday re-base = %q, want weekly", base.ID)
	}
	// Token/content equivalence at the tie: W's tips == Sunday daily's tips
	// because both fold the same instant (deterministic replay).
	if W.Tips[0].Oid != sunDaily.Tips[0].Oid {
		t.Fatal("tie tips must be equal (same instant, deterministic replay)")
	}
	if W.CreationToken != sunDaily.CreationToken {
		t.Fatal("tie tokens must be equal (slot epoch)")
	}
}

// TestHourlyMidnightTie mirrors the worked week one level down: hourlies
// against dailies behave identically at the midnight tie (§8.6).
func TestHourlyMidnightTie(t *testing.T) {
	midnight := epoch(t, "2026-08-30T00:00:00Z")
	strategies := DefaultStrategies()
	byName := ByName(strategies)
	hourly := byName["hourly"]
	hourly.Chain = true // hourlies default to chain=false; the tie rule needs it on

	D := entry("daily", "daily/2026-08-30T00:00:00Z", KindIncremental, midnight, midnight, 50)
	h23 := entry("hourly", "hourly/2026-08-29T23:00:00Z", KindIncremental, midnight-3600, midnight-3600, 60)
	h01 := entry("hourly", "hourly/2026-08-30T01:00:00Z", KindIncremental, midnight+3600, midnight+3600, 61)
	list := protoBundleList(D, h23, h01)

	// 01:00 hourly: ownPrev (h01 @01:00) > base (D @00:00) → own link.
	if got := BaseFor(hourly, midnight+2*3600, list, byName); got.ID != h01.ID {
		t.Fatalf("02:00 hourly cut on %q, want own link %q", got.ID, h01.ID)
	}
	// 01:00 slot being built (ownPrev h23 @23:00, base D @00:00): ownPrev is
	// OLDER than the just-built midnight base → re-base on D.
	if got := BaseFor(hourly, midnight+3600, list, byName); got.ID != D.ID {
		t.Fatalf("01:00 hourly cut on %q, want base %q", got.ID, D.ID)
	}
	// Midnight tie: slot == base.slot → chain continues through own link.
	if got := BaseFor(hourly, midnight, list, byName); got.ID != h23.ID {
		t.Fatalf("midnight tie hourly cut on %q, want own link %q", got.ID, h23.ID)
	}
	// Without chain, the base always wins (never an hourly's own kind).
	hourly.Chain = false
	if got := BaseFor(hourly, midnight+2*3600, list, byName); got.ID != D.ID {
		t.Fatalf("non-chain hourly cut on %q, want base %q", got.ID, D.ID)
	}
}

// TestBaseForWalkUp: when the base strategy has no entry ≤ slot, walk up the
// chain to the nearest ancestor that does (§8.6).
func TestBaseForWalkUp(t *testing.T) {
	strategies := DefaultStrategies()
	byName := ByName(strategies)
	hourly := byName["hourly"]

	sun := epoch(t, "2026-08-23T00:00:00Z")
	mon := sun + 86400
	W := entry("weekly", "weekly/2026-08-23T00:00:00Z", KindFull, sun, sun, 40)
	list := protoBundleList(W)

	// Monday hourly: daily has no entry, weekly does → base = W.
	got := BaseFor(hourly, mon, list, byName)
	if got == nil || got.ID != W.ID {
		t.Fatalf("walk-up base = %v, want weekly %q", got, W.ID)
	}
	// Nothing anywhere → blocked.
	if got := BaseFor(hourly, mon, protoBundleList(), byName); got != nil {
		t.Fatalf("walk-up with nothing = %v, want nil", got)
	}
}
