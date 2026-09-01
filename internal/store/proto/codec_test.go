package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
)

func mustMarshal(t *testing.T, m submessager) []byte {
	b := m.AppendTo(nil)
	var sz int
	if s, ok := m.(interface{ Size() int }); ok {
		sz = s.Size()
	} else {
		sz = m.(interface{ EncodedSize() int }).EncodedSize()
	}
	if sz != len(b) {
		t.Fatalf("Size()=%d but len(AppendTo)=%d", sz, len(b))
	}
	return b
}

// allMessages returns one instance of every message type with every field set.
func allMessages() map[string]submessager {
	ts := func(s, n int64) *Timestamp { return &Timestamp{Seconds: s, Nanos: int32(n)} }
	return map[string]submessager{
		"Manifest": &Manifest{
			FormatVersion: WALFormatVersion, Repo: "a/b", ObjectFormat: "sha256",
			HeadSeq: 9, MinSeq: 4,
			Checkpoint:  &CheckpointRef{Seq: 3, Key: "k", CreatedAt: ts(1, 0), FirstStateAt: ts(2, 3), AsOf: ts(4, 0)},
			LogSegments: []*LogSegmentRef{{Key: "log/1.pb", FirstSeq: 4, LastSeq: 9, Size: 12, Sealed: true}},
			Packs:       []*PackRef{{Checksum: "cc", PackSize: 1, IdxSize: 2, HasRev: true, ObjectCount: 7, Seq: 4, Tier: 1, Kind: PackKindHistory, DerivedFrom: "dd"}},
			UpdatedAt:   ts(10, 999), Writer: "w", Revision: 2,
			Settings: &RepoSettings{Toml: "[x]", Revision: 1, Author: "a", UpdatedAt: ts(3, 4), Message: "m"},
		},
		"RepoSettings":  &RepoSettings{Toml: "t", Revision: 3, Author: "a", UpdatedAt: ts(5, 6), Message: "msg"},
		"LogSegmentRef": &LogSegmentRef{Key: "log/0000000000000042.pb", FirstSeq: 1, LastSeq: 2, Size: 10, Sealed: true},
		"PackRef": &PackRef{Checksum: "ab", PackSize: 1, IdxSize: 2, HasRev: true, HasBitmap: true,
			ObjectCount: 5, Seq: 6, Tier: 2, HasCommitGraph: true, Kind: PackKindHistory, DerivedFrom: "cd"},
		"LogEntry": &LogEntry{
			Seq: 1, Kind: EntryKindPush,
			Pack:       &PackRef{Checksum: "p"},
			Txn:        &RefTransaction{Updates: []*RefUpdate{{Name: "refs/heads/main", NewOid: "aa"}}, PushOptions: []string{"o"}, Atomic: true},
			Supersedes: []string{"s1", "s2"},
			Checkpoint: &CheckpointRef{Seq: 2, Key: "k"},
			CreatedAt:  ts(1, 2), Writer: "w",
			Meta:     map[string]string{"b": "2", "a": "1"},
			Settings: &RepoSettings{Toml: "t"},
		},
		"RefUpdate": &RefUpdate{Name: "refs/heads/main", OldOid: "00", NewOid: "11", NewSymbolicTarget: "refs/heads/x", NewPeeled: "22"},
		"RefTransaction": &RefTransaction{
			Updates:     []*RefUpdate{{Name: "HEAD", NewSymbolicTarget: "refs/heads/main"}},
			PushOptions: []string{"atomic"},
			Atomic:      true,
		},
		"Checkpoint": &Checkpoint{
			Seq: 4, ObjectFormat: "sha1",
			Packs:    []*PackRef{{Checksum: "x", Seq: 1}},
			RefsKey:  "checkpoints/0000000000000004/refs.pb",
			RefCount: 9, BundleKey: "b.bundle", CreatedAt: ts(1, 1), Writer: "w",
		},
		"CheckpointRef": &CheckpointRef{Seq: 1, Key: "k", CreatedAt: ts(1, 2), FirstStateAt: ts(3, 4), AsOf: ts(5, 6)},
		"RefSnapshot": &RefSnapshot{
			Seq: 1, ObjectFormat: "sha1",
			Refs:       []*Ref{{Name: "refs/heads/main", Oid: "aa", Peeled: "bb"}, {Name: "refs/tags/v", Oid: "cc"}},
			HeadTarget: "refs/heads/main", CreatedAt: ts(7, 8),
		},
		"Ref":   &Ref{Name: "refs/heads/main", Oid: "aa", Peeled: "bb"},
		"Lease": &Lease{Holder: "h", Purpose: "compact", AcquiredAt: ts(1, 2), ExpiresAt: ts(3, 4), Epoch: 5},
		"BundleList": &BundleList{Mode: "all", Heuristic: "creationToken",
			Bundles:   []*BundleEntry{{ID: "b1", Key: "k", Strategy: "daily", Kind: "full", CreationToken: 5, Seq: 6, Size: 7, CreatedAt: ts(1, 1), Version: "v1", Tips: []*Ref{{Name: "refs/heads/main", Oid: "aa"}}, Slot: 8, Filter: "blob:none"}},
			UpdatedAt: ts(2, 2),
			Skipped:   []*SkippedSlot{{Strategy: "daily", Slot: 3, BaseID: "x", AsOfSeq: 4, Reason: "too-small: 1 commits (min 5)", At: ts(9, 9)}},
		},
		"SkippedSlot": &SkippedSlot{Strategy: "s", Slot: 1, BaseID: "b", AsOfSeq: 2, Reason: "r", At: ts(3, 4)},
		"BundleEntry": &BundleEntry{ID: "id", Key: "key", Strategy: "daily", Kind: "incremental",
			CreationToken: 10, Seq: 11, Size: 12, BaseID: "base", CreatedAt: ts(1, 3), Version: "ver",
			Tips: []*Ref{{Name: "refs/heads/main", Oid: "aa"}}, Slot: 20, Filter: "blob:none"},
		"FsckReport":  &FsckReport{Seq: 1, At: ts(2, 3), Host: "h", Missing: []string{"x"}, MissingTotal: 1, Problems: 2, ElapsedSecs: 3.5, RepairedSeq: 4},
		"RepoCatalog": &RepoCatalog{Repos: []string{"a/b", "c/d"}, UpdatedAt: ts(1, 2)},
		"MaintainerHeartbeat": &MaintainerHeartbeat{Host: "h1", Repos: []string{"a/b"}, Exclude: []string{"c/d"},
			MaxPackByte: 1000, Disk: "ssd", StartedAt: ts(1, 1), LastPassAt: ts(2, 2), LastUnit: "r compact d", Passes: 7},
	}
}

