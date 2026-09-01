// Log framing (02_storage_protobuf.md §2.4, from MASTER_RUST_SPEC §5.3):
//
//	frame := uvarint(len(entry_bytes)) || entry_bytes
//
// Encoding appends only. Decoding tolerates a partial TRAILING frame (growing segment) and
// errors on a corrupt COMPLETE frame. Frame length cap: 32 MiB.
//
// This file is byte-level on purpose: the proto package (internal/store/proto) wraps these
// helpers with typed LogEntry encode/decode (AppendEntries / DecodeEntries).
package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxFrameBytes bounds a single frame's length (corrupt-stream allocation guard).
const MaxFrameBytes = 32 << 20

// ErrCorruptFrame is returned for a complete-but-unparsable frame or an oversized length.
var ErrCorruptFrame = errors.New("corrupt log frame")

// AppendFrame appends one uvarint-prefixed frame carrying entryBytes to buf.
func AppendFrame(buf []byte, entryBytes []byte) []byte {
	var v [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(v[:], uint64(len(entryBytes)))
	buf = append(buf, v[:n]...)
	return append(buf, entryBytes...)
}

// DecodeFrames streams frames from r, invoking cb per complete frame body.
// Returns the number of complete frames consumed. A partial trailing frame is NOT an error
// (stop, nil); a corrupt complete frame is ErrCorruptFrame (or the callback error wrapped).
func DecodeFrames(r io.Reader, cb func(frame []byte) error) (int, error) {
	br, ok := r.(io.ByteReader)
	if !ok {
		br = &byteReader{r: r}
	}
	n := 0
	for {
		ln, err := binary.ReadUvarint(br)
		if err == io.EOF {
			return n, nil // clean end at a frame boundary
		}
		if err != nil {
			return n, nil // partial varint == partial trailing frame: tolerated
		}
		if ln > MaxFrameBytes {
			return n, fmt.Errorf("%w: frame length %d exceeds %d", ErrCorruptFrame, ln, MaxFrameBytes)
		}
		body := make([]byte, ln)
		if _, err := io.ReadFull(r, body); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return n, nil // partial trailing frame: tolerated
			}
			return n, err
		}
		if err := cb(body); err != nil {
			return n, fmt.Errorf("%w at frame %d: %v", ErrCorruptFrame, n, err)
		}
		n++
	}
}

// byteReader adapts an io.Reader to io.ByteReader (callers wanting speed wrap in bufio.Reader,
// which already implements io.ByteReader).
type byteReader struct{ r io.Reader }

func (b *byteReader) ReadByte() (byte, error) {
	var buf [1]byte
	if _, err := io.ReadFull(b.r, buf[:]); err != nil {
		return 0, err
	}
	return buf[0], nil
}
