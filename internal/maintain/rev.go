// rev.go — the git pack reverse-index (.rev) writer (§10): byte-identical to
// git's output, built from the .idx alone, no subprocess. Format (verified
// against git's own index-pack --rev-index output):
//
//	12-byte header: "RIDX" magic + u32 version(1) + u32 hash id (1=SHA-1, 2=SHA-256)
//	N × u32 BE: entry i = the idx-order index of the object whose pack offset
//	            is the i-th smallest
//	trailer: the pack's trailing checksum (20/32 bytes) + SHA checksum
//	         (SHA-1/SHA-256 by format) of everything before it
//
// (Doc 10's "EWAH encode" note is a misnomer — EWAH belongs to .bitmap files;
// the pack .rev is a plain u32 array. This is the format git itself writes.)
package maintain

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"slices"
)

var revMagic = [4]byte{'R', 'I', 'D', 'X'} // "RIDX"

// errBadIdx marks an idx this writer cannot parse (only v2 is produced).
var errBadIdx = errors.New("unsupported pack idx")

// buildRevFile renders the .rev bytes for the given .idx contents. oidLen is
// 20 (SHA-1) or 32 (SHA-256).
func buildRevFile(idx []byte, oidLen int) ([]byte, error) {
	if oidLen != 20 && oidLen != 32 {
		return nil, errBadIdx
	}
	if len(idx) < 8+1024 || string(idx[:4]) != "\xfftOc" {
		return nil, errBadIdx
	}
	if binary.BigEndian.Uint32(idx[4:8]) != 2 {
		return nil, errBadIdx
	}
	count := int(binary.BigEndian.Uint32(idx[8+1024-4 : 8+1024]))

	// Layout: header(8) + fanout(1024) + N*oidLen + N*4 (crc) + N*4 (offsets)
	// [+ N*8 large offsets] + trailer (2*oidLen).
	oidsAt := 8 + 1024
	crcsAt := oidsAt + count*oidLen
	offsAt := crcsAt + count*4
	largeAt := offsAt + count*4
	trailerAt := largeAt
	if trailerAt+2*oidLen > len(idx) || count == 0 {
		return nil, errBadIdx
	}
	// Large offsets (MSB set): the trailer sits after the N*8 table.
	for i := range count {
		if binary.BigEndian.Uint32(idx[offsAt+4*i:])&0x80000000 != 0 {
			trailerAt = largeAt + count*8
			break
		}
	}
	if trailerAt+2*oidLen > len(idx) {
		return nil, errBadIdx
	}

	offset := func(i int) uint64 {
		off := binary.BigEndian.Uint32(idx[offsAt+4*i:])
		if off&0x80000000 == 0 {
			return uint64(off)
		}
		slot := int(off &^ 0x80000000)
		return binary.BigEndian.Uint64(idx[largeAt+8*slot:])
	}
	// rev[rank] = idx-order index of the object with the rank-th smallest
	// offset. Offsets are unique within a pack.
	order := make([]int, count)
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		oa, ob := offset(a), offset(b)
		switch {
		case oa < ob:
			return -1
		case oa > ob:
			return 1
		default:
			return a - b // deterministic tie-break; unreachable for real packs
		}
	})

	hashID := uint32(1)
	var h hash.Hash
	if oidLen == 32 {
		hashID = 2
		h = sha256.New()
	} else {
		h = sha1.New()
	}

	out := make([]byte, 0, 12+4*count+2*oidLen)
	out = append(out, revMagic[:]...)
	out = binary.BigEndian.AppendUint32(out, 1) // version
	out = binary.BigEndian.AppendUint32(out, hashID)
	for _, idxIdx := range order {
		out = binary.BigEndian.AppendUint32(out, uint32(idxIdx))
	}
	// Trailer: the pack's trailing checksum (the idx trailer's first half),
	// then the checksum of everything above.
	out = append(out, idx[trailerAt:trailerAt+oidLen]...)
	h.Write(out)
	out = h.Sum(out)
	return out, nil
}
