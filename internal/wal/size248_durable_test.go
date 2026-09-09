// size248_durable_test.go — Forgejo #248 durability (review #258 unblock):
// the publish path must durably write the per-repo sidecar on PUSH/COMPACT
// (parallel PUT, +0 sequential trips) — sweep-only is insufficient because
// maintain-less deployments would have unbounded absence. These tests assert
// the durable sidecar bytes after publish, never running the sweep:
// push → sidecar present; compact (supersede) → sidecar reflects the live
// set; ref-only/settings publishes skip; a failed sidecar PUT stays
// best-effort (push still commits — the manifest CAS is the commit point).
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
	"git.packden.us/crueber/walhub/internal/sizecatalog"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func readSidecar(t *testing.T, ctx context.Context, st store.ObjectStore, id string) *sizecatalog.Stats {
	t.Helper()
	body, _, err := store.GetBytes(ctx, st, "repos/"+id+"/"+store.StatsKeySuffix, store.GetOptions{})
	if err != nil {
		t.Fatalf("sidecar GET: %v", err)
	}
	s, ok, err := sizecatalog.DecodeStats(body)
	if err != nil {
		t.Fatalf("sidecar decode: %v", err)
	}
	if !ok {
		t.Fatalf("sidecar absent after pack-changing publish (body len %d)", len(body))
	}
	return s
}

func TestPublishSidecarDurablePushThenCompact(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/sized", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	zero := git.Sha1.ZeroHex()
	oid1 := strings.Repeat("a", 40)

	// PUSH with a pack. Content is opaque to the WAL (git never opens it
	// here) but the bytes must exist: later syncs materialize them.
	packData1 := []byte("PACK-one-payload-for-size-sidecar")
	idxData1 := []byte("idx-one-bytes")
	dir := t.TempDir()
	packPath1 := filepath.Join(dir, "pack-packone.pack")
	idxPath1 := filepath.Join(dir, "pack-packone.idx")
	if err := os.WriteFile(packPath1, packData1, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath1, idxData1, 0o644); err != nil {
		t.Fatal(err)
	}
	wantSize1 := uint64(len(packData1) + len(idxData1))
	res, err := h.Publish(ctx, PublishRequest{
		Pack: &PreparedPack{Checksum: "packone", PackPath: packPath1, IdxPath: idxPath1,
			PackSize: uint64(len(packData1)), IdxSize: uint64(len(idxData1)), ObjectCount: 10},
		Txn: refTxn("refs/heads/main", zero, oid1),
	})
	if err != nil || res.Seq == 0 {
		t.Fatalf("push publish: res=%+v err=%v", res, err)
	}
	s := readSidecar(t, ctx, st, "acme/sized")
	if s.SizeBytes != wantSize1 || s.ObjectCount != 10 || s.HeadSeq != res.Seq {
		t.Fatalf("sidecar after push = %+v, want size=%d objs=10 head=%d", s, wantSize1, res.Seq)
	}
	// The wal mirror agrees with the sizecatalog derivation, and the sidecar
	// agrees with the committed manifest snapshot.
	m, _ := h.ManifestSnapshot()
	if ws, wo := statsSizeOf(m.Packs); ws != s.SizeBytes || wo != s.ObjectCount {
		t.Fatalf("wal mirror (%d,%d) != sidecar (%d,%d)", ws, wo, s.SizeBytes, s.ObjectCount)
	}
	if ms, mo, ok := sizecatalog.ManifestSize(m); !ok || ms != s.SizeBytes || mo != s.ObjectCount {
		t.Fatalf("sizecatalog manifest size (%d,%d,%v) != sidecar (%d,%d)", ms, mo, ok, s.SizeBytes, s.ObjectCount)
	}

	// COMPACT superseding "packone" with a merged pack: "packone" leaves the
	// live set and the durable sidecar must reflect that.
	packData2 := []byte("PACK-two-merged-payload")
	idxData2 := []byte("idx-two-bytes!")
	packPath2 := filepath.Join(dir, "pack-packtwo.pack")
	idxPath2 := filepath.Join(dir, "pack-packtwo.idx")
	if err := os.WriteFile(packPath2, packData2, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath2, idxData2, 0o644); err != nil {
		t.Fatal(err)
	}
	wantSize2 := uint64(len(packData2) + len(idxData2))
	res2, err := h.PublishCompact(ctx,
		&PreparedPack{Checksum: "packtwo", PackPath: packPath2, IdxPath: idxPath2,
			PackSize: uint64(len(packData2)), IdxSize: uint64(len(idxData2)), ObjectCount: 20},
		[]string{"packone"}, map[string]string{"principal": "t"})
	if err != nil || res2.Seq == 0 {
		t.Fatalf("compact publish: res=%+v err=%v", res2, err)
	}
	s2 := readSidecar(t, ctx, st, "acme/sized")
	// Live set is {packtwo}: size = its pack+idx; objects 20.
	if s2.SizeBytes != wantSize2 || s2.ObjectCount != 20 || s2.HeadSeq != res2.Seq {
		t.Fatalf("sidecar after compact = %+v, want size=%d objs=20 head=%d", s2, wantSize2, res2.Seq)
	}
	m2, _ := h.ManifestSnapshot()
	for _, p := range m2.Packs {
		if p.Checksum == "packone" {
			t.Fatalf("superseded pack still live: %+v", m2.Packs)
		}
	}
}

