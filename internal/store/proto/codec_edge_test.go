package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"

	"git.packden.us/crueber/walhub/internal/store"
)

// unmarshaler is the exported decode entry point every message type offers.
type unmarshaler interface {
	Unmarshal([]byte) error
}

// edgeMessages is one fully-populated instance per message type (allMessages
// plus the typeless Timestamp, which codec_test covers only ad hoc).
func edgeMessages() map[string]submessager {
	msgs := allMessages()
	msgs["Timestamp"] = &Timestamp{Seconds: -7, Nanos: 42}
	return msgs
}

// freshOf returns a new zero value of the same message type as m.
func freshOf(t *testing.T, m submessager) unmarshaler {
	t.Helper()
	v := reflect.New(reflect.TypeOf(m).Elem()).Interface()
	u, ok := v.(unmarshaler)
	if !ok {
		t.Fatalf("%T has no Unmarshal([]byte) error", v)
	}
	return u
}

// TestEdgeTruncationPrefixes: for every message type, every byte-prefix of a
// fully-populated encoding is either a valid (shorter) message or fails with
// ErrTruncated/ErrInvalid — never a silent misparse. Walking every prefix
// drives each field's decode-error arm (truncated varint, truncated length,
// truncated fixed64/fixed32, truncated submessage) with real bytes.
func TestEdgeTruncationPrefixes(t *testing.T) {
	for name, m := range edgeMessages() {
		full := mustMarshal(t, m)
		if len(full) == 0 {
			t.Fatalf("%s: fully-populated encoding is empty", name)
		}
		for p := 0; p < len(full); p++ {
			u := freshOf(t, m)
			err := u.Unmarshal(full[:p])
			if err == nil {
				continue // a field boundary prefix is itself a valid message
			}
			if !errors.Is(err, ErrTruncated) && !errors.Is(err, ErrInvalid) {
				t.Fatalf("%s prefix %d/%d: got %v, want ErrTruncated/ErrInvalid", name, p, len(full), err)
			}
		}
		if err := freshOf(t, m).Unmarshal(full); err != nil {
			t.Fatalf("%s: full encoding rejected: %v", name, err)
		}
	}
}

// TestEdgeUnknownWireTypesAllMessages: unknown field 15 with every wire type
// is skipped by every message decoder; groups and truncated fixed values fail
// with the documented errors. Covers each message's default arm plus every
// skip() branch.
func TestEdgeUnknownWireTypesAllMessages(t *testing.T) {
	cases := []struct {
		name    string
		suffix  []byte
		wantErr error
	}{
		{"varint", []byte{0x78, 0x2A}, nil},                    // field 15, wt 0
		{"fixed64", []byte{0x79, 1, 2, 3, 4, 5, 6, 7, 8}, nil}, // field 15, wt 1
		{"bytes", []byte{0x7A, 3, 'x', 'y', 'z'}, nil},         // field 15, wt 2
		{"fixed32", []byte{0x7D, 1, 2, 3, 4}, nil},             // field 15, wt 5
		{"group", []byte{0x7B}, ErrInvalid},                    // field 15, wt 3: never valid
		{"truncated varint", []byte{0x78, 0x80}, ErrTruncated},
		{"truncated fixed64", []byte{0x79, 1, 2, 3}, ErrTruncated},
		{"truncated fixed32", []byte{0x7D, 1}, ErrTruncated},
		{"length past end", []byte{0x7A, 9, 'x'}, ErrTruncated},
	}
	for name, m := range edgeMessages() {
		base := mustMarshal(t, m)
		for _, tc := range cases {
			u := freshOf(t, m)
			err := u.Unmarshal(append(append([]byte{}, base...), tc.suffix...))
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("%s + unknown %s: unexpected error %v", name, tc.name, err)
				}
				continue
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("%s + unknown %s: got %v, want %v", name, tc.name, err, tc.wantErr)
			}
		}
	}
}

// TestEdgeVarintOverflow: an 11-byte tag varint overflows (shift >= 64) and a
// 10-byte unterminated varint is truncated.
func TestEdgeVarintOverflow(t *testing.T) {
	overflow := bytes.Repeat([]byte{0x80}, 10)
	overflow = append(overflow, 0x01) // 11 bytes: shift climbs past 64
	m := &Manifest{}
	if err := m.Unmarshal(overflow); !errors.Is(err, ErrInvalid) {
		t.Fatalf("11-byte varint: got %v, want ErrInvalid", err)
	}
	trunc := bytes.Repeat([]byte{0x80}, 10) // unterminated
	if err := m.Unmarshal(trunc); !errors.Is(err, ErrTruncated) {
		t.Fatalf("unterminated varint: got %v, want ErrTruncated", err)
	}
}

