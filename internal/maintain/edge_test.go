// edge_test.go — the remaining §-path edges: runRebuild pre-flight/lease/
// phase-failure matrix, publishRebuild variants, the geometric fold + the
// retention GC, the store lease CAS ladder corners, the loop helpers, the
// follow round fan, and the .rev writer error matrix.
package maintain

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// ---- leases: CAS ladder corners ---------------------------------------------

// errStore wraps memStore with failure knobs.
type errStore struct {
	*memStore
	getErr error
	putErr error
}

func (s *errStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.memStore.Get(ctx, key, opts)
}

func (s *errStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if s.putErr != nil {
		return store.ObjectMeta{}, s.putErr
	}
	return s.memStore.Put(ctx, key, body, opts)
}

func TestStoreLeaser_ErrorsAndSteal(t *testing.T) {
	ctx := context.Background()

	// A non-notfound Get error surfaces.
	es := &errStore{memStore: newMemStore(), getErr: errors.New("get boom")}
	if _, err := (StoreLeaser{St: es}).Acquire(ctx, "compact", "a", "p", time.Minute, 0); err == nil {
		t.Fatal("get error must surface")
	}

	// A create error that is not 412 surfaces.
	es2 := &errStore{memStore: newMemStore(), putErr: errors.New("put boom")}
	if _, err := (StoreLeaser{St: es2}).Acquire(ctx, "compact", "a", "p", time.Minute, 0); err == nil {
		t.Fatal("put error must surface")
	}

	// A corrupted lease body surfaces the decode error.
	st := newMemStore()
	if _, err := st.Put(ctx, store.LeaseKey("compact"), store.PutBody{Bytes: []byte("garbage")},
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := (StoreLeaser{St: st}).Acquire(ctx, "compact", "a", "p", time.Minute, 0); err == nil {
		t.Fatal("corrupt lease must surface")
	}

	// A PUT that always fails CAS exhausts the ladder → ErrLeaseHeld (§3.3).
	st2 := newMemStore()
	past := proto.TimeFromGo(time.Now().Add(-time.Hour))
	old := &proto.Lease{Holder: "ghost", Purpose: "old", Epoch: 3,
		AcquiredAt: &past, ExpiresAt: &past}
	if _, err := st2.Put(ctx, store.LeaseKey("compact"), store.PutBody{Bytes: old.Marshal()},
		store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	es3 := &errStore{memStore: st2, putErr: store.NewPrecondition("cas", "v")}
	if _, err := (StoreLeaser{St: es3}).Acquire(ctx, "compact", "b", "p", time.Minute, 0); err != ErrLeaseHeld {
		t.Fatalf("ladder exhaustion err = %v, want ErrLeaseHeld", err)
	}

	// An expired lease is stolen, and release deletes it.
	if _, err := (StoreLeaser{St: st2}).Acquire(ctx, "compact", "b", "p", time.Minute, 0); err != nil {
		t.Fatalf("steal: %v", err)
	}
	body, _, err := store.GetBytes(ctx, st2, store.LeaseKey("compact"), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stolen := &proto.Lease{}
	if err := stolen.Unmarshal(body); err != nil || stolen.Holder != "b" {
		t.Fatalf("stolen lease = %+v err=%v", stolen, err)
	}
	// The steal must increment the epoch (proto.Lease: incremented on every
	// heartbeat/steal): the pre-steal lease carried epoch 3.
	if stolen.Epoch != 4 {
		t.Fatalf("stolen lease epoch = %d, want 4", stolen.Epoch)
	}
	// Release from the wrong holder leaves the lease; the right one deletes.
	(StoreLeaser{St: st2}).releaseFunc(store.LeaseKey("compact"), "ghost", "")()
	if meta, err := st2.Head(ctx, store.LeaseKey("compact")); err != nil || meta == nil {
		t.Fatalf("wrong-holder release deleted the lease: %v %v", meta, err)
	}
	(StoreLeaser{St: st2}).releaseFunc(store.LeaseKey("compact"), "b", "")()
	if meta, err := st2.Head(ctx, store.LeaseKey("compact")); err != nil || meta != nil {
		t.Fatalf("lease not deleted: %v %v", meta, err)
	}
}

// ---- runRebuild failure matrix ----------------------------------------------

func rebuildSnap(eff *config.Config, m *proto.Manifest) *Snapshot {
	return &Snapshot{ID: "acme/widget", Manifest: m, Eff: eff, Local: LocalState{Present: map[string]bool{}}}
}

func TestRunRebuild_FailureMatrix(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Cache.Dir = t.TempDir()
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	eff.Git.HistoryPack = true
	mst := &proto.Manifest{Repo: "acme/widget", HeadSeq: 1}
	mst.Packs = []*proto.PackRef{pack("old1", 1, 10, 1, 0)}

	base := func(t *testing.T) (*config.Config, *fakeRepo, *Maintainer) {
		t.Helper()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "objects", "pack"), 0o755); err != nil {
			t.Fatal(err)
		}
		repo := &fakeRepo{id: "acme/widget", dir: dir, m: mst, git: &fakeGit{
			fullDiff:    gitPackDiff("newbase"),
			historyPack: "pack-hist",
			commitGraph: "cg1",
		}}
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{}})
		return eff, repo, m
	}

	t.Run("bad-repo-id", func(t *testing.T) {
		_, repo, m := base(t)
		repo.id = "malformed"
		out, detail := m.runRebuild(context.Background(), repo, rebuildSnap(eff, mst), nopLogger{})
		if out != OutcomeError || detail == "" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})
	t.Run("empty-cache-dir", func(t *testing.T) {
		empty := defaultEff()
		empty.Cache.Dir = ""
		_, repo, m := base(t)
		out, detail := m.runRebuild(context.Background(), repo, rebuildSnap(empty, mst), nopLogger{})
		if out != OutcomeError || detail != "cache.dir empty; rebuild pre-flight impossible" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})
	t.Run("lease-held", func(t *testing.T) {
		_, repo, m := base(t)
		m.opt.Leaser = &fakeLeaser{held: map[string]bool{"compact": true}}
		out, detail := m.runRebuild(context.Background(), repo, rebuildSnap(eff, mst), nopLogger{})
		if out != OutcomeHeld || detail != "compact lease held by another instance" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})
	t.Run("lease-error", func(t *testing.T) {
		_, repo, m := base(t)
		m.opt.Leaser = &fakeLeaser{err: errors.New("lease boom")}
		out, _ := m.runRebuild(context.Background(), repo, rebuildSnap(eff, mst), nopLogger{})
		if out != OutcomeError {
			t.Fatalf("outcome=%v", out)
		}
	})
	t.Run("statfs-error", func(t *testing.T) {
		_, repo, m := base(t)
		m.freeSpace = func(string) (uint64, error) { return 0, errors.New("statfs boom") }
		out, detail := m.runRebuild(context.Background(), repo, rebuildSnap(eff, mst), nopLogger{})
		if out != OutcomeError || detail != "statfs: statfs boom" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})
	t.Run("scratch-copy-fails", func(t *testing.T) {
		_, repo, m := base(t)
		repo.dir = filepath.Join(t.TempDir(), "ghost") // copyDir source missing
		out, detail := m.runRebuild(context.Background(), repo, rebuildSnap(eff, mst), nopLogger{})
		if out != OutcomeError || detail != "scratch copy: lstat "+repo.dir+": no such file or directory" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})
	t.Run("repack-fails", func(t *testing.T) {
		_, repo, m := base(t)
		repo.git.failRepack = true
		out, detail := m.runRebuild(context.Background(), repo, rebuildSnap(eff, mst), nopLogger{})
		if out != OutcomeError || detail != "full repack: repack boom" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})
	t.Run("history-fails", func(t *testing.T) {
		_, repo, m := base(t)
		repo.git.failHistory = true
		out, detail := m.runRebuild(context.Background(), repo, rebuildSnap(eff, mst), nopLogger{})
		if out != OutcomeError || detail != "history pack: history boom" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})
	t.Run("commit-graph-fails", func(t *testing.T) {
		_, repo, m := base(t)
		repo.git.failCommitGraph = true
		out, detail := m.runRebuild(context.Background(), repo, rebuildSnap(eff, mst), nopLogger{})
		if out != OutcomeError || detail != "commit-graph: commit-graph boom" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})
}

