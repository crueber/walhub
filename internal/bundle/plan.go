package bundle

import (
	"fmt"
	"sort"
	"time"

	"git.packden.us/crueber/walhub/internal/store/proto"
)

// Plan windows (§8.10): what can be missing at all. Fulls — the `keep` newest
// fire slots ≤ now; chain incrementals — fire slots ≥ the newest base-strategy
// bundle's slot (includes the tie slot); non-chain — the 2 newest fire slots
// ≤ now. Slots before first_state_at are unavailable (filtered by callers with
// FirstStateAt).
func PlanWindows(strategies []Strategy, byName map[string]*Strategy, list *proto.BundleList, now time.Time) map[string][]uint64 {
	out := make(map[string][]uint64, len(strategies))
	for i := range strategies {
		s := &strategies[i]
		switch {
		case s.Kind == KindFull:
			keep := s.EffectiveKeep()
			slots := newestSlots(s, now, keep)
			out[s.Name] = slots
		case s.Chain:
			// Fire slots ≥ the newest base-strategy bundle's slot.
			var from uint64
			if base := newestBaseBundle(s, byName, list); base != nil {
				from = base.Slot
				out[s.Name] = slotsFrom(s, from, now)
			}
			// No base bundle yet → empty window; slots are "blocked".
		default:
			out[s.Name] = newestSlots(s, now, 2)
		}
	}
	return out
}

// newestSlots returns the n newest fire slots ≤ now (ascending).
func newestSlots(s *Strategy, now time.Time, n int) []uint64 {
	if n <= 0 {
		return nil
	}
	// Walk backwards in expanding lookback windows until n slots found.
	lookback := 24 * time.Hour
	for {
		start := now.Add(-lookback)
		fires, err := s.Schedule.Between(start, now)
		if err != nil {
			return nil
		}
		if len(fires) >= n {
			out := make([]uint64, 0, n)
			for _, f := range fires[len(fires)-n:] {
				out = append(out, uint64(f.Unix()))
			}
			return out
		}
		if lookback > 400*24*time.Hour {
			out := make([]uint64, 0, len(fires))
			for _, f := range fires {
				out = append(out, uint64(f.Unix()))
			}
			return out
		}
		lookback *= 4
	}
}

// slotsFrom returns the fire slots in [from, now] (ascending), from inclusive
// (includes the tie slot).
func slotsFrom(s *Strategy, from uint64, now time.Time) []uint64 {
	start := time.Unix(int64(from), 0).UTC()
	fires, err := s.Schedule.Between(start, now)
	if err != nil {
		return nil
	}
	out := make([]uint64, 0, len(fires))
	for _, f := range fires {
		out = append(out, uint64(f.Unix()))
	}
	return out
}

// newestBaseBundle is the newest built bundle of the strategy's nearest
// ancestor that has one (§8.6 walk).
func newestBaseBundle(s *Strategy, byName map[string]*Strategy, list *proto.BundleList) *proto.BundleEntry {
	name := s.Base
	for name != "" {
		b := byName[name]
		if b == nil {
			return nil
		}
		if e := NewestEntry(list, b.Name, uint64(1<<62), true); e != nil {
			return e
		}
		name = b.Base
	}
	return nil
}

// PlanSlot is one classified fire slot (§8.14 plan states).
type PlanSlot struct {
	Strategy string
	Slot     uint64 // epoch seconds = creationToken
	When     time.Time
	State    string // built|pending|missing|blocked|too-small|skipped|unavailable|wrong-host
	Detail   string // bundle id or verdict reason
}

