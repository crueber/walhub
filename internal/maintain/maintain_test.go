package maintain

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// TestPass_OutcomesAndHeartbeat: the pass runs one bounded unit per assigned
// repo in registration order, records metrics, and writes the heartbeat
// before and after (§3.2/§4.2).
func TestPass_OutcomesAndHeartbeat(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.FsckInterval = 0
	eff.Maintenance.Checkpoints = false
	eff.Compaction.Enabled = false

	// Two repos: both idle → idle outcomes, no units.
	r1 := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}}
	r2 := &fakeRepo{id: "acme/legacy", m: &proto.Manifest{Repo: "acme/legacy"}}
	eng := newFakeEngine(eff, r1, r2)
	m := New(eng, Options{Leaser: &fakeLeaser{}})

	m.RunPass(context.Background())

	snap := m.Metrics()
	if snap.Passes != 1 {
		t.Fatalf("passes = %d, want 1", snap.Passes)
	}
	if n := snap.Units[""][OutcomeIdle]; n != 2 {
		t.Fatalf("idle outcomes = %d, want 2", n)
	}

	// Heartbeat: written, alive, carries the assignment (bucket-root
	// maintain/<host>.pb, §4.2).
	hbBody, _, err := store.GetBytes(context.Background(), eng.st, "maintain/test-host.pb", store.GetOptions{})
	if err != nil && !store.IsNotFound(err) || hbBody == nil {
		t.Fatalf("heartbeat not written: %v", err)
	}
	hb := &proto.MaintainerHeartbeat{}
	if err := hb.Unmarshal(hbBody); err != nil {
		t.Fatalf("heartbeat decode: %v", err)
	}
	if hb.Host != "test-host" || hb.Passes != 1 || hb.LastPassAt == nil {
		t.Fatalf("heartbeat shape = %+v", hb)
	}
	if !HeartbeatAlive(hb, time.Now()) {
		t.Fatal("fresh heartbeat must be alive")
	}
	if HeartbeatAlive(hb, time.Now().Add(601*time.Second)) {
		t.Fatal("alive must be false 601s after the last write (§4.2)")
	}
}

// TestPass_PlacementExclude: excluded repos are not assigned.
func TestPass_PlacementExclude(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	eff.Placement.MaintainExclude = []string{"archive/*"}

	in := &fakeRepo{id: "acme/widget", m: &proto.Manifest{}}
	out := &fakeRepo{id: "archive/old", m: &proto.Manifest{}}
	eng := newFakeEngine(eff, in, out)
	m := New(eng, Options{Leaser: &fakeLeaser{}})
	m.RunPass(context.Background())

	snap := m.Metrics()
	if n := snap.Units[""][OutcomeIdle]; n != 1 {
		t.Fatalf("only the assigned repo must be visited (idle=%d)", n)
	}
}

// TestPass_UnitPriorityEndToEnd: a repo in repair state runs checkpoint? No —
// checkpoint only when triggered; verify repair heals and fsck.pb flips.
func TestPass_UnitPriorityEndToEnd(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	eff.Upstream.Git = "https://upstream.example/acme/widget.git"

	repo := &fakeRepo{
		id: "acme/widget",
		m:  &proto.Manifest{Repo: "acme/widget", HeadSeq: 5, MinSeq: 1},
		git: &fakeGit{
			fetchPackPath: "/cache/pack-abc.pack",
			fetchErr:      nil,
		},
	}
	// fsck.pb with a missing oid, repaired_seq == 0 → unit 2.
	if err := putFsckReport(context.Background(), eng0Store(), repo.Prefix(),
		&proto.FsckReport{Missing: []string{"oid1", "oid1", "oid2"}, RepairedSeq: 0}); err != nil {
		t.Fatal(err)
	}
	eng := newFakeEngine(eff, repo)
	// Seed the store fsck.pb (loadRepo reads it from the engine's store).
	if err := putFsckReport(context.Background(), eng.Store(), repo.Prefix(),
		&proto.FsckReport{Missing: []string{"oid1", "oid1", "oid2"}, RepairedSeq: 0}); err != nil {
		t.Fatal(err)
	}
	m := New(eng, Options{Leaser: &fakeLeaser{}})
	m.RunPass(context.Background())

	if len(repo.compacts) == 0 {
		t.Fatal("repair must publish the fetched pack via PublishCompact")
	}
	c := repo.compacts[0]
	if c.tier != 0 || len(c.supersedes) != 0 {
		t.Fatalf("repair publish = %+v, want tier-0 superseding nothing", c)
	}
	report := &proto.FsckReport{}
	has, err := getFsckReport(context.Background(), eng.Store(), repo.Prefix(), report)
	if err != nil || !has {
		t.Fatalf("fsck.pb missing after repair: %v", err)
	}
	if report.RepairedSeq == 0 {
		t.Fatal("repaired_seq must be set so the repair does not re-fire")
	}
	if snap := m.Metrics(); snap.Units[KindRepair][OutcomeOK] != 1 {
		t.Fatalf("repair outcome = %v", snap.Units[KindRepair])
	}
}

