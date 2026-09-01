// remote.go — the remote reader (doc 05 §5.7): the process-wide 1 MiB block
// cache with per-block single-flight, per-revision pack indexes in remote-idx/,
// iterative delta resolution (≤ 4096) with a decoded-object LRU, the git delta
// format (normative copy), and the fetch-path faulter.
package wal

import (
	"bytes"
	"compress/zlib"
	"container/list"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// ---- object type constants ----------------------------------------------------

const (
	objCommit   = 1
	objTree     = 2
	objBlob     = 3
	objTag      = 4
	objOfsDelta = 6
	objRefDelta = 7
)

var objTypeNames = map[int]string{objCommit: "commit", objTree: "tree", objBlob: "blob", objTag: "tag"}

// ---- pack index parser (v2 .idx; the on-disk index format of doc 04) -----------

// packIndex is an opened .idx: sorted oids with their pack offsets.
type packIndex struct {
	Checksum string
	oids     []string
	offsets  []int64
	byOid    map[string]int64
}

// openPackIndex parses a v2 idx file: magic \ff744f63, version 2, fanout,
// sha table, crc32 table, 32-bit offsets (MSB set → 64-bit large-offset table).
func openPackIndex(path string) (*packIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &WalError{Kind: WalErrIo, Detail: path, Wrapped: err}
	}
	return parsePackIndex(filepath.Base(path), data)
}

func parsePackIndex(name string, data []byte) (*packIndex, error) {
	if len(data) < 8 || data[0] != 0xff || data[1] != 't' || data[2] != 'O' || data[3] != 'c' {
		return nil, &WalError{Kind: WalErrCorrupt, Detail: name + ": not a v2 pack index"}
	}
	if binary.BigEndian.Uint32(data[4:8]) != 2 {
		return nil, &WalError{Kind: WalErrCorrupt, Detail: name + ": unsupported index version"}
	}
	n := int(binary.BigEndian.Uint32(data[255*4+8 : 255*4+12]))
	shaBase := 8 + 256*4
	offBase := shaBase + 20*n + 4*n // skip the crc32 table
	bigBase := offBase + 4*n
	if len(data) < bigBase {
		return nil, &WalError{Kind: WalErrCorrupt, Detail: name + ": truncated index"}
	}
	ix := &packIndex{
		Checksum: trimExt(name),
		oids:     make([]string, 0, n),
		offsets:  make([]int64, 0, n),
		byOid:    make(map[string]int64, n),
	}
	for i := 0; i < n; i++ {
		oid := fmt.Sprintf("%x", data[shaBase+20*i:shaBase+20*i+20])
		v := binary.BigEndian.Uint32(data[offBase+4*i : offBase+4*i+4])
		var off int64
		if v&0x80000000 != 0 {
			idx := int(v & 0x7fffffff)
			if bigBase+8*idx+8 > len(data) {
				return nil, &WalError{Kind: WalErrCorrupt, Detail: name + ": bad large offset"}
			}
			off = int64(binary.BigEndian.Uint64(data[bigBase+8*idx : bigBase+8*idx+8]))
		} else {
			off = int64(v)
		}
		ix.oids = append(ix.oids, oid)
		ix.offsets = append(ix.offsets, off)
		ix.byOid[oid] = off
	}
	return ix, nil
}

func trimExt(name string) string {
	for _, s := range []string{".idx", ".pack", ".rev", ".bitmap"} {
		if len(name) > len(s) && name[len(name)-len(s):] == s {
			return name[:len(name)-len(s)]
		}
	}
	return name
}

// ---- RemotePacks (per manifest revision, §5.7) ---------------------------------

// RemotePacks is the per-revision set of remote pack indexes, owned by the
// handle via atomic pointer (build-then-swap; old revision stays alive until
// the swap — §5.7 hazard (c)).
type RemotePacks struct {
	Revision uint64
	packs    []*proto.PackRef
	idxs     []*packIndex
	blocks   *BlockCache
}

// locate finds oid across the indexes → (index, offset, ok).
func (rp *RemotePacks) locate(oid string) (*packIndex, int64, bool) {
	for _, ix := range rp.idxs {
		if off, ok := ix.byOid[oid]; ok {
			return ix, off, true
		}
	}
	return nil, 0, false
}

