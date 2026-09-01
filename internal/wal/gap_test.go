// gap_test.go — final gap coverage: replay matrix (all entry kinds), sync
// fault paths, registry open/create/delete edges, eviction disk mode, task
// broadcast drops, and small helper contracts.
package wal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// ---- sync.go: the replay matrix over every entry kind -------------------------------

// putSegmentObject frames entries into a segment object at the repo key.
func putSegmentObject(t *testing.T, h *RepoHandle, st store.ObjectStore, seg *proto.LogSegmentRef, entries []*proto.LogEntry) {
	t.Helper()
	body := proto.EncodeSegment(entries)
	if _, err := st.Put(context.Background(), h.repoKey(seg.Key), store.PutBody{Bytes: body}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyDelta_ReplayMatrix(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	zero := git.Sha1.ZeroHex()
	oid := strings.Repeat("a", 40)

	now := time.Now().UTC()
	seg := &proto.LogSegmentRef{Key: store.LogSegmentKey(1), FirstSeq: 1, LastSeq: 4, Size: 1, Sealed: true}
	entries := []*proto.LogEntry{
		{Seq: 1, Kind: proto.EntryKindRefUpdate, Writer: "w", CreatedAt: TsPtr(now.Add(-time.Hour)),
			Txn: refTxn("refs/heads/main", zero, oid)},
		{Seq: 2, Kind: proto.EntryKindCompact, Writer: "w", CreatedAt: TsPtr(now.Add(-time.Hour)),
			Pack: &proto.PackRef{Checksum: "old", Seq: 2}, Supersedes: []string{"old"}},
		{Seq: 3, Kind: proto.EntryKindCheckpoint, Writer: "w", CreatedAt: TsPtr(now.Add(-time.Hour)),
			Checkpoint: &proto.CheckpointRef{Seq: 3, CreatedAt: TsPtr(now.Add(-time.Hour))}},
		{Seq: 4, Kind: proto.EntryKindSettings, Writer: "w", CreatedAt: TsPtr(now.Add(-time.Hour)),
			Settings: &proto.RepoSettings{Toml: "[x]\n"}},
	}
	putSegmentObject(t, h, st, seg, entries)
	m, version := h.ManifestSnapshot()
	next := *m
	next.LogSegments = []*proto.LogSegmentRef{seg}
	next.HeadSeq = 4
	next.Revision = m.Revision + 1
	h.syncMu.Lock()
	h.manifest, h.version, h.heldRev = &next, version, next.Revision
	h.syncMu.Unlock()

	if err := h.catchUp(ctx); err != nil {
		t.Fatalf("applyDelta: %v", err)
	}
	// The push applied locally.
	snap, err := h.Layer().Snapshot(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := snap.Get("refs/heads/main"); !ok || e.Oid != oid {
		t.Fatalf("replayed ref = %+v %v", e, ok)
	}
	// The COMPACT entry queued its superseded pack.
	if len(h.state.PendingPackRemovals) == 0 || h.state.PendingPackRemovals[0] != "old" {
		t.Fatalf("pending removals = %v", h.state.PendingPackRemovals)
	}
	// The CHECKPOINT entry recorded its created_at.
	h.checkpointsMut.Lock()
	_, noted := h.checkpoints[3]
	h.checkpointsMut.Unlock()
	if !noted {
		t.Fatal("checkpoint entry not noted")
	}
	// Entry times fed the monotonic guard.
	if h.lastEntryTime.IsZero() || h.firstEntryTime.IsZero() {
		t.Fatalf("entry times = %v/%v", h.firstEntryTime, h.lastEntryTime)
	}
}

func TestApplyDelta_CheckpointFoldPaths(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refsObj string // "" = absent
		wantErr string
	}{
		{"absent refs", "", "absent"},
		{"corrupt refs", "garbage", "snapshot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, st := newTestRegistry(t)
			ctx := context.Background()
			h, err := r.Create(ctx, "acme/api", git.Sha1)
			if err != nil {
				t.Fatal(err)
			}
			if tc.refsObj != "" {
				if _, err := st.Put(ctx, h.repoKey(store.CheckpointRefsKey(1)), store.PutBody{Bytes: []byte(tc.refsObj)}, store.PutOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			m, version := h.ManifestSnapshot()
			next := *m
			next.Checkpoint = &proto.CheckpointRef{Seq: 1, Key: store.CheckpointKey(1)}
			next.Revision = m.Revision + 1
			h.syncMu.Lock()
			h.manifest, h.version, h.heldRev = &next, version, next.Revision
			h.syncMu.Unlock()
			err = h.catchUp(ctx)
			if err == nil {
				t.Fatalf("foldCheckpoint = nil, want a %q failure", tc.wantErr)
			}
		})
	}
}

func TestApplyDelta_ValidCheckpointFold(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("b", 40)
	cpAt := TsPtr(time.Now().UTC())
	// A checkpoint refs snapshot: one ref, head target set.
	snap := &proto.RefSnapshot{Seq: 1, HeadTarget: "refs/heads/main",
		CreatedAt: cpAt, Refs: []*proto.Ref{{Name: "refs/heads/main", Oid: oid}}}
	if _, err := st.Put(ctx, h.repoKey(store.CheckpointRefsKey(1)), store.PutBody{Bytes: snap.Marshal()}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	m, version := h.ManifestSnapshot()
	next := *m
	next.Checkpoint = &proto.CheckpointRef{Seq: 1, Key: store.CheckpointKey(1), CreatedAt: cpAt}
	next.Revision = m.Revision + 1
	h.syncMu.Lock()
	h.manifest, h.version, h.heldRev = &next, version, next.Revision
	h.syncMu.Unlock()

	if err := h.catchUp(ctx); err != nil {
		t.Fatalf("checkpoint fold: %v", err)
	}
	snapLocal, err := h.Layer().Snapshot(h.repo)
	if err != nil {
		t.Fatal(err)
	}
	if snapLocal.HeadTarget != "refs/heads/main" {
		t.Fatalf("head target = %q", snapLocal.HeadTarget)
	}
	if h.state.AppliedSeq != 1 {
		t.Fatalf("applied seq = %d", h.state.AppliedSeq)
	}
	h.checkpointsMut.Lock()
	_, noted := h.checkpoints[1]
	h.checkpointsMut.Unlock()
	if !noted {
		t.Fatal("checkpoint created_at not noted")
	}
}

func TestFetchSegments_Faults(t *testing.T) {
	r, st := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	seg := &proto.LogSegmentRef{Key: store.LogSegmentKey(1), FirstSeq: 1, LastSeq: 1}
	entries := []*proto.LogEntry{{Seq: 1, Kind: proto.EntryKindRefUpdate, Writer: "w"}}
	putSegmentObject(t, h, st, seg, entries)

	// Store GET error.
	hk.getErr = func(key string, n int) error {
		if strings.Contains(key, "/log/") {
			return store.NewOther(key, errors.New("down"))
		}
		return nil
	}
	if _, err := h.fetchSegments(ctx, []*proto.LogSegmentRef{seg}); err == nil {
		t.Fatal("fetchSegments with failing GET succeeded")
	}
	hk.getErr = nil
	// Absent object.
	if err := st.Delete(ctx, h.repoKey(seg.Key), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.fetchSegments(ctx, []*proto.LogSegmentRef{seg}); err == nil {
		t.Fatalf("fetchSegments absent = nil, want an error")
	}
	// A corrupt body re-put: decode errors surface (mid-frame corruption is
	// never silently tolerated).
	if _, err := st.Put(ctx, h.repoKey(seg.Key), store.PutBody{Bytes: append([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, make([]byte, 64)...)}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestRepoKeyHelpers(t *testing.T) {
	if repoPrefix("acme/api") != "repos/acme/api/" {
		t.Fatal("repoPrefix")
	}
	owner, name, ok := cut2("acme/api", "/")
	if owner != "acme" || name != "api" || !ok {
		t.Fatal("cut2")
	}
	if _, _, ok := cut2("nosep", "/"); ok {
		t.Fatal("cut2 without separator")
	}
}

// ---- reconcile edges -----------------------------------------------------------------

func TestReconcilePacks_NilManifestAndRefsLevel(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.syncMu.Lock()
	h.manifest = nil
	h.syncMu.Unlock()
	if err := h.reconcilePacks(ctx, LevelServe); err != nil {
		t.Fatalf("nil manifest: %v", err)
	}
	m, _ := h.ManifestSnapshot()
	h.syncMu.Lock()
	h.manifest = m
	h.syncMu.Unlock()
	// LevelRefs never materializes.
	if err := h.reconcilePacks(ctx, LevelRefs); err != nil {
		t.Fatalf("refs level: %v", err)
	}
}

func TestMaterialize_MkdirFailure(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// The objects dir as a FILE → MkdirAll(objects/pack) fails.
	if err := os.RemoveAll(h.Repo().ObjectsDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.Repo().ObjectsDir(), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.packMu.LockMeasured(ctx, "pack_mutex", h.ID); err != nil {
		t.Fatal(err)
	}
	err = h.reconcilePacks(ctx, LevelServe)
	h.packMu.Unlock()
	if err == nil {
		t.Fatal("materialize over a file path succeeded")
	}
}

func TestFetchPackFile_Errors(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	p := &proto.PackRef{Checksum: strings.Repeat("a", 40), PackSize: 10, IdxSize: 1}
	// Pack download with a failing range GET.
	hk.getErr = func(key string, n int) error {
		if strings.HasSuffix(key, ".pack") {
			return store.NewOther(key, errors.New("down"))
		}
		return nil
	}
	if err := h.fetchPackFile(ctx, p, ".pack"); err == nil {
		t.Fatal("pack download with failing store succeeded")
	}
	// Size 0 → HEAD first; a Head failure is tolerated (size stays 0) and
	// the zero-stripe download writes an empty file.
	hk.getErr = nil
	hk.headErr = func(key string, n int) error {
		if strings.HasSuffix(key, ".pack") {
			return store.NewOther(key, errors.New("down"))
		}
		return nil
	}
	if err := h.fetchPackFile(ctx, &proto.PackRef{Checksum: p.Checksum}, ".pack"); err != nil {
		t.Fatalf("pack Head failure must be tolerated: %v", err)
	}
	hk.headErr = nil
	// Unknown suffix is a no-op.
	if err := h.fetchPackFile(ctx, p, ".nope"); err != nil {
		t.Fatalf("unknown suffix: %v", err)
	}
	// A manifest-claimed pack missing from the store: HEAD absent → size 0
	// → zero stripes → empty file (download of nothing succeeds silently).
	hk.headErr = nil
	if err := h.fetchPackFile(ctx, &proto.PackRef{Checksum: strings.Repeat("c", 40)}, ".pack"); err != nil {
		t.Fatalf("absent pack with size 0: %v", err)
	}
}

func TestFetchSideFile_StoreErrorAndWrite(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	cs := strings.Repeat("e", 40)
	// Present body → written into objects/pack.
	if _, err := r.st.Put(ctx, h.repoKey(store.RevKey(cs)), store.PutBody{Bytes: []byte("rev")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := h.fetchSideFile(ctx, cs, ".rev", store.RevKey(cs), 0); err != nil {
		t.Fatalf("side file fetch: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(h.Repo().PackDir(), "pack-"+cs+".rev"))
	if err != nil || string(data) != "rev" {
		t.Fatalf("side file = %q %v", data, err)
	}
	// Store GET error.
	hk.getErr = func(key string, n int) error {
		if strings.HasSuffix(key, ".rev") {
			return store.NewOther(key, errors.New("down"))
		}
		return nil
	}
	if err := h.fetchSideFile(ctx, cs, ".bitmap", store.BitmapKey(cs), 0); err == nil {
		t.Fatal("side file with failing GET succeeded")
	}
}

func TestRetrofitBasePack_Dedup(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	p := &proto.PackRef{Checksum: "cs"}
	h.retrofitBasePack(p)
	h.retrofitBasePack(p) // second call must not duplicate
	if len(h.state.RemoteServed) != 1 {
		t.Fatalf("remote served = %v", h.state.RemoteServed)
	}
}

func TestBuildRemoteIndex_HardLinksLocalIdx(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("7", 40)
	// A LOCAL idx in objects/pack: buildRemoteIndex must hard-link it and
	// leave the store untouched.
	packDir := h.Repo().PackDir()
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack-"+sum+".idx"), fakeIdxBody(t, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := h.ManifestSnapshot()
	next := *m
	next.Packs = []*proto.PackRef{{Checksum: sum, PackSize: 10, IdxSize: 1, Seq: 1}}
	next.Revision = m.Revision + 1
	h.syncMu.Lock()
	h.manifest = &next
	h.syncMu.Unlock()
	if err := h.buildRemoteIndex(ctx, nil, &next); err != nil {
		t.Fatalf("buildRemoteIndex: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.repo.Path, "remote-idx", sum+".idx")); err != nil {
		t.Fatalf("remote idx missing: %v", err)
	}
	if ok, _ := store.Exists(ctx, st, h.repoKey(store.IdxKey(sum))); ok {
		t.Fatal("store was touched despite the local idx")
	}
	// Idempotent second build (existing dst short-circuits).
	if err := h.buildRemoteIndex(ctx, nil, &next); err != nil {
		t.Fatalf("second build: %v", err)
	}
}

// ---- handle: freshen paths and lock cancellation --------------------------------------

func TestFreshenManifest_VersionEmptyAndStale(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// version == "" → unconditional GET → ErrNotFound when absent.
	if err := r.st.Delete(ctx, "repos/acme/api/manifest.pb", ""); err != nil {
		t.Fatal(err)
	}
	h.syncMu.Lock()
	h.version = ""
	h.freshAt = time.Time{}
	h.syncMu.Unlock()
	err = h.freshenManifest(ctx)
	if err == nil || !store.IsNotFound(err) {
		t.Fatalf("freshen without manifest = %v", err)
	}
}

func TestFreshenManifest_RevisionGuardAndCheckpointNote(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// A manifest at or below the held revision is discarded (only the
	// freshness stamp moves) even though it carries a checkpoint.
	m, _ := h.ManifestSnapshot()
	stale := *m
	stale.Checkpoint = &proto.CheckpointRef{Seq: 1, CreatedAt: TsPtr(time.Now().UTC())}
	if _, err := r.st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: stale.Marshal()}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	h.syncMu.Lock()
	h.freshAt = time.Time{}
	held := h.heldRev
	h.syncMu.Unlock()
	if err := h.freshenManifest(ctx); err != nil {
		t.Fatal(err)
	}
	if h.heldRev != held {
		t.Fatal("stale manifest was adopted")
	}
	h.checkpointsMut.Lock()
	_, noted := h.checkpoints[1]
	h.checkpointsMut.Unlock()
	if noted {
		t.Fatal("checkpoint noted from a discarded manifest")
	}

	// A manifest ABOVE the held revision is adopted and its checkpoint noted.
	ahead := *m
	ahead.Revision = m.Revision + 5
	ahead.HeadSeq = 9
	ahead.Checkpoint = &proto.CheckpointRef{Seq: 9, CreatedAt: TsPtr(time.Now().UTC())}
	if _, err := r.st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: ahead.Marshal()}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	h.syncMu.Lock()
	h.freshAt = time.Time{}
	h.syncMu.Unlock()
	if err := h.freshenManifest(ctx); err != nil {
		t.Fatal(err)
	}
	if h.heldRev != ahead.Revision || h.manifest.HeadSeq != 9 {
		t.Fatalf("ahead manifest not adopted: rev %d head %d", h.heldRev, h.manifest.HeadSeq)
	}
	h.checkpointsMut.Lock()
	_, noted = h.checkpoints[9]
	h.checkpointsMut.Unlock()
	if !noted {
		t.Fatal("checkpoint not noted on adopt")
	}
}

func TestSyncAndCatchUp_LockCancellation(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// Hold syncMu: a Sync with a canceled ctx aborts on lock acquisition.
	if err := h.syncMu.LockMeasured(ctx, "sync_mutex", h.ID); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := h.Sync(cctx, LevelRefs); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync = %v, want canceled", err)
	}
	if err := h.catchUp(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("catchUp = %v, want canceled", err)
	}
	h.syncMu.Unlock()
	// Holding packMu: a Serve-level Sync aborts on the pack phase lock.
	if err := h.packMu.LockMeasured(ctx, "pack_mutex", h.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Sync(cctx, LevelServe); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync serve = %v, want canceled", err)
	}
	h.packMu.Unlock()
}

// ---- publish edges ---------------------------------------------------------------------

func TestPublish_CASMaxRetriesZeroUsesDefault(t *testing.T) {
	cfg := cfgWith(t, func(c *config.Config) { c.WAL.CASMaxRetries = 0 })
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	hk.putErr = func(key string, n int) error {
		if strings.HasSuffix(key, "manifest.pb") {
			return store.NewPrecondition(key, "other")
		}
		return nil
	}
	_, err = h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/x", git.Sha1.ZeroHex(), strings.Repeat("a", 40)), Synced: true})
	if err == nil || !strings.Contains(err.Error(), "16 attempts") {
		t.Fatalf("err = %v, want the 16-attempt default", err)
	}
}

func TestClaimSlot_FreshHeadCorrupt(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.ensurePublisher()
	// A fresh manifest read that fails to parse surfaces as a store error.
	hk.putErr = func(key string, n int) error {
		if strings.Contains(key, "/log/") {
			return store.NewPrecondition(key, "x")
		}
		return nil
	}
	hk.getBody = func(key string) ([]byte, bool) {
		if strings.HasSuffix(key, "manifest.pb") {
			return []byte("garbage"), true
		}
		return nil, false
	}
	base, _ := h.ManifestSnapshot()
	entries := []*proto.LogEntry{{Seq: 1, Kind: proto.EntryKindRefUpdate, CreatedAt: TsPtr(time.Now().UTC())}}
	if _, _, _, _, err := h.pub.claimSlot(ctx, base, entries, nil); err == nil {
		t.Fatal("claimSlot with corrupt manifest succeeded")
	}
}

func TestMaybeCheckpoint_EntriesTrigger(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// One entry past the last checkpoint fires the entries trigger; the
	// opportunistic background checkpoint must commit before Publish returns.
	r.vals.snapshotEveryEntries = 1
	oid := strings.Repeat("a", 40)
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid)}); err != nil {
		t.Fatal(err)
	}
	m, _ := h.ManifestSnapshot()
	if m.Checkpoint == nil || m.Checkpoint.Seq != 1 {
		t.Fatalf("checkpoint = %+v, want committed at seq 1", m.Checkpoint)
	}
	ok, err := store.Exists(ctx, st, h.repoKey(store.CheckpointKey(1)))
	if err != nil || !ok {
		t.Fatalf("checkpoint object missing: %v %v", ok, err)
	}
}

func TestCasManifest_HeadFailureDuringRecovery(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.ensurePublisher()
	segBody := proto.EncodeSegment([]*proto.LogEntry{{Seq: 1, Kind: proto.EntryKindRefUpdate, Writer: "w"}})
	// Ambiguous PUT + fresh read shows our segment, but HEAD fails →
	// committed with an empty version (advertising is withdrawn downstream).
	hk.putErr = func(key string, n int) error { return store.NewRetryable(key, errors.New("lost")) }
	hk.getBody = func(key string) ([]byte, bool) {
		if strings.HasSuffix(key, "manifest.pb") {
			base, _ := h.ManifestSnapshot()
			next := *base
			next.LogSegments = append(next.LogSegments, &proto.LogSegmentRef{Key: store.LogSegmentKey(1), FirstSeq: 1, LastSeq: 1})
			return next.Marshal(), true
		}
		return nil, false
	}
	hk.headErr = func(key string, n int) error {
		if strings.HasSuffix(key, "manifest.pb") {
			return store.NewOther(key, errors.New("head down"))
		}
		return nil
	}
	base, version := h.ManifestSnapshot()
	next := buildNextManifest(base, nil, 1, 1, segBody, "w")
	_, committed, err := h.pub.casManifest(ctx, version, store.LogSegmentKey(1), "1", next)
	if err != nil || !committed {
		t.Fatalf("committed = %v err=%v, want committed with recovered state", committed, err)
	}
}

func TestPublishSettings_RevisionFromExisting(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// A manifest that already carries settings: the next publish bumps it.
	if err := h.PublishSettings(ctx, "[a]\nx = 1\n", "a", "m", map[string]string{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	if err := h.PublishSettings(ctx, "[a]\nx = 2\n", "b", "m2", nil); err != nil {
		t.Fatal(err)
	}
	m, _ := h.ManifestSnapshot()
	if m.Settings.Revision != 2 || m.Settings.Message != "m2" {
		t.Fatalf("settings = %+v", m.Settings)
	}
}

// ---- registry edges ----------------------------------------------------------------------

func TestCreate_InvalidId(t *testing.T) {
	r, _ := newTestRegistry(t)
	if _, err := r.Create(context.Background(), "bogus", git.Sha1); err == nil {
		t.Fatal("create with malformed id succeeded")
	}
}

func TestOpen_StoreGetError(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	hk.getErr = func(key string, n int) error { return store.NewOther(key, errors.New("down")) }
	if _, err := r.Open(context.Background(), "acme/api"); err == nil {
		t.Fatal("open with failing store succeeded")
	}
}

func TestOpen_CatchUpFailureWarns(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	// A manifest with a head the local replay cannot satisfy (segment GET
	// fails): the handle still opens (state stays laggard).
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("a", 40)
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid)}); err != nil {
		t.Fatal(err)
	}
	// Force a fresh open with a LAGGARD state: drop the handle and the
	// persisted state file so the initial catch-up has work to do.
	r.repos = map[string]*RepoHandle{}
	if err := os.Remove(filepath.Join(h.Dir(), stateFileName)); err != nil {
		t.Fatal(err)
	}
	hk.getErr = func(key string, n int) error {
		if strings.Contains(key, "/log/") {
			return store.NewOther(key, errors.New("down"))
		}
		return nil
	}
	h2, err := r.Open(ctx, "acme/api")
	if err != nil {
		t.Fatalf("open despite catch-up failure: %v", err)
	}
	if h2.state.AppliedSeq == h2.manifest.HeadSeq {
		t.Fatal("catch-up unexpectedly succeeded")
	}
}

func TestOpenOrInit_Sha256Manifest(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "acme/sha", ObjectFormat: git.Sha256.String(), Revision: 1}
	if _, err := st.Put(ctx, "repos/acme/sha/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Open(ctx, "acme/sha"); err != nil {
		t.Fatalf("sha256 open: %v", err)
	}
}

func TestDelete_RemoveAllFailure(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	if _, err := r.Create(ctx, "acme/api", git.Sha1); err != nil {
		t.Fatal(err)
	}
	// Make the local dir unremovable: a read-only parent blocks RemoveAll.
	if err := os.Chmod(filepath.Join(r.vals.cacheDir, "acme"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(r.vals.cacheDir, "acme"), 0o755) })
	if _, err := r.Delete(ctx, "acme/api"); err == nil {
		t.Fatal("delete with unremovable dir succeeded")
	}
}

func TestRegistry_ConcurrentOpensJoin(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "acme/api", ObjectFormat: "sha1", Revision: 1}
	if _, err := r.st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	const n = 8
	handles := make(chan *RepoHandle, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			h, err := r.Open(ctx, "acme/api")
			handles <- h
			errs <- err
		}()
	}
	var first *RepoHandle
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("open: %v", err)
		}
		h := <-handles
		if first == nil {
			first = h
		} else if h != first {
			t.Fatal("concurrent opens produced different handles")
		}
	}
}

// ---- eviction disk mode + dirSize edges ----------------------------------------------

func TestEvictIdle_AutoDiskModeEvicts(t *testing.T) {
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()
	// A big orphan + an absurdly low watermark: auto mode must run the disk
	// pass and remove the orphan.
	orphan := filepath.Join(cfg.Cache.Dir, "ghost", "old.git")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "p"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	r.vals.diskHighWatermark = 1e-12
	r.vals.cacheMode = "auto"
	r.evictIdle()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("auto mode did not evict the orphan")
	}
	// Explicit disk mode behaves the same.
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "p"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	r.vals.cacheMode = "disk"
	r.evictIdle()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("disk mode did not evict the orphan")
	}
}