// TestPublishRebuild_Variants: happy path with commit-graph + history, the
// no-packs refusal, and the install failure path (§6.2 step 5).
func TestPublishRebuild_Variants(t *testing.T) {
	ctx := context.Background()
	eff := defaultEff()
	eff.Cache.Dir = t.TempDir()

	newRepo := func(serving string) *fakeRepo {
		dir := t.TempDir()
		if serving != "" {
			dir = serving
		}
		return &fakeRepo{id: "acme/widget", dir: dir,
			m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 1, Packs: []*proto.PackRef{pack("old1", 1, 10, 1, 0)}}}
	}

	scratch := filepath.Join(eff.Cache.Dir, "_rebuild", "acme", "widget.git")
	scratchPackDir := filepath.Join(scratch, "objects", "pack")
	if err := os.MkdirAll(scratchPackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"pack-aaa.pack": "P", "pack-aaa.idx": "I",
		"pack-hist.pack": "H", "pack-hist.idx": "HI",
		"pack-cg1.commit-graph": "C",
	} {
		if err := os.WriteFile(filepath.Join(scratchPackDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("happy-with-history-and-commit-graph", func(t *testing.T) {
		repo := newRepo("")
		if err := os.MkdirAll(filepath.Join(repo.dir, "objects", "pack"), 0o755); err != nil {
			t.Fatal(err)
		}
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{}})
		marker := &rebuildMarker{StartedHeadSeq: 1, Phase: phaseCommitGraph,
			NewPacks: []string{"aaa"}, History: "hist", CommitGraph: "cg1"}
		out, detail := m.publishRebuild(ctx, repo, rebuildSnap(eff, repo.m), scratch, marker, nopLogger{})
		if out != OutcomeOK || detail == "" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
		// Base + history + commit-graph all landed create-if-absent.
		for key, want := range map[string]string{
			"repos/acme/widget/wal/aaa.pack":         "P",
			"repos/acme/widget/wal/aaa.idx":          "I",
			"repos/acme/widget/wal/hist.pack":        "H",
			"repos/acme/widget/wal/cg1.commit-graph": "C",
		} {
			body, _, err := store.GetBytes(ctx, eng.st, key, store.GetOptions{})
			if err != nil || string(body) != want {
				t.Fatalf("key %s: %q %v", key, body, err)
			}
		}
		// The COMPACT publish superseded old1 and annotated the base.
		if len(repo.compacts) != 1 || len(repo.compacts[0].supersedes) != 1 {
			t.Fatalf("compacts = %+v", repo.compacts)
		}
		if len(repo.annotated) != 1 || repo.annotated[0] != "aaa" {
			t.Fatalf("annotated = %v", repo.annotated)
		}
	})

	t.Run("no-packs", func(t *testing.T) {
		repo := newRepo("")
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{}})
		out, detail := m.publishRebuild(ctx, repo, rebuildSnap(eff, repo.m), scratch,
			&rebuildMarker{StartedHeadSeq: 1, Phase: phaseCommitGraph}, nopLogger{})
		if out != OutcomeError || detail != "rebuild produced no packs" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})

	t.Run("install-fails", func(t *testing.T) {
		// A serving copy without an objects/pack dir cannot take the install.
		repo := newRepo(filepath.Join(t.TempDir(), "empty"))
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{}})
		out, detail := m.publishRebuild(ctx, repo, rebuildSnap(eff, repo.m), scratch,
			&rebuildMarker{StartedHeadSeq: 1, Phase: phaseCommitGraph, NewPacks: []string{"aaa"}}, nopLogger{})
		if out != OutcomeError || detail == "" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})
}

