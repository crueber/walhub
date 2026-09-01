// gap3_test.go — targeted remaining branches: push-with-pack jobs, ladder
// error replies, checkpoint race/cancellation provenance, singleflight open
// join, and reader edge paths.
package wal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// getGate blocks every Get until closed — deterministic single-flight control.
func (h *hookStore) gate() (chan struct{}, func()) {
	ch := make(chan struct{})
	h.mu.Lock()
	h.gateCh = ch
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		h.gateCh = nil
		h.mu.Unlock()
		close(ch)
	}
}

func TestOpen_SingleflightJoin(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "acme/api", ObjectFormat: "sha1", Revision: 1}
	if _, err := st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	hk, _ := attachHooks(t, r)
	_, release := hk.gate()

	type res struct {
		h   *RepoHandle
		err error
	}
	done := make(chan res, 2)
	run := func() {
		h, err := r.Open(ctx, "acme/api")
		done <- res{h, err}
	}
	go run()
	time.Sleep(20 * time.Millisecond) // the leader parks inside the gated GET
	go run()
	time.Sleep(20 * time.Millisecond) // the joiner parks on the in-flight call
	release()

	var first *RepoHandle
	for i := 0; i < 2; i++ {
		r := <-done
		if r.err != nil {
			t.Fatalf("open: %v", r.err)
		}
		if first == nil {
			first = r.h
		} else if r.h != first {
			t.Fatal("joiner got a different handle")
		}
	}
}