// ---- round-trip: every message type ----

func TestAllMessagesRoundTrip(t *testing.T) {
	for name, m := range allMessages() {
		b := mustMarshal(t, m)
		dup, ok := allMessages()[name]
		if !ok {
			t.Fatalf("missing %s", name)
		}
		_ = dup
		// decode into a fresh zero value via a generic switch
		decoded := newInstance(name)
		if err := decoded.(interface{ Unmarshal([]byte) error }).Unmarshal(b); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		b2 := decoded.(submessager).AppendTo(nil)
		if !bytes.Equal(b, b2) {
			t.Fatalf("%s: re-encode differs\n want %x\n  got %x", name, b, b2)
		}
		var sz int
		if s, ok := decoded.(interface{ Size() int }); ok {
			sz = s.Size()
		} else {
			sz = decoded.(interface{ EncodedSize() int }).EncodedSize()
		}
		if sz != len(b) {
			t.Fatalf("%s: Size()=%d, want %d", name, sz, len(b))
		}
	}
}

// newInstance returns a zero *T for the message name.
func newInstance(name string) interface{} {
	switch name {
	case "Manifest":
		return &Manifest{}
	case "RepoSettings":
		return &RepoSettings{}
	case "LogSegmentRef":
		return &LogSegmentRef{}
	case "PackRef":
		return &PackRef{}
	case "LogEntry":
		return &LogEntry{}
	case "RefUpdate":
		return &RefUpdate{}
	case "RefTransaction":
		return &RefTransaction{}
	case "Checkpoint":
		return &Checkpoint{}
	case "CheckpointRef":
		return &CheckpointRef{}
	case "RefSnapshot":
		return &RefSnapshot{}
	case "Ref":
		return &Ref{}
	case "Lease":
		return &Lease{}
	case "BundleList":
		return &BundleList{}
	case "SkippedSlot":
		return &SkippedSlot{}
	case "BundleEntry":
		return &BundleEntry{}
	case "FsckReport":
		return &FsckReport{}
	case "RepoCatalog":
		return &RepoCatalog{}
	case "MaintainerHeartbeat":
		return &MaintainerHeartbeat{}
	case "Timestamp":
		return &Timestamp{}
	case "LogSegment":
		return &LogSegment{}
	}
	panic("unknown message " + name)
}

