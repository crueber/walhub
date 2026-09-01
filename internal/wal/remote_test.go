// remote_test.go — the remote reader (05 §5.7): git delta format vectors,
// pack index parsing, block-cache single-flight/LRU, and end-to-end decode of
// a hand-built pack with an OFS_DELTA chain.
package wal

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// ---- git delta format (normative vectors) --------------------------------------

func TestApplyGitDelta_InsertCopyAndSize0(t *testing.T) {
	base := []byte("hello world, hello walhub")

	// varint helpers
	put := func(v uint64) []byte {
		var buf []byte
		for {
			b := byte(v & 0x7f)
			v >>= 7
			if v != 0 {
				b |= 0x80
			}
			buf = append(buf, b)
			if v == 0 {
				return buf
			}
		}
	}

	var d []byte
	d = append(d, put(uint64(len(base)))...)       // base_size
	result := []byte("HELLO world, hello walhub!") // 13 copy + edits
	d = append(d, put(uint64(len(result)))...)
	// insert "HELLO " first, then copy the tail in order
	d = append(d, 0x06)
	d = append(d, []byte("HELLO ")...)
	// copy [6,13) "world, " : offset=6 (1 byte), size=7 (1 byte)
	d = append(d, 0x80|0x01|0x10, 6, 7)
	// copy [13,25) "hello walhub" : offset=13, size=12
	d = append(d, 0x80|0x01|0x10, 13, 12)
	// insert "!"
	d = append(d, 0x01, '!')

	got, err := applyGitDelta(base, d)
	if err != nil {
		t.Fatalf("applyGitDelta: %v", err)
	}
	if !bytes.Equal(got, result) {
		t.Fatalf("got %q, want %q", got, result)
	}

	// size 0 → 0x10000 copy (bounds check against a short base must fail).
	d2 := append([]byte{}, put(3)...)
	d2 = append(d2, put(0x10000)...)
	d2 = append(d2, 0x90, 0, 0) // copy offset 0, size field 0 → 0x10000
	if _, err := applyGitDelta([]byte("abc"), d2); err == nil {
		t.Fatal("size-0 copy past base end must error")
	}

	// base_size mismatch must error.
	bad := append(put(99), put(0)...)
	if _, err := applyGitDelta(base, bad); err == nil {
		t.Fatal("base size mismatch must error")
	}
	// reserved cmd 0 must error.
	bad2 := append(put(0), put(1)...)
	bad2 = append(bad2, 0x00)
	if _, err := applyGitDelta(nil, bad2); err == nil {
		t.Fatal("reserved cmd 0 must error")
	}
}

// ---- pack index parsing ----------------------------------------------------------

func TestParsePackIndex(t *testing.T) {
	oids := []string{
		"1111111111111111111111111111111111111111",
		"2222222222222222222222222222222222222222",
	}
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 't', 'O', 'c'})
	binary.Write(&buf, binary.BigEndian, uint32(2))
	counts := make([]uint32, 256)
	decode := func(o string) [20]byte {
		var raw [20]byte
		b, _ := hex.DecodeString(o)
		copy(raw[:], b)
		return raw
	}
	for _, o := range oids {
		counts[decode(o)[0]]++
	}
	var run uint32
	for i := range counts { // fanout must be cumulative
		run += counts[i]
		binary.Write(&buf, binary.BigEndian, run)
	}
	for _, o := range oids {
		raw := decode(o)
		buf.Write(raw[:])
	}
	for range oids {
		binary.Write(&buf, binary.BigEndian, uint32(0)) // crc
	}
	binary.Write(&buf, binary.BigEndian, uint32(0x80000000)) // offsets: idx 0 → large table (MSB set)
	binary.Write(&buf, binary.BigEndian, uint32(24))
	binary.Write(&buf, binary.BigEndian, uint64(0xdeadbeef)) // large offset for oid 1

	ix, err := parsePackIndex("pack-abc.idx", buf.Bytes())
	if err != nil {
		t.Fatalf("parsePackIndex: %v", err)
	}
	if ix.Checksum != "pack-abc" {
		t.Fatalf("checksum = %q", ix.Checksum)
	}
	if off, ok := ix.byOid[oids[0]]; !ok || off != 0xdeadbeef {
		t.Fatalf("oid0 offset = %d ok=%v", off, ok)
	}
	if off, ok := ix.byOid[oids[1]]; !ok || off != 24 {
		t.Fatalf("oid1 offset = %d ok=%v", off, ok)
	}
}

// ---- block cache ------------------------------------------------------------------