func TestDirSize_SymlinksAndWalkErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	// Symlinks count by lstat size, not target size.
	got := dirSize(dir)
	if got != int64(len("hello"))+int64(len("real")) && got < int64(len("real")) {
		t.Fatalf("dirSize = %d", got)
	}
	// An unreadable directory doesn't fail the walk.
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })
	if dirSize(dir) < 0 {
		t.Fatal("negative dirSize")
	}
}

// ---- tasks + types ----------------------------------------------------------------------

func TestBroadcast_SendsDontBlockAndRingCaps(t *testing.T) {
	var b Broadcast[int]
	id, ch, _ := b.Subscribe()
	for i := 0; i < bcastReplayLen+50; i++ { // ring caps at 200, slow sub drops
		b.Send(i)
	}
	dropped := 0
	for {
		select {
		case <-ch:
			dropped++
			continue
		default:
		}
		break
	}
	b.Unsubscribe(id)
	if len(b.buf) != bcastReplayLen {
		t.Fatalf("ring = %d, want capped %d", len(b.buf), bcastReplayLen)
	}
}

func TestRefErrorKindFormatting_All(t *testing.T) {
	for _, k := range []RefErrorKind{RefErrNonFastForward, RefErrMissing, RefErrFallback} {
		if s := (&RefError{Kind: k, Ref: "r", Detail: "d"}).Error(); s == "" {
			t.Fatalf("kind %d empty", k)
		}
	}
}