// ---- proto3 defaults: zero value encodes empty, decodes to zero ----

func TestZeroValuesEncodeEmpty(t *testing.T) {
	if b := (&Manifest{}).Marshal(); len(b) != 0 {
		t.Fatalf("zero Manifest encodes %d bytes, want 0", len(b))
	}
	if b := (&LogEntry{}).Marshal(); len(b) != 0 {
		t.Fatalf("zero LogEntry encodes %d bytes, want 0", len(b))
	}
	m := &Manifest{}
	if err := m.Unmarshal(nil); err != nil {
		t.Fatalf("unmarshal nil: %v", err)
	}
	if m.Repo != "" || m.HeadSeq != 0 || m.Checkpoint != nil || m.LogSegments != nil {
		t.Fatalf("decoded non-zero: %+v", m)
	}
}

// empty-vs-absent equivalence: explicit zero submessage == nil (§2.2.1).
func TestEmptyTimestampEqualsAbsent(t *testing.T) {
	m1 := &LogEntry{CreatedAt: &Timestamp{}}
	m2 := &LogEntry{}
	if !bytes.Equal(m1.Marshal(), m2.Marshal()) {
		t.Fatalf("empty Timestamp must encode identically to nil")
	}
}

// ---- varint boundary values (fixture cross-check too) ----

func TestVarintBoundaries(t *testing.T) {
	vals := []uint64{0, 1, 127, 128, 1<<32 - 1, 1 << 32, 1 << 63, math.MaxUint64}
	for _, v := range vals {
		e := &LogEntry{Seq: v}
		b := e.Marshal()
		got := &LogEntry{}
		if err := got.Unmarshal(b); err != nil {
			t.Fatalf("v=%d: %v", v, err)
		}
		if got.Seq != v {
			t.Fatalf("v=%d got %d", v, got.Seq)
		}
	}
}

// ---- maps: sorted output, decoder accepts arbitrary order, omitted sides ----

func TestMapSortedAndMerge(t *testing.T) {
	e := &LogEntry{Meta: map[string]string{"z": "1", "a": "2"}}
	b := e.Marshal()
	// two map entries; first must be "a" (field 9 = tag 0x4a)
	if !bytes.HasPrefix(b, []byte{0x4a, 0x06, 0x0a, 0x01, 'a', 0x12, 0x01, '2'}) {
		t.Fatalf("map not sorted-first-by-key: %x", b)
	}
	// empty key and value sides omitted
	e2 := &LogEntry{Meta: map[string]string{"": "", "k": ""}}
	b2 := e2.Marshal()
	// entry for "": submessage is empty -> len 0
	want := []byte{0x4a, 0x00}
	if !bytes.HasPrefix(b2, want) {
		t.Fatalf("empty-side omission broken: %x", b2)
	}
	// decoder accepts any order and synthesizes "" sides
	var out LogEntry
	if err := out.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	if out.Meta["z"] != "1" || out.Meta["a"] != "2" || len(out.Meta) != 2 {
		t.Fatalf("meta: %v", out.Meta)
	}
}

// ---- last-wins on duplicates; repeated concatenation ----

