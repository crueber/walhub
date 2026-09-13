package maintain

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// TestForkNetworkGCMetaParent pins the #451 GC rule: after a fork parent
// is deleted (manifest gone, meta/forks.json + tombstone preserved), a
// GC pass that reaches the network through the preserved index still
// protects packs live children reference. The walk never probes the
// parent manifest — the preserved index is its only input — so the
// absent manifest changes nothing; a corrupt meta index still fails
// closed (nothing deleted).
func TestForkNetworkGCMetaParent(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	eff.Compaction.RetentionSuperseded = config.Duration(7 * 24 * time.Hour)

	// The snapshot is in-memory (as if read before the delete); the
	// STORE holds the post-delete shape: no parent manifest, index +
	// tombstone + packs intact.
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 9, MinSeq: 1}, git: &fakeGit{}}
	repo.m.Packs = append(repo.m.Packs, pack("live", 9, 10, 1, 0))
	repo.entries = []*proto.LogEntry{
		{Seq: 9, Kind: proto.EntryKindCompact,
			CreatedAt:  ptrTs(time.Now().Add(-8 * 24 * time.Hour)),
			Supersedes: []string{"shared-old", "gone-old"}},
	}
	eng := newFakeEngine(eff, repo)
	maint := New(eng, Options{Leaser: &fakeLeaser{}})
	st := eng.Store()
	ctx := context.Background()
	put := func(key string, body []byte) {
		t.Helper()
		if _, err := st.Put(ctx, key, store.PutBody{Bytes: body}, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// Live child referencing the shared pack (verbatim §7 fork).
	cm := &proto.Manifest{Repo: "f/c", HeadSeq: 9, MinSeq: 10, Revision: 1,
		Packs: []*proto.PackRef{pack("shared-old", 9, 10, 1, 1)}}
	put("repos/f/c/manifest.pb", cm.Marshal())
	fx, _ := json.Marshal(&forkIndex{Version: 1, Forks: []forkIndexEntry{{Repo: "f/c", ForkedAt: "2026-09-13T12:00:00Z"}}})
	put("repos/acme/widget/meta/forks.json", fx)
	put("repos/acme/widget/meta/tombstone.json", []byte(`{"version":1,"repo":"acme/widget","reason":"fork-parent"}`))
	put(repo.Prefix()+"wal/shared-old.pack", []byte("x"))
	put(repo.Prefix()+"wal/gone-old.pack", []byte("x"))
	// No parent manifest on the store — the meta shape.

	removed, err := maint.gcSuperseded(ctx, repo, &Snapshot{ID: repo.id, Manifest: repo.m, Eff: eff})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (gone-old only — shared-old is pinned by f/c through the meta index)", removed)
	}
	if !eng.st.has(repo.Prefix() + "wal/shared-old.pack") {
		t.Fatal("shared-old.pack is pinned by the live fork and must survive the meta parent")
	}
	if eng.st.has(repo.Prefix() + "wal/gone-old.pack") {
		t.Fatal("gone-old.pack is unreferenced and must be collected")
	}
}

// TestForkNetworkGCMetaCorruptIndex: the meta shape with a corrupt
// preserved index fails closed — doubt keeps every pack.
func TestForkNetworkGCMetaCorruptIndex(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	eff.Compaction.RetentionSuperseded = config.Duration(7 * 24 * time.Hour)

	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 9, MinSeq: 1}, git: &fakeGit{}}
	repo.entries = []*proto.LogEntry{
		{Seq: 9, Kind: proto.EntryKindCompact,
			CreatedAt:  ptrTs(time.Now().Add(-8 * 24 * time.Hour)),
			Supersedes: []string{"gone-old"}},
	}
	eng := newFakeEngine(eff, repo)
	maint := New(eng, Options{Leaser: &fakeLeaser{}})
	st := eng.Store()
	ctx := context.Background()
	if _, err := st.Put(ctx, "repos/acme/widget/meta/forks.json", store.PutBody{Bytes: []byte("{bad")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, repo.Prefix()+"wal/gone-old.pack", store.PutBody{Bytes: []byte("x")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// No parent manifest on the store — the meta shape.
	removed, err := maint.gcSuperseded(ctx, repo, &Snapshot{ID: repo.id, Manifest: repo.m, Eff: eff})
	if err == nil {
		t.Fatal("corrupt meta index must fail closed")
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if !eng.st.has(repo.Prefix() + "wal/gone-old.pack") {
		t.Fatal("fail-closed sweep must delete nothing")
	}
}