// ---- the geometric fold and the retention GC ---------------------------------

func TestRunCompact_Branches(t *testing.T) {
	ctx := context.Background()
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = true
	eff.Compaction.RetentionSuperseded = config.Duration(7 * 24 * time.Hour)
	eff.Git.HistoryPack = true

	manifest := func() *proto.Manifest {
		m := &proto.Manifest{Repo: "acme/widget", HeadSeq: 2, MinSeq: 1}
		m.Packs = []*proto.PackRef{
			nil,
			pack("base2", 1, 10, 1, 2), // tier-2 base: kept
			pack("t0a", 2, 10, 1, 0),   // folded
			pack("t0b", 3, 10, 1, 0),   // folded
		}
		return m
	}

	t.Run("repack-error", func(t *testing.T) {
		repo := &fakeRepo{id: "acme/widget", dir: t.TempDir(), m: manifest(),
			git: &fakeGit{geoErr: errors.New("geo boom")}}
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{}})
		out, detail := m.runCompact(ctx, repo, rebuildSnap(eff, repo.m), nopLogger{})
		if out != OutcomeError || detail != "repack: geo boom" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})
	t.Run("no-fold-produced", func(t *testing.T) {
		repo := &fakeRepo{id: "acme/widget", dir: t.TempDir(), m: manifest(),
			git: &fakeGit{geoDiff: &git.PackDiff{}}}
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{}})
		out, detail := m.runCompact(ctx, repo, rebuildSnap(eff, repo.m), nopLogger{})
		if out != OutcomeOK || detail != "no fold produced" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})
	t.Run("publish-error", func(t *testing.T) {
		repo := &fakeRepo{id: "acme/widget", dir: t.TempDir(), m: manifest(),
			git: &fakeGit{geoDiff: &git.PackDiff{New: []string{"pack-new.pack"}}}, compactErr: errors.New("publish boom")}
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{Leaser: &fakeLeaser{}})
		out, detail := m.runCompact(ctx, repo, rebuildSnap(eff, repo.m), nopLogger{})
		if out != OutcomeError || detail == "" {
			t.Fatalf("outcome=%v detail=%q", out, detail)
		}
	})
	t.Run("leaser-fallback", func(t *testing.T) {
		repo := &fakeRepo{id: "acme/widget", dir: t.TempDir(), m: manifest(),
			git: &fakeGit{geoDiff: &git.PackDiff{}}}
		eng := newFakeEngine(eff, repo)
		m := New(eng, Options{}) // no Leaser → the StoreLeaser fallback
		out, _ := m.runCompact(ctx, repo, rebuildSnap(eff, repo.m), nopLogger{})
		if out != OutcomeOK {
			t.Fatalf("outcome=%v", out)
		}
	})
}

