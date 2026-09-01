// gap4_test.go — final branch coverage: backend responses that report absence
// without an error (NotModified-style bodies), native-compose upload setup,
// and task narration helpers.
package wal

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// ---- hookStore extensions: absent-without-error and erroring bodies ----------------

// errBody is a reader that always fails.
type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, errors.New("body read failed") }
func (errBody) Close() error             { return nil }

// TestGetBytesAbsentWithoutError exercises the body==nil branches across the
// engine: backends that report absence as (nil, nil) rather than an error.
func TestGetBytesAbsentWithoutError(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	hk0 := &hookStore{ObjectStore: st}
	hk0.markNM("checkpoints/", "log/", "wal/")
	r.st = hk0

	// foldCheckpoint: absent refs body → corrupt (not a store error).
	m, _ := h.ManifestSnapshot()
	next := *m
	next.Checkpoint = &proto.CheckpointRef{Seq: 1, Key: store.CheckpointKey(1)}
	next.LogSegments = []*proto.LogSegmentRef{{Key: store.LogSegmentKey(1), FirstSeq: 1, LastSeq: 1}}
	next.HeadSeq = 1
	next.Revision = m.Revision + 1
	h.syncMu.Lock()
	h.manifest = &next
	h.syncMu.Unlock()
	err = h.catchUp(ctx)
	if err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("foldCheckpoint absent = %v, want absent-corrupt", err)
	}

	// ReadLog: absent segment body → corrupt.
	if _, err := h.ReadLog(ctx, 1, 0); err == nil {
		t.Fatal("ReadLog with absent segment succeeded")
	}

	// fetchSegments: absent body → corrupt.
	seg := &proto.LogSegmentRef{Key: store.LogSegmentKey(1), FirstSeq: 1, LastSeq: 1}
	if _, err := h.fetchSegments(ctx, []*proto.LogSegmentRef{seg}); err == nil {
		t.Fatal("fetchSegments with absent body succeeded")
	}

	// fetchSideFile: absent optional (.rev) tolerated; absent .idx corrupt.
	if err := h.fetchSideFile(ctx, strings.Repeat("a", 40), ".rev", store.RevKey(strings.Repeat("a", 40)), 0); err != nil {
		t.Fatalf("absent optional side file: %v", err)
	}
	err = h.fetchSideFile(ctx, strings.Repeat("b", 40), ".idx", store.IdxKey(strings.Repeat("b", 40)), 0)
	if err == nil || !strings.Contains(err.Error(), "idx absent") {
		t.Fatalf("absent idx = %v", err)
	}

	// fetchPackFile: absent pack body → zero-size download (empty file).
	if err := h.fetchPackFile(ctx, &proto.PackRef{Checksum: strings.Repeat("c", 40), PackSize: 0}, ".pack"); err != nil {
		t.Fatalf("absent pack body: %v", err)
	}

	// freshHead: absent body → (nil, nil), treated as "no rival head".
	h.ensurePublisher()
	hk := &hookStore{ObjectStore: st}
	hk.markNM("repos/acme/api/manifest.pb")
	r.st = hk
	if _, err := h.pub.freshHead(ctx); err != nil {
		t.Fatalf("freshHead = %v, want nil/nil", err)
	}
}

// ---- freshenManifest: an unreadable manifest body ----------------------------------

func TestFreshenManifest_BodyReadError(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	hkErr := &hookStore{ObjectStore: st}
	hkErr.errKeys = map[string]bool{"repos/acme/api/manifest.pb": true}
	r.st = hkErr
	h.syncMu.Lock()
	h.freshAt = time.Time{}
	h.syncMu.Unlock()
	if err := h.freshenManifest(ctx); err == nil {
		t.Fatal("freshen with unreadable body succeeded")
	}
}

// ---- uploadPack: native-compose branch ----------------------------------------------

type nativeComposeStore struct {
	store.ObjectStore
}

func (n *nativeComposeStore) ComposeIsNative() bool { return true }

func (n *nativeComposeStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	// The striped uploader only parts files above the threshold; the single
	// Put path below it must see a real body.
	if body.Stream != nil {
		if _, err := io.Copy(io.Discard, io.LimitReader(body.Stream, 16)); err != nil {
			return store.ObjectMeta{}, err
		}
	}
	return n.ObjectStore.Put(ctx, key, store.PutBody{Bytes: make([]byte, 16)}, opts)
}

func TestUploadPack_StripedSetup(t *testing.T) {
	r := NewRegistry(context.Background(), store2(), testConfig(t))
	defer r.Close()
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.ensurePublisher()
	// A native-compose backend with an oversized pack whose file is missing:
	// the striped setup fails on open, wrapped as an engine IO error.
	h.pub.uploadPack(ctx, &PreparedPack{Checksum: strings.Repeat("a", 40),
		PackPath: filepath.Join(t.TempDir(), "missing.pack"), PackSize: 300 << 20})
	// (The failure is returned to the ladder; assert via a direct call.)
	err = h.pub.uploadPack(ctx, &PreparedPack{Checksum: strings.Repeat("b", 40),
		PackPath: filepath.Join(t.TempDir(), "missing.pack"), PackSize: 300 << 20})
	if err == nil {
		t.Fatal("striped upload with missing file succeeded")
	}
}

