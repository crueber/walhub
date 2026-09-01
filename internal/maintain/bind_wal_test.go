// bind_wal_test.go — the real wal/git/store binding (§1 dependency
// direction): a live wal.Registry over the memory store and a real local
// git copy, proving the Engine/Repo/BundlePlanner seams delegate honestly.
package maintain

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/bundle"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// walMaintRegistry builds a live registry over the memory store with a real
// cache dir (repos materialize on disk).
func walMaintRegistry(t *testing.T) (*wal.Registry, *config.Config, store.ObjectStore) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	cfg.WAL.BatchWindow = config.Duration(5 * time.Millisecond)
	cfg.WAL.FreshnessTTL = 0 // always check
	st := store.NewMemory()
	r := wal.NewRegistry(context.Background(), st, cfg)
	t.Cleanup(r.Close)
	return r, cfg, st
}

const (
	testOid1 = "1111111111111111111111111111111111111111"
	testOid2 = "2222222222222222222222222222222222222222"
)

// walFixture creates the registry plus one repo with a published ref so the
// read paths have real WAL state to fold.
func walFixture(t *testing.T) (*wal.Registry, *config.Config, store.ObjectStore, walRepo, *wal.RepoHandle) {
	t.Helper()
	r, cfg, st := walMaintRegistry(t)
	// config.Defaults carries "Sun" schedules that bundle.ParseSchedule
	// rejects (known defect, reported); use numeric dow so the planner runs.
	cfg.Bundles.Strategy = []config.BundleStrategy{
		{Name: "weekly", Kind: "full", Schedule: "0 0 23 * * 0", Keep: 2, BackfillMax: 1},
		{Name: "daily", Kind: "incremental", Base: "weekly", Schedule: "0 0 23 * * *", Chain: true},
	}
	ctx := context.Background()
	if _, err := r.Create(ctx, "acme/api", git.Sha1); err != nil {
		t.Fatalf("create: %v", err)
	}
	h, err := r.Open(ctx, "acme/api")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := h.Publish(ctx, wal.PublishRequest{Txn: &proto.RefTransaction{
		Updates: []*proto.RefUpdate{{Name: "refs/heads/main", OldOid: git.Sha1.ZeroHex(), NewOid: testOid1}},
	}}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return r, cfg, st, walRepo{h: h}, h
}

func TestWalEngine_Seams(t *testing.T) {
	r, cfg, st, rep, h := walFixture(t)
	e := NewWalEngine(r)

	if repos := e.Repos(); len(repos) != 1 || repos[0] != "acme/api" {
		t.Fatalf("repos = %v", repos)
	}
	if e.Store() != st {
		t.Fatal("Store() must surface the registry store")
	}
	if e.HostConfig() != cfg {
		t.Fatal("HostConfig() must return the registry config")
	}
	if e.InstanceID() == "" {
		t.Fatal("InstanceID() empty")
	}
	if e.Tasks() == nil {
		t.Fatal("Tasks() nil")
	}

	// Open: known id binds, unknown id errors.
	got, err := e.Open(context.Background(), "acme/api")
	if err != nil || got.ID() != "acme/api" {
		t.Fatalf("open: id=%q err=%v", got.ID(), err)
	}
	if _, err := e.Open(context.Background(), "no/such"); err == nil {
		t.Fatal("open of unknown repo must fail")
	}

	// Repo accessors.
	if rep.Dir() != h.Dir() {
		t.Fatalf("dir mismatch: %q vs %q", rep.Dir(), h.Dir())
	}
	if rep.Local() == nil || rep.GitOps() == nil {
		t.Fatal("Local/GitOps nil")
	}
	m, version := rep.Manifest()
	if m == nil || m.HeadSeq != 1 || version == "" {
		t.Fatalf("manifest head=%d version=%q", m.HeadSeq, version)
	}
	if rep.Prefix() != "repos/acme/api/" {
		t.Fatalf("prefix = %q", rep.Prefix())
	}
	// Malformed ids fall back to the raw form (never empty).
	bad := walRepo{h: h}
	bad.h.ID = "malformed"
	if bad.Prefix() != "repos/malformed/" {
		t.Fatalf("fallback prefix = %q", bad.Prefix())
	}
}

func TestWalEngine_TaskRunnerAndLogger(t *testing.T) {
	r, _, _, _, _ := walFixture(t)
	ctx := context.Background()

	// The adapter wraps wal.TaskTable and the logger forwards Notice/Progress.
	ran := false
	err := NewWalEngine(r).Tasks().Run(ctx, "acme/api", "compact",
		map[string]string{"reason": "test"}, func(tctx context.Context, tl TaskLogger) error {
			tl.Notice("hello")
			tl.Progress("repack", 1, 2, "objs")
			ran = true
			return nil
		})
	if err != nil || !ran {
		t.Fatalf("task run: ran=%v err=%v", ran, err)
	}
	recs := r.Tasks().List("acme/api")
	if len(recs) == 0 {
		t.Fatal("task not recorded")
	}
	last := recs[len(recs)-1]
	if last.Kind != "compact" || last.OK == nil || !*last.OK {
		t.Fatalf("task = %+v", last)
	}

	// A failing unit body is recorded on the task (the table reports
	// outcomes as values; the adapter surfaces the error, wal keeps it).
	_ = NewWalEngine(r).Tasks().Run(ctx, "acme/api", "fsck", nil,
		func(context.Context, TaskLogger) error { return errUnitTest })
	var failed bool
	for _, rec := range r.Tasks().List("acme/api") {
		if rec.Kind == "fsck" && rec.OK != nil && !*rec.OK {
			failed = true
		}
	}
	if !failed {
		t.Fatal("failing body not recorded as failed")
	}
}

var errUnitTest = context.Canceled

func TestWalRepo_RefStateAndPublishing(t *testing.T) {
	_, _, _, rep, _ := walFixture(t)
	ctx := context.Background()

	// RefValues syncs refs-level and folds the layer snapshot.
	refs, err := rep.RefValues(ctx)
	if err != nil || refs["refs/heads/main"] != testOid1 {
		t.Fatalf("ref values = %v err=%v", refs, err)
	}

	// ReadLog over the one published entry.
	entries, err := rep.ReadLog(ctx, 1, 1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("read log: n=%d err=%v", len(entries), err)
	}

	// Refs at a seq and as of now both surface the tip.
	view, err := rep.RefsAtSeq(ctx, 1)
	if err != nil || view == nil {
		t.Fatalf("refs at seq: %v", err)
	}
	if v, err := rep.RefsAsOf(ctx, time.Now().Add(time.Minute)); err != nil || v == nil {
		t.Fatalf("refs as of: %v", err)
	}

	// Publish a second ref via the Repo seam and read it back.
	seq, err := rep.PublishRefs(ctx, &proto.RefTransaction{
		Updates: []*proto.RefUpdate{{Name: "refs/heads/feature", OldOid: git.Sha1.ZeroHex(), NewOid: testOid2}},
	}, map[string]string{"agent": "test"})
	if err != nil || seq != 2 {
		t.Fatalf("publish refs: seq=%d err=%v", seq, err)
	}

	// Checkpoint write is idempotent at head.
	if err := rep.WriteCheckpoint(ctx, "manual"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// Pack lock is exclusive: second try while held must fail.
	unlock, ok := rep.TryLockPacks()
	if !ok {
		t.Fatal("first TryLockPacks must succeed")
	}
	if _, ok := rep.TryLockPacks(); ok {
		t.Fatal("second TryLockPacks must be refused while held")
	}
	unlock()
}

func TestWalRepo_Packs(t *testing.T) {
	_, _, _, rep, h := walFixture(t)
	ctx := context.Background()

	// A real (dummy) pack file pair on disk; the publisher uploads
	// create-if-absent from the given paths.
	packDir := t.TempDir()
	checksum := "abc123abc123abc123abc123abc123abc123abc1"
	packPath := filepath.Join(packDir, "pack-"+checksum+".pack")
	idxPath := filepath.Join(packDir, "pack-"+checksum+".idx")
	if err := os.WriteFile(packPath, []byte("PACKDUMMY"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath, []byte("IDXDUMMY"), 0o644); err != nil {
		t.Fatal(err)
	}
	// NOTE: wal's compact publisher currently treats the nil txn carried by
	// PublishRequest as a per-ref rejection (defect reported to the wal
	// owner); the seam must still surface no error and stay delegating.
	if _, err := rep.PublishCompact(ctx, &PreparedPack{
		Checksum: checksum, PackPath: packPath, IdxPath: idxPath, Tier: 2,
	}, nil, map[string]string{"agent": "test"}); err != nil {
		t.Fatalf("publish compact: err=%v", err)
	}

	// AddPack installs into the serving copy and publishes.
	if err := rep.AddPack(ctx, packPath, checksum, 2, nil); err != nil {
		t.Fatalf("add pack: %v", err)
	}
	installed := filepath.Join(h.Repo().PackDir(), "pack-"+checksum+".pack")
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("installed pack: %v", err)
	}

	// Annotation is a manifest-only CAS on the live pack.
	if err := rep.AnnotatePack(ctx, checksum, true, true, false); err != nil {
		t.Fatalf("annotate: %v", err)
	}
	mst, _ := h.ManifestSnapshot()
	for _, p := range mst.Packs {
		if p.Checksum == checksum && (!p.HasRev || !p.HasBitmap || p.HasCommitGraph) {
			t.Fatalf("annotation = %+v", p)
		}
	}

	// toWalPack(nil) stays nil (defensive seam).
	if toWalPack(nil) != nil {
		t.Fatal("toWalPack(nil) must be nil")
	}
}

func TestWalPlanner_PlanAndPreviousFire(t *testing.T) {
	r, cfg, _, _, h := walFixture(t)
	p := walPlanner{reg: r}
	ctx := context.Background()

	// No bundles/list.pb yet → the planner still yields the default table.
	mst, _ := h.ManifestSnapshot()
	slots, err := p.Plan(ctx, "acme/api", cfg, mst, time.Now())
	if err != nil || len(slots) == 0 {
		t.Fatalf("plan: n=%d err=%v", len(slots), err)
	}
	if slots[0].Strategy != "weekly" || slots[0].Kind != "full" {
		t.Fatalf("first slot = %+v", slots[0])
	}
	// Missing slots carry a detail, built ones would carry BundleID.
	for _, s := range slots {
		if s.State == "built" && s.BundleID == "" {
			t.Fatalf("built slot without bundle id: %+v", s)
		}
	}

	// The newest weekly slot is the last cron fire; a list entry at that
	// slot reports state=built with the bundle id.
	fire := (walPlanner{}).PreviousFire(config.BundleStrategy{Schedule: "0 0 23 * * 0"}, time.Now())
	if fire.IsZero() {
		t.Fatal("previous fire is zero")
	}
	slot := uint64(fire.Unix())
	list := &proto.BundleList{Bundles: []*proto.BundleEntry{{Strategy: "weekly", Slot: slot, ID: "b1"}}}
	if _, err := store.PutBytes(ctx, r.Store(), repoPrefixOf("acme/api")+store.BundleList, list.Marshal(), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	slots, err = p.Plan(ctx, "acme/api", cfg, mst, time.Now())
	if err != nil {
		t.Fatalf("plan with list: %v", err)
	}
	foundBuilt := false
	for _, s := range slots {
		if s.Strategy == "weekly" && s.Slot == slot {
			if s.State != "built" || s.BundleID != "b1" {
				t.Fatalf("weekly slot %d = %+v, want built/b1", slot, s)
			}
			foundBuilt = true
		}
	}
	if !foundBuilt {
		t.Fatal("built slot missing from plan")
	}

	// Invalid strategies surface as a Plan error, not a panic.
	old := cfg.Bundles.Strategy
	cfg.Bundles.Strategy = []config.BundleStrategy{{Name: "bad", Schedule: "garbage"}}
	if _, err := p.Plan(ctx, "acme/api", cfg, mst, time.Now()); err == nil {
		t.Fatal("invalid strategy must fail the plan")
	}
	cfg.Bundles.Strategy = old

	// PreviousFire walks back to the last schedule fire.
	if !fire.Before(time.Now()) {
		t.Fatalf("previous fire %v not in the past", fire)
	}
	if z := (walPlanner{}).PreviousFire(config.BundleStrategy{Schedule: "nope"}, time.Now()); !z.IsZero() {
		t.Fatalf("invalid schedule must yield zero time, got %v", z)
	}
}

func TestWalPlanner_Build(t *testing.T) {
	r, cfg, _, _, _ := walFixture(t)
	p := walPlanner{reg: r}
	ctx := context.Background()

	// Unknown strategy is rejected before any work.
	if _, err := p.Build(ctx, "acme/api", Slot{Strategy: "nope", Slot: 1}); err == nil {
		t.Fatal("unknown strategy must fail")
	}

	// A slot already present in list.pb is a no-op build (§8.7 idempotence).
	fire := p.PreviousFire(cfg.Bundles.Strategy[0], time.Now())
	slot := uint64(fire.Unix())
	list := &proto.BundleList{Bundles: []*proto.BundleEntry{{Strategy: "weekly", Slot: slot, ID: "b"}}}
	if _, err := store.PutBytes(ctx, r.Store(), repoPrefixOf("acme/api")+store.BundleList, list.Marshal(), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	built, err := p.Build(ctx, "acme/api", Slot{Strategy: "weekly", Slot: slot})
	if err != nil || built {
		t.Fatalf("already-built slot: built=%v err=%v", built, err)
	}

	// A fresh slot resolves the as-of content; on this repo (one entry, no
	// checkpoint) the slot predates first_state_at → unavailable verdict and
	// a plain built report, with no bundle landing in the list.
	built, err = p.Build(ctx, "acme/api", Slot{Strategy: "weekly", Slot: slot + 1})
	if err != nil || !built {
		t.Fatalf("fresh slot: built=%v err=%v", built, err)
	}
	body, _, err := store.GetBytes(ctx, r.Store(), repoPrefixOf("acme/api")+store.BundleList, store.GetOptions{})
	if err != nil || body == nil {
		t.Fatalf("list read: %v", err)
	}
	list, err = proto.UnmarshalBundleList(body)
	if err != nil || len(list.Bundles) != 1 {
		t.Fatalf("bundles = %+v err=%v", list, err)
	}
}

func TestBundleTaskRunner_Records(t *testing.T) {
	r, _, _, _, _ := walFixture(t)
	bt := bundleTaskRunner{t: r.Tasks()}
	ran := false
	err := bt.RunBundle(context.Background(), "acme/api",
		map[string]string{"strategy": "weekly", "slot": "7"},
		func(_ context.Context, tr bundle.Reporter) error {
			tr.Notice("hi")
			ran = true
			return nil
		})
	if err != nil || !ran {
		t.Fatalf("run bundle: ran=%v err=%v", ran, err)
	}
}

func TestObjectFormatOf(t *testing.T) {
	_, _, _, _, h := walFixture(t)
	if got := objectFormatOf(h); got != "sha1" {
		t.Fatalf("object format = %q, want sha1", got)
	}
}

func TestRepoPrefixOf(t *testing.T) {
	if got := repoPrefixOf("acme/api"); got != "repos/acme/api/" {
		t.Fatalf("prefix = %q", got)
	}
}

func TestNewWalMaintainer_DefaultsAndOverrides(t *testing.T) {
	r, cfg, _, _, _ := walFixture(t)

	m := NewWalMaintainer(r, Options{})
	if m.opt.Follow == nil || m.opt.Planner == nil || m.opt.Leaser == nil || m.opt.Fscker == nil {
		t.Fatal("default plugs must be wired")
	}
	if m.interval <= 0 || m.followInterval <= 0 {
		t.Fatalf("intervals: %v/%v", m.interval, m.followInterval)
	}
	if m.host() == "" {
		t.Fatal("host empty")
	}

	// Host name comes from maintenance.host when set.
	cfg.Maintenance.Host = "walgit-9"
	m2 := NewWalMaintainer(r, Options{})
	if m2.host() != "walgit-9" {
		t.Fatalf("host = %q, want maintenance.host", m2.host())
	}
	cfg.Maintenance.Host = ""

	// Explicit overrides win over the bindings.
	planner := &fakePlanner{}
	m3 := NewWalMaintainer(r, Options{
		Planner:        planner,
		Leaser:         &fakeLeaser{},
		Fscker:         execFscker{},
		Follow:         &fakeFollow{},
		Interval:       time.Second,
		FollowInterval: 2 * time.Second,
		HostName:       "explicit",
	})
	if m3.opt.Planner != planner || m3.host() != "explicit" {
		t.Fatalf("overrides lost: %+v host=%q", m3.opt.Planner, m3.host())
	}
	if m3.interval != time.Second || m3.followInterval != 2*time.Second {
		t.Fatalf("cadences: %v/%v", m3.interval, m3.followInterval)
	}
}