func eng0Store() store.ObjectStore { return newMemStore() }

// TestPass_UnitCapTimeout: the wait cap releases the pass while the task
// keeps running (§3.2 step 4).
func TestPass_UnitCapTimeout(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false

	repo := &fakeRepo{id: "acme/huge", m: &proto.Manifest{Repo: "acme/huge", HeadSeq: 5000}}
	eng := newFakeEngine(eff, repo)

	// Checkpoint unit blocks past the cap.
	block := make(chan struct{})
	eng.tasks.hook = func(repo, kind string, fn func(ctx context.Context, t TaskLogger) error) error {
		if kind == KindCheckpoint {
			<-block
			return nil
		}
		return fn(context.Background(), nopLogger{})
	}
	defer close(block)

	m := New(eng, Options{
		Leaser:  &fakeLeaser{},
		UnitCap: 30 * time.Millisecond,
	})
	done := make(chan struct{})
	go func() {
		m.RunPass(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pass must move on past the unit cap")
	}
	if snap := m.Metrics(); snap.Units[KindCheckpoint][OutcomeTimeout] != 1 {
		t.Fatalf("timeout outcome = %v", snap.Units[KindCheckpoint])
	}
	// The task stays discoverable: the fake runner's start was recorded and
	// the body is still blocked.
	eng.tasks.mu.Lock()
	started := len(eng.tasks.starts)
	eng.tasks.mu.Unlock()
	if started == 0 {
		t.Fatal("unit must have been started via the task registry")
	}
}

// TestPass_StaleSlotSkipCap: bundle planning that keeps skipping through
// stale slots ends the repo's turn at the cap (§3.2 step 5).
func TestPass_StaleSlotSkipCap(t *testing.T) {
	eff := defaultEff()
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false

	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}}
	eng := newFakeEngine(eff, repo)
	planner := &fakePlanner{slots: map[string][]Slot{
		"acme/widget": {{Strategy: "weekly", Kind: "full", Slot: 1, State: "missing"}},
	}}
	// Build never builds: the pass re-plans forever → skip cap must end it.
	planner.buildsOK = false
	// A "missing" slot that builds nothing reports state=… nothing-built;
	// stale-skip counting keys on the state token in the detail line.
	m := New(eng, Options{
		Leaser:  &fakeLeaser{},
		Planner: planner,
		SkipCap: 3,
	})
	m.RunPass(context.Background())
	if got := len(planner.built); got > 4 {
		t.Fatalf("bundle builds attempted %d times; skip cap 3 must bound the turn", got)
	}
}

// TestCompact_HeldOutcomes: a held compact lease or pack mutex reports held
// and defers to the next pass (§3.3/§6.1).
func TestCompact_HeldOutcomes(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = true
	eff.Compaction.TriggerPacks = 2

	newRepo := func() *fakeRepo {
		r := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}}
		for i := range 3 {
			r.m.Packs = append(r.m.Packs, pack(fmt2("p%d", i), uint64(i), 100, 10, 0))
		}
		r.git = &fakeGit{geoDiff: gitPackDiff("folded")}
		return r
	}

	t.Run("lease-held", func(t *testing.T) {
		repo := newRepo()
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{held: map[string]bool{"compact": true}}})
		m.RunPass(context.Background())
		if snap := m.Metrics(); snap.Units[KindCompact][OutcomeHeld] != 1 {
			t.Fatalf("held outcome = %v", snap.Units[KindCompact])
		}
		if repo.git.geoCalls != 0 {
			t.Fatal("a held lease must never run the repack")
		}
	})
	t.Run("pack-mutex-busy", func(t *testing.T) {
		repo := newRepo()
		repo.packMuBusy = true
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{}})
		m.RunPass(context.Background())
		if snap := m.Metrics(); snap.Units[KindCompact][OutcomeHeld] != 1 {
			t.Fatalf("held outcome = %v", snap.Units[KindCompact])
		}
		if repo.git.geoCalls != 0 {
			t.Fatal("readers must never queue behind maintenance")
		}
	})
}

