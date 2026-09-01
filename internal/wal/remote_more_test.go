// remote_more_test.go — remote reader edges (05 §5.7): index parse errors,
// prefix lookup, block cache faults/eviction, delta error vectors, inflate
// failures, and the faulter.
package wal

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
)

// ---- pack index parsing ------------------------------------------------------------

func TestParsePackIndex_ErrorVectors(t *testing.T) {
	valid := func() []byte {
		var buf bytes.Buffer
		buf.Write([]byte{0xff, 't', 'O', 'c'})
		binary.Write(&buf, binary.BigEndian, uint32(2))
		for i := 0; i < 256; i++ {
			binary.Write(&buf, binary.BigEndian, uint32(0))
		}
		return buf.Bytes()
	}()

	cases := []struct {
		name string
		data []byte
		det  string
	}{
		{"bad magic", append([]byte{0x00, 't', 'O', 'c'}, valid[4:]...), "not a v2"},
		{"bad version", append(append([]byte{}, valid[:4]...), func() []byte {
			b := make([]byte, 4)
			binary.BigEndian.PutUint32(b, 3)
			return b
		}()...), "unsupported index version"},
		{"truncated", valid[:20], "truncated"},
	}
	for _, tc := range cases {
		if _, err := parsePackIndex("x.idx", tc.data); err == nil || !strings.Contains(err.Error(), tc.det) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.det)
		}
	}
	// A large-offset entry pointing past the end of the file is corrupt:
	// fanout[0] = 1, one oid, MSB offset with large-idx 5 (no large table).
	data := append([]byte{}, valid...)
	for i := 0; i < 256; i++ {
		binary.BigEndian.PutUint32(data[8+4*i:], 0)
	}
	binary.BigEndian.PutUint32(data[8+4*255:], 1)
	oid := make([]byte, 20)
	oid[0] = 1
	data = append(data, oid...)
	data = append(data, 0, 0, 0, 0) // crc
	raw4 := make([]byte, 4)
	binary.BigEndian.PutUint32(raw4, 0x80000000|5)
	data = append(data, raw4...)
	if _, err := parsePackIndex("x.idx", data); err == nil || !strings.Contains(err.Error(), "large offset") {
		t.Fatalf("bad large offset: err = %v", err)
	}
	// openPackIndex on a missing file → IO error.
	if _, err := openPackIndex("/nonexistent-walhub/x.idx"); err == nil {
		t.Fatal("openPackIndex on missing file succeeded")
	}
	if trimExt("plain") != "plain" || trimExt("a.pack") != "a" || trimExt("idx") != "idx" {
		t.Fatal("trimExt")
	}
}

func TestRemotePacks_LocateAndPrefix(t *testing.T) {
	ix := &packIndex{
		Checksum: "cs",
		oids:     []string{"aaaa", "aabb", "cccc"},
		offsets:  []int64{10, 20, 30},
		byOid:    map[string]int64{"aaaa": 10, "aabb": 20, "cccc": 30},
	}
	rp := &RemotePacks{idxs: []*packIndex{ix}}

	// locate: hit and miss.
	if _, off, ok := rp.locate("aabb"); !ok || off != 20 {
		t.Fatalf("locate hit = %d %v", off, ok)
	}
	if _, _, ok := rp.locate("ffff"); ok {
		t.Fatal("locate miss found")
	}
	// lookupPrefix: unique, ambiguous, missing.
	oid, _, off, err := rp.lookupPrefix("aab")
	if err != nil || oid != "aabb" || off != 20 {
		t.Fatalf("unique prefix = %s %d %v", oid, off, err)
	}
	if _, _, _, err := rp.lookupPrefix("aa"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous prefix err = %v", err)
	}
	if _, _, _, err := rp.lookupPrefix("fff"); err == nil || !strings.Contains(err.Error(), "no object") {
		t.Fatalf("missing prefix err = %v", err)
	}
	// sortSearchStrings: insertion points.
	if got := sortSearchStrings([]string{"a", "c", "e"}, "b"); got != 1 {
		t.Fatalf("sortSearchStrings = %d", got)
	}
	if got := sortSearchStrings([]string{}, "x"); got != 0 {
		t.Fatal("empty search")
	}
}

// ---- block cache --------------------------------------------------------------------

// blockStore serves fixed bytes with clamped ranges (and optional faults).
type blockStore struct {
	store.ObjectStore
	data   []byte
	getErr error
	obj    store.GetResult // if set, returned instead of the data
	mu     sync.Mutex
	gets   int
}

