// gaps_test.go — the last-mile paths: fsckDue branches, HeartbeatAlive,
// baseWindow/headSeqAt bar math, lease contention (StoreLeaser), fsck report
// round-trip, and the unknown-unit dispatch.
package maintain

import (
	"context"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func TestFsckDue_Branches(t *testing.T) {
	eff := defaultEff()
	eff.Maintenance.FsckInterval = config.Duration(time.Hour)
	now := time.Now()

	off := defaultEff()
	off.Maintenance.FsckInterval = 0
	if fsckDue(off, nil, now) {
		t.Fatal("interval 0 must disable the unit")
	}
	if !fsckDue(eff, nil, now) {
		t.Fatal("never audited must be due")
	}
	if !fsckDue(eff, &proto.FsckReport{RepairedSeq: 3, At: ptrTs(now)}, now) {
		t.Fatal("repaired since audit must be due")
	}
	if fsckDue(eff, &proto.FsckReport{RepairedSeq: 0, At: ptrTs(now.Add(-time.Minute))}, now) {
		t.Fatal("fresh report must not be due")
	}
	if !fsckDue(eff, &proto.FsckReport{RepairedSeq: 0, At: ptrTs(now.Add(-2 * time.Hour))}, now) {
		t.Fatal("stale report must be due")
	}
}

func TestHeartbeatAlive_Branches(t *testing.T) {
	now := time.Now()
	if HeartbeatAlive(nil, now) {
		t.Fatal("nil heartbeat must not be alive")
	}
	if HeartbeatAlive(&proto.MaintainerHeartbeat{}, now) {
		t.Fatal("no last_pass_at must not be alive")
	}
	if !HeartbeatAlive(&proto.MaintainerHeartbeat{LastPassAt: ptrTs(now.Add(-time.Minute))}, now) {
		t.Fatal("recent pass must be alive")
	}
	if HeartbeatAlive(&proto.MaintainerHeartbeat{LastPassAt: ptrTs(now.Add(-700 * time.Second))}, now) {
		t.Fatal("stale pass must not be alive")
	}
}

func TestBaseWindow_PlannerAndCheckpoint(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = []config.BundleStrategy{
		{Name: "weekly", Kind: "full", Schedule: "0 0 23 * * 0"},
	}
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}}
	fire := time.Unix(1_700_000_000, 0)

	// With a planner: the strategy's previous fire wins.
	planner := &fakePlanner{fireAt: map[string]time.Time{"weekly": fire}}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}, Planner: planner})
	snap := snapshotFor(eff, repo.m, nil, nil)
	if got := m.baseWindow(snap, "weekly"); !got.Equal(fire) {
		t.Fatalf("baseWindow = %v, want planner fire %v", got, fire)
	}
	// Unknown strategy with no checkpoint → zero time.
	if got := m.baseWindow(snap, "other"); !got.IsZero() {
		t.Fatalf("unknown strategy window = %v", got)
	}

	// Without a planner: the checkpoint's first_state_at is the fallback.
	m2 := New(eng, Options{Leaser: &fakeLeaser{}, Planner: nil})
	first := time.Unix(1_600_000_000, 0)
	ts := proto.TimeFromGo(first)
	repo.m.Checkpoint = &proto.CheckpointRef{FirstStateAt: &ts}
	if got := m2.baseWindow(snap, "anything"); !got.Equal(first) {
		t.Fatalf("checkpoint window = %v, want %v", got, first)
	}
}

func TestHeadSeqAt_BarMath(t *testing.T) {
	ctx := context.Background()
	eff := defaultEff()
	older := time.Unix(1_600_000_000, 0)
	cut := time.Unix(1_600_001_000, 0)

	repo := &fakeRepo{
		id:  "acme/widget",
		m:   &proto.Manifest{Repo: "acme/widget", MinSeq: 1, HeadSeq: 3},
		dir: t.TempDir(),
	}
	repo.entries = []*proto.LogEntry{
		{Seq: 1, CreatedAt: ptrTs(older)},
		{Seq: 2, CreatedAt: ptrTs(cut.Add(-time.Second))},
		{Seq: 3, CreatedAt: ptrTs(cut.Add(time.Second))},
	}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}})

	// Zero instant → bar 0 without reading the log.
	if got := m.headSeqAt(ctx, repo, time.Time{}); got != 0 {
		t.Fatalf("zero at → %d", got)
	}
	// Entries before the cut count; the first after it stops the walk.
	if got := m.headSeqAt(ctx, repo, cut); got != 2 {
		t.Fatalf("bar = %d, want 2", got)
	}
	// An empty log folds to bar 0 (the max(bar,1) rule keeps the repo
	// rebuildable at seq 1 at the caller).
	repo.entries = nil
	if got := m.headSeqAt(ctx, repo, cut); got != 0 {
		t.Fatalf("empty log bar = %d, want 0", got)
	}
}

func TestExecUnit_UnknownKind(t *testing.T) {
	eff := defaultEff()
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}})
	out, detail := m.execUnit(context.Background(), repo, &Snapshot{Eff: eff}, Selection{Kind: "nope"}, nopLogger{})
	if out != OutcomeError || detail != "unknown unit kind nope" {
		t.Fatalf("outcome=%v detail=%q", out, detail)
	}
}

func TestGetPutFsckReport_RoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newMemStore()
	prefix := "repos/acme/widget/"

	ok, err := getFsckReport(ctx, st, prefix, &proto.FsckReport{})
	if ok || err != nil {
		t.Fatalf("absent report: ok=%v err=%v", ok, err)
	}
	want := &proto.FsckReport{Missing: []string{"abc"}, Problems: 2, At: ptrTs(time.Now())}
	if err := putFsckReport(ctx, st, prefix, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got := &proto.FsckReport{}
	ok, err = getFsckReport(ctx, st, prefix, got)
	if !ok || err != nil || len(got.Missing) != 1 {
		t.Fatalf("round trip: ok=%v err=%v got=%+v", ok, err, got)
	}
}

func TestStoreLeaser_ExclusiveAcquireAndRelease(t *testing.T) {
	ctx := context.Background()
	l := StoreLeaser{St: newMemStore()}

	rel, err := l.Acquire(ctx, "compact", "host-a", "test", time.Minute, time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := l.Acquire(ctx, "compact", "host-b", "test", time.Minute, time.Second); err == nil {
		t.Fatal("held lease must be refused")
	}
	rel()
	if _, err := l.Acquire(ctx, "compact", "host-b", "test", time.Minute, time.Second); err != nil {
		t.Fatalf("reacquire: %v", err)
	}
}

func TestShortChecksum(t *testing.T) {
	if got := shortChecksum("abcdef1234567890"); got != "abcdef12" {
		t.Fatalf("short = %q", got)
	}
	if got := shortChecksum("ab"); got != "ab" {
		t.Fatalf("short input passthrough = %q", got)
	}
}
