// catalog248_test.go — Forgejo #248 proto compat: field-3 entries are
// append-only; legacy readers/writers (fields 1–2) interoperate.
package proto

import (
	"bytes"
	"testing"
)

func TestRepoCatalogEntriesRoundTrip(t *testing.T) {
	cat := &RepoCatalog{
		Repos:     []string{"a/b", "c/d"},
		UpdatedAt: &Timestamp{Seconds: 1, Nanos: 2},
		Entries: []*RepoCatalogEntry{
			{Repo: "a/b", SizeBytes: 330, ObjectCount: 3, HeadSeq: 7, UpdatedAt: &Timestamp{Seconds: 5}},
			{Repo: "c/d"}, // verified-empty rides zeros, not absence
		},
	}
	b := cat.Marshal()
	if len(b) == 0 {
		t.Fatal("empty encoding")
	}
	if cat.Size() != len(b) {
		t.Fatalf("Size=%d len=%d", cat.Size(), len(b))
	}
	dup := &RepoCatalog{}
	if err := dup.Unmarshal(b); err != nil {
		t.Fatal(err)
	}
	if len(dup.Entries) != 2 || dup.Entries[0].SizeBytes != 330 || dup.Repos[1] != "c/d" {
		t.Fatalf("decoded: %+v", dup)
	}
	if re := dup.Marshal(); !bytes.Equal(b, re) {
		t.Fatal("re-encode not byte-identical")
	}
}

func TestRepoCatalogLegacyCompat(t *testing.T) {
	// Legacy bytes: fields 1+2 only (pre-#248 writer). New reader → nil entries.
	legacy := &RepoCatalog{Repos: []string{"a/b"}, UpdatedAt: &Timestamp{Seconds: 1}}
	raw := legacy.Marshal()
	got := &RepoCatalog{}
	if err := got.Unmarshal(raw); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 0 {
		t.Fatalf("legacy must decode to nil entries: %+v", got.Entries)
	}
	// Unknown future fields (e.g. #247 activity field 6, varint) skip cleanly.
	got2 := &RepoCatalog{}
	future := append(append([]byte{}, raw...), 0x30, 0x01) // field 6, wt 0, value 1
	if err := got2.Unmarshal(future); err != nil {
		t.Fatalf("future fields must skip: %v", err)
	}
	if len(got2.Repos) != 1 || len(got2.Entries) != 0 {
		t.Fatalf("future skip: %+v", got2)
	}
}
