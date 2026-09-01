// gap2_test.go — last-mile coverage: engine cache/inflate/delta faults,
// claimSlot ladder restarts, checkpoint provenance fallbacks, foldTo time
// cuts, and buildRemoteIndex fault paths.
package wal

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
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

// ---- remote engine: object LRU, readRaw/headerAt/inflate faults ---------------------

func TestRemoteEngine_ObjLRUAndEviction(t *testing.T) {
	pack, _, _, _, _ := buildTestPack(t)
	eng, _, ix := newTestEngine(t, &blockStore{data: pack}, "acme/api")
	eng.objCap = 10 // tiny: every cacheObj evicts the previous
	ctx := context.Background()

	eng.cacheObj(objKey{ix.Checksum, 1}, "blob", []byte("0123456789"))
	eng.cacheObj(objKey{ix.Checksum, 1}, "blob", []byte("0123456789")) // refresh, no growth
	eng.cacheObj(objKey{ix.Checksum, 2}, "blob", []byte("x"))
	if _, _, ok := eng.lookupObj(objKey{ix.Checksum, 2}); !ok {
		t.Fatal("newest object evicted")
	}
	if _, _, ok := eng.lookupObj(objKey{ix.Checksum, 1}); ok {
		t.Fatal("LRU object survived eviction")
	}
	if _, _, ok := eng.lookupObj(objKey{ix.Checksum, 99}); ok {
		t.Fatal("phantom object found")
	}
	// decodeAt caches: decode twice, second from the LRU.
	_, _, err := eng.decodeAt(ctx, ix, baseOffOf(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := eng.decodeAt(ctx, ix, baseOffOf(t)); err != nil {
		t.Fatalf("cached decode: %v", err)
	}
}

func TestRemoteEngine_ReadFaults(t *testing.T) {
	pack, _, _, _, _ := buildTestPack(t)
	bs := &blockStore{data: pack}
	eng, _, ix := newTestEngine(t, bs, "acme/api")
	ctx := context.Background()
	pk := eng.packKey(ix)

	// readRaw → block Get error.
	bs.getErr = errors.New("down")
	if _, err := eng.readRaw(ctx, pk, 0, 10); err == nil {
		t.Fatal("readRaw with failing store succeeded")
	}
	// headerAt → readRaw error.
	if _, err := eng.headerAt(ctx, pk, 0); err == nil {
		t.Fatal("headerAt with failing store succeeded")
	}
	// inflateAt → readRaw error.
	if _, err := eng.inflateAt(ctx, pk, 0, 10); err == nil {
		t.Fatal("inflateAt with failing store succeeded")
	}
	// decodeAt → inflate error on the base.
	bs.getErr = nil
	if _, err := eng.inflateAt(ctx, pk, 0, 64<<20+1); err == nil {
		t.Fatal("huge inflate succeeded")
	}
	// readRaw past the object end: clamped store returns empty blocks → nil.
	if d, err := eng.readRaw(ctx, pk, int64(len(pack))+10, 4); err != nil || d != nil {
		t.Fatalf("readRaw past end = %v %v", d, err)
	}
}

func TestRemoteEngine_HeaderAtDeltaFaults(t *testing.T) {
	ctx := context.Background()
	// An OFS-delta header whose varint runs off the end of the pack.
	ofsPack := []byte{byte(objOfsDelta << 4), 0x80} // continuation, then EOF
	eng, _, ix := newTestEngine(t, &blockStore{data: ofsPack}, "acme/api")
	if _, err := eng.headerAt(ctx, eng.packKey(ix), 0); err == nil {
		t.Fatal("truncated ofs varint accepted")
	}
	// A REF-delta header: base oid parsed, dataOff advanced; with no base in
	// any pack, decodeAt and header both report the missing base.
	missingOid := strings.Repeat("9", 40)
	rawMissing, _ := hex.DecodeString(missingOid)
	refPack := append([]byte{byte(objRefDelta<<4) | 0x05},
		append(append([]byte{}, rawMissing...), make([]byte, 8)...)...)
	eng2, _, ix2 := newTestEngine(t, &blockStore{data: refPack}, "acme/api")
	pe, err := eng2.headerAt(ctx, eng2.packKey(ix2), 0)
	if err != nil || pe.typ != objRefDelta || pe.baseOid != missingOid {
		t.Fatalf("ref-delta header = %+v %v", pe, err)
	}
	if _, _, err := eng2.decodeAt(ctx, ix2, 0); err == nil || !strings.Contains(err.Error(), "delta base") {
		t.Fatalf("decode missing base = %v", err)
	}
	if _, _, err := eng2.header(ctx, ix2, 0); err == nil || !strings.Contains(err.Error(), "delta base") {
		t.Fatalf("header missing base = %v", err)
	}
	// A REF-delta whose base lives in ANOTHER pack: header walks across packs.
	pack1, _, _, baseOid, _ := buildTestPack(t)
	pack1Sum := fmt.Sprintf("%x", pack1[len(pack1)-20:])
	ix1 := &packIndex{Checksum: pack1Sum, oids: []string{baseOid}, offsets: []int64{baseOffOf(t)},
		byOid: map[string]int64{baseOid: baseOffOf(t)}}
	rawBase, _ := hex.DecodeString(baseOid)
	// git delta: base_size, result_size, insert "ok" — zlib-framed payload.
	base := []byte("hello world, hello walhub!\n")
	inst := []byte{}
	for v := uint64(len(base)); ; {
		inst = append(inst, byte(v&0x7f))
		v >>= 7
		if v == 0 {
			break
		}
		inst[len(inst)-1] |= 0x80
	}
	inst = append(inst, 0x02) // result size = 2
	inst = append(inst, 0x02) // insert command: copy next 2 bytes
	inst = append(inst, 'o', 'k')
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	zw.Write(inst)
	zw.Close()
	zDelta := zbuf.Bytes()
	delta2 := append([]byte{byte(objRefDelta<<4) | byte(len(inst))}, append(append([]byte{}, rawBase...), zDelta...)...)
	ix2b := &packIndex{Checksum: "p2", oids: []string{testOid2}, offsets: []int64{0}, byOid: map[string]int64{testOid2: 0}}
	eng3 := &remoteEngine{
		packs:  &RemotePacks{idxs: []*packIndex{ix1, ix2b}},
		blocks: newBlockCache(1 << 20),
		st: &multiPackStore{packs: map[string][]byte{
			repoPrefix("acme/api") + store.PackKey(pack1Sum): pack1,
			repoPrefix("acme/api") + store.PackKey("p2"):     delta2,
		}},
		repoID: "acme/api", objCap: 0,
	}
	kind, size, err := eng3.header(ctx, ix2b, 0)
	if err != nil || kind != "blob" || size != int64(len("hello world, hello walhub!\n")) {
		t.Fatalf("cross-pack header = %s %d %v", kind, size, err)
	}
	kind, data, err := eng3.decodeAt(ctx, ix2b, 0)
	if err != nil || kind != "blob" || string(data) != "ok" {
		t.Fatalf("cross-pack decode = %s %q %v", kind, data, err)
	}
	// decodeAt: inflate fault on the delta payload mid-fold (fresh engine:
	// the previous decode cached the result in the object LRU).
	eng3b := &remoteEngine{packs: eng3.packs, blocks: newBlockCache(1 << 20),
		st:     &errAtStore{inner: eng3.st, failKey: repoPrefix("acme/api") + store.PackKey("p2")},
		repoID: "acme/api", objCap: 0}
	if _, _, err := eng3b.decodeAt(ctx, ix2b, 0); err == nil {
		t.Fatal("decode with failing delta store succeeded")
	}
}

// multiPackStore serves whole-object bytes keyed by store key.
type multiPackStore struct {
	store.ObjectStore
	packs map[string][]byte
}

func (m *multiPackStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	data, ok := m.packs[key]
	if !ok {
		return nil, store.NewNotFound(key)
	}
	start, end := int64(0), int64(len(data))
	if opts.Range != nil {
		start, end = opts.Range[0], opts.Range[1]
		if start > int64(len(data)) {
			start = int64(len(data))
		}
		if end > int64(len(data)) {
			end = int64(len(data))
		}
	}
	return store.Object{Meta: store.ObjectMeta{Key: key}, Body: io.NopCloser(bytes.NewReader(data[start:end]))}, nil
}

// errAtStore fails Get for one specific key.
type errAtStore struct {
	store.ObjectStore
	inner   store.ObjectStore
	failKey string
}

func (e *errAtStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	if key == e.failKey {
		return nil, store.NewOther(key, errors.New("mid-fold failure"))
	}
	return e.inner.Get(ctx, key, opts)
}

func TestApplyGitDelta_TruncationVectors(t *testing.T) {
	base := []byte("0123456789")
	varint := func(vs ...uint64) []byte {
		var out []byte
		for _, v := range vs {
			for {
				x := byte(v & 0x7f)
				v >>= 7
				if v != 0 {
					x |= 0x80
				}
				out = append(out, x)
				if v == 0 {
					break
				}
			}
		}
		return out
	}
	// Copy command whose offset byte is missing entirely.
	if _, err := applyGitDelta(base, append(append(varint(10), varint(2)...), 0x81)); err == nil {
		t.Fatal("offset-truncated copy accepted")
	}
	// Copy command whose size byte is missing entirely.
	if _, err := applyGitDelta(base, append(append(varint(10), varint(2)...), 0x90)); err == nil {
		t.Fatal("size-truncated copy accepted")
	}
}

// ---- checkpoint provenance fallbacks and CAS exhaustion ------------------------------

func TestWriteCheckpoint_ProvenanceFallbacksAndRound1Failure(t *testing.T) {
	// A repo with no entries: firstEntryTime/lastEntryTime are zero, so the
	// provenance chain falls all the way through to `now`.
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.WriteCheckpoint(ctx, TriggerManual); err != nil {
		t.Fatalf("checkpoint of an empty repo: %v", err)
	}
	m, _ := h.ManifestSnapshot()
	if m.Checkpoint == nil || !m.Checkpoint.FirstStateAt.Go().After(time.Unix(0, 0)) {
		t.Fatalf("provenance = %+v", m.Checkpoint)
	}
	if lastEntryOf(h, &pbManifest{}, time.Time{}) != (time.Time{}) {
		t.Fatal("lastEntryOf must be zero without updated_at")
	}

	// Round-1 store failures surface as a store error.
	hk, _ := attachHooks(t, r)
	hk.putErr = func(key string, n int) error {
		if strings.Contains(key, "checkpoints/") {
			return store.NewOther(key, errors.New("round1 down"))
		}
		return nil
	}
	if _, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/x", git.Sha1.ZeroHex(), strings.Repeat("a", 40))}); err != nil {
		t.Fatal(err)
	}
	if err := h.WriteCheckpoint(ctx, TriggerManual); err == nil {
		t.Fatal("checkpoint with failing round-1 PUTs succeeded")
	}

	// Every round-2 CAS loses: retries exhaust with a typed error.
	hk.putErr = func(key string, n int) error {
		if strings.HasSuffix(key, "manifest.pb") {
			return store.NewPrecondition(key, "other")
		}
		return nil
	}
	err = h.WriteCheckpoint(ctx, TriggerManual)
	if err == nil || !strings.Contains(err.Error(), "retries exhausted") {
		t.Fatalf("checkpoint CAS = %v, want exhaustion", err)
	}
}

// ---- publish: claimSlot and casManifest ladder restarts -------------------------------

func TestPublish_ClaimSlotRestartLadder(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// A rival writer commits seq 1 (its segment is in the bucket but our
	// cached manifest is stale): our first slot claim hits the phantom,
	// freshHead shows head ≥ segFirst → the ladder restarts against the
	// fresh manifest and the push commits at seq 2.
	oid := strings.Repeat("a", 40)
	seg := proto.EncodeSegment([]*proto.LogEntry{{
		Seq: 1, Kind: proto.EntryKindRefUpdate, Writer: "rival",
		CreatedAt: TsPtr(time.Now().UTC()),
		Txn:       refTxn("refs/heads/rival", git.Sha1.ZeroHex(), strings.Repeat("b", 40)),
	}})
	if _, err := r.st.Put(ctx, h.repoKey(store.LogSegmentKey(1)), store.PutBody{Bytes: seg}, store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
	rival := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "acme/api", ObjectFormat: "sha1",
		HeadSeq: 1, Revision: 2, LogSegments: []*proto.LogSegmentRef{{Key: store.LogSegmentKey(1), FirstSeq: 1, LastSeq: 1}}}
	if _, err := r.st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: rival.Marshal()}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// Our handle still holds the STALE manifest (head 0). Force the next
	// freshness check so the claim-time freshHead observes the rival.
	h.syncMu.Lock()
	h.freshAt = time.Time{}
	h.syncMu.Unlock()
	hk.putErr = func(key string, n int) error {
		if strings.HasSuffix(key, "/log/0000000000000001.pb") {
			return store.NewPrecondition(key, "rival")
		}
		return nil
	}
	res, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), oid)})
	if err != nil {
		t.Fatalf("publish across a slot restart: %v", err)
	}
	if res.Seq != 2 {
		t.Fatalf("seq = %d, want 2 (restarted past the rival)", res.Seq)
	}
	m, _ := h.ManifestSnapshot()
	if m.HeadSeq != 2 {
		t.Fatalf("head = %d", m.HeadSeq)
	}
}