func TestGcSuperseded(t *testing.T) {
	ctx := context.Background()
	eff := defaultEff()
	eff.Compaction.RetentionSuperseded = config.Duration(7 * 24 * time.Hour)
	now := time.Unix(1_800_000_000, 0).UTC()
	old := now.Add(-30 * 24 * time.Hour)
	recent := now.Add(-24 * time.Hour)

	repo := &fakeRepo{id: "acme/widget", dir: t.TempDir()}
	repo.m = &proto.Manifest{Repo: "acme/widget", HeadSeq: 3, MinSeq: 1}
	repo.m.Packs = []*proto.PackRef{pack("live", 1, 10, 1, 0)}
	repo.entries = []*proto.LogEntry{
		{Seq: 1, Kind: proto.EntryKindCompact, CreatedAt: ptrTs(old), Supersedes: []string{"old"}},
		{Seq: 2, Kind: proto.EntryKindCompact, CreatedAt: ptrTs(recent), Supersedes: []string{"fresh"}},
		{Seq: 2, Kind: proto.EntryKindRefUpdate, CreatedAt: ptrTs(old)},
		{Seq: 3, Kind: proto.EntryKindCompact, CreatedAt: nil},
	}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}})
	m.now = func() time.Time { return now }
	prefix := "repos/acme/widget/"
	for _, k := range []string{"old.pack", "old.idx", "fresh.pack", "orphan.pack", "live.pack"} {
		if _, err := eng.st.Put(ctx, prefix+"wal/"+k, store.PutBody{Bytes: []byte("x")},
			store.PutOptions{Mode: store.PutCreate}); err != nil {
			t.Fatal(err)
		}
	}
	snap := rebuildSnap(eff, repo.m)
	removed, err := m.gcSuperseded(ctx, repo, snap)
	if err != nil || removed != 2 {
		t.Fatalf("removed=%d err=%v, want 2 (old pack+idx; fresh within retention, orphan unknown, live live)", removed, err)
	}
	if _, _, err := store.GetBytes(ctx, eng.st, prefix+"wal/old.pack", store.GetOptions{}); !store.IsNotFound(err) {
		t.Fatalf("old.pack not removed: %v", err)
	}
	for _, kept := range []string{"fresh.pack", "orphan.pack", "live.pack"} {
		if _, _, err := store.GetBytes(ctx, eng.st, prefix+"wal/"+kept, store.GetOptions{}); err != nil {
			t.Fatalf("%s must be kept: %v", kept, err)
		}
	}

	// Retention 0 and a nil store both no-op.
	eff.Compaction.RetentionSuperseded = 0
	if removed, err := m.gcSuperseded(ctx, repo, snap); err != nil || removed != 0 {
		t.Fatalf("retention 0: removed=%d err=%v", removed, err)
	}
	m2 := New(nil, Options{})
	if removed, err := m2.gcSuperseded(ctx, repo, snap); err != nil || removed != 0 {
		t.Fatalf("nil store: removed=%d err=%v", removed, err)
	}
}

