// liveness_test.go — the liveness scenarios (15_testing.md §4 rows 2, 3, 4,
// 6, 8, 9, 10): crash/partition injections, then heal and converge.
package sim

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/fault"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
	"git.packden.us/crueber/walhub/internal/wal/rw"
)

func TestSim_OrphanedLogSegmentDoesNotBlockWriters(t *testing.T) {
	if testing.Short() {
		t.Skip("sim tier: run with make sim")
	}
	c := newCluster(t, defaultRepo)
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Minute)
	defer cancel()

	a := c.AddInstance("a", 31)
	zero := git.Sha1.ZeroHex()
	oid := zero
	for k := 0; k < 4; k++ {
		next := oidFor(31, 0, k)
		if _, err := a.PushRef(ctx, "refs/heads/main", oid, next); err != nil {
			c.failf("setup push %d: %v", k, err)
		}
		oid = next
	}

	// Crash between the log-segment PUT and the manifest CAS: the CAS Put
	// panics (recovered inside the publisher as a batch error — the manifest
	// never moves, so the crash is invisible). The recovered failure path
	// sweeps what it burned, so no durable orphan is asserted here — only
	// that truth did not move and lists nothing absent.
	a.link.Set(fault.Plan{PanicOnceKeys: []string{"put:manifest.pb"}})
	res, err, crashed := a.Publish(ctx, wal.PublishRequest{
		Txn: refTxn("refs/heads/main", oid, oidFor(31, 0, 4)), Synced: true,
	})
	if crashed || err == nil || res.Seq != 0 {
		c.failf("crashed push: res=%+v err=%v crashed=%v (want batch error, no crash, no seq)", res, err, crashed)
	}
	if m, merr := c.TruthManifest(); merr != nil || m.HeadSeq != 4 || len(verifySegments(m)) != 0 {
		c.failf("truth moved under a crashed push: m=%+v problems=%v err=%v", m, verifySegments(m), merr)
	}

	// The crashed batch burned nothing (its slot was fresh), so failure-path
	// GC leaves its log segment behind: a durable orphan, exactly as a true
	// process-death orphan. The next writer must burn past its seq (seq 5
	// gone, commit at 6), sweep it on commit, and converge — writers never
	// block on it.
	orphanKey := "repos/" + defaultRepo + "/" + store.LogSegmentKey(5)
	if ok, herr := store.Exists(ctx, c.truth, orphanKey); herr != nil || !ok {
		c.failf("crashed push left no orphan (exists=%v err=%v)", ok, herr)
	}
	a.Heal()
	oid5 := oidFor(31, 0, 5)
	seq, err := a.PushRef(ctx, "refs/heads/main", oid, oid5)
	if err != nil {
		c.failf("post-orphan push: %v", err)
	}
	if seq != 6 {
		c.failf("post-orphan push committed at seq %d (want 6: orphan seq 5 burned)", seq)
	}
	if ok, herr := store.Exists(ctx, c.truth, orphanKey); herr != nil || ok {
		c.failf("orphan not swept (exists=%v err=%v)", ok, herr)
	}

	// A Sync-path panic is a synthetic process death: recover at the
	// instance boundary, then restart (the old handle is dropped, never
	// reused) and converge.
	a.link.Set(fault.Plan{PanicOnceKeys: []string{"get:manifest.pb"}})
	if _, _, crashed := a.Sync(ctx, wal.LevelRefs); !crashed {
		c.failf("injected Sync panic did not surface as a crash")
	}
	// Heal first (uniform rule: never reboot onto a faulted link), then
	// restart — the old handle is dropped, never reused.
	a.Heal()
	a.Restart(c)
	if g, err, crashed := a.Sync(ctx, wal.LevelRefs); crashed || err != nil {
		c.failf("post-restart sync: err=%v crashed=%v", err, crashed)
	} else {
		g.Release()
	}
	if err := c.CheckTruth(map[string]string{"refs/heads/main": oid5}); err != nil {
		c.failf("post-orphan truth: %v", err)
	}
}