// PlanStates classifies every slot in every strategy's window (§8.14).
func PlanStates(repo string, strategies []Strategy, list *proto.BundleList, now time.Time, firstStateAt func(string) (time.Time, bool), hostFits func(*Strategy) bool) []PlanSlot {
	byName := ByName(strategies)
	windows := PlanWindows(strategies, byName, list, now)
	var out []PlanSlot
	for i := range strategies {
		s := &strategies[i]
		for _, slot := range windows[s.Name] {
			when := time.Unix(int64(slot), 0).UTC()
			ps := PlanSlot{Strategy: s.Name, Slot: slot, When: when}
			if e := entryAt(list, s.Name, slot); e != nil {
				ps.State, ps.Detail = "built", e.ID
				out = append(out, ps)
				continue
			}
			if v := skippedAt(list, s.Name, slot); v != nil {
				if hasPrefix(v.Reason, "too-small") {
					ps.State, ps.Detail = "too-small", v.Reason
				} else {
					ps.State, ps.Detail = "skipped", v.Reason
				}
				out = append(out, ps)
				continue
			}
			if hostFits != nil && !hostFits(s) {
				ps.State = "wrong-host"
				out = append(out, ps)
				continue
			}
			if firstStateAt != nil {
				if first, ok := firstStateAt(repo); ok && when.Before(first) {
					ps.State = "unavailable"
					out = append(out, ps)
					continue
				}
			}
			// Future slot, or the open slot (within the 120 s close grace)
			// not yet due — re-measured each pass, never recorded (§8.7).
			if when.After(now) || now.Sub(when) <= CloseGrace {
				ps.State = "pending"
			} else if s.Kind == KindIncremental {
				if _, err := BaseIDFor(s, slot, list, byName); err != nil {
					ps.State = "blocked"
				} else {
					ps.State = "missing"
				}
			} else {
				ps.State = "missing"
			}
			out = append(out, ps)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Strategy != out[j].Strategy {
			return out[i].Strategy < out[j].Strategy
		}
		return out[i].Slot < out[j].Slot
	})
	return out
}

// CloseGrace is the 120 s close grace (§8.7): a slot whose as-of instant is
// more than 120 s in the past is closed; verdicts on closed slots are final.
const CloseGrace = 120 * time.Second

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

func skippedAt(list *proto.BundleList, strategy string, slot uint64) *proto.SkippedSlot {
	if list == nil {
		return nil
	}
	for _, v := range list.Skipped {
		if v.Strategy == strategy && v.Slot == slot {
			return v
		}
	}
	return nil
}

// BackfillPlan lists the missing slots to build this pass, oldest-first within
// a strategy, strategies in config order, ≤ backfill_max per strategy
// (§8.10). A deleted/corrupt bundle is "missing" and rebuilt identically.
func BackfillPlan(strategies []Strategy, list *proto.BundleList, now time.Time) []PlanSlot {
	byName := ByName(strategies)
	windows := PlanWindows(strategies, byName, list, now)
	var out []PlanSlot
	for i := range strategies {
		s := &strategies[i]
		n := 0
		for _, slot := range windows[s.Name] {
			if entryAt(list, s.Name, slot) != nil || skippedAt(list, s.Name, slot) != nil {
				continue
			}
			when := time.Unix(int64(slot), 0).UTC()
			if when.After(now) || now.Sub(when) <= CloseGrace {
				continue // pending/open slots are re-measured, not backfilled
			}
			if s.Kind == KindIncremental {
				if _, err := BaseIDFor(s, slot, list, byName); err != nil {
					continue // blocked: no base bundle
				}
			}
			out = append(out, PlanSlot{Strategy: s.Name, Slot: slot, When: when, State: "missing"})
			n++
			if s.BackfillMax > 0 && n >= s.BackfillMax {
				break // ≤ backfill_max per strategy per pass (§8.10)
			}
		}
	}
	return out
}