func TestPublish_PushWithPackJob(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// A PUSH entry that also carries a pack (receive-pack with objects).
	dir := t.TempDir()
	packPath := filepath.Join(dir, "pack-ff.pack")
	if err := os.WriteFile(packPath, []byte("PACK pushpack"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("f", 40)
	res, err := h.Publish(ctx, PublishRequest{
		Txn:  refTxn("refs/heads/main", git.Sha1.ZeroHex(), strings.Repeat("a", 40)),
		Pack: &PreparedPack{Checksum: sum, PackPath: packPath, PackSize: 13},
	})
	if err != nil || res.Seq == 0 {
		t.Fatalf("push with pack: %+v %v", res, err)
	}
	m, _ := h.ManifestSnapshot()
	found := false
	for _, p := range m.Packs {
		if p.Checksum == sum && p.Seq == res.Seq {
			found = true
		}
	}
	if !found {
		t.Fatalf("pack missing at its seq: %+v", m.Packs)
	}
	ok, _ := store.Exists(ctx, st, h.repoKey(store.PackKey(sum)))
	if !ok {
		t.Fatal("push pack not uploaded")
	}
}

func TestPublish_ClaimSlotErrorRepliesAll(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// The slot create loses to a phantom and the orphan probe HEAD fails:
	// claimSlot surfaces a store error and the whole batch is answered.
	hk.putErr = func(key string, n int) error {
		if strings.Contains(key, "/log/") {
			return store.NewPrecondition(key, "phantom")
		}
		return nil
	}
	hk.headErr = func(key string, n int) error {
		if strings.Contains(key, "/log/") {
			return store.NewOther(key, errors.New("probe down"))
		}
		return nil
	}
	_, err = h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/x", git.Sha1.ZeroHex(), strings.Repeat("a", 40)), Synced: true})
	if err == nil {
		t.Fatal("publish with failing claim succeeded")
	}
}

func TestPublish_CasManifestErrorRepliesAll(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// The manifest CAS fails with an ambiguous error AND the fresh re-read
	// fails: the ladder cannot recover and answers the batch with the error.
	hk.putErr = func(key string, n int) error {
		if strings.HasSuffix(key, "manifest.pb") {
			return store.NewRetryable(key, errors.New("lost"))
		}
		return nil
	}
	hk.getErr = func(key string, n int) error {
		if strings.HasSuffix(key, "manifest.pb") {
			return store.NewRetryable(key, errors.New("re-read lost"))
		}
		return nil
	}
	_, err = h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/x", git.Sha1.ZeroHex(), strings.Repeat("a", 40)), Synced: true})
	if err == nil || !strings.Contains(err.Error(), "casLanded") {
		t.Fatalf("err = %v, want casLanded failure", err)
	}
}

func TestAnnotatePack_FreshenError(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pack-ab.pack")
	if err := os.WriteFile(path, []byte("PACK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddPack(ctx, path, strings.Repeat("a", 40), 0, nil); err != nil {
		t.Fatal(err)
	}
	// CAS 412 followed by a failing refresh aborts the annotate.
	hk.putErr = func(key string, n int) error {
		if strings.HasSuffix(key, "manifest.pb") {
			return store.NewPrecondition(key, "other")
		}
		return nil
	}
	hk.getErr = func(key string, n int) error {
		if strings.HasSuffix(key, "manifest.pb") {
			return store.NewOther(key, errors.New("refresh down"))
		}
		return nil
	}
	if err := h.AnnotatePack(ctx, strings.Repeat("a", 40), true, false, false); err == nil {
		t.Fatal("annotate with failing refresh succeeded")
	}
}

func TestAddPack_InstallWriteFailure(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// objects/pack as a FILE: installPackFile's write fails.
	if err := os.RemoveAll(h.Repo().PackDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.Repo().PackDir(), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pack-cd.pack")
	if err := os.WriteFile(path, []byte("PACK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddPack(ctx, path, strings.Repeat("c", 40), 0, nil); err == nil {
		t.Fatal("add-pack with failing install succeeded")
	}
}

func TestWriteCheckpoint_CanceledCtxAndUpdatedAtNil(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// Hold syncMu: a canceled caller aborts on lock acquisition.
	if err := h.syncMu.LockMeasured(ctx, "sync_mutex", h.ID); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := h.WriteCheckpoint(cctx, TriggerManual); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteCheckpoint = %v, want canceled", err)
	}
	h.syncMu.Unlock()

	// A manifest without UpdatedAt: the as-of provenance falls through to
	// the checkpoint time.
	m, version := h.ManifestSnapshot()
	next := *m
	next.UpdatedAt = nil
	next.Revision = m.Revision + 1
	if _, err := r.st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: next.Marshal()},
		store.PutOptions{Mode: store.PutUpdate, IfVersion: store.Version(version)}); err != nil {
		t.Fatal(err)
	}
	h.syncMu.Lock()
	h.manifest, h.version = &next, version
	h.freshAt = time.Time{} // let the next freshen adopt the new revision
	h.syncMu.Unlock()
	if err := h.WriteCheckpoint(ctx, TriggerManual); err != nil {
		t.Fatalf("checkpoint without updated_at: %v", err)
	}
	m2, _ := h.ManifestSnapshot()
	if m2.Checkpoint == nil || m2.Checkpoint.AsOf == nil {
		t.Fatalf("checkpoint = %+v", m2.Checkpoint)
	}
}

func TestCheckpoint_CASRaceConcedes(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("a", 40)
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid)}); err != nil {
		t.Fatal(err)
	}
	base, _ := h.ManifestSnapshot()
	doctored := *base
	doctored.Revision = base.Revision + 1 // adoptable by the freshness guard
	doctored.Checkpoint = &proto.CheckpointRef{Seq: base.HeadSeq, Key: store.CheckpointKey(base.HeadSeq)}
	// The FIRST manifest read (inside writeCheckpoint's freshen) must see the
	// plain manifest; later reads (the CAS-loop refresh) see a rival
	// checkpoint at the same seq → the loop concedes.
	calls := 0
	hk.getBody = func(key string) ([]byte, bool) {
		if strings.HasSuffix(key, "manifest.pb") {
			calls++
			if calls > 1 {
				return doctored.Marshal(), true
			}
		}
		return nil, false
	}
	hk.putErr = func(key string, n int) error {
		if strings.HasSuffix(key, "manifest.pb") {
			return store.NewPrecondition(key, "rival")
		}
		return nil
	}
	if err := h.WriteCheckpoint(ctx, TriggerManual); err != nil {
		t.Fatalf("checkpoint race: %v", err)
	}
	if calls < 2 {
		t.Fatal("the CAS-loop refresh never ran")
	}
}

func TestCheckpointRefs_CorruptSnapshot(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.WriteCheckpoint(ctx, TriggerManual); err != nil {
		t.Fatal(err)
	}
	// Garbage at the refs.pb key → unmarshal failure, not a store error.
	if err := st.Delete(ctx, h.repoKey(store.CheckpointRefsKey(0)), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, h.repoKey(store.CheckpointRefsKey(0)), store.PutBody{Bytes: []byte("garbage")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = h.RefsAtSeq(ctx, 0)
	if err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("corrupt snapshot = %v", err)
	}
}