func TestSim_AfterALostCASResponse(t *testing.T) {
	if testing.Short() {
		t.Skip("sim tier: run with make sim")
	}
	c := newCluster(t, defaultRepo)
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Minute)
	defer cancel()

	a := c.AddInstance("a", 32)
	zero := git.Sha1.ZeroHex()
	oid0 := oidFor(32, 0, 0)
	if _, err := a.PushRef(ctx, "refs/heads/main", zero, oid0); err != nil {
		c.failf("setup push: %v", err)
	}

	// errAfter on the manifest CAS: the write lands, the response is lost.
	// The writer re-reads fresh ("cas_landed") and treats "my segment is
	// listed" as committed — no duplicate seq, no lost ref.
	a.link.Set(fault.Plan{PErrAfter: 1.0}.WithOnly("manifest.pb"))
	oid1 := oidFor(32, 0, 1)
	res, err, crashed := a.Publish(ctx, wal.PublishRequest{
		Txn: refTxn("refs/heads/main", oid0, oid1), Synced: true,
	})
	if crashed || err != nil || res.Seq != 2 {
		c.failf("lost-CAS push: res=%+v err=%v crashed=%v (want commit at seq 2)", res, err, crashed)
	}
	a.Heal()
	oid2 := oidFor(32, 0, 2)
	seq, err := a.PushRef(ctx, "refs/heads/main", oid1, oid2)
	if err != nil {
		c.failf("follow-up push: %v", err)
	}
	if seq != 3 {
		c.failf("follow-up push at seq %d (want 3: no duplicate seq)", seq)
	}
	if m, merr := c.TruthManifest(); merr != nil || m.HeadSeq != 3 {
		c.failf("truth head: m=%+v err=%v (want head 3)", m, merr)
	}
	if err := c.CheckTruth(map[string]string{"refs/heads/main": oid2}); err != nil {
		c.failf("lost-CAS truth: %v", err)
	}
}

func TestSim_StaleInstanceCannotStarveTheCore(t *testing.T) {
	if testing.Short() {
		t.Skip("sim tier: run with make sim")
	}
	c := newCluster(t, defaultRepo)
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Minute)
	defer cancel()

	a := c.AddInstance("a", 33)
	b := c.AddInstance("b", 34)
	zero := git.Sha1.ZeroHex()
	oid0 := oidFor(33, 0, 0)
	if _, err := a.PushRef(ctx, "refs/heads/main", zero, oid0); err != nil {
		c.failf("setup push: %v", err)
	}
	oidB := oidFor(34, 0, 0)
	if _, err := b.PushRef(ctx, "refs/heads/main", oid0, oidB); err != nil {
		c.failf("core push: %v", err)
	}

	// A goes stale (every conditional GET is a 304: it never learns B's
	// commit). Its push against the moved ref cannot land — the CAS ladder
	// 412-restarts against a manifest it can never see — and fails WITHOUT
	// wedging the core.
	a.link.Set(fault.StaleForever())
	res, err, crashed := a.Publish(ctx, wal.PublishRequest{
		Txn: refTxn("refs/heads/main", oid0, oidFor(33, 0, 1)), Synced: true,
	})
	if crashed || err == nil || res.Seq != 0 {
		c.failf("stale push: res=%+v err=%v crashed=%v (want failure, core untouched)", res, err, crashed)
	}

	// The monotonic revision guard holds: a's held manifest is still the old
	// revision even after its own failed publish.
	if m, _ := a.h.ManifestSnapshot(); m.Revision != 2 {
		c.failf("stale instance moved to revision %d (want 2: guard must ignore)", m.Revision)
	}

	// The core still commits while A is stale.
	oidB2 := oidFor(34, 0, 1)
	if _, err := b.PushRef(ctx, "refs/heads/main", oidB, oidB2); err != nil {
		c.failf("core push under stale peer: %v", err)
	}

	// Healed, A converges onto the core and pushes again.
	a.Heal()
	if g, err, crashed := a.Sync(ctx, wal.LevelRefs); crashed || err != nil {
		c.failf("healed sync: err=%v crashed=%v", err, crashed)
	} else {
		g.Release()
	}
	if oid, ok := a.LocalRef("refs/heads/main"); !ok || oid != oidB2 {
		c.failf("healed instance sees %s (want core %s)", oid, oidB2)
	}
	if _, err := a.PushRef(ctx, "refs/heads/main", oidB2, oidFor(33, 0, 2)); err != nil {
		c.failf("healed push: %v", err)
	}
	if err := c.CheckTruth(map[string]string{"refs/heads/main": oidFor(33, 0, 2)}); err != nil {
		c.failf("stale-scenario truth: %v", err)
	}
}

