// checkpoint.go — unit 1 (§11): evaluate the §6.5 triggers; when fired, call
// internal/wal's checkpoint writer (refs-level, idempotent:
// cp.seq == head_seq returns the existing checkpoint). Provenance chain is
// the WAL engine's job — the unit only decides WHEN.
package maintain

import (
	"context"
	"fmt"
)

// runCheckpoint is the priority-1 unit. Selection evaluated the trigger
// already (plan.go checkpointTrigger); the reason names it.
func (m *Maintainer) runCheckpoint(ctx context.Context, rep Repo, snap *Snapshot, t TaskLogger, trigger string) (Outcome, string) {
	// Lag gauges (§11/§12): entries + age behind the last checkpoint.
	cpSeq := uint64(0)
	var cpAgeSecs int64
	if snap.Manifest.Checkpoint != nil {
		cpSeq = snap.Manifest.Checkpoint.Seq
		if snap.Manifest.Checkpoint.CreatedAt != nil {
			cpAgeSecs = int64(m.now().Sub(snap.Manifest.Checkpoint.CreatedAt.Go()).Seconds())
		}
	}
	m.metrics.checkpointLagEntries.Store(int64(snap.Manifest.HeadSeq - cpSeq))
	m.metrics.checkpointAgeSecs.Store(cpAgeSecs)

	// The Repo seam carries the exported wal checkpoint writer (bind_wal.go:
	// RepoHandle.WriteCheckpoint — the §5.5 two-object write + CAS trim).
	if err := rep.WriteCheckpoint(ctx, trigger); err != nil {
		return OutcomeError, err.Error()
	}
	t.Notice(fmt.Sprintf("checkpoint fired (%s) at seq %d", trigger, snap.Manifest.HeadSeq))
	return OutcomeOK, trigger
}