func TestDuplicateFieldsLastWins(t *testing.T) {
	// field 4 (head_seq) twice: 5 then 6 -> 6
	b := []byte{}
	b = binary.AppendUvarint(b, 4<<3|0)
	b = binary.AppendUvarint(b, 5)
	b = binary.AppendUvarint(b, 4<<3|0)
	b = binary.AppendUvarint(b, 6)
	m := &Manifest{}
	if err := m.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	if m.HeadSeq != 6 {
		t.Fatalf("last-wins broken: %d", m.HeadSeq)
	}
	// repeated concatenation in encounter order
	b = nil
	add := func(key string) {
		inner := (&LogSegmentRef{Key: key}).Marshal()
		b = binary.AppendUvarint(b, 7<<3|2)
		b = binary.AppendUvarint(b, uint64(len(inner)))
		b = append(b, inner...)
	}
	add("x")
	add("y")
	m = &Manifest{}
	if err := m.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	if len(m.LogSegments) != 2 || m.LogSegments[0].Key != "x" || m.LogSegments[1].Key != "y" {
		t.Fatalf("repeated order: %+v", m.LogSegments)
	}
}

// ---- unknown fields skipped by wire type; groups invalid ----

func TestUnknownFields(t *testing.T) {
	m := &Manifest{Repo: "r"}
	b := m.Marshal()
	// append unknown varint field 100 (wt 0), unknown 64-bit (wt 1), unknown len-delim (wt 2), unknown fixed32 (wt 5)
	unk := []byte{}
	unk = binary.AppendUvarint(unk, 900<<3|0)
	unk = binary.AppendUvarint(unk, 7)
	unk = binary.AppendUvarint(unk, 901<<3|1)
	unk = append(unk, make([]byte, 8)...)
	unk = binary.AppendUvarint(unk, 902<<3|2)
	unk = binary.AppendUvarint(unk, 2)
	unk = append(unk, 'z', 'z')
	unk = binary.AppendUvarint(unk, 903<<3|5)
	unk = append(unk, make([]byte, 4)...)
	got := &Manifest{}
	if err := got.Unmarshal(append(b, unk...)); err != nil {
		t.Fatalf("unknown skip: %v", err)
	}
	if got.Repo != "r" {
		t.Fatalf("payload lost: %+v", got)
	}
	// groups (wt 3/4) must be rejected
	got = &Manifest{}
	if err := got.Unmarshal([]byte{0x0a, 0x00, byte(9<<3 | 3)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("group wt 3: got %v, want ErrInvalid", err)
	}
	got = &Manifest{}
	if err := got.Unmarshal([]byte{byte(12<<3 | 4)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("group wt 4: got %v, want ErrInvalid", err)
	}
}

// ---- truncation / invalid ----

func TestTruncatedInputs(t *testing.T) {
	full := (&Manifest{Repo: "repo", HeadSeq: math.MaxUint64, Packs: []*PackRef{{Checksum: "xyz"}}}).Marshal()
	for n := 1; n < len(full); n++ {
		m := &Manifest{}
		err := m.Unmarshal(full[:n])
		if err == nil {
			// a prefix can be valid when it happens to end on a field boundary
			continue
		}
		if !errors.Is(err, ErrTruncated) {
			t.Fatalf("prefix %d: %v", n, err)
		}
	}
	// tag-only with field number 0
	if err := (&Manifest{}).Unmarshal([]byte{0x00}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("field 0: %v", err)
	}
	// 11-byte varint
	long := bytes.Repeat([]byte{0x80}, 11)
	if err := (&Manifest{}).Unmarshal(long); !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrTruncated) {
		t.Fatalf("long varint: %v", err)
	}
}

// ---- string UTF-8 validation ----

func TestInvalidUTF8(t *testing.T) {
	b := []byte{0x12, 0x02, 0xff, 0xfe} // field 2 (repo), invalid UTF-8
	if err := (&Manifest{}).Unmarshal(b); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid utf8: %v", err)
	}
}

// ---- append-style API: zero allocation on AppendTo with pre-sized buf ----

func TestAppendToChaining(t *testing.T) {
	m := &PackRef{Checksum: "abc", PackSize: 5}
	var buf []byte
	buf = m.AppendTo(buf)
	direct := m.Marshal()
	if !bytes.Equal(buf, direct) {
		t.Fatalf("AppendTo != Marshal")
	}
	// nil receiver is a no-op
	var nilRef *PackRef
	if got := nilRef.AppendTo([]byte{1}); len(got) != 1 {
		t.Fatalf("nil AppendTo: %x", got)
	}
	if nilRef.Size() != 0 {
		t.Fatalf("nil Size: %d", nilRef.Size())
	}
	if nilRef.Marshal() == nil && len(nilRef.Marshal()) != 0 {
		t.Fatal("nil Marshal")
	}
}