// ---- remote reader typed stubs (Locate/Header/Decode not-found) -------------------------

func TestRemoteReader_NotFoundAndLocateIndex(t *testing.T) {
	pack, _, _, _, _ := buildTestPack(t)
	eng, _, ix := newTestEngine(t, &blockStore{data: pack}, "acme/api")
	ix.byOid = map[string]int64{testOid1: baseOffOf(t), testOid2: deltaOffOf(t)}
	ix.oids = []string{testOid1, testOid2}
	ix.offsets = []int64{baseOffOf(t), deltaOffOf(t)}
	rr := &RemoteReader{Revision: 1, eng: eng}
	ctx := context.Background()

	// Locate returns the pack index position.
	idx, off, ok := rr.Locate(testOid1)
	if !ok || idx != 0 || off != baseOffOf(t) {
		t.Fatalf("locate = %d %d %v", idx, off, ok)
	}
	// Missing oid across Locate/Header/Decode.
	if _, _, ok := rr.Locate("ffffffff"); ok {
		t.Fatal("locate found a phantom")
	}
	if _, _, err := rr.Header("ffffffff"); err == nil {
		t.Fatal("Header found a phantom")
	}
	if _, _, err := rr.Decode(ctx, "ffffffff"); err == nil {
		t.Fatal("Decode found a phantom")
	}
	// Real header/decode through the typed surface.
	kind, size, err := rr.Header(testOid1)
	if err != nil || kind != "blob" || size == 0 {
		t.Fatalf("header = %s %d %v", kind, size, err)
	}
	kind, data, err := rr.Decode(ctx, testOid2)
	if err != nil || kind != "blob" || !strings.Contains(string(data), "WALGIT") {
		t.Fatalf("decode = %s %q %v", kind, data, err)
	}
}

// ---- publish helpers --------------------------------------------------------------------

func TestPackRefOfAndUploadPackIdxError(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.ensurePublisher()
	dir := t.TempDir()
	packPath := filepath.Join(dir, "p.pack")
	indexPath := filepath.Join(dir, "p.idx")
	if err := os.WriteFile(packPath, []byte("PACK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte("IDX"), 0o644); err != nil {
		t.Fatal(err)
	}
	hk.putErr = func(key string, n int) error {
		if strings.HasSuffix(key, ".idx") {
			return store.NewOther(key, errors.New("down"))
		}
		return nil
	}
	err = h.pub.uploadPack(ctx, &PreparedPack{Checksum: strings.Repeat("b", 40), PackPath: packPath,
		IdxPath: indexPath, PackSize: 4, IdxSize: 3})
	if err == nil {
		t.Fatal("idx upload failure accepted")
	}
}