// frameBody prefixes b with its uvarint length (one §2.4 frame).
func frameBody(b []byte) []byte { return append(binary.AppendUvarint(nil, uint64(len(b))), b...) }

// TestEdgeNilReceivers: every message's Size/AppendTo/Marshal tolerate a nil
// receiver (proto3 nil = absent = zero bytes).
func TestEdgeNilReceivers(t *testing.T) {
	for name, m := range edgeMessages() {
		nilPtr := reflect.Zero(reflect.TypeOf(m)).Interface()
		if s, ok := nilPtr.(interface{ Size() int }); ok {
			if got := s.Size(); got != 0 {
				t.Fatalf("%s: nil Size() = %d, want 0", name, got)
			}
		}
		if ap, ok := nilPtr.(submessager); ok {
			if got := ap.AppendTo(nil); len(got) != 0 {
				t.Fatalf("%s: nil AppendTo = %d bytes, want 0", name, len(got))
			}
		}
	}
	var seg *LogSegment
	if got := seg.Marshal(); got != nil {
		t.Fatalf("nil LogSegment.Marshal = %v, want nil", got)
	}
	var ls LogSegment
	if err := ls.Unmarshal(frameBody([]byte{0x00, 0x00})); !errors.Is(err, store.ErrCorruptFrame) {
		t.Fatalf("LogSegment.Unmarshal corrupt: %v", err)
	}
}

// mapField wraps entry bytes as a LogEntry.meta (field 9, length-delimited)
// value.
func mapField(entry []byte) []byte {
	b := binary.AppendUvarint(nil, 9<<3|2)
	b = binary.AppendUvarint(b, uint64(len(entry)))
	return append(b, entry...)
}

// TestEdgeMapEntryDecodeErrors: a map entry submessage with a broken tag, a
// truncated string, an unknown inner field or a group fails the LogEntry
// decode; invalid UTF-8 in a value is rejected.
func TestEdgeMapEntryDecodeErrors(t *testing.T) {
	cases := []struct {
		name    string
		entry   []byte
		wantErr error
	}{
		{"unknown inner field", []byte{0x18, 0x01}, nil}, // field 3, wt 0 → skip
		{"inner group", []byte{0x0B}, ErrInvalid},        // field 1, wt 3
		{"truncated tag", []byte{0x80}, ErrTruncated},
		{"truncated string", []byte{0x0A, 0x05, 'a', 'b'}, ErrTruncated}, // key, len 5, 2 bytes
		{"invalid utf8 value", []byte{0x12, 0x02, 0xFF, 0xFE}, ErrInvalid},
	}
	for _, tc := range cases {
		e := &LogEntry{}
		err := e.Unmarshal(mapField(tc.entry))
		if tc.wantErr == nil {
			if err != nil {
				t.Fatalf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, tc.wantErr) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.wantErr)
		}
	}
	// Happy path sanity: key+value land in the map.
	e := &LogEntry{}
	if err := e.Unmarshal(mapField([]byte{0x0A, 0x01, 'k', 0x12, 0x01, 'v'})); err != nil {
		t.Fatalf("valid entry: %v", err)
	}
	if e.Meta["k"] != "v" {
		t.Fatalf("meta = %v", e.Meta)
	}
}