// ---- the loop seams ----------------------------------------------------------

func TestMaintainer_NilEngineSeams(t *testing.T) {
	m := New(nil, Options{})
	if st := m.store(); st != nil {
		t.Fatal("nil engine store must be nil")
	}
	if m.host() != "unknown" {
		t.Fatalf("host = %q", m.host())
	}
	if m.hbTicks() != nil {
		t.Fatal("no ticker before hbStart")
	}
	m.hbTick(context.Background()) // no heartbeat → no-op
}

func TestRunUnit_TimeoutLeavesTaskRunning(t *testing.T) {
	eff := defaultEff()
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 1}}
	eng := newFakeEngine(eff, repo)
	block := make(chan struct{})
	eng.tasks.hook = func(_ string, _ string, fn func(ctx context.Context, t TaskLogger) error) error {
		<-block // the unit hangs; the §3.2 wait cap moves the pass on
		return fn(context.Background(), nopLogger{})
	}
	m := New(eng, Options{Leaser: &fakeLeaser{}, UnitCap: time.Millisecond})
	defer close(block)
	outcome, detail := m.runUnit(context.Background(), repo, rebuildSnap(eff, repo.m),
		Selection{Kind: KindCompact, Reason: "test"})
	if outcome != OutcomeTimeout || detail != "still running; will re-check next pass" {
		t.Fatalf("outcome=%v detail=%q", outcome, detail)
	}
}

// nilCfgEngine wraps fakeEngine with a nil host config (§3.2 pass skip).
type nilCfgEngine struct{ *fakeEngine }

func (e nilCfgEngine) HostConfig() *config.Config { return nil }

func TestRunPass_NilConfigSkips(t *testing.T) {
	eff := defaultEff()
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}}
	eng := newFakeEngine(eff, repo)
	m := New(nilCfgEngine{eng}, Options{Leaser: &fakeLeaser{}})
	m.RunPass(context.Background())
	if m.Metrics().Passes != 1 {
		t.Fatal("pass must still be counted")
	}
}