func TestSim_BlackHoledInstanceIsInvisibleToTheCore(t *testing.T) {
	if testing.Short() {
		t.Skip("sim tier: run with make sim")
	}
	c := newCluster(t, defaultRepo)
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Minute)
	defer cancel()

	a := c.AddInstance("a", 35)
	b := c.AddInstance("b", 36)
	zero := git.Sha1.ZeroHex()
	oid0 := oidFor(35, 0, 0)
	if _, err := b.PushRef(ctx, "refs/heads/main", zero, oid0); err != nil {
		c.failf("setup push: %v", err)
	}

	// Hard partition of A's link: nothing ever returns.
	a.link.Set(fault.BlackHole())

	// The core commits untouched by the partition.
	oid1 := oidFor(35, 0, 1)
	if _, err := b.PushRef(ctx, "refs/heads/main", oid0, oid1); err != nil {
		c.failf("core push under partition: %v", err)
	}

	// Leases never wedge on the partition: an expired lease is stealable
	// over a healthy link (epoch+1) while the black-holed link's acquire
	// hangs into its context deadline instead of answering.
	leaseKey := store.LeaseKey("sim-bh")
	past := time.Now().UTC().Add(-time.Hour)
	acq, exp := proto.TimeFromGo(past), proto.TimeFromGo(past.Add(time.Minute))
	stale := &proto.Lease{Holder: "old", Purpose: "sim", AcquiredAt: &acq, ExpiresAt: &exp, Epoch: 0}
	if _, err := c.truth.Put(ctx, leaseKey, store.PutBody{Bytes: stale.Marshal()},
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/x-protobuf"}); err != nil {
		c.failf("seed expired lease: %v", err)
	}
	if _, err := acquireLease(ctx, b.link, leaseKey, "core", time.Minute); err != nil {
		c.failf("core lease steal: %v", err)
	}
	body, _, err := store.GetBytes(ctx, c.truth, leaseKey, store.GetOptions{})
	if err != nil {
		c.failf("read stolen lease: %v", err)
	}
	cur := &proto.Lease{}
	if err := cur.Unmarshal(body); err != nil || cur.Holder != "core" || cur.Epoch != 1 {
		c.failf("stolen lease = %+v err=%v (want holder=core epoch=1)", cur, err)
	}
	hungCtx, hungCancel := context.WithTimeout(ctx, 3*time.Second)
	defer hungCancel()
	if _, err := acquireLease(hungCtx, a.link, leaseKey, "partitioned", time.Minute); err == nil {
		c.failf("black-holed acquire succeeded (want hung-into-deadline error)")
	}
	if a.link.Stats().Hang.Load() == 0 {
		c.failf("black hole recorded no hangs: %s", a.link.Stats().Summary())
	}

	// The partition heals, then the instance restarts (fresh process, same
	// disk, healed link) and converges without disturbing the core. Order
	// matters: rebooting onto a still-black-holed link hangs the open (the
	// FaultStore hang only answers to context cancel, and the registry
	// context has no timeout by design).
	a.Heal()
	a.RestartKeepDisk(c)
	if g, err, crashed := a.Sync(ctx, wal.LevelRefs); crashed || err != nil {
		c.failf("healed sync: err=%v crashed=%v", err, crashed)
	} else {
		g.Release()
	}
	if err := c.CheckTruth(map[string]string{"refs/heads/main": oid1}); err != nil {
		c.failf("partition-scenario truth: %v", err)
	}
}