// TestEdgeFramingErrors: the §2.4 framing wrappers reject corrupt sealed
// segments and report partial tails through store.ErrCorruptFrame.
func TestEdgeFramingErrors(t *testing.T) {
	// Unterminated frame length.
	if _, err := DecodeSegment([]byte{0x80}); !errors.Is(err, store.ErrCorruptFrame) {
		t.Fatalf("unterminated length: %v", err)
	}
	// Length varint overflows (shift >= 64).
	if _, err := DecodeSegment(bytes.Repeat([]byte{0x80}, 11)); !errors.Is(err, store.ErrCorruptFrame) {
		t.Fatalf("overflow length: %v", err)
	}
	// Frame length exceeds MaxFrameBytes.
	if _, err := DecodeSegment(binary.AppendUvarint(nil, uint64(store.MaxFrameBytes)+1)); !errors.Is(err, store.ErrCorruptFrame) {
		t.Fatalf("oversized frame: %v", err)
	}
	// Partial trailing frame in a sealed segment.
	if _, err := DecodeSegment(append([]byte{0x05}, "abc"...)); !errors.Is(err, store.ErrCorruptFrame) {
		t.Fatalf("partial tail: %v", err)
	}
	// A complete frame whose body is invalid proto (field number 0).
	if _, err := DecodeSegment(frameBody([]byte{0x00, 0x00})); !errors.Is(err, store.ErrCorruptFrame) {
		t.Fatalf("bad frame body: %v", err)
	}
	// Nil entries are skipped when framing.
	framed := AppendEntries(nil, []*LogEntry{nil, {Seq: 7}})
	entries, err := DecodeSegment(framed)
	if err != nil || len(entries) != 1 || entries[0].Seq != 7 {
		t.Fatalf("AppendEntries nil skip: %v %v", entries, err)
	}
	// LogSegment wrappers: nil encodes empty; corrupt input is an error.
	var nilSeg *LogSegment
	if b := nilSeg.Marshal(); len(b) != 0 {
		t.Fatalf("nil segment marshal = %d bytes", len(b))
	}
	var ls LogSegment
	if err := ls.Unmarshal(frameBody([]byte{0x00, 0x00})); !errors.Is(err, store.ErrCorruptFrame) {
		t.Fatalf("LogSegment.Unmarshal corrupt: %v", err)
	}
	// DecodeEntries surfaces a corrupt complete frame.
	n, err := DecodeEntries(bytes.NewReader(append(frameBody([]byte{0x00}), frameBody([]byte{0x08, 0x01})...)), func(*LogEntry) error { return nil })
	if n != 0 || !errors.Is(err, store.ErrCorruptFrame) {
		t.Fatalf("DecodeEntries corrupt = (%d, %v)", n, err)
	}
	// UnmarshalBundleList rejects garbage.
	if _, err := UnmarshalBundleList([]byte{0x00}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("UnmarshalBundleList: %v", err)
	}
}

// TestEdgeFloatTruncation: a fixed64 field cut mid-value is ErrTruncated.
func TestEdgeFloatTruncation(t *testing.T) {
	full := (&FsckReport{ElapsedSecs: 3.5}).Marshal()
	// Drop the last byte of the double: the tag+6 bytes remain.
	u := &FsckReport{}
	if err := u.Unmarshal(full[:len(full)-1]); !errors.Is(err, ErrTruncated) {
		t.Fatalf("truncated double: %v", err)
	}
}

// TestEdgeMergeSemantics: re-decoding into a populated message merges
// field-granular (last wins) without resetting (§2.3.1) — the unmarshal path
// behind every submessage merge arm.
func TestEdgeMergeSemantics(t *testing.T) {
	m := &Manifest{Writer: "old", HeadSeq: 1}
	if err := m.unmarshal((&Manifest{Writer: "new"}).Marshal()); err != nil {
		t.Fatal(err)
	}
	if m.Writer != "new" || m.HeadSeq != 1 {
		t.Fatalf("merge = %+v", m)
	}
	if !strings.HasPrefix(ErrTruncated.Error(), "proto:") {
		t.Fatalf("error text = %q", ErrTruncated.Error())
	}
}

// TestEdgeCorruptBytes: flipping any single byte of a fully-populated
// encoding to 0xFF either still decodes (e.g. a value byte) or fails with
// ErrTruncated/ErrInvalid — never panics and never a non-proto error. Mangling
// submessage interiors (lengths, tags) drives the nested unmarshal error arms.
func TestEdgeCorruptBytes(t *testing.T) {
	for name, m := range edgeMessages() {
		full := mustMarshal(t, m)
		for i := range full {
			bad := append([]byte{}, full...)
			bad[i] = 0xFF
			err := freshOf(t, m).Unmarshal(bad)
			if err == nil {
				continue
			}
			if !errors.Is(err, ErrTruncated) && !errors.Is(err, ErrInvalid) {
				t.Fatalf("%s byte %d: got %v, want ErrTruncated/ErrInvalid", name, i, err)
			}
		}
	}
}