// pruneRetention removes entries beyond the retention set (§8.10): keep = fulls
// listed (keep, default 2) + the chain under every kept full (walk base_id
// links to the root full) + non-chain incrementals: the 2 newest per strategy
// whose base is kept. Returns the mutated list.
func pruneRetention(cur *proto.BundleList, strategies []Strategy, byName map[string]*Strategy) *proto.BundleList {
	if cur == nil {
		return nil
	}
	kept := make(map[string]bool, len(cur.Bundles)) // by entry ID
	// 1. Fulls listed.
	for i := range strategies {
		s := &strategies[i]
		if s.Kind != KindFull {
			continue
		}
		fullEntries := entriesOfStrategy(cur, s.Name)
		sort.Slice(fullEntries, func(a, b int) bool { return fullEntries[a].Slot > fullEntries[b].Slot })
		for k := 0; k < s.EffectiveKeep() && k < len(fullEntries); k++ {
			kept[fullEntries[k].ID] = true
		}
	}
	// 2. The chain under every kept full: CHAIN-strategy entries whose base_id
	// chain reaches a kept entry (transitive closure over BaseID links).
	// Non-chain incrementals are restricted to step 3 (2 newest).
	for changed := true; changed; {
		changed = false
		for _, e := range cur.Bundles {
			if kept[e.ID] || e.BaseID == "" {
				continue
			}
			s := byName[e.Strategy]
			if s == nil || s.Kind != KindIncremental || !s.Chain {
				continue
			}
			if kept[e.BaseID] {
				kept[e.ID] = true
				changed = true
			}
		}
	}
	// 3. Non-chain incrementals: the 2 newest per strategy whose base is kept.
	for i := range strategies {
		s := &strategies[i]
		if s.Kind != KindIncremental || s.Chain {
			continue
		}
		var cands []*proto.BundleEntry
		for _, e := range entriesOfStrategy(cur, s.Name) {
			if e.BaseID != "" && kept[e.BaseID] {
				cands = append(cands, e)
			}
		}
		sort.Slice(cands, func(a, b int) bool { return cands[a].Slot > cands[b].Slot })
		for k := 0; k < 2 && k < len(cands); k++ {
			kept[cands[k].ID] = true
		}
	}
	// Rebuild.
	out := &proto.BundleList{Mode: cur.Mode, Heuristic: cur.Heuristic, Skipped: cur.Skipped, UpdatedAt: cur.UpdatedAt}
	for _, e := range cur.Bundles {
		if kept[e.ID] {
			out.Bundles = append(out.Bundles, e)
		}
	}
	return out
}

// entriesOfStrategy returns the live entries of one strategy (unordered).
func entriesOfStrategy(list *proto.BundleList, strategy string) []*proto.BundleEntry {
	var out []*proto.BundleEntry
	if list == nil {
		return out
	}
	for _, e := range list.Bundles {
		if e.Strategy == strategy {
			out = append(out, e)
		}
	}
	return out
}

// entriesOfStrategySorted returns the live entries of one strategy ascending slot.
func entriesOfStrategySorted(list *proto.BundleList, strategy string) []*proto.BundleEntry {
	out := entriesOfStrategy(list, strategy)
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out
}

// FormatSlot renders the object-key slot text (§8.9.6: 20060102T150405Z).
func FormatSlot(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// EntryID is the stable entry id (§8.11): "<strategy>/<slotRFC3339Z>".
func EntryID(strategy string, slot time.Time) string {
	return fmt.Sprintf("%s/%s", strategy, slot.UTC().Format(time.RFC3339))
}

// CloneEntries is the kept fulls + the chain (for clones): every entry plus
// every incremental reachable through base_id links (§8.11). Orphaned
// incrementals whose base entry is gone are dropped.
func CloneEntries(list *proto.BundleList) []*proto.BundleEntry {
	return nonOrphaned(list, func(e *proto.BundleEntry) bool { return true })
}

// CatchupEntries is the same list without fulls (§8.11: every recipe records
// the catchup URL in fetch.bundleURI so a fetching client never re-pulls the
// new full).
func CatchupEntries(list *proto.BundleList) []*proto.BundleEntry {
	return nonOrphaned(list, func(e *proto.BundleEntry) bool { return e.Kind != KindFull })
}

// nonOrphaned selects the entries passing keep, dropping incrementals whose
// base entry is absent (orphans), ascending creationToken.
func nonOrphaned(list *proto.BundleList, keep func(*proto.BundleEntry) bool) []*proto.BundleEntry {
	if list == nil {
		return nil
	}
	ids := make(map[string]bool, len(list.Bundles))
	for _, e := range list.Bundles {
		ids[e.ID] = true
	}
	var out []*proto.BundleEntry
	for _, e := range list.Bundles {
		if !keep(e) {
			continue
		}
		if e.Kind == KindIncremental && e.BaseID != "" && !ids[e.BaseID] {
			continue // orphaned incremental (§8.11)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreationToken < out[j].CreationToken })
	return out
}

// FamilyFilter filters entries to one filter family (§8.11: families never
// mix on one list). filter must be "" or FilterBlobNone; anything else is an
// error (HTTP 400 at the serving seam).
func FamilyFilter(list *proto.BundleList, filter string) ([]*proto.BundleEntry, error) {
	if filter != "" && filter != FilterBlobNone {
		return nil, fmt.Errorf("bundle: unknown filter %q (supported: %q)", filter, FilterBlobNone)
	}
	var out []*proto.BundleEntry
	for _, e := range list.Bundles {
		if e.Filter == filter {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreationToken < out[j].CreationToken })
	return out, nil
}