func TestRunPass_SnapshotFailureLogged(t *testing.T) {
	eff := defaultEff()
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 1}}
	repo.syncErr = errors.New("sync boom")
	eng := newFakeEngine(eff, repo)
	// A repo id that Open resolves but whose SyncRefs fails → loadRepo error.
	m := New(eng, Options{Leaser: &fakeLeaser{}})
	m.RunPass(context.Background()) // must not panic; failure logged
	if m.Metrics().Passes != 1 {
		t.Fatal("pass must complete")
	}
}

func TestFollowRound_Helpers(t *testing.T) {
	if isZeroHex("") {
		t.Fatal("empty is not zero")
	}
	if !isZeroHex("0000") || isZeroHex("0001") {
		t.Fatal("zero hex detection")
	}
	detail := movedDetail([]*proto.RefUpdate{
		{Name: "refs/heads/main", OldOid: "1234567890", NewOid: "0987654321"},
	})
	if detail != "refs/heads/main 1234567→0987654" {
		t.Fatalf("detail = %q", detail)
	}
}

func TestFollowOnce_Branches(t *testing.T) {
	ctx := context.Background()

	// Nothing to follow: no upstream configured → nil, no round.
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	eff.Maintenance.FollowInterval = -1
	eff.Upstream.Git = ""
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}, refs: map[string]string{}}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}})
	if err := m.followOnce(ctx, "acme/widget"); err != nil {
		t.Fatalf("no upstream: %v", err)
	}
	if _, ok := m.LastRound("acme/widget"); ok {
		t.Fatal("no round must be recorded")
	}

	// Open failure records a failed round.
	m2 := New(eng, Options{Leaser: &fakeLeaser{}})
	if err := m2.followOnce(ctx, "ghost/repo"); err == nil {
		t.Fatal("open failure must surface")
	}
	if round, ok := m2.LastRound("ghost/repo"); !ok || round.Outcome != "failed" {
		t.Fatalf("round = %+v", round)
	}

	// A fetcher-less maintainer fails the round cleanly.
	eff2 := defaultEff()
	eff2.Maintenance.FollowInterval = -1
	eff2.Upstream.Git = "https://example.com/x.git"
	eff2.Upstream.Follow = []string{"refs/heads/main"}
	repo2 := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget"}, refs: map[string]string{}}
	eng2 := newFakeEngine(eff2, repo2)
	m3 := New(eng2, Options{Leaser: &fakeLeaser{}})
	if err := m3.followOnce(ctx, "acme/widget"); err != nil {
		t.Fatalf("no fetcher: %v", err)
	}
	if round, _ := m3.LastRound("acme/widget"); round.Outcome != "failed" {
		t.Fatalf("outcome = %q, want failed", round.Outcome)
	}
}

// ---- the .rev writer error matrix (§10) --------------------------------------

func TestBuildRevFile_ErrorMatrix(t *testing.T) {
	valid := syntheticIdx()
	if _, err := buildRevFile(valid, 20); err != nil {
		t.Fatalf("valid idx rejected: %v", err)
	}
	if _, err := buildRevFile(valid, 21); err != errBadIdx {
		t.Fatal("bad oidLen must be rejected")
	}
	if _, err := buildRevFile(valid[:100], 20); err != errBadIdx {
		t.Fatal("short idx must be rejected")
	}
	badMagic := append([]byte{}, valid...)
	badMagic[1] = 'X'
	if _, err := buildRevFile(badMagic, 20); err != errBadIdx {
		t.Fatal("bad magic must be rejected")
	}
	badVersion := append([]byte{}, valid...)
	badVersion[7] = 3
	if _, err := buildRevFile(badVersion, 20); err != errBadIdx {
		t.Fatal("version 3 must be rejected")
	}
	zeroCount := append([]byte{}, valid...)
	for i := 8 + 1020; i < 8+1024; i++ {
		zeroCount[i] = 0
	}
	if _, err := buildRevFile(zeroCount, 20); err != errBadIdx {
		t.Fatal("count 0 must be rejected")
	}
	// A large-offset entry extends the trailer position; an idx that stops
	// where the 64-bit table should be is rejected.
	large := append([]byte{}, valid...)
	offs := 8 + 1024 + 20 + 4
	binary.BigEndian.PutUint32(large[offs:offs+4], 0x80000000) // offsets[0] MSB
	if _, err := buildRevFile(large, 20); err != errBadIdx {
		t.Fatalf("idx with large-offset slot but no table must be rejected: %v", err)
	}
}