func TestPublish_CasManifestRestartThenCommit(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// First manifest CAS loses (412): our own segment is deleted and the
	// ladder restarts; the second attempt commits.
	hk.putErr = func(key string, n int) error {
		if strings.HasSuffix(key, "manifest.pb") && n == 2 { // create=1, cas=2
			return store.NewPrecondition(key, "other")
		}
		return nil
	}
	res, err := h.Publish(ctx, PublishRequest{Txn: refTxn("refs/heads/main", git.Sha1.ZeroHex(), strings.Repeat("c", 40)), Synced: true})
	if err != nil || res.Seq != 1 {
		t.Fatalf("res = %+v err=%v, want a committed push", res, err)
	}
	// The loser's segment from attempt 1 was CAS-deleted.
	if ok, _ := store.Exists(ctx, r.st, h.repoKey(store.LogSegmentKey(1))); !ok {
		t.Fatal("segment missing after restart (it should have been re-created)")
	}
}

func TestAnnotatePack_PutCreatePath(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pack-ee.pack")
	if err := os.WriteFile(path, []byte("PACK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.AddPack(ctx, path, strings.Repeat("e", 40), 0, nil); err != nil {
		t.Fatal(err)
	}
	// Remove the bucket manifest and clear the CAS version: the annotate
	// CAS then takes the PutCreate branch.
	if err := r.st.Delete(ctx, "repos/acme/api/manifest.pb", ""); err != nil {
		t.Fatal(err)
	}
	h.syncMu.Lock()
	h.version = ""
	h.syncMu.Unlock()
	if err := h.AnnotatePack(ctx, strings.Repeat("e", 40), true, false, false); err != nil {
		t.Fatalf("annotate with empty version: %v", err)
	}
}

// ---- logreader: lock cancellation, foldTo cuts, checkpointRefs faults ------------------

func TestLogReaders_LockCancellationAndFreshenError(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.syncMu.LockMeasured(ctx, "sync_mutex", h.ID); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := h.ReadLog(cctx, 1, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadLog = %v, want canceled", err)
	}
	if _, err := h.RefsAtSeq(cctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("RefsAtSeq = %v, want canceled", err)
	}
	if _, err := h.RefsAsOf(cctx, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("RefsAsOf = %v, want canceled", err)
	}
	h.syncMu.Unlock()
	// A failing manifest GET fails every reader (freshenManifest under the
	// hood).
	hk.getErr = func(key string, n int) error {
		if strings.HasSuffix(key, "manifest.pb") {
			return store.NewOther(key, errors.New("down"))
		}
		return nil
	}
	if _, err := h.ReadLog(ctx, 1, 0); err == nil {
		t.Fatal("ReadLog with failing freshen succeeded")
	}
	hk.getErr = nil
	// checkpointRefs: store error and absent object.
	if err := h.WriteCheckpoint(ctx, TriggerManual); err != nil {
		t.Fatal(err)
	}
	hk.getErr = func(key string, n int) error {
		if strings.Contains(key, "checkpoints/") {
			return store.NewOther(key, errors.New("down"))
		}
		return nil
	}
	if _, err := h.RefsAtSeq(ctx, 1); err == nil {
		t.Fatal("RefsAtSeq with failing checkpoint GET succeeded")
	}
	hk.getErr = nil
	if err := r.st.Delete(ctx, h.repoKey(store.CheckpointRefsKey(1)), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.RefsAtSeq(ctx, 1); err == nil {
		t.Fatal("RefsAtSeq with absent refs snapshot succeeded")
	}
}

func TestFoldTo_TimeCutsAndEntryKinds(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	// Segment with: a HEAD symbolic move, a ref push WITHOUT created_at, and
	// an entry created AFTER the cut.
	early := TsPtr(time.Now().UTC().Add(-time.Hour))
	late := TsPtr(time.Now().UTC().Add(time.Hour))
	seg := &proto.LogSegmentRef{Key: store.LogSegmentKey(1), FirstSeq: 1, LastSeq: 3, Sealed: true}
	entries := []*proto.LogEntry{
		{Seq: 1, Kind: proto.EntryKindRefUpdate, Writer: "w", CreatedAt: early,
			Txn: &proto.RefTransaction{Updates: []*proto.RefUpdate{
				{Name: "HEAD", NewSymbolicTarget: "refs/heads/main"},
				{Name: "refs/heads/main", OldOid: git.Sha1.ZeroHex(), NewOid: strings.Repeat("a", 40)},
			}}},
		{Seq: 2, Kind: proto.EntryKindSettings, Writer: "w", // no CreatedAt: must not break the walk
			Settings: &proto.RepoSettings{Toml: "[x]\n"}},
		{Seq: 3, Kind: proto.EntryKindRefUpdate, Writer: "w", CreatedAt: late,
			Txn: refTxn("refs/heads/future", git.Sha1.ZeroHex(), strings.Repeat("b", 40))},
	}
	body := proto.EncodeSegment(entries)
	if _, err := st.Put(ctx, h.repoKey(seg.Key), store.PutBody{Bytes: body}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	m, version := h.ManifestSnapshot()
	next := *m
	next.LogSegments = []*proto.LogSegmentRef{seg}
	next.HeadSeq = 3
	next.Revision = m.Revision + 1
	h.syncMu.Lock()
	h.manifest, h.version, h.heldRev = &next, version, next.Revision
	h.syncMu.Unlock()

	// As of a time between seq 2 and seq 3: the future branch is excluded,
	// the created_at-less entry still applied.
	cut := time.Now().UTC()
	v, err := h.RefsAsOf(ctx, cut)
	if err != nil {
		t.Fatal(err)
	}
	if v.HeadTarget != "refs/heads/main" {
		t.Fatalf("head target = %q", v.HeadTarget)
	}
	found := false
	for _, r := range v.Refs {
		if r.Name == "refs/heads/future" {
			found = true
		}
	}
	if found {
		t.Fatalf("future entry leaked past the time cut: %+v", v.Refs)
	}
	if len(v.Refs) != 1 {
		t.Fatalf("refs = %+v", v.Refs)
	}
	// foldTo with a failing segment GET surfaces the store error.
	hk, _ := attachHooks(t, r)
	hk.getErr = func(key string, n int) error {
		if strings.Contains(key, "/log/") {
			return store.NewOther(key, errors.New("down"))
		}
		return nil
	}
	if _, err := h.RefsAsOf(ctx, cut); err == nil {
		t.Fatal("foldTo with failing segment GET succeeded")
	}
}

// ---- reconcile: buildRemoteIndex faults ------------------------------------------------

func TestBuildRemoteIndex_Faults(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("1", 40)
	m, _ := h.ManifestSnapshot()
	next := *m
	next.Packs = []*proto.PackRef{{Checksum: sum, PackSize: 10, IdxSize: 1, Seq: 1}}
	next.Revision = m.Revision + 1
	h.syncMu.Lock()
	h.manifest = &next
	h.syncMu.Unlock()

	// remote-idx as a FILE → MkdirAll fails.
	if err := os.WriteFile(filepath.Join(h.repo.Path, "remote-idx"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.buildRemoteIndex(ctx, nil, &next); err == nil {
		t.Fatal("buildRemoteIndex over a file succeeded")
	}
	if err := os.Remove(filepath.Join(h.repo.Path, "remote-idx")); err != nil {
		t.Fatal(err)
	}
	// Corrupt idx in the store → openPackIndex error.
	if _, err := st.Put(ctx, h.repoKey(store.IdxKey(sum)), store.PutBody{Bytes: []byte("garbage")}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := h.buildRemoteIndex(ctx, nil, &next); err == nil {
		t.Fatal("buildRemoteIndex with corrupt idx succeeded")
	}
	// Absent idx → corrupt (the download gets nothing).
	if err := st.Delete(ctx, h.repoKey(store.IdxKey(sum)), ""); err != nil {
		t.Fatal(err)
	}
	if err := h.buildRemoteIndex(ctx, nil, &next); err == nil {
		t.Fatal("buildRemoteIndex with absent idx succeeded")
	}
}

// ---- sync/registry leftovers ------------------------------------------------------------

func TestApplyDelta_NilManifest(t *testing.T) {
	r, _ := newTestRegistry(t)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	h.syncMu.Lock()
	h.manifest = nil
	h.syncMu.Unlock()
	if err := h.applyDelta(ctx); err != nil {
		t.Fatalf("applyDelta nil manifest: %v", err)
	}
}

func TestFoldCheckpoint_StoreError(t *testing.T) {
	r, _ := newTestRegistry(t)
	hk, _ := attachHooks(t, r)
	ctx := context.Background()
	h, err := r.Create(ctx, "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	m, version := h.ManifestSnapshot()
	next := *m
	next.Checkpoint = &proto.CheckpointRef{Seq: 1, Key: store.CheckpointKey(1)}
	next.Revision = m.Revision + 1
	h.syncMu.Lock()
	h.manifest, h.version, h.heldRev = &next, version, next.Revision
	h.syncMu.Unlock()
	hk.getErr = func(key string, n int) error {
		if strings.Contains(key, "checkpoints/") {
			return store.NewOther(key, errors.New("down"))
		}
		return nil
	}
	if err := h.catchUp(ctx); err == nil {
		t.Fatal("foldCheckpoint with failing GET succeeded")
	}
}

func TestOpenOrInit_LocalPathIsFile(t *testing.T) {
	r, st := newTestRegistry(t)
	ctx := context.Background()
	m := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "acme/api", ObjectFormat: "sha1", Revision: 1}
	if _, err := st.Put(ctx, "repos/acme/api/manifest.pb", store.PutBody{Bytes: m.Marshal()}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	// The local repo path is a FILE: both open and init fail.
	if err := os.MkdirAll(filepath.Join(r.vals.cacheDir, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.vals.cacheDir, "acme", "api.git"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Open(ctx, "acme/api"); err == nil {
		t.Fatal("open over a file path succeeded")
	}
}
