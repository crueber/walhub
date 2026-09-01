package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// plainReader wraps a Reader stripping the ByteReader interface so DecodeFrames
// exercises the byteReader adapter.
type plainReader struct{ r io.Reader }

func (p plainReader) Read(b []byte) (int, error) { return p.r.Read(b) }

func TestAppendDecodeRoundtrip(t *testing.T) {
	var buf bytes.Buffer
	entries := [][]byte{nil, []byte("a"), bytes.Repeat([]byte{0x7f}, 300)}
	for _, e := range entries {
		buf.Write(AppendFrame(nil, e)) // AppendFrame must also work as a pure concat helper
	}
	var got [][]byte
	n, err := DecodeFrames(bytes.NewReader(buf.Bytes()), func(frame []byte) error {
		got = append(got, frame)
		return nil
	})
	if err != nil {
		t.Fatalf("DecodeFrames: %v", err)
	}
	if n != len(entries) {
		t.Fatalf("decoded %d frames, want %d", n, len(entries))
	}
	for i, e := range entries {
		if !bytes.Equal(got[i], e) {
			t.Fatalf("frame %d = %v, want %v", i, got[i], e)
		}
	}
}

func TestDecodeFramesByteReaderAdapter(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(AppendFrame(nil, []byte("hello")))
	n, err := DecodeFrames(plainReader{r: bytes.NewReader(buf.Bytes())}, func([]byte) error { return nil })
	if err != nil || n != 1 {
		t.Fatalf("via byteReader adapter: n=%d err=%v", n, err)
	}
}

func TestDecodeFramesPartialTrailing(t *testing.T) {
	// Complete frame then a partial one: varint claims 10 bytes, 3 arrive.
	stream := append(AppendFrame(nil, []byte("ok")), 10, 'x', 'y', 'z')
	n, err := DecodeFrames(bytes.NewReader(stream), func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("partial trailing frame must not error: %v", err)
	}
	if n != 1 {
		t.Fatalf("decoded %d, want 1", n)
	}
	// A dangling multi-byte varint is likewise a tolerated partial frame.
	_, err = DecodeFrames(bytes.NewReader([]byte{0x80, 0x80}), func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("partial varint: %v", err)
	}
}

func TestDecodeFramesOversized(t *testing.T) {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], MaxFrameBytes+1)
	_, err := DecodeFrames(bytes.NewReader(buf[:n]), func([]byte) error { return nil })
	if !errors.Is(err, ErrCorruptFrame) || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized frame: err=%v", err)
	}
}

func TestDecodeFramesCallbackError(t *testing.T) {
	stream := AppendFrame(AppendFrame(nil, []byte("one")), []byte("two"))
	sentinel := errors.New("bad entry")
	n, err := DecodeFrames(bytes.NewReader(stream), func([]byte) error {
		return sentinel
	})
	if !errors.Is(err, ErrCorruptFrame) || !strings.Contains(err.Error(), "at frame 0") {
		t.Fatalf("callback error not wrapped: %v", err)
	}
	if n != 0 {
		t.Fatalf("n=%d, want 0", n)
	}
}

func TestDecodeFramesCleanEOF(t *testing.T) {
	n, err := DecodeFrames(bytes.NewReader(nil), func([]byte) error { return nil })
	if n != 0 || err != nil {
		t.Fatalf("empty stream: n=%d err=%v", n, err)
	}
}