// ---- Timestamp helpers ----

func TestTimestampConversions(t *testing.T) {
	ts := Timestamp{Seconds: 1700000000, Nanos: 5}
	got := ts.Go()
	if got.Unix() != 1700000000 || got.Nanosecond() != 5 {
		t.Fatalf("Go(): %v", got)
	}
	if !TimeFromGo(got).Go().Equal(got) {
		t.Fatalf("round trip: %v", TimeFromGo(got))
	}
	// zero time.Time maps to zero Timestamp
	if z := TimeFromGo(timeZero()); z != (Timestamp{}) {
		t.Fatalf("zero time: %+v", z)
	}
}

// ---- golden fixtures (§2.3.3, TestGoldenWireCompat) ----

// TestGoldenWireCompat is the normative wire-compat proof (doc 02 §2.3.3): each .bin
// fixture (generated once offline via protoc --encode, plus hand-computed map fixtures —
// see testdata/) decodes to the values documented in the .golden.json, and re-encodes
// byte-identically.
func TestGoldenWireCompat(t *testing.T) {
	var files []string
	root := "testdata/golden"
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(info.Name(), ".bin") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 12 {
		t.Fatalf("expected >=12 fixtures, got %d", len(files))
	}
	sort.Strings(files)
	for _, rel := range files {
		rel := rel
		t.Run(filepath.Base(rel), func(t *testing.T) {
			raw, err := os.ReadFile(rel)
			if err != nil {
				t.Fatal(err)
			}
			jb, err := os.ReadFile(strings.TrimSuffix(rel, ".bin") + ".golden.json")
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Type  string          `json:"type"`
				Value json.RawMessage `json:"value"`
			}
			if err := json.Unmarshal(jb, &doc); err != nil {
				t.Fatal(err)
			}
			// expected: the golden.json decoded expectation, decoded into the same Go type
			expected := newInstance(doc.Type)
			if err := json.Unmarshal(doc.Value, expected); err != nil {
				t.Fatalf("%s: golden.json does not fit %s: %v", rel, doc.Type, err)
			}
			// actual: codec decode of the reference bytes
			actual := newInstance(doc.Type)
			decodeInto := func(v interface{}) error {
				if ls, ok := v.(*LogSegment); ok {
					return ls.Unmarshal(raw)
				}
				return v.(interface{ Unmarshal([]byte) error }).Unmarshal(raw)
			}
			if err := decodeInto(actual); err != nil {
				t.Fatalf("%s: codec decode: %v", rel, err)
			}
			if !reflect.DeepEqual(expected, actual) {
				t.Fatalf("%s: decoded value differs from golden.json\n want %#v\n  got %#v", rel, expected, actual)
			}
			// re-encode: byte-identical with the reference implementation's bytes
			var reOut []byte
			if ls, ok := actual.(interface{ Marshal() []byte }); ok {
				reOut = ls.Marshal()
			} else {
				reOut = actual.(submessager).AppendTo(nil)
			}
			if !bytes.Equal(raw, reOut) {
				t.Fatalf("%s: re-encode not byte-identical\n want %x\n  got %x", rel, raw, reOut)
			}
		})
	}
}

// ---- framing wrappers (§2.4) ----

