// safety_test.go — TestSim_SafetyThenLiveness and
// TestSim_ConcurrentPushersExactlyOneWinner (15_testing.md §4 rows 1, 5).
package sim

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/fault"
	"git.packden.us/crueber/walhub/internal/wal"
)

// safetyCore runs the safety-then-liveness core: nPushers instances push
// pushesPerPusher sequential updates to their OWN ref under a Chaos plan,
// concurrently (manifest CAS races exercise the 412-restart ladder); then all
// links heal, every instance re-syncs, and the truth oracle proves no lost
// commits and full convergence. It returns the final oid per pusher ref.
// Deterministic under seed.
func safetyCore(t *testing.T, c *Cluster, seed uint64, nPushers, pushesPerPusher int, rate float64) map[string]string {
	t.Helper()
	if testing.Short() {
		t.Skip("sim tier: run with make sim")
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Minute)
	defer cancel()

	type pusher struct {
		in   *Instance
		ref  string
		oids []string
	}
	pushers := make([]*pusher, nPushers)
	for i := range pushers {
		in := c.AddInstance(fmt.Sprintf("i%d", i), seed)
		in.link.Set(fault.Chaos(rate))
		pushers[i] = &pusher{in: in, ref: fmt.Sprintf("refs/heads/p%d", i)}
	}

	// Safety phase: every pusher advances its own ref; concurrency is across
	// pushers (the publisher's group commit may also batch within one).
	var wg sync.WaitGroup
	errs := make([]error, nPushers)
	seqs := make([][]uint64, nPushers)
	for i, p := range pushers {
		wg.Add(1)
		go func(i int, p *pusher) {
			defer wg.Done()
			old := git.Sha1.ZeroHex()
			for k := 0; k < pushesPerPusher; k++ {
				oid := oidFor(seed, i, k)
				seq, err := p.in.PushRef(ctx, p.ref, old, oid)
				if err != nil {
					errs[i] = fmt.Errorf("pusher %d push %d: %w", i, k, err)
					return
				}
				seqs[i] = append(seqs[i], seq)
				p.oids = append(p.oids, oid)
				old = oid
			}
		}(i, p)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			c.failf("safety phase: %v", err)
		}
		_ = i
	}

	// Exactly-one-winner per commit: every committed seq is distinct (two
	// batches never share a seq; group-commit shares are one seq by design).
	seen := map[uint64]string{}
	for i, ss := range seqs {
		for _, s := range ss {
			if owner, dup := seen[s]; dup {
				c.failf("seq %d committed twice (pushers %s and %d)", s, owner, i)
			}
			seen[s] = fmt.Sprintf("pusher %d", i)
		}
	}

	// Liveness phase: heal the core, re-sync every instance, prove the truth.
	for _, p := range pushers {
		p.in.Heal()
	}
	for _, p := range pushers {
		g, err, crashed := p.in.Sync(ctx, wal.LevelRefs)
		if crashed {
			c.failf("liveness sync: instance %s crashed after heal", p.in.name)
		}
		if err != nil {
			c.failf("liveness sync %s: %v", p.in.name, err)
		}
		g.Release()
	}
	expected := map[string]string{}
	for _, p := range pushers {
		expected[p.ref] = p.oids[len(p.oids)-1]
	}
	if err := c.CheckTruth(expected); err != nil {
		c.failf("liveness truth: %v", err)
	}
	return expected
}

func TestSim_SafetyThenLiveness(t *testing.T) {
	if testing.Short() {
		t.Skip("sim tier: run with make sim")
	}
	seeds, err := simSeeds()
	if err != nil {
		t.Fatalf("sim seeds: %v", err)
	}
	c := newCluster(t, defaultRepo)
	got := safetyCore(t, c, seeds[0], 3, 3, 0.05)
	if len(got) != 3 {
		t.Fatalf("expected 3 converged refs, got %d", len(got))
	}
	t.Logf("converged 3x3 pushes under chaos; traces:\n%s", c.DumpTraces())
}

func TestSim_ConcurrentPushersExactlyOneWinner(t *testing.T) {
	if testing.Short() {
		t.Skip("sim tier: run with make sim")
	}
	const k = 4
	c := newCluster(t, defaultRepo)
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Minute)
	defer cancel()

	instances := make([]*Instance, k)
	for i := range instances {
		instances[i] = c.AddInstance(fmt.Sprintf("racer%d", i), 1000+uint64(i))
	}

	// K instances race conflicting updates of ONE ref from zero. The store
	// CAS admits exactly one manifest revision per attempt; losers observe
	// the winner via 412 → re-sync and report per-ref conflicts.
	const ref = "refs/heads/race"
	oids := make([]string, k)
	for i := range oids {
		oids[i] = oidFor(1000, i, 0)
	}
	type outcome struct {
		i   int
		seq uint64
		err error
	}
	start := make(chan struct{})
	out := make(chan outcome, k)
	for i, in := range instances {
		go func(i int, in *Instance) {
			<-start
			res, err, crashed := in.Publish(ctx, wal.PublishRequest{
				Txn:    refTxn(ref, git.Sha1.ZeroHex(), oids[i]),
				Synced: true,
			})
			if crashed {
				err = fmt.Errorf("crashed")
			}
			out <- outcome{i: i, seq: res.Seq, err: err}
		}(i, in)
	}
	close(start)

	var winners []outcome
	var conflicted int
	for range k {
		select {
		case o := <-out:
			if o.err != nil {
				t.Fatalf("racer %d transport error (must be a per-ref verdict, not an error): %v", o.i, o.err)
			}
			if o.seq > 0 {
				winners = append(winners, o)
			} else {
				conflicted++
			}
		case <-ctx.Done():
			c.failf("racers did not finish: %v", ctx.Err())
		}
	}
	if len(winners) != 1 {
		c.failf("exactly one winner required, got %d", len(winners))
	}
	if conflicted != k-1 {
		c.failf("expected %d conflicted losers, got %d", k-1, conflicted)
	}
	winnerOid := oids[winners[0].i]
	if err := c.CheckTruth(map[string]string{ref: winnerOid}); err != nil {
		c.failf("winner not reflected in truth: %v", err)
	}
	// Every loser re-syncs onto the winner's version (no stale fork survives).
	for _, in := range instances {
		g, err, crashed := in.Sync(ctx, wal.LevelRefs)
		if crashed {
			c.failf("loser %s crashed re-syncing", in.name)
		}
		if err != nil {
			c.failf("loser %s re-sync: %v", in.name, err)
		}
		oid, ok := in.LocalRef(ref)
		g.Release()
		if !ok || oid != winnerOid {
			c.failf("loser %s sees %s (want winner %s)", in.name, oid, winnerOid)
		}
	}
	t.Logf("one winner (%s), %d losers converged", winnerOid[:8], conflicted)
}
