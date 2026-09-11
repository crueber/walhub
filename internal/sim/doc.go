// Package sim is the simulation tier (docs/go/15_testing.md §4, from
// MASTER_RUST_SPEC.md §17): fault-injection over one truth store, with one
// FaultStore per instance link, proving safety ("exactly one winner per
// competing transaction, no lost commits") and liveness ("the core converges
// after faults heal") plus the §4.8 round-trip budgets counted at the
// transport layer via FaultStore Stats.Ops.
//
// # What this package covers vs what 15_testing.md §4 specifies
//
// Landed here (each a TestSim_ test, all skipped under -short so `make test`
// stays under a minute; `make sim` runs them with -timeout 15m):
//
//   - TestSim_SafetyThenLiveness — N pushers x M pushes under Chaos faults,
//     then heal and converge (§4 row 1).
//   - TestSim_HealthyRequestRoundTripBudgets — the §4.1/§4.8 budgets (§4 row 11).
//   - TestSim_LivenessUnderRandomSeeds — seed loop over the safety core
//     (WALHUB_SIM_SEED / WALHUB_SIM_SEEDS override; §4 row 12).
//   - TestSim_ConcurrentPushersExactlyOneWinner (§4 row 5).
//   - TestSim_OrphanedLogSegmentDoesNotBlockWriters (§4 row 2).
//   - TestSim_AfterALostCASResponse (§4 row 3).
//   - TestSim_StaleInstanceCannotStarveTheCore (§4 row 4).
//   - TestSim_BlackHoledInstanceIsInvisibleToTheCore (§4 row 10).
//   - TestSim_CheckpointWriterCrashIsInvisibleAndRepaired (§4 row 9) with one
//     documented nuance (gap G2 below).
//   - TestSim_ReaderWriterReadGuardDuringCompaction (§4 row 6): the try-write
//     rule pinned against the real rw.TryRWMutex plus the dual-direction
//     liveness proof (a held read guard never blocks a writer).
//   - TestSim_DrainInterruptsRunningUnit (§4 row 8): drain cancels the running
//     unit with the documented 503 and refuses new starts; the SIGTERM wiring
//     itself is e2e territory (gap G3 below).
//
// Specified in §4 but NOT landed (documented gaps, each with a reason; §4 and
// its Decisions section were amended in the same change that landed this
// package, per AGENTS.md law 12):
//
//   - G1 TestSim_BaseRebuildResumesAfterKillBetweenAnyTwoPhases: killing the
//     rebuild between two phases needs an injection hook in the maintain phase
//     machine that does not exist; resume is unit-covered in
//     internal/maintain (rebuild_test.go). Wiring a kill hook is maintain
//     work, not sim-harness work.
//   - G2 Checkpoint retry at the SAME head after a round-1 crash returns the
//     round-1 Create-412 error instead of succeeding: the checkpoint objects
//     are create-only keyed by seq, so a resumed writer re-Creates keys that
//     already exist. The crash is still invisible (manifest untouched, writers
//     proceed) and repaired by progress (the next head checkpoints cleanly) —
//     that is what TestSim_CheckpointWriterCrashIsInvisibleAndRepaired
//     asserts, including a hard pin on the same-head 412 (if the wal layer
//     ever learns same-seq idempotence, this gap and that assertion go away
//     together). True same-seq idempotence is wal work (doc 05 §5.5), not sim
//     work.
//   - G3 SIGTERM-to-drain wiring: the phase-1 drain hook is what the sim can
//     drive (TaskTable.Drain, unit-covered in internal/wal); the signal
//     delivery path belongs to the e2e tier (real process, real signal).
//
// # Budget reconciliation (AGENTS.md law 6 vs 15_testing.md §4.1)
//
// Law 6 says "push ≤ 5 requests"; §4.1 asserts `ops ≤ 6 (5 if already
// synced)". Both are right: the Rust §4.8 model counts freshness GET → (pack
// PUTs ∥ log PUT) → manifest CAS = 5 total (4 synced). The Go publish path
// adds exactly one op on top — the Forgejo #248 meta/stats.json sidecar PUT,
// which runs IN PARALLEL with the manifest CAS (+1 total op, +0 sequential
// trips, R1 B1). So the asserted bound is Rust 5/4 + 1 sidecar = 6/5, with
// sequential depth unchanged. The with-pack budget case pins it exactly:
// freshness + pack PUT + idx PUT + log PUT + sidecar PUT + manifest CAS = 6
// (5 synced). Warm refs ≤ 1, cold refs (one tail) ≤ 2, checkpoint ≤ 4 are
// unchanged from the Rust model and asserted as specified.
//
// # Concurrency
//
// Hazard: FaultStore Hang/BlackHole leaks a goroutine per op that never
// returns; an injected panic unwinds through wal locks (notably syncMu, which
// stays locked); background publisher/materialize goroutines outlive the call
// that spawned them; concurrent pushes batch in the group-commit window.
// Avoidance (13_concurrency.md playbook): every FaultStore op takes a ctx —
// black-holed instances are abandoned via Instance.Restart (their goroutines
// die with the registry ctx), never waited on; panics are recovered ONLY at
// the instance boundary (Instance.Publish/Sync/Checkpoint) and a crashed
// handle is dropped, never reused; the test binary's -timeout 15m is the
// outer watchdog; the shared memory truth store serializes under one mutex
// and sim load stays modest (≤ 4 instances) so -race stays fast. Lock order
// inside the harness: none — the harness holds no locks of its own; each
// Instance is driven by its test goroutine (pushers get one goroutine per
// instance, joined explicitly).
package sim
