// heal.go — the mirror self-heal (issue #320): a degraded mirror is
// re-materialized automatically, with backoff, until it flips back to
// healthy — no operator action required.
//
// The trigger lives in the scheduled loop (round in sync.go): a mirror
// that is NOT due for a sync but carries a serve-health marker gets a
// heal fire when its backoff elapses. The heal re-drives exactly what a
// demand request would — Sync(LevelServe) with the patient probe budget
// plus the object check — deliberately WITHOUT tearing down cache-dir
// state: the diagnosed wedge is unbounded work (no deadline on the
// serve materialization), not corrupt local state (the present-check
// resume is sound and .tmp orphans are swept at body start), so
// deletion would be data-loss-adjacent without addressing the
// mechanism. A passing heal deletes the marker (healthy again); a
// failing heal refreshes it with attempts+1 (the backoff anchor).
//
// ### Concurrency
// Hazard: a heal racing a due sync (or a second instance's heal)
// drives the same handle concurrently.
// Avoidance: lock order is unchanged (Sync takes syncMu then packMu,
// Publish takes syncMu only — never inverted), the (repo,mirror-heal)
// single-flight joins overlapping heals, and every outcome is a
// whole-object marker overwrite (last writer wins) plus task narration.
// The heal never takes the sync lease (no clone, nothing to arbitrate)
// and never touches the mirror doc (sync outcomes stay the sync's
// story alone).
package mirror

import (
	"context"
	"fmt"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// KindMirrorHeal is the Seam 5 task kind for heal fires, registered
// once from composition next to KindMirrorSync (duplicate registration
// panics by the same contract).
const KindMirrorHeal = "mirror-heal"

// serveHealthSidebandTimeout bounds best-effort marker writes/clears
// issued on detached contexts (they must never stall a sync or heal).
const serveHealthSidebandTimeout = 10 * time.Second

// sidebandCtx is the detached bounded ctx for marker writes: the
// caller's ctx may already be expired (marks happen exactly then), and
// sideband state must never block serving or syncing.
func sidebandCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), serveHealthSidebandTimeout)
}

// markServeDegraded records a serve failure (sync probe or demand path
// via the engine): best-effort overwrite preserving heal backoff state.
func markServeDegraded(st store.ObjectStore, owner, name, reason string) {
	ctx, cancel := sidebandCtx()
	defer cancel()
	prev, _ := wal.LoadServeHealth(ctx, st, owner, name)
	if err := wal.WriteServeHealth(ctx, st, owner, name, reason, prev); err != nil {
		_ = err // best-effort sideband: narrated by the caller, never fatal
	}
}

// clearServeHealth deletes the marker after proof of servability
// (probe pass, heal pass, successful serve). Best-effort.
func clearServeHealth(st store.ObjectStore, owner, name string) {
	ctx, cancel := sidebandCtx()
	defer cancel()
	_ = wal.ClearServeHealth(ctx, st, owner, name)
}

// healDue reports whether a marked repo should fire now: never healed
// → immediately; otherwise the failure-backoff window since the last
// heal attempt must have elapsed (the BackoffDelay ladder, capped at
// 24h — retries are rate-bounded, never count-capped, so recovery
// never needs an operator).
func healDue(doc *wal.ServeHealth, now time.Time) bool {
	if doc == nil {
		return false
	}
	if doc.LastHealAt == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339, doc.LastHealAt)
	if err != nil {
		return true
	}
	return !now.Before(last.Add(BackoffDelay(doc.Attempts)))
}

// runHeal is the heal task body: open, patient probe, then clear or
// refresh the marker. It never touches the mirror doc (ConsecutiveFailures
// and LastResult stay the sync's story) and never returns a silent
// outcome — every path narrates on the task.
func (s *Service) runHeal(ctx context.Context, task *wal.Task, owner, name string) error {
	target := owner + "/" + name
	task.Notice(fmt.Sprintf("mirror heal %s: re-materializing serving copy", target))
	h, err := s.reg.Open(ctx, target)
	if err != nil {
		reason := "heal open: " + scrubText(wal.ShortErr(err))
		refreshHealMark(s.store, owner, name, reason)
		task.Notice(fmt.Sprintf("mirror heal %s failed: %s", target, reason))
		return fmt.Errorf("mirror heal: %s", reason)
	}
	oid := servingHeadOid(ctx, s, h)
	if perr := s.probe(ctx, h, oid); perr != nil {
		reason := "heal probe: " + scrubText(wal.ShortErr(perr))
		refreshHealMark(s.store, owner, name, reason)
		task.Notice(fmt.Sprintf("mirror heal %s failed: %s", target, reason))
		return fmt.Errorf("mirror heal: %s", reason)
	}
	clearServeHealth(s.store, owner, name)
	task.Notice(fmt.Sprintf("mirror heal %s: serving copy healthy", target))
	return nil
}

// servingHeadOid resolves the serving copy's HEAD tip for the heal's
// object check ("" when the copy has no refs — the probe then passes
// on the Sync alone).
func servingHeadOid(ctx context.Context, s *Service, h *wal.RepoHandle) string {
	tips, _, head, err := s.currentRefs(ctx, h)
	if err != nil {
		return ""
	}
	if head != "" {
		if oid := tips[head]; oid != "" && !isZeroHex(oid) {
			return oid
		}
	}
	for _, oid := range tips {
		if oid != "" && !isZeroHex(oid) {
			return oid
		}
	}
	return ""
}

// refreshHealMark records a failed heal: attempts+1 stamped at now,
// preserving the last failure reason when the caller has none.
func refreshHealMark(st store.ObjectStore, owner, name, reason string) {
	ctx, cancel := sidebandCtx()
	defer cancel()
	prev, _ := wal.LoadServeHealth(ctx, st, owner, name)
	_ = wal.WriteHealAttempt(ctx, st, owner, name, reason, prev)
}