// ---- writeLooseObject / fault --------------------------------------------------------

func TestWriteLooseObject_MkdirFailure(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// The objects dir as a FILE: MkdirAll(objects/xx) fails.
	if err := os.RemoveAll(h.Repo().ObjectsDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.Repo().ObjectsDir(), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeLooseObject(h.repo, strings.Repeat("a", 40), "blob", []byte("x")); err == nil {
		t.Fatal("writeLooseObject over a file path succeeded")
	}
	_ = ctx
}

func TestFault_DecodeErrorSurfaces(t *testing.T) {
	pack, _, _, baseOid, _ := buildTestPack(t)
	bs := &blockStore{data: pack}
	eng, _, _ := newTestEngine(t, bs, "acme/api")
	eng.packs.idxs[0].oids = []string{baseOid}
	eng.packs.idxs[0].byOid = map[string]int64{baseOid: baseOffOf(t)}
	eng.packs.idxs[0].offsets = []int64{baseOffOf(t)}
	eng.st = &errAtStore{inner: bs, failKey: eng.packKey(eng.packs.idxs[0])}
	r, _ := newTestRegistry(t)
	h, err := r.Create(context.Background(), "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.fault(context.Background(), h, []string{baseOid}); err == nil {
		t.Fatal("fault with failing decode succeeded")
	}
}

// ---- reconcilePacks: surfacing a failed materialize task -----------------------------

func TestReconcilePacks_MaterializeFailureSurfaces(t *testing.T) {
	r, st := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("a", 40)
	m, version := h.ManifestSnapshot()
	next := *m
	next.Packs = []*proto.PackRef{{Checksum: sum, PackSize: 10, IdxSize: 1, Seq: 1}}
	next.Revision = m.Revision + 1
	h.syncMu.Lock()
	h.manifest, h.version, h.heldRev = &next, version, next.Revision
	h.syncMu.Unlock()
	// The pack body cannot be read → materialize fails → reconcilePacks
	// reports the task failure instead of silently continuing.
	hk.getErr = func(key string, n int) error {
		if strings.HasSuffix(key, ".pack") {
			return store.NewOther(key, errors.New("down"))
		}
		return nil
	}
	if err := h.packMu.LockMeasured(ctx, "pack_mutex", h.ID); err != nil {
		t.Fatal(err)
	}
	err = h.reconcilePacks(ctx, LevelServe)
	h.packMu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "materialize failed") {
		t.Fatalf("reconcilePacks = %v, want surfaced task failure", err)
	}
	_ = st
}

// ---- registry: malformed object format falls back to sha1 ----------------------------

func TestOpenOrInit_BadObjectFormatFallsBack(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "acme/odd", ObjectFormat: "bogus", Revision: 1}
	if _, err := st.Put(ctx, "repos/acme/odd/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	h, err := r.Open(ctx, "acme/odd")
	if err != nil {
		t.Fatalf("open with bogus format: %v", err)
	}
	if h.repo.Format() != git.Sha1 {
		t.Fatalf("format = %v, want sha1 fallback", h.repo.Format())
	}
}

// ---- eviction: disk mode loops over multiple repos ------------------------------------

func TestEvictDisk_MultipleOrphans(t *testing.T) {
	cfg := testConfig(t)
	r := NewRegistry(context.Background(), store2(), cfg)
	defer r.Close()
	for _, id := range []string{"ghost/one", "ghost/two"} {
		dir := filepath.Join(cfg.Cache.Dir, filepath.FromSlash(id))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "p"), make([]byte, 1<<20), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r.vals.diskHighWatermark = 1e-12
	r.evictDisk()
	for _, id := range []string{"ghost/one", "ghost/two"} {
		dir := filepath.Join(cfg.Cache.Dir, filepath.FromSlash(id))
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s survived disk eviction", id)
		}
	}
}

// ---- tasks: Errorf narration ----------------------------------------------------------

func TestTask_Errorf(t *testing.T) {
	tt := newTaskTable("h", context.Background())
	done := make(chan *Task, 1)
	_, err := tt.Run(context.Background(), "acme/api", "narrate", nil, func(ctx context.Context, task *Task) error {
		task.Errorf("step %d failed: %s", 3, "bad thing")
		done <- task
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	task := <-done
	if len(task.rec.LogTail) == 0 || task.rec.LogTail[len(task.rec.LogTail)-1] != "ERROR step 3 failed: bad thing" {
		t.Fatalf("log tail = %v", task.rec.LogTail)
	}
}
