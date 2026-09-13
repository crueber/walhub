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

// TestForkNetworkGC pins the §7 rule (issue #424): a pack superseded on
// the parent is NOT deleted while any live fork-network manifest still
// references it; unreferenced superseded packs are still collected.
func TestForkNetworkGC(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	eff.Compaction.RetentionSuperseded = config.Duration(7 * 24 * time.Hour)

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

	// Child manifest referencing the shared pack (verbatim §7 fork).
	cm := &proto.Manifest{Repo: "f/c", HeadSeq: 9, MinSeq: 10, Revision: 1,
		Packs: []*proto.PackRef{pack("shared-old", 9, 10, 1, 1)}}
	put := func(key string, body []byte) {
		t.Helper()
		if _, err := st.Put(ctx, key, store.PutBody{Bytes: body}, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	put("repos/f/c/manifest.pb", cm.Marshal())
	fx, _ := json.Marshal(&forkIndex{Version: 1, Forks: []forkIndexEntry{{Repo: "f/c", ForkedAt: "2026-09-13T12:00:00Z"}}})
	put("repos/acme/widget/meta/forks.json", fx)
	put(repo.Prefix()+"wal/shared-old.pack", []byte("x"))
	put(repo.Prefix()+"wal/gone-old.pack", []byte("x"))

	removed, err := maint.gcSuperseded(ctx, repo, &Snapshot{ID: repo.id, Manifest: repo.m, Eff: eff})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (gone-old only — shared-old is pinned by f/c)", removed)
	}
	if !eng.st.has(repo.Prefix() + "wal/shared-old.pack") {
		t.Fatal("shared-old.pack is pinned by the live fork and must survive")
	}
	if eng.st.has(repo.Prefix() + "wal/gone-old.pack") {
		t.Fatal("gone-old.pack is unreferenced and must be collected")
	}
}

// A deleted child (stale index row, manifest 404) pins nothing: the sweep
// skips the subtree and still collects.
func TestForkNetworkGCDeletedChild(t *testing.T) {
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
	fx, _ := json.Marshal(&forkIndex{Version: 1, Forks: []forkIndexEntry{{Repo: "f/ghost", ForkedAt: "2026-09-13T12:00:00Z"}}})
	if _, err := st.Put(ctx, "repos/acme/widget/meta/forks.json", store.PutBody{Bytes: fx}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, repo.Prefix()+"wal/gone-old.pack", store.PutBody{Bytes: []byte("x")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	removed, err := maint.gcSuperseded(ctx, repo, &Snapshot{ID: repo.id, Manifest: repo.m, Eff: eff})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
}

// A corrupt parent index fails closed: nothing is deleted when the only
// map to the network is unreadable.
func TestForkNetworkGCCorruptIndex(t *testing.T) {
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
	removed, err := maint.gcSuperseded(ctx, repo, &Snapshot{ID: repo.id, Manifest: repo.m, Eff: eff})
	if err == nil {
		t.Fatal("corrupt index must fail closed")
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if !eng.st.has(repo.Prefix() + "wal/gone-old.pack") {
		t.Fatal("fail-closed sweep must delete nothing")
	}
}

// A grandchild pins through the transitive walk: the pack is referenced
// only by g/gc (child of f/c), yet the parent sweep must keep it. A
// corrupt grandchild manifest skips that subtree without stalling the
// pass.
func TestForkNetworkGCGrandchild(t *testing.T) {
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
			Supersedes: []string{"deep-shared", "gone-old"}},
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
	// Child f/c references nothing shared; grandchild g/gc pins deep-shared.
	cm := &proto.Manifest{Repo: "f/c", HeadSeq: 9, MinSeq: 10, Revision: 1}
	put("repos/f/c/manifest.pb", cm.Marshal())
	gm := &proto.Manifest{Repo: "g/gc", HeadSeq: 9, MinSeq: 10, Revision: 1,
		Packs: []*proto.PackRef{pack("deep-shared", 9, 10, 1, 1)}}
	put("repos/g/gc/manifest.pb", gm.Marshal())
	fx, _ := json.Marshal(&forkIndex{Version: 1, Forks: []forkIndexEntry{{Repo: "f/c", ForkedAt: "2026-09-13T12:00:00Z"}}})
	put("repos/acme/widget/meta/forks.json", fx)
	cfx, _ := json.Marshal(&forkIndex{Version: 1, Forks: []forkIndexEntry{{Repo: "g/gc", ForkedAt: "2026-09-13T12:00:00Z"}}})
	put("repos/f/c/meta/forks.json", cfx)
	put(repo.Prefix()+"wal/deep-shared.pack", []byte("x"))
	put(repo.Prefix()+"wal/gone-old.pack", []byte("x"))

	removed, err := maint.gcSuperseded(ctx, repo, &Snapshot{ID: repo.id, Manifest: repo.m, Eff: eff})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 (gone-old only)", removed)
	}
	if !eng.st.has(repo.Prefix() + "wal/deep-shared.pack") {
		t.Fatal("deep-shared.pack is pinned transitively and must survive")
	}
}