func TestRunRevIndex_ErrorPaths(t *testing.T) {
	checksum := "beef000000000000000000000000000000000000"
	dir := t.TempDir()
	packDir := filepath.Join(dir, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	candidate := pack(checksum, 1, 100, 300_000, 0)

	// The idx is a directory → a non-NotExist read error.
	repo := &fakeRepo{id: "acme/widget", dir: dir,
		m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 1, Packs: []*proto.PackRef{candidate}}}
	if err := os.Mkdir(filepath.Join(packDir, "pack-"+checksum+".idx"), 0o755); err != nil {
		t.Fatal(err)
	}
	eng := newFakeEngine(eff, repo)
	m := New(eng, Options{Leaser: &fakeLeaser{}})
	out, detail := m.runRevIndex(context.Background(), repo, &Snapshot{Eff: eff, Manifest: repo.m}, nopLogger{})
	if out != OutcomeError || detail[:9] != "read idx:" {
		t.Fatalf("outcome=%v detail=%q", out, detail)
	}

	// A sha256 repo rejects the 20-byte-trailer synthetic idx at build time.
	repo2 := &fakeRepo{id: "acme/widget", dir: dir,
		m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 1, Packs: []*proto.PackRef{candidate}}}
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte("[core]\n\trepositoryformatversion = 0\n[extensions]\n\tobjectformat = sha256\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(packDir, "pack-"+checksum+".idx")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack-"+checksum+".idx"), syntheticIdx(), 0o644); err != nil {
		t.Fatal(err)
	}
	eng2 := newFakeEngine(eff, repo2)
	m2 := New(eng2, Options{Leaser: &fakeLeaser{}})
	out, detail = m2.runRevIndex(context.Background(), repo2, &Snapshot{Eff: eff, Manifest: repo2.m}, nopLogger{})
	if out != OutcomeError || detail[:11] != "build .rev:" {
		t.Fatalf("sha256 idx: outcome=%v detail=%q", out, detail)
	}
}

// ---- the rev writer large-offset round trip ----------------------------------

func TestBuildRevFile_LargeOffsets(t *testing.T) {
	// A synthetic idx whose single object uses a 64-bit large offset: the
	// trailer must move past the N*8 table and the entry stays byte-stable.
	idx := syntheticIdx()
	// Rewrite offsets[0] to MSB|0 → slot 0 of the large table (value 100).
	offsAt := 8 + 1024 + 20 + 4
	binary.BigEndian.PutUint32(idx[offsAt:offsAt+4], 0x80000000)
	large := make([]byte, 0, len(idx)+8)
	largeAt := offsAt + 4
	large = append(large, idx[:largeAt]...)
	large = binary.BigEndian.AppendUint64(large, 0x1000) // the large offset
	trailer := sha1Sum(idx[offsAt+4:])
	large = append(large, trailer[:]...)
	sum := sha1.Sum(large)
	large = append(large, sum[:]...)

	rev, err := buildRevFile(large, 20)
	if err != nil {
		t.Fatalf("large-offset idx rejected: %v", err)
	}
	if len(rev) != 12+4+20+20 {
		t.Fatalf("rev length = %d", len(rev))
	}
}
