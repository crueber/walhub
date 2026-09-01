// bundles.go — unit 3 (§5): planning + ordering only; the building lives in
// internal/bundle (04/07 docs). The maintainer's responsibilities: run the
// planner (retention + slot settlement), choose the oldest missing slot with
// strategies in config order, and fork to the BaseRebuild phase machine when
// a Full strategy slot is due on an eligible host.
package maintain

import (
	"context"
	"fmt"
	"time"
)

// runBundles is the priority-3 unit. Outcome detail carries the slot state
// token ("state=<s>") that feeds the §3.2 step 5 stale-skip cap.
func (m *Maintainer) runBundles(ctx context.Context, rep Repo, snap *Snapshot, t TaskLogger) (Outcome, string) {
	if m.opt.Planner == nil {
		return OutcomeError, "bundle planner not wired"
	}
	slots, err := m.opt.Planner.Plan(ctx, rep.ID(), snap.Eff, snap.Manifest, m.now())
	if err != nil {
		return OutcomeError, err.Error()
	}
	// Choose the oldest missing slot; strategies are already in config order
	// in the planner's table (§4 tie-break).
	var chosen *Slot
	for i := range slots {
		s := &slots[i]
		if s.State == "missing" && (chosen == nil || s.Slot < chosen.Slot) {
			chosen = s
		}
	}
	if chosen == nil {
		return OutcomeOK, "no missing slot"
	}

	// BaseRebuild fork (§5 step 4): a missing slot on a Full strategy on an
	// ssd host whose base is due (§6.2 triggers) yields BaseRebuild; the slot
	// composes from the new base afterwards (zero bytes through the host).
	if chosen.Kind == "full" && baseRebuildDue(snap.Manifest, m.baseWindow(snap, chosen.Strategy), func(at time.Time) uint64 {
		return m.headSeqAt(ctx, rep, at)
	}) {
		if snap.Eff.Maintenance.Disk != "ssd" {
			// §4.1: a tmpfs host never rebuilds bases — the due base slot is
			// planned but reported wrong-host (the slot stays missing until
			// someone eligible takes it: the intended pressure mechanism).
			return OutcomeWrongHost, fmt.Sprintf("slot=%d state=wrong-host base rebuild requires disk=ssd", chosen.Slot)
		}
		t.Notice(fmt.Sprintf("full slot %d due → base rebuild", chosen.Slot))
		return m.runRebuild(ctx, rep, snap, t)
	}

	built, err := m.opt.Planner.Build(ctx, rep.ID(), *chosen)
	if err != nil {
		return OutcomeError, err.Error()
	}
	if !built {
		// Built nothing (no slot settled, retention no-op) — the pass loop
		// re-plans so compaction/base triggers are not starved (§3.2 step 5).
		return OutcomeOK, fmt.Sprintf("strategy=%s slot=%d state=%s nothing built", chosen.Strategy, chosen.Slot, chosen.State)
	}
	return OutcomeOK, fmt.Sprintf("strategy=%s slot=%d state=built", chosen.Strategy, chosen.Slot)
}

// baseWindow returns the §6.2 trigger-4 window start: the previous fire time
// of the strategy's schedule, or the repo's first_state_at (checkpoint
// provenance) when the repo is younger than one slot.
func (m *Maintainer) baseWindow(snap *Snapshot, strategy string) time.Time {
	if m.opt.Planner != nil {
		for _, st := range snap.Eff.Bundles.Strategy {
			if st.Name == strategy {
				return m.opt.Planner.PreviousFire(st, m.now())
			}
		}
	}
	if snap.Manifest.Checkpoint != nil && snap.Manifest.Checkpoint.FirstStateAt != nil {
		return snap.Manifest.Checkpoint.FirstStateAt.Go()
	}
	return time.Time{}
}

// headSeqAt folds the WAL for the last committed seq at instant at (the §6.2
// bar math). Entries before the window (or a walk failure) yield bar 0 → the
// max(bar, 1) rule keeps an empty-at-slot-time repo rebuildable at seq 1.
func (m *Maintainer) headSeqAt(ctx context.Context, rep Repo, at time.Time) uint64 {
	if at.IsZero() {
		return 0
	}
	mst, _ := rep.Manifest()
	entries, err := rep.ReadLog(ctx, mst.MinSeq, mst.HeadSeq)
	if err != nil {
		return 0
	}
	bar := uint64(0)
	for _, e := range entries {
		if e.CreatedAt != nil && e.CreatedAt.Go().After(at) {
			break
		}
		if e.Seq > bar {
			bar = e.Seq
		}
	}
	return bar
}