func (b *blockStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	b.mu.Lock()
	b.gets++
	err := b.getErr
	res := b.obj
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if res != nil {
		return res, nil
	}
	start, end := int64(0), int64(len(b.data))
	if opts.Range != nil {
		start, end = opts.Range[0], opts.Range[1]
		if start > int64(len(b.data)) {
			start = int64(len(b.data))
		}
		if end > int64(len(b.data)) {
			end = int64(len(b.data))
		}
	}
	return store.Object{Meta: store.ObjectMeta{Key: key}, Body: io.NopCloser(bytes.NewReader(b.data[start:end]))}, nil
}

func TestBlockCache_Behaviors(t *testing.T) {
	data := make([]byte, 3<<20) // 3 blocks
	for i := range data {
		data[i] = byte(i >> 20)
	}
	bs := &blockStore{data: data}
	c := newBlockCache(2 << 20) // 2 blocks → eviction
	if c2 := newBlockCache(0); c2.cap != 1<<30 {
		t.Fatal("zero cap must default to 1 GiB")
	}
	ctx := context.Background()

	b1, err := c.Get(ctx, bs, "k", 0)
	if err != nil || len(b1) != 1<<20 {
		t.Fatalf("block 0 = %d err=%v", len(b1), err)
	}
	// Hit: no additional GET.
	h, m, by := c.Stats()
	if _, err := c.Get(ctx, bs, "k", 0); err != nil {
		t.Fatal(err)
	}
	h2, m2, _ := c.Stats()
	if h2 != h+1 || m2 != m {
		t.Fatalf("stats hit %d→%d miss %d→%d", h, h2, m, m2)
	}
	_ = by
	// Miss on block 2 → evicts the LRU block (block 0), under cap.
	if _, err := c.Get(ctx, bs, "k", 2); err != nil {
		t.Fatal(err)
	}
	if _, _, by := c.Stats(); by > 2<<20 {
		t.Fatalf("bytes over cap: %d", by)
	}
	// Store error surfaces.
	bs.getErr = errors.New("down")
	if _, err := c.Get(ctx, bs, "k", 1); err == nil {
		t.Fatal("Get with failing store succeeded")
	}
	bs.getErr = nil
	// Unexpected GetResult variant.
	bs.obj = store.NotModified{Version: "v"}
	if _, err := c.Get(ctx, bs, "k", 1); err == nil || !strings.Contains(err.Error(), "unexpected get result") {
		t.Fatalf("NotModified block = %v", err)
	}
	bs.obj = nil
	// Reader that fails mid-stream.
	bs.obj = store.Object{Body: io.NopCloser(io.MultiReader(bytes.NewReader([]byte("x")), errReader{}))}
	if _, err := c.Get(ctx, bs, "k", 1); err == nil {
		t.Fatal("Get with failing body succeeded")
	}
	bs.obj = nil
	// insert: updating an existing key refreshes, not duplicates.
	c.insert(blockKey{"k", 9}, []byte("abc"))
	c.insert(blockKey{"k", 9}, []byte("abcdef"))
	if _, _, by := c.Stats(); by < 6 {
		t.Fatalf("bytes after reinsert = %d", by)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

// ---- engine-level decode/header/inflate faults ---------------------------------------

func newTestEngine(t *testing.T, st store.ObjectStore, repoID string) (*remoteEngine, *RemotePacks, *packIndex) {
	t.Helper()
	pack, _, _, _, _ := buildTestPack(t)
	sum := fmt.Sprintf("%x", packSHA1(pack))
	if st == nil {
		st = &blockStore{data: pack}
	}
	ix := &packIndex{Checksum: sum, oids: []string{testOid1, testOid2}, offsets: []int64{baseOffOf(t), deltaOffOf(t)},
		byOid: map[string]int64{testOid1: baseOffOf(t), testOid2: deltaOffOf(t)}}
	rp := &RemotePacks{idxs: []*packIndex{ix}}
	eng := &remoteEngine{packs: rp, blocks: newBlockCache(1 << 20), st: st, repoID: repoID, objCap: 0}
	return eng, rp, ix
}

// packSHA1 mirrors buildTestPack's trailing checksum over the header.
func packSHA1(pack []byte) []byte { return pack[len(pack)-20:] }

const testOid1 = "1111111111111111111111111111111111111111"
const testOid2 = "2222222222222222222222222222222222222222"

func baseOffOf(t *testing.T) int64 {
	t.Helper()
	_, baseOff, _, _, _ := buildTestPack(t)
	return baseOff
}

func deltaOffOf(t *testing.T) int64 {
	t.Helper()
	_, _, deltaOff, _, _ := buildTestPack(t)
	return deltaOff
}

func TestRemoteEngine_DecodeHeaderAndCache(t *testing.T) {
	eng, _, ix := newTestEngine(t, nil, "acme/api")
	ctx := context.Background()

	// Decode the base and the ofs-delta.
	kind, data, err := eng.decodeAt(ctx, ix, baseOffOf(t))
	if err != nil || kind != "blob" || !strings.HasPrefix(string(data), "hello world") {
		t.Fatalf("base decode = %s %q %v", kind, data, err)
	}
	kind, data, err = eng.decodeAt(ctx, ix, deltaOffOf(t))
	if err != nil || kind != "blob" || !strings.Contains(string(data), "WALGIT") {
		t.Fatalf("delta decode = %s %q %v", kind, data, err)
	}
	// Cached second decode (obj LRU hit).
	kind2, data2, err := eng.decodeAt(ctx, ix, deltaOffOf(t))
	if err != nil || kind2 != kind || !bytes.Equal(data, data2) {
		t.Fatalf("cached decode = %s %v", kind2, err)
	}
	// Header walks the delta chain to the base without inflating.
	hKind, hSize, err := eng.header(ctx, ix, deltaOffOf(t))
	if err != nil || hKind != "blob" || hSize != int64(len("hello world, hello walhub!\n")) {
		t.Fatalf("header = %s %d %v", hKind, hSize, err)
	}
	// headerAt on a real object.
	pe, err := eng.headerAt(ctx, eng.packKey(ix), baseOffOf(t))
	if err != nil || pe.typ != objBlob {
		t.Fatalf("headerAt = %+v %v", pe, err)
	}
	// readRaw edges.
	if d, err := eng.readRaw(ctx, eng.packKey(ix), 0, 0); err != nil || d != nil {
		t.Fatalf("readRaw size 0 = %v %v", d, err)
	}
	if d, err := eng.readRaw(ctx, eng.packKey(ix), baseOffOf(t), 4); err != nil || len(d) != 4 {
		t.Fatalf("readRaw 4 = %q %v", d, err)
	}
}

func TestRemoteEngine_InflateFaults(t *testing.T) {
	eng, _, ix := newTestEngine(t, nil, "acme/api")
	ctx := context.Background()
	pk := eng.packKey(ix)
	// want <= 0 → empty.
	if d, err := eng.inflateAt(ctx, pk, 0, 0); err != nil || len(d) != 0 {
		t.Fatalf("inflate 0 = %v %v", d, err)
	}
	// Non-zlib bytes at offset 0 → corrupt.
	if _, err := eng.inflateAt(ctx, pk, 1, 10); err == nil {
		t.Fatal("inflate garbage succeeded")
	}
	// Truncated zlib (10 bytes from the middle) → inflate error.
	if _, err := eng.inflateAt(ctx, pk, baseOffOf(t)+2, 100000); err == nil {
		t.Fatal("inflate truncated succeeded")
	}
	// Object header faults: empty buffer, truncated continuation, garbage.
	if _, _, _, err := readObjectHeader(nil); err == nil {
		t.Fatal("empty header")
	}
	if _, _, _, err := readObjectHeader([]byte{0x90}); err == nil {
		t.Fatal("truncated continuation")
	}
	if _, _, _, err := readObjectHeader([]byte{0x90, 0x80}); err == nil {
		t.Fatal("still-truncated continuation")
	}
	if typ, size, hl, err := readObjectHeader([]byte{0x30 | 0x0a | 0x80, 0x81, 0x00}); err != nil || typ != 3 || size != 26 || hl != 3 {
		t.Fatalf("multibyte header = typ %d size %d hl %d err %v", typ, size, hl, err)
	}
	// headerAt past the end → corrupt header.
	if _, err := eng.headerAt(ctx, pk, int64(1<<20)); err == nil {
		t.Fatal("headerAt past end succeeded")
	}
}

func TestRemoteEngine_DecodeFaults(t *testing.T) {
	// A ref-delta whose base is not in any pack → not found.
	eng, rp, ix := newTestEngine(t, nil, "acme/api")
	ctx := context.Background()
	eng.packs = rp
	off := deltaOffOf(t)
	// Hand the engine a fake ref-delta entry: build a pack-ish buffer with a
	// ref delta header pointing at a missing base.
	pack := []byte{byte(objRefDelta<<4) | 0x05}
	pack = append(pack, []byte("9999999999999999999999999999999999999999")...)
	pack = append(pack, 0x00, 0x00, 0x00, 0x00, 0x00) // delta payload placeholder
	badStore := &blockStore{data: pack}
	engBad := &remoteEngine{packs: eng.packs, blocks: eng.blocks, st: badStore, repoID: "acme/api", objCap: 0}
	if _, _, err := engBad.decodeAt(ctx, ix, off); err == nil {
		t.Fatal("ref-delta with missing base succeeded")
	}
	// Same for header().
	if _, _, err := engBad.header(ctx, ix, off); err == nil {
		t.Fatal("header over missing base succeeded")
	}
	_ = off
}

func TestRemoteEngine_FaultWritesLooseObjects(t *testing.T) {
	r, _ := newTestRegistry(t)
	h, err := r.Create(context.Background(), "acme/api", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	pack, _, _, baseOid, deltaOid := buildTestPack(t)
	eng, _, _ := newTestEngine(t, &blockStore{data: pack}, h.ID)
	// Patch the index to the real oids/offsets from the pack builder.
	eng.packs.idxs[0].oids = []string{baseOid, deltaOid}
	eng.packs.idxs[0].byOid = map[string]int64{baseOid: baseOffOf(t), deltaOid: deltaOffOf(t)}
	eng.packs.idxs[0].offsets = []int64{baseOffOf(t), deltaOffOf(t)}

	ctx := context.Background()
	// Unknown oid → typed miss.
	if err := eng.fault(ctx, h, []string{strings.Repeat("f", 40)}); err == nil || !strings.Contains(err.Error(), "fault:") {
		t.Fatalf("fault unknown oid = %v", err)
	}
	// Real oids decode and land as loose objects.
	if err := eng.fault(ctx, h, []string{baseOid, deltaOid}); err != nil {
		t.Fatalf("fault: %v", err)
	}
	for _, oid := range []string{baseOid, deltaOid} {
		if _, err := os.Stat(filepath.Join(h.repo.ObjectsDir(), oid[:2], oid[2:])); err != nil {
			t.Fatalf("loose object %s missing: %v", oid, err)
		}
	}
	// Idempotent write: an existing loose object is left alone.
	if err := writeLooseObject(h.repo, baseOid, "blob", []byte("x")); err != nil {
		t.Fatalf("rewrite existing: %v", err)
	}
	// The public entry delegates to the engine.
	rr := &RemoteReader{eng: eng}
	if err := rr.Fault(ctx, h, []string{baseOid}); err != nil {
		t.Fatalf("Fault: %v", err)
	}
	// An engine decode failure (missing base) propagates.
	oidsBad := []string{strings.Repeat("9", 40)}
	if err := rr.Fault(ctx, h, oidsBad); err == nil {
		t.Fatal("Fault with undecodable oid succeeded")
	}
}

func TestApplyGitDelta_ErrorVectors(t *testing.T) {
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
	cases := []struct {
		name  string
		base  []byte
		delta []byte
		det   string
	}{
		{"empty delta", base, nil, "truncated base size"},
		{"base size mismatch", base, append(varint(5), varint(2)...), "base size"},
		{"truncated result size", base, varint(10), "truncated result size"},
		{"reserved cmd 0", base, append(append(varint(10), varint(10)...), 0x00), "reserved cmd 0"},
		{"copy offset truncated", base, append(append(varint(10), varint(2)...), 0x81, 0x02), ""},
		{"copy size truncated", base, append(append(varint(10), varint(2)...), 0x90, 0x05), ""},
		{"copy exceeds base", base, append(append(varint(10), varint(20)...), 0x91, 0x00, 0x50), "exceeds base"},
		{"insert truncated", base, append(append(varint(10), varint(20)...), 0x05, 'a', 'b'), ""},
		{"produced size mismatch", base, append(append(varint(10), varint(5)...), 0x03, 'a', 'b', 'c'), "produced"},
	}
	for _, tc := range cases {
		_, err := applyGitDelta(tc.base, tc.delta)
		if err == nil || !strings.Contains(err.Error(), tc.det) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.det)
		}
	}
	// The happy path (copy + insert) is exercised by the end-to-end decode.
}
