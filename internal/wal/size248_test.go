// size248_test.go — Forgejo #248 acceptance: size = Σ PackSize+IdxSize over
// the live pack set maintained by buildNextManifest (pure arithmetic, +0
// trips, no I/O). Covers: push→compact supersede removal, annotate-pack
// no-op (manifest-only flag CAS carries no size), empty-repo → 0.
package wal

import (
	"testing"

	"git.packden.us/crueber/walhub/internal/sizecatalog"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

func liveSize(m *proto.Manifest) (uint64, uint64) {
	s, o, _ := sizecatalog.ManifestSize(m)
	return s, o
}

func TestSizePushThenCompact(t *testing.T) {
	base := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "o/r", Revision: 1}
	p1 := &proto.PackRef{Checksum: "a", PackSize: 1000, IdxSize: 100, ObjectCount: 10, Seq: 1}
	p2 := &proto.PackRef{Checksum: "b", PackSize: 2000, IdxSize: 200, ObjectCount: 20, Seq: 2}
	afterPush := buildNextManifest(base,
		[]*proto.LogEntry{
			{Seq: 1, Kind: proto.EntryKindPush, Pack: p1},
			{Seq: 2, Kind: proto.EntryKindPush, Pack: p2},
		}, 1, 1, []byte("seg"), "w")
	if s, o := liveSize(afterPush); s != 3300 || o != 30 {
		t.Fatalf("after pushes: size=%d objs=%d, want 3300/30", s, o)
	}
	// Compaction supersedes "a" with a merged pack: "a" leaves the live set.
	merged := &proto.PackRef{Checksum: "c", PackSize: 2500, IdxSize: 150, ObjectCount: 30, Seq: 3}
	afterCompact := buildNextManifest(afterPush,
		[]*proto.LogEntry{{Seq: 3, Kind: proto.EntryKindCompact, Pack: merged, Supersedes: []string{"a"}}},
		3, 3, []byte("seg"), "w")
	// Live set is {b, c}: (2000+200) + (2500+150) = 4850; objects 20+30=50.
	if s, o := liveSize(afterCompact); s != 4850 || o != 50 {
		t.Fatalf("after compact: size=%d objs=%d, want 4850/50", s, o)
	}
	for _, p := range afterCompact.Packs {
		if p.Checksum == "a" {
			t.Fatal("superseded pack 'a' must leave the live set")
		}
	}
}

func TestSizeEmptyAndAnnotateNoOp(t *testing.T) {
	base := &proto.Manifest{FormatVersion: proto.WALFormatVersion, Repo: "o/r", Revision: 1}
	if s, o := liveSize(base); s != 0 || o != 0 {
		t.Fatalf("empty repo → (%d,%d), want (0,0)", s, o)
	}
	// AnnotatePack touches only flags (.rev/.bitmap/.commit-graph presence):
	// the byte sum is unchanged by construction — assert the helper ignores
	// flag-only copies.
	p := &proto.PackRef{Checksum: "a", PackSize: 100, IdxSize: 10, ObjectCount: 3, Seq: 1}
	flagged := *p
	flagged.HasRev, flagged.HasBitmap, flagged.HasCommitGraph = true, true, true
	m1 := &proto.Manifest{Packs: []*proto.PackRef{p}}
	m2 := &proto.Manifest{Packs: []*proto.PackRef{&flagged}}
	s1, o1 := liveSize(m1)
	s2, o2 := liveSize(m2)
	if s1 != s2 || o1 != o2 {
		t.Fatalf("annotate must not change size: (%d,%d) vs (%d,%d)", s1, o1, s2, o2)
	}
	// AddPack approximation (review S2): PreparedPack carries PackSize only
	// (IdxSize/ObjectCount 0) — the sum is a documented lower bound there.
	approx := &proto.PackRef{Checksum: "x", PackSize: 1000, Seq: 9}
	if s, _ := liveSize(&proto.Manifest{Packs: []*proto.PackRef{approx}}); s != 1000 {
		t.Fatalf("add-pack approx: %d", s)
	}
}