func TestSim_CheckpointWriterCrashIsInvisibleAndRepaired(t *testing.T) {
	if testing.Short() {
		t.Skip("sim tier: run with make sim")
	}
	c := newCluster(t, defaultRepo)
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Minute)
	defer cancel()

	a := c.AddInstance("a", 37)
	zero := git.Sha1.ZeroHex()
	oid0 := oidFor(37, 0, 0)
	if _, err := a.PushRef(ctx, "refs/heads/main", zero, oid0); err != nil {
		c.failf("setup push: %v", err)
	}

	// Panic-once on the manifest CAS: round 1 (the two create-only
	// checkpoint PUTs) lands, round 2 dies in the caller.
	a.link.Set(fault.Plan{PanicOnceKeys: []string{"put:manifest.pb"}})
	if err, crashed := a.Checkpoint(ctx, wal.TriggerManual); !crashed {
		c.failf("checkpoint crash: err=%v crashed=%v (want crash)", err, crashed)
	}
	// Invisible: the manifest never moved, writers proceed.
	if m, merr := c.TruthManifest(); merr != nil || m.HeadSeq != 1 || m.Checkpoint != nil {
		c.failf("truth moved under crashed checkpoint: m=%+v err=%v", m, merr)
	}
	a.Heal()
	a.Restart(c)

	// Gap G2 (doc.go): the retry at the SAME head is not idempotent — round
	// 1 re-Creates keys that already exist and deterministically reports the
	// Create-412. This assertion pins the gap: if the wal layer ever learns
	// same-seq idempotence, G2 and this assertion go away together.
	if err, crashed := a.Checkpoint(ctx, wal.TriggerManual); crashed || err == nil {
		c.failf("same-head checkpoint retry: err=%v crashed=%v (want the round-1 412 per G2)", err, crashed)
	}

	// Repair comes from progress, not from retry: writers proceed (the push
	// below is the "invisible" half), and the next head checkpoints cleanly.
	if _, err := a.PushRef(ctx, "refs/heads/main", oid0, oidFor(37, 0, 1)); err != nil {
		c.failf("push after checkpoint crash: %v", err)
	}
	oid2 := oidFor(37, 0, 2)
	if _, err := a.PushRef(ctx, "refs/heads/main", oidFor(37, 0, 1), oid2); err != nil {
		c.failf("progress push: %v", err)
	}
	if err, crashed := a.Checkpoint(ctx, wal.TriggerManual); crashed || err != nil {
		c.failf("checkpoint at new head: err=%v crashed=%v", err, crashed)
	}
	if m, merr := c.TruthManifest(); merr != nil || m.Checkpoint == nil || m.Checkpoint.Seq != 3 {
		c.failf("checkpoint not committed at head 3: m=%+v err=%v", m, merr)
	}
	if err := c.CheckTruth(map[string]string{"refs/heads/main": oid2}); err != nil {
		c.failf("checkpoint-scenario truth: %v", err)
	}
}

