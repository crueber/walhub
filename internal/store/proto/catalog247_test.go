// catalog247_test.go — Forgejo #247 activity fields on RepoCatalogEntry
// (walgit.v1 fields 6-8, append-only on the #248 row family).
//
// The catalog_activity golden pair (hand-encoded wire bytes + expectation)
// pins the field numbers; the tests below pin round-trip fidelity and the
// legacy-compat rule (pre-#247 rows without 6-8 decode to empty activity,
// never to an error or a fake epoch).
package proto

import (
	"bytes"
	"testing"
)

func TestRepoCatalogEntryActivityRoundTrip(t *testing.T) {
	in := &RepoCatalogEntry{
		Repo: "alice/widget", SizeBytes: 3296, ObjectCount: 12, HeadSeq: 66,
		UpdatedAt:      &Timestamp{Seconds: 1700000000, Nanos: 500000000},
		LastCommitSHA:  "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4",
		LastCommitTime: &Timestamp{Seconds: 1700000100},
		LastPushAt:     &Timestamp{Seconds: 1700000200},
	}
	b := in.Marshal()
	if len(b) == 0 {
		t.Fatal("empty encoding")
	}
	if in.Size() != len(b) {
		t.Fatalf("Size()=%d, want %d", in.Size(), len(b))
	}
	got := &RepoCatalogEntry{}
	if err := got.Unmarshal(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.LastCommitSHA != in.LastCommitSHA {
		t.Fatalf("sha = %q, want %q", got.LastCommitSHA, in.LastCommitSHA)
	}
	if got.LastCommitTime == nil || got.LastCommitTime.Seconds != 1700000100 {
		t.Fatalf("commit time = %+v", got.LastCommitTime)
	}
	if got.LastPushAt == nil || got.LastPushAt.Seconds != 1700000200 {
		t.Fatalf("push time = %+v", got.LastPushAt)
	}
	if !bytes.Equal(b, got.Marshal()) {
		t.Fatal("re-encode differs")
	}
}

func TestRepoCatalogEntryLegacyRowDecodesToEmptyActivity(t *testing.T) {
	// A pre-#247 row (fields 1-5 only): decode must succeed with empty
	// activity — unknown, never a fake epoch.
	legacy := &RepoCatalogEntry{
		Repo: "bob/tool", SizeBytes: 100, ObjectCount: 1, HeadSeq: 1,
	}
	b := legacy.Marshal()
	got := &RepoCatalogEntry{}
	if err := got.Unmarshal(b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.LastCommitSHA != "" || got.LastCommitTime != nil || got.LastPushAt != nil {
		t.Fatalf("legacy row must decode to empty activity: %+v", got)
	}
}

func TestRepoCatalogEntryEmptyActivityEncodesToNothing(t *testing.T) {
	// proto3 zero-value rule: empty activity fields add zero bytes, so a
	// size-only writer emits exactly the pre-#247 encoding (byte-compat).
	bare := &RepoCatalogEntry{Repo: "x/y", SizeBytes: 7}
	full := &RepoCatalogEntry{Repo: "x/y", SizeBytes: 7,
		LastCommitTime: &Timestamp{}, LastPushAt: &Timestamp{}}
	if !bytes.Equal(bare.Marshal(), full.Marshal()) {
		t.Fatalf("empty timestamps must encode to nothing:\n%x\n%x", bare.Marshal(), full.Marshal())
	}
}
