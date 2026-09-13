package maintain

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
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
// only by g/gc (child of f/c), yet the parent sweep must keep it.
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

// failStore injects a transport error on one key: transient store
// failures are doubt, and doubt must abort the sweep (fail closed) —
// never silently unpin a subtree the way a deleted (404) fork does.
type failStore struct {
	store.ObjectStore
	failKey string
	err     error
}

func (s *failStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if key == s.failKey {
		return nil, s.err
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

// failEngine serves the bucket through failStore (embedded fakeEngine
// covers the rest of the Engine interface).
type failEngine struct {
	*fakeEngine
	st store.ObjectStore
}

func (e *failEngine) Store() store.ObjectStore { return e.st }

func gcTestEff() *config.Config {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	eff.Compaction.RetentionSuperseded = config.Duration(7 * 24 * time.Hour)
	return eff
}

// A transient child-manifest read failure aborts the sweep: the child's
// pack set is unknown, so deleting would risk a live fork's packs. The
// next pass retries (deferred, never wrong).
func TestForkNetworkGCTransientChildError(t *testing.T) {
	eff := gcTestEff()
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 9, MinSeq: 1}, git: &fakeGit{}}
	repo.entries = []*proto.LogEntry{
		{Seq: 9, Kind: proto.EntryKindCompact,
			CreatedAt:  ptrTs(time.Now().Add(-8 * 24 * time.Hour)),
			Supersedes: []string{"shared-old", "gone-old"}},
	}
	eng := newFakeEngine(eff, repo)
	ctx := context.Background()
	cm := &proto.Manifest{Repo: "f/c", HeadSeq: 9, MinSeq: 10, Revision: 1,
		Packs: []*proto.PackRef{pack("shared-old", 9, 10, 1, 1)}}
	put := func(key string, body []byte) {
		t.Helper()
		if _, err := eng.st.Put(ctx, key, store.PutBody{Bytes: body}, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	put("repos/f/c/manifest.pb", cm.Marshal())
	fx, _ := json.Marshal(&forkIndex{Version: 1, Forks: []forkIndexEntry{{Repo: "f/c", ForkedAt: "2026-09-13T12:00:00Z"}}})
	put("repos/acme/widget/meta/forks.json", fx)
	put(repo.Prefix()+"wal/shared-old.pack", []byte("x"))
	put(repo.Prefix()+"wal/gone-old.pack", []byte("x"))

	ferr := &store.StoreError{Kind: store.ErrKindRetryable, Key: "repos/f/c/manifest.pb"}
	feng := &failEngine{fakeEngine: eng, st: &failStore{ObjectStore: eng.st, failKey: "repos/f/c/manifest.pb", err: ferr}}
	maint := New(feng, Options{Leaser: &fakeLeaser{}})
	removed, err := maint.gcSuperseded(ctx, repo, &Snapshot{ID: repo.id, Manifest: repo.m, Eff: eff})
	if err == nil {
		t.Fatal("transient child error must fail closed")
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if !eng.st.has(repo.Prefix()+"wal/shared-old.pack") || !eng.st.has(repo.Prefix()+"wal/gone-old.pack") {
		t.Fatal("fail-closed sweep must delete nothing")
	}
}

// An unreadable child index aborts the sweep: its grandchildren are
// unknown. An absent index is a leaf fork (covered by the base test).
func TestForkNetworkGCChildIndexError(t *testing.T) {
	eff := gcTestEff()
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 9, MinSeq: 1}, git: &fakeGit{}}
	repo.entries = []*proto.LogEntry{
		{Seq: 9, Kind: proto.EntryKindCompact,
			CreatedAt:  ptrTs(time.Now().Add(-8 * 24 * time.Hour)),
			Supersedes: []string{"gone-old"}},
	}
	eng := newFakeEngine(eff, repo)
	ctx := context.Background()
	cm := &proto.Manifest{Repo: "f/c", HeadSeq: 9, MinSeq: 10, Revision: 1}
	put := func(key string, body []byte) {
		t.Helper()
		if _, err := eng.st.Put(ctx, key, store.PutBody{Bytes: body}, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	put("repos/f/c/manifest.pb", cm.Marshal())
	fx, _ := json.Marshal(&forkIndex{Version: 1, Forks: []forkIndexEntry{{Repo: "f/c", ForkedAt: "2026-09-13T12:00:00Z"}}})
	put("repos/acme/widget/meta/forks.json", fx)
	put(repo.Prefix()+"wal/gone-old.pack", []byte("x"))

	ferr := &store.StoreError{Kind: store.ErrKindRetryable, Key: "repos/f/c/meta/forks.json"}
	feng := &failEngine{fakeEngine: eng, st: &failStore{ObjectStore: eng.st, failKey: "repos/f/c/meta/forks.json", err: ferr}}
	maint := New(feng, Options{Leaser: &fakeLeaser{}})
	removed, err := maint.gcSuperseded(ctx, repo, &Snapshot{ID: repo.id, Manifest: repo.m, Eff: eff})
	if err == nil {
		t.Fatal("child index error must fail closed")
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if !eng.st.has(repo.Prefix() + "wal/gone-old.pack") {
		t.Fatal("fail-closed sweep must delete nothing")
	}
}

// A corrupt child manifest aborts the sweep: a broken child may still be
// repaired, and repair needs its packs.
func TestForkNetworkGCCorruptChild(t *testing.T) {
	eff := gcTestEff()
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
	put := func(key string, body []byte) {
		t.Helper()
		if _, err := st.Put(ctx, key, store.PutBody{Bytes: body}, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	put("repos/f/c/manifest.pb", []byte("{bad"))
	fx, _ := json.Marshal(&forkIndex{Version: 1, Forks: []forkIndexEntry{{Repo: "f/c", ForkedAt: "2026-09-13T12:00:00Z"}}})
	put("repos/acme/widget/meta/forks.json", fx)
	put(repo.Prefix()+"wal/gone-old.pack", []byte("x"))

	removed, err := maint.gcSuperseded(ctx, repo, &Snapshot{ID: repo.id, Manifest: repo.m, Eff: eff})
	if err == nil {
		t.Fatal("corrupt child manifest must fail closed")
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if !eng.st.has(repo.Prefix() + "wal/gone-old.pack") {
		t.Fatal("fail-closed sweep must delete nothing")
	}
}

// Probe-cap exhaustion with unvisited children remaining aborts the
// sweep: the unvisited subtrees' pack sets are unknown, so proceeding
// would delete packs a live fork still references.
func TestForkNetworkGCCapExceeded(t *testing.T) {
	eff := gcTestEff()
	repo := &fakeRepo{id: "acme/widget", m: &proto.Manifest{Repo: "acme/widget", HeadSeq: 9, MinSeq: 1}, git: &fakeGit{}}
	var supers []string
	for i := 0; i < 40; i++ {
		supers = append(supers, "shared-"+strconv.Itoa(i))
	}
	supers = append(supers, "gone-old")
	repo.entries = []*proto.LogEntry{
		{Seq: 9, Kind: proto.EntryKindCompact,
			CreatedAt:  ptrTs(time.Now().Add(-8 * 24 * time.Hour)),
			Supersedes: supers},
	}
	eng := newFakeEngine(eff, repo)
	maint := New(eng, Options{Leaser: &fakeLeaser{}})
	st := eng.Store()
	ctx := context.Background()
	var rows []forkIndexEntry
	for i := 0; i < 40; i++ {
		id := "f/c" + strconv.Itoa(i)
		cm := &proto.Manifest{Repo: id, HeadSeq: 9, MinSeq: 10, Revision: 1,
			Packs: []*proto.PackRef{pack("shared-"+strconv.Itoa(i), 9, 10, 1, 1)}}
		if _, err := st.Put(ctx, "repos/"+id+"/manifest.pb", store.PutBody{Bytes: cm.Marshal()}, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, forkIndexEntry{Repo: id, ForkedAt: "2026-09-13T12:00:00Z"})
		put := repo.Prefix() + "wal/shared-" + strconv.Itoa(i) + ".pack"
		if _, err := st.Put(ctx, put, store.PutBody{Bytes: []byte("x")}, store.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	fx, _ := json.Marshal(&forkIndex{Version: 1, Forks: rows})
	if _, err := st.Put(ctx, "repos/acme/widget/meta/forks.json", store.PutBody{Bytes: fx}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, repo.Prefix()+"wal/gone-old.pack", store.PutBody{Bytes: []byte("x")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	removed, err := maint.gcSuperseded(ctx, repo, &Snapshot{ID: repo.id, Manifest: repo.m, Eff: eff})
	if err == nil || !strings.Contains(err.Error(), "probe cap") {
		t.Fatalf("cap exhaustion must fail closed, got removed=%d err=%v", removed, err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	for i := 0; i < 40; i++ {
		if !eng.st.has(repo.Prefix() + "wal/shared-" + strconv.Itoa(i) + ".pack") {
			t.Fatalf("shared-%d.pack must survive the deferred sweep", i)
		}
	}
}