func TestPublishSidecarSkippedForRefOnly(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/nopack", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	zero := git.Sha1.ZeroHex()
	// Ref-only push: no pack change → no sidecar write.
	if _, err := h.Publish(ctx, PublishRequest{
		Txn: refTxn("refs/heads/main", zero, strings.Repeat("b", 40)),
	}); err != nil {
		t.Fatalf("ref-only publish: %v", err)
	}
	// Settings publish: manifest-only settings change → no sidecar write.
	if err := h.PublishSettings(ctx, "[git]\ndefault_branch = \"main\"\n", "op", "set defaults", nil); err != nil {
		t.Fatalf("settings publish: %v", err)
	}
	body, _, err := store.GetBytes(ctx, st, "repos/acme/nopack/"+store.StatsKeySuffix, store.GetOptions{})
	if err != nil && !store.IsNotFound(err) {
		t.Fatalf("sidecar probe: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("ref-only/settings publishes must not write the sidecar (got %d bytes)", len(body))
	}
}

// failStatsStore fails sidecar PUTs while delegating everything else.
type failStatsStore struct {
	store.ObjectStore
}

func (f failStatsStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if strings.HasSuffix(key, store.StatsKeySuffix) {
		return store.ObjectMeta{}, errors.New("boom: stats sidecar write failed")
	}
	return f.ObjectStore.Put(ctx, key, body, opts)
}

func TestPublishSidecarBestEffortOnWriteFailure(t *testing.T) {
	base := store.NewMemory()
	r := NewRegistry(context.Background(), failStatsStore{base}, testConfig(t))
	defer r.Close()
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/flaky", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Publish(ctx, PublishRequest{
		Pack: &PreparedPack{Checksum: "flaky1", PackSize: 500, IdxSize: 50, ObjectCount: 5},
		Txn:  refTxn("refs/heads/main", git.Sha1.ZeroHex(), strings.Repeat("c", 40)),
	})
	if err != nil || res.Seq == 0 {
		t.Fatalf("push must commit despite sidecar failure: res=%+v err=%v", res, err)
	}
	m, _ := h.ManifestSnapshot()
	if m.HeadSeq != res.Seq {
		t.Fatalf("manifest head=%d, want committed seq %d", m.HeadSeq, res.Seq)
	}
	body, _, err := store.GetBytes(ctx, base, "repos/acme/flaky/"+store.StatsKeySuffix, store.GetOptions{})
	if err != nil && !store.IsNotFound(err) {
		t.Fatalf("sidecar probe: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("failed sidecar write must leave no bytes (got %d)", len(body))
	}
}

func TestStatsHelpersAgreeWithSizecatalog(t *testing.T) {
	packs := []*proto.PackRef{
		nil,
		{Checksum: "a", PackSize: 1000, IdxSize: 100, ObjectCount: 10},
		{Checksum: "b", PackSize: 2000, IdxSize: 200, ObjectCount: 20},
	}
	if ws, wo := statsSizeOf(packs); ws != 3300 || wo != 30 {
		t.Fatalf("statsSizeOf = (%d,%d), want (3300,30)", ws, wo)
	}
	if cs, co := sizecatalog.SizeOf(packs); cs != 3300 || co != 30 {
		t.Fatalf("sizecatalog.SizeOf = (%d,%d), want (3300,30)", cs, co)
	}
	// Saturation (never wraps): both helpers pin MaxUint64.
	huge := []*proto.PackRef{{Checksum: "h", PackSize: ^uint64(0), IdxSize: 1, ObjectCount: ^uint64(0)}}
	if ws, wo := statsSizeOf(huge); ws != ^uint64(0) || wo != ^uint64(0) {
		t.Fatalf("saturating statsSizeOf = (%d,%d), want max/max", ws, wo)
	}
	// batchChangesLiveSet: push/compact write, ref-only/settings skip.
	push := &proto.LogEntry{Seq: 1, Kind: proto.EntryKindPush}
	compact := &proto.LogEntry{Seq: 2, Kind: proto.EntryKindCompact}
	refupd := &proto.LogEntry{Seq: 3, Kind: proto.EntryKindRefUpdate}
	settings := &proto.LogEntry{Seq: 4, Kind: proto.EntryKindSettings}
	if !batchChangesLiveSet([]*proto.LogEntry{refupd, push}) {
		t.Fatal("batch with PUSH must write the sidecar")
	}
	if !batchChangesLiveSet([]*proto.LogEntry{compact}) {
		t.Fatal("batch with COMPACT must write the sidecar")
	}
	if batchChangesLiveSet([]*proto.LogEntry{refupd, settings, nil}) {
		t.Fatal("ref-only/settings batch must skip the sidecar")
	}
	if batchChangesLiveSet(nil) {
		t.Fatal("empty batch must skip the sidecar")
	}
	// encodeStatsSidecar decodes through the owned shape (law 8 mirror pin).
	body := encodeStatsSidecar(3300, 30, 7, time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC))
	s, ok, err := sizecatalog.DecodeStats(body)
	if err != nil || !ok {
		t.Fatalf("sidecar round-trip: ok=%v err=%v body=%s", ok, err, body)
	}
	if s.SizeBytes != 3300 || s.ObjectCount != 30 || s.HeadSeq != 7 || s.Version != sizecatalog.StatsVersion {
		t.Fatalf("sidecar round-trip = %+v", s)
	}
}