func TestSim_ReaderWriterReadGuardDuringCompaction(t *testing.T) {
	if testing.Short() {
		t.Skip("sim tier: run with make sim")
	}
	// The try-write rule (13_concurrency.md) pinned against the real mutex:
	// a held read guard refuses the compaction write lock (defer, never
	// wait); after release the write proceeds. A leaked guard pins only
	// until drop — it never deadlocks the writer, which never blocks.
	var mu rw.TryRWMutex
	mu.RLock()
	if mu.TryWriteLock() {
		t.Fatal("compaction write lock acquired under a held read guard (must defer)")
	}
	if got := mu.Readers(); got != 1 {
		t.Fatalf("readers = %d (want 1: the guard is held)", got)
	}
	mu.RUnlock()
	if !mu.TryWriteLock() {
		t.Fatal("compaction write lock refused with no readers held")
	}
	mu.WriteUnlock()

	// The dual direction at the handle level: a clone-equivalent read guard
	// held through a whole publish never blocks the writer, and the guarded
	// view stays usable after.
	c := newCluster(t, defaultRepo)
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Minute)
	defer cancel()
	a := c.AddInstance("a", 38)
	b := c.AddInstance("b", 39)
	zero := git.Sha1.ZeroHex()
	oid0 := oidFor(38, 0, 0)
	if _, err := a.PushRef(ctx, "refs/heads/main", zero, oid0); err != nil {
		c.failf("setup push: %v", err)
	}
	g, err, crashed := b.Sync(ctx, wal.LevelRefs)
	if crashed || err != nil {
		c.failf("guard sync: err=%v crashed=%v", err, crashed)
	}
	oid1 := oidFor(38, 0, 1)
	done := make(chan error, 1)
	go func() {
		_, perr := a.PushRef(ctx, "refs/heads/main", oid0, oid1)
		done <- perr
	}()
	select {
	case perr := <-done:
		if perr != nil {
			g.Release()
			c.failf("publish under held read guard: %v", perr)
		}
	case <-time.After(60 * time.Second):
		g.Release()
		c.failf("publish blocked on a held read guard (writers must never wait on readers)")
	}
	g.Release()
	if err := c.CheckTruth(map[string]string{"refs/heads/main": oid1}); err != nil {
		c.failf("read-guard truth: %v", err)
	}
}

func TestSim_DrainInterruptsRunningUnit(t *testing.T) {
	if testing.Short() {
		t.Skip("sim tier: run with make sim")
	}
	c := newCluster(t, defaultRepo)
	ctx, cancel := context.WithTimeout(c.ctx, time.Minute)
	defer cancel()
	a := c.AddInstance("a", 40)

	// A running maintenance unit, interrupted by phase-1 drain: its ctx
	// fires, it records the documented 503, the next pass retries it.
	const dropErr = "503 interrupted: instance shut down; will be retried by the next pass"
	sawCancel := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := a.reg.Tasks().Run(ctx, defaultRepo, "sim-unit", nil,
			func(tctx context.Context, task *wal.Task) error {
				select {
				case <-tctx.Done():
					sawCancel <- struct{}{}
					return errors.New(dropErr)
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		done <- err
	}()
	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal("test ctx done before drain")
	}
	a.reg.Tasks().Drain()
	select {
	case <-sawCancel:
	case <-time.After(10 * time.Second):
		c.failf("drain did not cancel the running unit")
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "503 interrupted") {
			c.failf("interrupted unit returned %v (want the 503 drop)", err)
		}
	case <-time.After(10 * time.Second):
		c.failf("interrupted unit did not return")
	}
	recs := a.reg.Tasks().List(defaultRepo)
	if len(recs) == 0 {
		c.failf("interrupted unit left no record")
	}
	if last := recs[len(recs)-1]; last.OK == nil || *last.OK {
		c.failf("interrupted unit not recorded as failure: %+v", last)
	} else if !strings.Contains(last.Summary, "503 interrupted") {
		c.failf("failure record summary = %q (want the 503 drop)", last.Summary)
	}

	// Serving stays addressable through phase 1 in the sim sense: reads and
	// the refusal shape hold — a NEW unit start during drain is refused with
	// the same 503 (never silently queued).
	if _, err := a.reg.Tasks().Run(ctx, defaultRepo, "sim-unit-2", nil,
		func(tctx context.Context, task *wal.Task) error { return nil }); err == nil ||
		!strings.Contains(err.Error(), "503 interrupted") {
		c.failf("post-drain start returned %v (want 503 refusal)", err)
	}
}
