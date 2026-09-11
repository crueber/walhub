// budgets_test.go — TestSim_HealthyRequestRoundTripBudgets (15_testing.md
// §4 row 11, §4.1): the §4.8 budgets counted at the transport layer via the
// acting instance's link Stats.Ops. See doc.go for the law-6/§4.1
// reconciliation (Rust 5/4 + 1 parallel sidecar PUT = asserted 6/5).
package sim

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/wal"
)

// budget asserts a link-ops delta against its bound; the failure message
// carries the measured count, the budget, and the trace artifact.
func budget(t *testing.T, c *Cluster, in *Instance, what string, before, bound uint64) {
	t.Helper()
	if got := in.Ops() - before; got > bound {
		c.failf("budget %s: %d ops > %d", what, got, bound)
	} else {
		t.Logf("budget %s: %d ops <= %d", what, got, bound)
	}
}

func TestSim_HealthyRequestRoundTripBudgets(t *testing.T) {
	if testing.Short() {
		t.Skip("sim tier: run with make sim")
	}
	c := newCluster(t, defaultRepo)
	ctx, cancel := context.WithTimeout(c.ctx, 3*time.Minute)
	defer cancel()

	a := c.AddInstance("a", 7)
	zero := git.Sha1.ZeroHex()
	oid1 := oidFor(7, 0, 0)

	// Warm refs sync: TTL expired (sim default) → exactly 1 conditional GET
	// (0 within the freshness TTL — asserted separately below).
	if g, err, crashed := a.Sync(ctx, wal.LevelRefs); crashed || err != nil {
		c.failf("setup sync: err=%v crashed=%v", err, crashed)
	} else {
		g.Release()
	}
	before := a.Ops()
	if g, err, crashed := a.Sync(ctx, wal.LevelRefs); crashed || err != nil {
		c.failf("warm sync: err=%v crashed=%v", err, crashed)
	} else {
		g.Release()
	}
	budget(t, c, a, "warm refs sync", before, 1)

	// Within the freshness TTL the sync costs nothing at all.
	a.reg.Config().WAL.FreshnessTTL = config.Duration(time.Hour)
	before = a.Ops()
	if g, err, crashed := a.Sync(ctx, wal.LevelRefs); crashed || err != nil {
		c.failf("ttl sync: err=%v crashed=%v", err, crashed)
	} else {
		g.Release()
	}
	budget(t, c, a, "refs sync within freshness TTL", before, 0)
	a.reg.Config().WAL.FreshnessTTL = 0

	// Push, unsynced (needs the freshness GET): ref-only, no packs.
	before = a.Ops()
	res, err, crashed := a.Publish(ctx, wal.PublishRequest{Txn: refTxn("refs/heads/main", zero, oid1)})
	if crashed || err != nil || res.Seq == 0 {
		c.failf("push 1: res=%+v err=%v crashed=%v", res, err, crashed)
	}
	budget(t, c, a, "push (needs sync)", before, 6)

	// Push, already synced: no freshness GET.
	oid2 := oidFor(7, 0, 1)
	before = a.Ops()
	res, err, crashed = a.Publish(ctx, wal.PublishRequest{
		Txn: refTxn("refs/heads/main", oid1, oid2), Synced: true,
	})
	if crashed || err != nil || res.Seq == 0 {
		c.failf("push 2: res=%+v err=%v crashed=%v", res, err, crashed)
	}
	budget(t, c, a, "push (already synced)", before, 5)

	// Cold refs sync (one tail): a fresh instance learns one new seq with
	// manifest GET → tail segment = 2 ops.
	b := c.AddInstance("b", 8)
	if g, err, crashed := b.Sync(ctx, wal.LevelRefs); crashed || err != nil {
		c.failf("b setup sync: err=%v crashed=%v", err, crashed)
	} else {
		g.Release()
	}
	oid3 := oidFor(7, 0, 2)
	if _, err := a.PushRef(ctx, "refs/heads/main", oid2, oid3); err != nil {
		c.failf("a push 3: %v", err)
	}
	before = b.Ops()
	if g, err, crashed := b.Sync(ctx, wal.LevelRefs); crashed || err != nil {
		c.failf("cold sync: err=%v crashed=%v", err, crashed)
	} else {
		g.Release()
	}
	budget(t, c, b, "cold refs sync (one tail)", before, 2)
	if oid, ok := b.LocalRef("refs/heads/main"); !ok || oid != oid3 {
		c.failf("b converged to %s (want %s)", oid, oid3)
	}

	// Checkpoint: freshness GET → (refs PUT ∥ checkpoint PUT) → manifest
	// CAS = 4. Provenance times come from what the writer already applied —
	// never a log GET (the 2026-08-22 6-request regression fence).
	before = a.Ops()
	if err, crashed := a.Checkpoint(ctx, wal.TriggerManual); crashed || err != nil {
		c.failf("checkpoint: err=%v crashed=%v", err, crashed)
	}
	budget(t, c, a, "checkpoint", before, 4)

	// Push with packs pins the doc's 6/5 bound exactly: freshness + pack PUT
	// + idx PUT + log PUT + sidecar PUT + manifest CAS = 6 (5 synced). The
	// fake pack bytes never touch git (upload only); the commit-graph fold
	// runs async off the reply path and out of the op window.
	dir := t.TempDir()
	packPath := filepath.Join(dir, "pack-pin.pack")
	idxPath := filepath.Join(dir, "pack-pin.idx")
	if err := os.WriteFile(packPath, []byte("PACK pin"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath, []byte("IDX pin"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("e", 40)
	oid4 := oidFor(7, 0, 3)
	before = a.Ops()
	res, err, crashed = a.Publish(ctx, wal.PublishRequest{
		Txn:  refTxn("refs/heads/main", oid3, oid4),
		Pack: &wal.PreparedPack{Checksum: sum, PackPath: packPath, PackSize: 8, IdxPath: idxPath, IdxSize: 7},
	})
	if crashed || err != nil || res.Seq == 0 {
		c.failf("push with pack: res=%+v err=%v crashed=%v", res, err, crashed)
	}
	budget(t, c, a, "push with pack (needs sync)", before, 6)

	sum2 := strings.Repeat("d", 40)
	packPath2 := filepath.Join(dir, "pack-pin2.pack")
	idxPath2 := filepath.Join(dir, "pack-pin2.idx")
	if err := os.WriteFile(packPath2, []byte("PACK pin2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath2, []byte("IDX pin2"), 0o644); err != nil {
		t.Fatal(err)
	}
	oid5 := oidFor(7, 0, 4)
	before = a.Ops()
	res, err, crashed = a.Publish(ctx, wal.PublishRequest{
		Txn:    refTxn("refs/heads/main", oid4, oid5),
		Pack:   &wal.PreparedPack{Checksum: sum2, PackPath: packPath2, PackSize: 9, IdxPath: idxPath2, IdxSize: 7},
		Synced: true,
	})
	if crashed || err != nil || res.Seq == 0 {
		c.failf("push with pack synced: res=%+v err=%v crashed=%v", res, err, crashed)
	}
	budget(t, c, a, "push with pack (already synced)", before, 5)

	if err := c.CheckTruth(map[string]string{"refs/heads/main": oid5}); err != nil {
		c.failf("budget-run truth: %v", err)
	}
}