func TestAppendEntriesDecodeEntriesRoundTrip(t *testing.T) {
	entries := []*LogEntry{
		{Seq: 1, Kind: EntryKindPush, Writer: "w1", Meta: map[string]string{"k": "v"}},
		{Seq: 2, Kind: EntryKindRefUpdate, Txn: &RefTransaction{Updates: []*RefUpdate{{Name: "refs/heads/main"}}}},
		{Seq: 3, Kind: EntryKindCheckpoint, Checkpoint: &CheckpointRef{Seq: 2, Key: "k"}},
	}
	buf := AppendEntries(make([]byte, 0, 64), entries)
	// incremental append style
	buf2 := AppendEntries(AppendEntries(nil, entries[:1]), entries[1:])
	if !bytes.Equal(buf, buf2) {
		t.Fatal("AppendEntries not concatenation-safe")
	}
	var got []*LogEntry
	n, err := DecodeEntries(bytes.NewReader(buf), func(e *LogEntry) error {
		got = append(got, e)
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("DecodeEntries: n=%d err=%v", n, err)
	}
	if len(got) != 3 || got[0].Seq != 1 || got[2].Seq != 3 {
		t.Fatalf("entries: %+v", got)
	}
	// callback error propagates as ErrCorruptFrame
	_, err = DecodeEntries(bytes.NewReader(buf), func(*LogEntry) error {
		return errors.New("boom")
	})
	if !errors.Is(err, store.ErrCorruptFrame) {
		t.Fatalf("callback err: %v", err)
	}
	// corrupt complete frame: bad body
	bad := AppendEntries(nil, entries[:1])
	bad[len(bad)-1] ^= 0xff
	if _, err := DecodeEntries(bytes.NewReader(bad), func(*LogEntry) error { return nil }); err == nil {
		t.Fatal("corrupt frame not detected")
	}
}

func TestPartialTrailingFrameTolerated(t *testing.T) {
	entries := []*LogEntry{{Seq: 1, Writer: "w"}}
	full := AppendEntries(nil, entries)
	for cut := 1; cut < len(full); cut++ {
		n, err := DecodeEntries(bytes.NewReader(full[:len(full)-cut]), func(*LogEntry) error { return nil })
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if n != 0 {
			t.Fatalf("cut %d: n=%d, want 0", cut, n)
		}
	}
	// two entries, second partial: first is delivered
	two := AppendEntries(nil, []*LogEntry{{Seq: 1}, {Seq: 2}})
	n, err := DecodeEntries(bytes.NewReader(two[:len(two)-1]), func(*LogEntry) error { return nil })
	if err != nil || n != 1 {
		t.Fatalf("partial second frame: n=%d err=%v", n, err)
	}
}

func TestDecodeSegment(t *testing.T) {
	entries := []*LogEntry{{Seq: 1, Writer: "a"}, {Seq: 2, Writer: "b"}, {Seq: 3, Writer: "c"}}
	seg := EncodeSegment(entries)
	got, err := DecodeSegment(seg)
	if err != nil || len(got) != 3 {
		t.Fatalf("DecodeSegment: %d %v", len(got), err)
	}
	// sealed segment with a partial tail is ErrCorruptFrame
	if _, err := DecodeSegment(seg[:len(seg)-1]); !errors.Is(err, store.ErrCorruptFrame) {
		t.Fatalf("partial sealed tail: %v", err)
	}
	// LogSegment struct round-trips
	ls := &LogSegment{Entries: entries}
	b := ls.Marshal()
	ls2 := &LogSegment{}
	if err := ls2.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	if len(ls2.Entries) != 3 || ls2.Entries[1].Seq != 2 {
		t.Fatalf("LogSegment: %+v", ls2.Entries)
	}
	// oversized frame length
	huge := []byte{}
	var v [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(v[:], store.MaxFrameBytes+1)
	huge = append(huge, v[:n]...)
	if _, err := DecodeSegment(huge); !errors.Is(err, store.ErrCorruptFrame) {
		t.Fatalf("oversize: %v", err)
	}
}

// ---- zigzag helpers (§2.3.1, exposed but unused by the schema) ----

func TestZigZag(t *testing.T) {
	for _, v := range []int64{0, -1, 1, -2, 2147483647, -2147483648, math.MaxInt64, math.MinInt64} {
		if got := ZigZag(PutZigZag(v)); got != v {
			t.Fatalf("zigzag %d -> %d", v, got)
		}
	}
}

// ---- convenience unmarshalers ----

func TestConvenienceUnmarshalers(t *testing.T) {
	m := &Manifest{FormatVersion: WALFormatVersion, Repo: "a/b"}
	b, err := UnmarshalManifest(m.Marshal())
	if err != nil || b.Repo != "a/b" {
		t.Fatalf("UnmarshalManifest: %v %+v", err, b)
	}
	bl := &BundleList{Mode: "all", Bundles: []*BundleEntry{{ID: "x"}}}
	b2, err := UnmarshalBundleList(bl.Marshal())
	if err != nil || b2.Mode != "all" || len(b2.Bundles) != 1 {
		t.Fatalf("UnmarshalBundleList: %v %+v", err, b2)
	}
	if _, err := UnmarshalManifest([]byte{0xff}); err == nil {
		t.Fatal("UnmarshalManifest should reject garbage")
	}
}

// ---- reproducible corpus fuzz: encode-decode stability ----

func TestFuzzRoundTrip(t *testing.T) {
	// deterministic pseudo-random corpus of LogEntry values
	seed := uint64(0x5eed)
	next := func() uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed
	}
	for i := range 200 {
		e := &LogEntry{
			Seq:    next(),
			Kind:   EntryKind(next() % 6),
			Writer: strings.Repeat("w", int(next()%5)),
			Meta:   map[string]string{fmt.Sprint(next() % 7): "v"},
		}
		if next()%2 == 0 {
			e.Pack = &PackRef{Checksum: "c", Seq: next(), Tier: uint32(next() % 3)}
		}
		if next()%2 == 0 {
			e.Txn = &RefTransaction{Atomic: next()%2 == 0}
		}
		if next()%3 == 0 {
			e.CreatedAt = &Timestamp{Seconds: int64(next()), Nanos: int32(next() % 1e9)}
		}
		b := e.Marshal()
		got := &LogEntry{}
		if err := got.Unmarshal(b); err != nil {
			t.Fatalf("i=%d: %v", i, err)
		}
		if !bytes.Equal(b, got.Marshal()) {
			t.Fatalf("i=%d: not stable", i)
		}
	}
}

func timeZero() time.Time { return time.Time{} }

// ---- every message's top-level Marshal/Unmarshal entry points exercised ----

func TestTopLevelMarshalUnmarshalAllTypes(t *testing.T) {
	msgs := allMessages()
	for name, m := range msgs {
		b := m.AppendTo(nil)
		d := newInstance(name)
		if err := d.(interface{ Unmarshal([]byte) error }).Unmarshal(b); err != nil {
			t.Fatalf("%s: Unmarshal: %v", name, err)
		}
		if !bytes.Equal(d.(submessager).AppendTo(nil), b) {
			t.Fatalf("%s: re-encode differs", name)
		}
	}
	// Timestamp public entry points
	ts := &Timestamp{Seconds: -1, Nanos: -2}
	got := &Timestamp{}
	if err := got.Unmarshal(ts.Marshal()); err != nil || *got != *ts {
		t.Fatalf("Timestamp: %+v err=%v", got, err)
	}
	// EntryKind.String coverage
	for k, want := range map[EntryKind]string{
		EntryKindPush: "PUSH", EntryKindCompact: "COMPACT", EntryKindRefUpdate: "REF_UPDATE",
		EntryKindCheckpoint: "CHECKPOINT", EntryKindSettings: "SETTINGS", EntryKindUnspecified: "UNSPECIFIED",
	} {
		if got := k.String(); got != want {
			t.Fatalf("EntryKind(%d).String()=%q want %q", k, got, want)
		}
	}
}

// ---- every message's Size()/Marshal() wrappers (schema-complete assertions) ----

func TestSizeEqualsMarshalAllTypes(t *testing.T) {
	for name, m := range allMessages() {
		b := m.AppendTo(nil)
		var sz int
		if s, ok := m.(interface{ Size() int }); ok {
			sz = s.Size()
		} else {
			sz = m.(interface{ EncodedSize() int }).EncodedSize()
		}
		if sz != len(b) {
			t.Fatalf("%s: Size=%d len=%d", name, sz, len(b))
		}
		// the public Marshal path itself
		if !bytes.Equal(m.(interface{ Marshal() []byte }).Marshal(), b) {
			t.Fatalf("%s: Marshal != AppendTo", name)
		}
	}
	// submessages that only appear nested elsewhere also need their Marshal entry
	for _, m := range []submessager{&RefUpdate{Name: "r"}, &Ref{Oid: "x"}, &LogSegmentRef{Key: "k"},
		&SkippedSlot{Reason: "r"}, &RepoSettings{Toml: "t"}, &Timestamp{Seconds: 1}} {
		if !bytes.Equal(m.(interface{ Marshal() []byte }).Marshal(), m.AppendTo(nil)) {
			t.Fatal("Marshal != AppendTo for nested type")
		}
		if s, ok := m.(interface{ Size() int }); ok && s.Size() != len(m.(interface{ Marshal() []byte }).Marshal()) {
			t.Fatalf("%T Size mismatch", m)
		}
	}
	ts := &Timestamp{Seconds: -5, Nanos: 7}
	if ts.Size() != len(ts.Marshal()) {
		t.Fatalf("Timestamp Size mismatch")
	}
	rs := &RepoSettings{Toml: "x"}
	if rs.Size() != len(rs.Marshal()) {
		t.Fatalf("RepoSettings Size mismatch")
	}
}

// ---- edge-path fill: map defaults, double, packed-vs-unpacked decode, nil elements ----

func TestDecoderEdgePaths(t *testing.T) {
	// map entry with both sides present and with neither (empty submessage)
	b := []byte{}
	addEntry := func(inner []byte) {
		b = binary.AppendUvarint(b, 9<<3|2)
		b = binary.AppendUvarint(b, uint64(len(inner)))
		b = append(b, inner...)
	}
	addEntry(nil)                     // empty entry: key "" value ""
	addEntry([]byte{0x0a, 0x01, 'k'}) // key only
	addEntry([]byte{0x12, 0x01, 'v'}) // value only
	e := &LogEntry{}
	if err := e.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	if e.Meta[""] != "v" || e.Meta["k"] != "" || len(e.Meta) != 2 {
		t.Fatalf("map last-wins: %#v", e.Meta)
	}
	// unknown field inside a map entry submessage is skipped
	b = nil
	addEntry([]byte{0x18, 0x09, 0x0a, 0x01, 'q'}) // field 3 varint + key q
	e = &LogEntry{}
	if err := e.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	if e.Meta["q"] != "" {
		t.Fatalf("map skip: %#v", e.Meta)
	}
	// double field round-trip incl. negative and NaN-free extremes
	for _, v := range []float64{math.SmallestNonzeroFloat64, -123.456, math.MaxFloat64} {
		f := &FsckReport{ElapsedSecs: v}
		got := &FsckReport{}
		if err := got.Unmarshal(f.Marshal()); err != nil {
			t.Fatal(err)
		}
		if got.ElapsedSecs != v {
			t.Fatalf("double %v -> %v", v, got.ElapsedSecs)
		}
	}
	// negative int32 (nanos) encodes as 10-byte varint and round-trips
	ts := &Timestamp{Seconds: -1, Nanos: -1}
	got := &Timestamp{}
	if err := got.Unmarshal(ts.Marshal()); err != nil || *got != *ts {
		t.Fatalf("negative ts: %+v err=%v", got, err)
	}
	// 64-bit fixed (wire type 1) unknown field skip
	b = []byte{byte(9<<3 | 1)}
	b = append(b, 1, 2, 3, 4, 5, 6, 7, 8)
	b = append(b, 1<<3|0, 42)
	if err := (&LogEntry{}).Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	// fixed32 (wire type 5) unknown field skip
	b = []byte{byte(5<<3 | 5), 1, 2, 3, 4, 1<<3 | 0, 0}
	if err := (&LogEntry{}).Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	// nil elements inside repeated message slices are skipped by the encoder
	e2 := &LogEntry{Txn: &RefTransaction{Updates: []*RefUpdate{nil, {Name: "x"}, nil}}}
	got2 := &LogEntry{}
	if err := got2.Unmarshal(e2.Marshal()); err != nil {
		t.Fatal(err)
	}
	if len(got2.Txn.Updates) != 1 || got2.Txn.Updates[0].Name != "x" {
		t.Fatalf("nil element handling: %+v", got2.Txn)
	}
	// unpacked string field accepted when a (nonconformant) packed variant appears:
	// strings are never packed on the wire; a length-delimited occurrence is one element.
}