type slowStore struct {
	store.ObjectStore
	fetches atomic.Int64
	delay   time.Duration
	mu      sync.Mutex
	gate    chan struct{}
}

func (s *slowStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	s.fetches.Add(1)
	if s.gate != nil {
		<-s.gate
	}
	return s.ObjectStore.Get(ctx, key, opts)
}

func TestBlockCache_SingleFlightAndLRU(t *testing.T) {
	inner := store.NewMemory()
	obj := bytes.Repeat([]byte{7}, 3<<20) // 3 MiB → 3 blocks
	if _, err := inner.Put(context.Background(), "repos/x/y/wal/aa.pack", store.PutBody{Bytes: obj}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	st := &slowStore{ObjectStore: inner, gate: make(chan struct{})}
	c := newBlockCache(2 << 20) // 2 MiB cap → LRU pressure

	// N concurrent misses share ONE fetch per block (single-flight).
	var wg sync.WaitGroup
	close(st.gate) // release all immediately
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Get(context.Background(), st, "repos/x/y/wal/aa.pack", 0); err != nil {
				t.Errorf("get: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := st.fetches.Load(); got != 1 {
		t.Fatalf("fetches = %d, want 1 (single-flight)", got)
	}

	// Second read hits the cache.
	c.Get(context.Background(), st, "repos/x/y/wal/aa.pack", 0)
	hits, _, bytes := c.Stats()
	if hits == 0 {
		t.Fatalf("hits=%d", hits)
	}
	if bytes > 2<<20 {
		t.Fatalf("cache bytes = %d over cap", bytes)
	}
}

// ---- end-to-end decode of a hand-built pack ----------------------------------------

// buildTestPack writes a two-entry pack: blob base + OFS_DELTA against it.
func buildTestPack(t *testing.T) (pack []byte, baseOff, deltaOff int64, baseOid, deltaOid string) {
	t.Helper()
	base := []byte("hello world, hello walhub!\n")
	result := []byte("hello world, hello WALGIT!\n")

	var body bytes.Buffer
	body.Write([]byte("PACK"))
	binary.Write(&body, binary.BigEndian, uint32(2))
	binary.Write(&body, binary.BigEndian, uint32(2))

	// Entry 1: blob (type 3), zlib of base.
	baseOff = int64(body.Len())
	writeObjHeader(&body, objBlob, int64(len(base)))
	zb := &bytes.Buffer{}
	zw := zlib.NewWriter(zb)
	zw.Write(base)
	zw.Close()
	body.Write(zb.Bytes())
	baseOid = fmt.Sprintf("%x", sha1.Sum(append([]byte("blob "+itoa(int64(len(base)))+"\x00"), base...)))

	// Entry 2: ofs-delta (type 6), delta transforming base → result.
	deltaOff = int64(body.Len())
	delta := []byte{}
	put := func(v uint64) []byte {
		var b []byte
		for {
			x := byte(v & 0x7f)
			v >>= 7
			if v != 0 {
				x |= 0x80
			}
			b = append(b, x)
			if v == 0 {
				return b
			}
		}
	}
	delta = append(delta, put(uint64(len(base)))...)
	delta = append(delta, put(uint64(len(result)))...)
	// copy [0,12) "hello world," → offset 0 size 12
	delta = append(delta, 0x80|0x01|0x10, 0, 12)
	// insert " hello WALGIT!\n"
	lit := []byte(" hello WALGIT!\n")
	delta = append(delta, byte(len(lit)))
	delta = append(delta, lit...)
	writeObjHeader(&body, objOfsDelta, int64(len(delta)))
	// ofs encoding: distance = deltaOff - baseOff
	dist := deltaOff - baseOff
	var ofs []byte
	ofs = append(ofs, byte(dist&0x7f))
	dist >>= 7
	for dist > 0 {
		dist--
		ofs = append([]byte{byte(0x80 | (dist & 0x7f))}, ofs...)
		dist >>= 7
	}
	body.Write(ofs)
	zd := &bytes.Buffer{}
	zw2 := zlib.NewWriter(zd)
	zw2.Write(delta)
	zw2.Close()
	body.Write(zd.Bytes())
	deltaOid = fmt.Sprintf("%x", sha1.Sum(append([]byte("blob "+itoa(int64(len(result)))+"\x00"), result...)))

	// Trailing sha1 over everything so far.
	sum := sha1.Sum(body.Bytes())
	body.Write(sum[:])
	return body.Bytes(), baseOff, deltaOff, baseOid, deltaOid
}

func writeObjHeader(buf *bytes.Buffer, typ int, size int64) {
	b := byte(typ<<4) | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		buf.WriteByte(b | 0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	buf.WriteByte(b)
}

func TestRemoteDecode_EndToEnd(t *testing.T) {
	packBytes, baseOff, deltaOff, baseOid, deltaOid := buildTestPack(t)
	inner := store.NewMemory()
	ctx := context.Background()
	if _, err := inner.Put(ctx, "repos/big/one/wal/ff.pack", store.PutBody{Bytes: packBytes}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(ctx, inner, testConfig(t))
	defer r.Close()
	h, err := r.Create(ctx, "big/one", git.Sha1)
	if err != nil {
		t.Fatal(err)
	}
	_ = h

	rp := &RemotePacks{
		Revision: 1,
		blocks:   r.blocks,
	}
	rp.packs = []*proto.PackRef{{Checksum: "ff", PackSize: uint64(len(packBytes)), IdxSize: 1}}
	// Synthetic index: both oids resolve to their pack offsets.
	ix := &packIndex{
		Checksum: "ff",
		byOid:    map[string]int64{baseOid: baseOff, deltaOid: deltaOff},
	}
	rp.idxs = []*packIndex{ix}
	eng := &remoteEngine{packs: rp, blocks: r.blocks, st: inner, repoID: "big/one"}

	// header: kind + size without materializing.
	kind, size, err := eng.header(ctx, ix, deltaOff)
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	if kind != "blob" || size != int64(len("hello world, hello WALGIT!\n")) {
		t.Fatalf("header(%q, %d), want blob/27", kind, size)
	}

	// decode the DELTA object: the chain folds to the result blob.
	kind, data, err := eng.decodeAt(ctx, ix, deltaOff)
	if err != nil {
		t.Fatalf("decode delta: %v", err)
	}
	if kind != "blob" || string(data) != "hello world, hello WALGIT!\n" {
		t.Fatalf("decode = (%q, %q)", kind, data)
	}

	// decode the base directly.
	_, data, err = eng.decodeAt(ctx, ix, baseOff)
	if err != nil {
		t.Fatalf("decode base: %v", err)
	}
	if string(data) != "hello world, hello walhub!\n" {
		t.Fatalf("base = %q", data)
	}

	// Lookup via the reader surface.
	rd := &RemoteReader{Revision: 1, eng: eng}
	idx, off, ok := rd.Locate(deltaOid)
	if !ok || idx != 0 || off != deltaOff {
		t.Fatalf("Locate = (%d,%d,%v)", idx, off, ok)
	}
	kind, data, err = rd.Decode(ctx, deltaOid)
	if err != nil || kind != "blob" || string(data) != "hello world, hello WALGIT!\n" {
		t.Fatalf("reader decode = (%q,%q,%v)", kind, data, err)
	}
}

func TestFaulter_WritesLooseObjects(t *testing.T) {
	packBytes, baseOff, _, baseOid, _ := buildTestPack(t)
	inner := store.NewMemory()
	ctx := context.Background()
	if _, err := inner.Put(ctx, "repos/big/one/wal/ff.pack", store.PutBody{Bytes: packBytes}, store.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(ctx, inner, testConfig(t))
	defer r.Close()
	h, _ := r.Create(ctx, "big/one", git.Sha1)

	rp := &RemotePacks{Revision: 1, blocks: r.blocks}
	ix := &packIndex{Checksum: "ff", byOid: map[string]int64{baseOid: baseOff}}
	rp.idxs = []*packIndex{ix}
	eng := &remoteEngine{packs: rp, blocks: r.blocks, st: inner, repoID: "big/one"}

	if err := eng.fault(ctx, h, []string{baseOid}); err != nil {
		t.Fatalf("fault: %v", err)
	}
	// The loose object exists and `git cat-file` accepts it.
	data, err := osReadLoose(h.Repo(), baseOid)
	if err != nil {
		t.Fatalf("loose object: %v", err)
	}
	if string(data) != "hello world, hello walhub!\n" {
		t.Fatalf("loose content = %q", data)
	}
}

// osReadLoose inflates a loose object from the repo's object store.
func osReadLoose(repo *git.LocalRepo, oid string) ([]byte, error) {
	raw, err := os.ReadFile(repo.ObjectsDir() + "/" + oid[:2] + "/" + oid[2:])
	if err != nil {
		return nil, err
	}
	// file = "type size\0" + zlib(body); skip the header, then inflate.
	i := bytes.IndexByte(raw, 0)
	zr, err := zlib.NewReader(bytes.NewReader(raw[i+1:]))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}