// lookupPrefix resolves a prefix across packs; non-unique → ambiguous.
func (rp *RemotePacks) lookupPrefix(prefix string) (string, *packIndex, int64, error) {
	var found string
	var ix *packIndex
	var off int64
	for _, p := range rp.idxs {
		i := sortSearchStrings(p.oids, prefix)
		for ; i < len(p.oids) && len(p.oids[i]) >= len(prefix) && p.oids[i][:len(prefix)] == prefix; i++ {
			if found != "" && found != p.oids[i] {
				return "", nil, 0, &WalError{Kind: WalErrInvalid, Detail: "ambiguous prefix: " + prefix}
			}
			found = p.oids[i]
			ix, off = p, p.offsets[i]
		}
	}
	if found == "" {
		return "", nil, 0, &WalError{Kind: WalErrNotFound, Detail: "no object with prefix " + prefix}
	}
	return found, ix, off, nil
}

func sortSearchStrings(a []string, x string) int {
	lo, hi := 0, len(a)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if a[mid] < x {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// ---- BlockCache (process-wide, §5.7) --------------------------------------------

type blockKey struct {
	globalKey string
	blockNo   uint64
}

type blockEntry struct {
	data []byte
	elem *list.Element
}

// BlockCache is the 1 MiB-block LRU shared by every repo's remote reader.
type BlockCache struct {
	mu       sync.Mutex
	blocks   map[blockKey]*blockEntry
	lru      list.List // of blockKey, front = newest
	inflight Group     // per-block single-flight (13 §3)
	bytes    atomic.Int64
	hits     atomic.Int64
	misses   atomic.Int64
	cap      int64 // cache.remote_block_bytes
}

const blockShift = 20 // 1 MiB blocks

func newBlockCache(cap int64) *BlockCache {
	if cap <= 0 {
		cap = 1 << 30
	}
	return &BlockCache{blocks: map[blockKey]*blockEntry{}, cap: cap}
}

// Get returns one block, deduplicating concurrent misses per key: N waiters
// share one GET (thundering-herd avoidance, §5.7 hazard (a)).
func (c *BlockCache) Get(ctx context.Context, st store.ObjectStore, key string, blockNo uint64) ([]byte, error) {
	bk := blockKey{key, blockNo}
	c.mu.Lock()
	if e, ok := c.blocks[bk]; ok {
		c.lru.MoveToFront(e.elem)
		c.mu.Unlock()
		c.hits.Add(1)
		return e.data, nil
	}
	c.mu.Unlock()
	c.misses.Add(1)

	data, err := c.inflight.DoCtx(ctx, fmt.Sprintf("block:%s,%d", key, blockNo), func() (any, error) {
		// Double-check: a racing fetch may have inserted while we queued.
		c.mu.Lock()
		if e, ok := c.blocks[bk]; ok {
			c.lru.MoveToFront(e.elem)
			c.mu.Unlock()
			return e.data, nil
		}
		c.mu.Unlock()

		start := int64(blockNo) << blockShift
		end := start + (1 << blockShift)
		res, err := st.Get(ctx, key, store.GetOptions{Range: &[2]int64{start, end}})
		if err != nil {
			return nil, err
		}
		obj, ok := res.(store.Object)
		if !ok {
			return nil, &WalError{Kind: WalErrStore, Detail: key + ": unexpected get result"}
		}
		defer obj.Body.Close()
		data, err := io.ReadAll(obj.Body)
		if err != nil {
			return nil, &WalError{Kind: WalErrIo, Detail: key, Wrapped: err}
		}
		c.insert(bk, data)
		return data, nil
	})
	if err != nil {
		return nil, err
	}
	return data.([]byte), nil
}

// insert adds a block and evicts LRU tail until under cap (under mu only,
// holding no other lock — §5.7 hazard (b)).
func (c *BlockCache) insert(bk blockKey, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.blocks[bk]; ok {
		e.data = data
		c.lru.MoveToFront(e.elem)
		return
	}
	el := c.lru.PushFront(bk)
	c.blocks[bk] = &blockEntry{data: data, elem: el}
	c.bytes.Add(int64(len(data)))
	for c.cap > 0 && c.bytes.Load() > c.cap {
		back := c.lru.Back()
		if back == nil {
			break
		}
		old := back.Value.(blockKey)
		c.lru.Remove(back)
		if e, ok := c.blocks[old]; ok {
			c.bytes.Add(-int64(len(e.data)))
			delete(c.blocks, old)
		}
	}
}

// Stats returns (hits, misses, cached bytes) counters.
func (c *BlockCache) Stats() (hits, misses, bytes int64) {
	return c.hits.Load(), c.misses.Load(), c.bytes.Load()
}

// ---- the engine behind a RemoteReader -------------------------------------------

// remoteEngine binds a per-revision pack set to the shared block cache and the
// decoded-object LRU. One per RemotePacks revision.
type remoteEngine struct {
	packs  *RemotePacks
	blocks *BlockCache
	st     store.ObjectStore
	repoID string

	objMu    sync.Mutex
	objLRU   map[objKey]*list.Element
	objList  *list.List // of objEntry, front = newest
	objBytes int64
	objCap   int64 // cache.remote_object_bytes
}

type objKey struct {
	pack   string
	offset int64
}

type objEntry struct {
	key  objKey
	kind string
	data []byte
}

func (e *remoteEngine) cacheObj(k objKey, kind string, data []byte) {
	e.objMu.Lock()
	defer e.objMu.Unlock()
	if e.objList == nil {
		e.objList = list.New()
		e.objLRU = map[objKey]*list.Element{}
	}
	if el, ok := e.objLRU[k]; ok {
		e.objList.MoveToFront(el)
		return
	}
	el := e.objList.PushFront(objEntry{k, kind, data})
	e.objLRU[k] = el
	e.objBytes += int64(len(data))
	for e.objCap > 0 && e.objBytes > e.objCap {
		back := e.objList.Back()
		if back == nil {
			break
		}
		old := back.Value.(objEntry)
		e.objList.Remove(back)
		delete(e.objLRU, old.key)
		e.objBytes -= int64(len(old.data))
	}
}

func (e *remoteEngine) lookupObj(k objKey) (string, []byte, bool) {
	e.objMu.Lock()
	defer e.objMu.Unlock()
	if el, ok := e.objLRU[k]; ok {
		e.objList.MoveToFront(el)
		v := el.Value.(objEntry)
		return v.kind, v.data, true
	}
	return "", nil, false
}

// packKey is the store key of a pack (index-local checksum).
func (e *remoteEngine) packKey(ix *packIndex) string {
	return repoPrefix(e.repoID) + store.PackKey(ix.Checksum)
}

// readRaw reads exactly size bytes of pack data at off through the block cache.
func (e *remoteEngine) readRaw(ctx context.Context, packKey string, off, size int64) ([]byte, error) {
	if size <= 0 {
		return nil, nil
	}
	first := uint64(off) >> blockShift
	last := uint64(off+size-1) >> blockShift
	var buf bytes.Buffer
	for b := first; b <= last; b++ {
		data, err := e.blocks.Get(ctx, e.st, packKey, b)
		if err != nil {
			return nil, err
		}
		buf.Write(data)
	}
	all := buf.Bytes()
	lo := int(off) - int(first<<blockShift)
	// The final block of an object may be partial (ranges clamp at the end);
	// callers tolerate a short tail — the zlib stream ends before it.
	if lo >= len(all) {
		return nil, nil
	}
	if lo+int(size) <= len(all) {
		return all[lo : lo+int(size)], nil
	}
	return all[lo:], nil
}

// readObjectHeader parses the (type, size) varint header.
func readObjectHeader(b []byte) (typ int, size int64, headerLen int, err error) {
	if len(b) == 0 {
		return 0, 0, 0, io.ErrUnexpectedEOF
	}
	c := b[0]
	typ = int(c>>4) & 7
	size = int64(c & 0x0f)
	shift := uint(4)
	i := 1
	for c&0x80 != 0 {
		if i >= len(b) {
			return 0, 0, 0, io.ErrUnexpectedEOF
		}
		c = b[i]
		i++
		size |= int64(c&0x7f) << shift
		shift += 7
	}
	return typ, size, i, nil
}

// packEntry is one object header read from the pack stream. size is the
// INFLATED size of this entry (for deltas: the delta payload size).
type packEntry struct {
	typ     int
	size    int64
	dataOff int64  // offset of the compressed payload
	baseOff int64  // ofs-delta: base offset
	baseOid string // ref-delta: base oid
}

// headerAt reads the object header at off (the first block prefetch carries
// the entry header with it, §5.7).
func (e *remoteEngine) headerAt(ctx context.Context, packKey string, off int64) (*packEntry, error) {
	head, err := e.readRaw(ctx, packKey, off, 64)
	if err != nil {
		return nil, err
	}
	typ, size, hdrLen, err := readObjectHeader(head)
	if err != nil {
		return nil, &WalError{Kind: WalErrCorrupt, Detail: fmt.Sprintf("%s@%d: object header", packKey, off)}
	}
	pe := &packEntry{typ: typ, size: size, dataOff: off + int64(hdrLen)}
	switch typ {
	case objOfsDelta:
		b, err := e.readRaw(ctx, packKey, pe.dataOff, 10)
		if err != nil {
			return nil, err
		}
		if len(b) < 1 {
			return nil, io.ErrUnexpectedEOF
		}
		i, delta := 0, int64(b[0]&0x7f)
		for b[i]&0x80 != 0 {
			i++
			if i >= len(b) {
				return nil, io.ErrUnexpectedEOF
			}
			delta = ((delta + 1) << 7) | int64(b[i]&0x7f)
		}
		pe.baseOff = off - delta
		pe.dataOff += int64(i + 1)
	case objRefDelta:
		b, err := e.readRaw(ctx, packKey, pe.dataOff, 20)
		if err != nil {
			return nil, err
		}
		if len(b) < 20 {
			return nil, io.ErrUnexpectedEOF
		}
		pe.baseOid = fmt.Sprintf("%x", b[:20])
		pe.dataOff += 20
	}
	return pe, nil
}

// inflateAt inflates an object body of exactly want bytes starting at off.
// The compressed extent is unknown, so it reads a slack window past the
// expected size and lets the zlib stream end wherever it ends.
func (e *remoteEngine) inflateAt(ctx context.Context, packKey string, off, want int64) ([]byte, error) {
	if want <= 0 {
		return []byte{}, nil
	}
	// Prefetch min(size, 64 MiB) of blocks up front (§5.7).
	prefetch := want + 64<<10
	if prefetch > 64<<20 {
		prefetch = 64 << 20
	}
	_ = prefetch
	raw, err := e.readRaw(ctx, packKey, off, want+64<<10)
	if err != nil {
		return nil, err
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, &WalError{Kind: WalErrCorrupt, Detail: packKey + ": zlib: " + err.Error()}
	}
	defer zr.Close()
	out := make([]byte, want)
	if _, err := io.ReadFull(zr, out); err != nil {
		return nil, &WalError{Kind: WalErrCorrupt, Detail: fmt.Sprintf("%s@%d: inflate: %v", packKey, off, err)}
	}
	return out, nil
}

// decodeAt iteratively resolves the delta chain at off (≤ 4096 deep), folds
// it in reverse with the git delta format, and caches every intermediate.
func (e *remoteEngine) decodeAt(ctx context.Context, ix *packIndex, off int64) (string, []byte, error) {
	if kind, data, ok := e.lookupObj(objKey{ix.Checksum, off}); ok {
		return kind, data, nil
	}

	// Walk the chain top-down collecting entries (iterative, never recursive).
	type hop struct {
		ix  *packIndex
		off int64
	}
	chain := []hop{{ix, off}}
	entries := map[objKey]*packEntry{}
	seen := map[objKey]bool{}
	for depth := 0; depth < 4096; depth++ {
		cur := chain[len(chain)-1]
		curIx := cur.ix
		curKey := e.packKey(curIx)
		if seen[objKey{curIx.Checksum, cur.off}] {
			return "", nil, &WalError{Kind: WalErrCorrupt, Detail: "delta cycle"}
		}
		pe, err := e.headerAt(ctx, curKey, cur.off)
		if err != nil {
			return "", nil, err
		}
		entries[objKey{curIx.Checksum, cur.off}] = pe
		seen[objKey{curIx.Checksum, cur.off}] = true
		switch pe.typ {
		case objOfsDelta:
			chain = append(chain, hop{curIx, pe.baseOff})
			continue
		case objRefDelta:
			bix, boff, ok := e.packs.locate(pe.baseOid)
			if !ok {
				return "", nil, &WalError{Kind: WalErrNotFound, Detail: "delta base " + pe.baseOid}
			}
			chain = append(chain, hop{bix, boff})
			continue
		}
		// Real base reached: inflate, then fold the chain in reverse.
		kind := objTypeNames[pe.typ]
		data, err := e.inflateAt(ctx, curKey, pe.dataOff, pe.size)
		if err != nil {
			return "", nil, err
		}
		e.cacheObj(objKey{curIx.Checksum, cur.off}, kind, data)
		for i := len(chain) - 2; i >= 0; i-- {
			h := chain[i]
			hIx := h.ix
			hpe := entries[objKey{hIx.Checksum, h.off}]
			if hpe == nil || (hpe.typ != objOfsDelta && hpe.typ != objRefDelta) {
				continue
			}
			delta, err := e.inflateAt(ctx, e.packKey(hIx), hpe.dataOff, hpe.size)
			if err != nil {
				return "", nil, err
			}
			applied, err := applyGitDelta(data, delta)
			if err != nil {
				return "", nil, err
			}
			data = applied
			e.cacheObj(objKey{hIx.Checksum, h.off}, kind, data)
		}
		return kind, data, nil
	}
	return "", nil, &WalError{Kind: WalErrCorrupt, Detail: "delta chain deeper than 4096"}
}

// header returns kind + inflated size without materializing (§5.7): walk the
// delta chain (≤ 256) inflating only delta headers — the result size comes
// from the first varints.
func (e *remoteEngine) header(ctx context.Context, ix *packIndex, off int64) (string, int64, error) {
	packKey := e.packKey(ix)
	cur := off
	for depth := 0; depth < 256; depth++ {
		pe, err := e.headerAt(ctx, packKey, cur)
		if err != nil {
			return "", 0, err
		}
		switch pe.typ {
		case objOfsDelta:
			cur = pe.baseOff
			continue
		case objRefDelta:
			bix, boff, ok := e.packs.locate(pe.baseOid)
			if !ok {
				return "", 0, &WalError{Kind: WalErrNotFound, Detail: "delta base " + pe.baseOid}
			}
			packKey = e.packKey(bix)
			cur = boff
			continue
		}
		return objTypeNames[pe.typ], pe.size, nil
	}
	return "", 0, &WalError{Kind: WalErrCorrupt, Detail: "delta chain deeper than 256 (header)"}
}

// applyGitDelta applies a git delta to base — normative copy (§5.7): varint
// base_size (must match), varint result_size, then copy (cmd & 0x80: offset
// bits 0x01|0x02|0x04|0x08 LSB→byte 0..3 little-endian, size bits
// 0x10|0x20|0x40, size 0 → 0x10000, bounds-checked) and insert (1 ≤ cmd < 0x80)
// commands until the buffer ends. cmd == 0 is reserved/error. Total produced
// bytes must equal result_size.
func applyGitDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int64, error) {
		var v int64
		var shift uint
		for {
			if pos >= len(delta) {
				return 0, io.ErrUnexpectedEOF
			}
			c := delta[pos]
			pos++
			v |= int64(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				return v, nil
			}
		}
	}
	baseSize, err := readVarint()
	if err != nil {
		return nil, &WalError{Kind: WalErrCorrupt, Detail: "delta: truncated base size"}
	}
	if baseSize != int64(len(base)) {
		return nil, &WalError{Kind: WalErrCorrupt,
			Detail: fmt.Sprintf("delta base size %d != actual %d", baseSize, len(base))}
	}
	resultSize, err := readVarint()
	if err != nil {
		return nil, &WalError{Kind: WalErrCorrupt, Detail: "delta: truncated result size"}
	}
	out := make([]byte, 0, resultSize)
	for pos < len(delta) {
		cmd := delta[pos]
		pos++
		switch {
		case cmd&0x80 != 0: // copy from base
			var off, size int64
			for bit := uint(0); bit < 4; bit++ {
				if cmd&(1<<bit) != 0 {
					if pos >= len(delta) {
						return nil, io.ErrUnexpectedEOF
					}
					off |= int64(delta[pos]) << (8 * bit)
					pos++
				}
			}
			for bit := uint(0); bit < 3; bit++ {
				if cmd&(0x10<<bit) != 0 {
					if pos >= len(delta) {
						return nil, io.ErrUnexpectedEOF
					}
					size |= int64(delta[pos]) << (8 * bit)
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > int64(len(base)) {
				return nil, &WalError{Kind: WalErrCorrupt,
					Detail: fmt.Sprintf("delta copy [%d,%d) exceeds base %d", off, off+size, len(base))}
			}
			out = append(out, base[off:off+size]...)
		case cmd != 0: // insert the next `cmd` literal bytes
			n := int(cmd)
			if pos+n > len(delta) {
				return nil, io.ErrUnexpectedEOF
			}
			out = append(out, delta[pos:pos+n]...)
			pos += n
		default:
			return nil, &WalError{Kind: WalErrCorrupt, Detail: "delta: reserved cmd 0"}
		}
	}
	if int64(len(out)) != resultSize {
		return nil, &WalError{Kind: WalErrCorrupt,
			Detail: fmt.Sprintf("delta produced %d bytes, want %d", len(out), resultSize)}
	}
	return out, nil
}

// ctx is the engine's background context (Header has no caller ctx; the block
// cache GETs it drives use the registry lifetime).
func (e *remoteEngine) ctx() context.Context { return context.Background() }

// ---- the faulter (fetch path, REQUIRED in v1, §5.7) -----------------------------

// fault decodes the missing oids and writes them into the local loose store
// in parallel batches of 32, in rounds (max 64), so subsequent `git` commands
// run unchanged.
func (e *remoteEngine) fault(ctx context.Context, h *RepoHandle, oids []string) error {
	const (
		batchSize = 32
		maxRounds = 64
	)
	for round := 0; round < maxRounds && round*batchSize < len(oids); round++ {
		lo := round * batchSize
		hi := lo + batchSize
		if hi > len(oids) {
			hi = len(oids)
		}
		batch := oids[lo:hi]
		g, gctx := store.WithContext(ctx)
		for _, oid := range batch {
			oid := oid
			g.Go(func() error {
				ix, off, ok := e.packs.locate(oid)
				if !ok {
					return &WalError{Kind: WalErrNotFound, Detail: "fault: " + oid}
				}
				kind, data, err := e.decodeAt(gctx, ix, off)
				if err != nil {
					return err
				}
				return writeLooseObject(h.repo, oid, kind, data)
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
	}
	return nil
}

// writeLooseObject writes a loose object (zlib of "type size\0" + body).
func writeLooseObject(repo *git.LocalRepo, oid, kind string, body []byte) error {
	dir := filepath.Join(repo.ObjectsDir(), oid[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &WalError{Kind: WalErrIo, Detail: dir, Wrapped: err}
	}
	dst := filepath.Join(dir, oid[2:])
	if _, err := os.Stat(dst); err == nil {
		return nil // already present
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s %d\x00", kind, len(body))
	zw := zlib.NewWriter(&buf)
	zw.Write(body)
	zw.Close()
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o444); err != nil {
		return &WalError{Kind: WalErrIo, Detail: dst, Wrapped: err}
	}
	return os.Rename(tmp, dst)
}

// Fault runs the faulter for the handle's current revision (public entry).
func (r *RemoteReader) Fault(ctx context.Context, h *RepoHandle, oids []string) error {
	if r.eng == nil {
		return &WalError{Kind: WalErrInvalid, Detail: "no remote reader attached"}
	}
	return r.eng.fault(ctx, h, oids)
}