// TestCompact_KeepsBaseAndHistoryPacks: --keep-pack for tier-2 + history
// (§6.1 invariant) and the supersedes set from the pre-repack snapshot.
func TestCompact_KeepsBaseAndHistoryPacks(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = true
	eff.Compaction.TriggerPacks = 2

	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}}
	for i := range 4 {
		repo.m.Packs = append(repo.m.Packs, pack(fmt2("fresh%d", i), uint64(i), 100, 10, 0))
	}
	base := pack("base", 100, 5000, 5000, 2)
	base.HasBitmap = true
	hist := &proto.PackRef{Checksum: "hist", Seq: 100, PackSize: 10, Tier: 2, Kind: proto.PackKindHistory}
	repo.m.Packs = append(repo.m.Packs, base, hist)
	repo.git = &fakeGit{geoDiff: gitPackDiff("folded")}

	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}})
	m.RunPass(context.Background())

	if len(repo.compacts) == 0 {
		t.Fatal("fold must publish")
	}
	c := repo.compacts[0]
	if c.tier != 1 {
		t.Fatalf("fold tier = %d, want 1", c.tier)
	}
	if len(c.supersedes) != 4 {
		t.Fatalf("supersedes = %v, want exactly the 4 fresh packs", c.supersedes)
	}
	keeps := repo.git.keepPacks[0]
	if len(keeps) != 2 || !contains(keeps, "pack-base.pack") || !contains(keeps, "pack-hist.pack") {
		t.Fatalf("keep-packs = %v, want base + history", keeps)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestLeaseCASLadder: acquire → held when fresh → steal after expiry+skew →
// release deletes (§4.9).
func TestLeaseCASLadder(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()
	l := StoreLeaser{St: st}

	release, err := l.Acquire(ctx, "compact", "host-a", "folding", time.Minute, leaseSkew)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// Held while live (skew included).
	if _, err := l.Acquire(ctx, "compact", "host-b", "folding", time.Minute, leaseSkew); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second acquire = %v, want ErrLeaseHeld", err)
	}
	// Steal only after expires_at + skew.
	body, _, _ := store.GetBytes(ctx, st, "leases/compact.pb", store.GetOptions{})
	cur := &proto.Lease{}
	_ = cur.Unmarshal(body)
	expired := proto.TimeFromGo(time.Now().Add(-leaseSkew - time.Minute))
	cur.ExpiresAt = &expired
	if _, err := st.Put(ctx, "leases/compact.pb", store.PutBody{Bytes: cur.Marshal()},
		store.PutOptions{Mode: store.PutUpdate, IfVersion: versionOf(t, st, "leases/compact.pb")}); err != nil {
		t.Fatal(err)
	}
	release2, err := l.Acquire(ctx, "compact", "host-b", "folding", time.Minute, leaseSkew)
	if err != nil {
		t.Fatalf("steal after expiry+skew: %v", err)
	}
	// Release deletes only when still ours.
	release2()
	if st.has("leases/compact.pb") {
		t.Fatal("release must delete the lease")
	}
	release() // double release is a no-op
}

// TestHeartbeatPurge: heartbeats older than 24 h are deleted (§4.2).
func TestHeartbeatPurge(t *testing.T) {
	st := newMemStore()
	ctx := context.Background()

	fresh := &proto.MaintainerHeartbeat{Host: "fresh", LastPassAt: ptrTs(time.Now().Add(-time.Hour))}
	stale := &proto.MaintainerHeartbeat{Host: "stale", LastPassAt: ptrTs(time.Now().Add(-25 * time.Hour))}
	for _, hb := range []*proto.MaintainerHeartbeat{fresh, stale} {
		if err := writeHeartbeat(ctx, st, hb); err != nil {
			t.Fatal(err)
		}
	}
	purged, err := purgeHeartbeats(ctx, st, time.Now(), hbPurgeAfter)
	if err != nil {
		t.Fatal(err)
	}
	if len(purged) != 1 || purged[0] != "maintain/stale.pb" {
		t.Fatalf("purged = %v, want [maintain/stale.pb]", purged)
	}
	if st.has("maintain/stale.pb") || !st.has("maintain/fresh.pb") {
		t.Fatal("only the stale heartbeat must be gone")
	}
}

func fmt2(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

// versionOf reads the current version of a key (test helper for CAS puts).
func versionOf(t *testing.T, st *memStore, key string) store.Version {
	t.Helper()
	meta, err := st.Head(context.Background(), key)
	if err != nil {
		t.Fatalf("head %s: %v", key, err)
	}
	if meta == nil {
		return ""
	}
	return meta.Version
}

// gitPackDiff builds a fake PackDiff naming one new pack.
func gitPackDiff(name string) *git.PackDiff {
	return &git.PackDiff{New: []string{"pack-" + name + ".idx"}, Removed: nil}
}
