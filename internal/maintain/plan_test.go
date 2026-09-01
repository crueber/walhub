package maintain

import (
	"fmt"
	"testing"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// TestSelect_ExactPriority walks the EXACT §4 order: for repos assembled in
// each state, the first triggered unit must match the table row — and after
// that unit is skipped, the next one.
func TestSelect_ExactPriority(t *testing.T) {
	now := time.Now()
	eff := defaultEff()

	base := func() *proto.Manifest {
		return &proto.Manifest{Repo: "acme/widget", HeadSeq: 100, MinSeq: 1}
	}

	tests := []struct {
		name     string
		mut      func(m *proto.Manifest, eff *config.Config, fsck **proto.FsckReport, present *[]string)
		wantKind string
	}{
		{
			name: "checkpoint-entries",
			mut: func(m *proto.Manifest, eff *config.Config, fsck **proto.FsckReport, present *[]string) {
				m.HeadSeq = 1356 // ≥ 256 entries since cp 0
			},
			wantKind: KindCheckpoint,
		},
		{
			name: "checkpoint-tail-bytes",
			mut: func(m *proto.Manifest, eff *config.Config, fsck **proto.FsckReport, present *[]string) {
				m.LogSegments = []*proto.LogSegmentRef{{Key: "log/1.pb", LastSeq: 5, Size: 9 << 20}} // 9 MiB > 8
			},
			wantKind: KindCheckpoint,
		},
		{
			name: "checkpoint-age",
			mut: func(m *proto.Manifest, eff *config.Config, fsck **proto.FsckReport, present *[]string) {
				old := now.Add(-2 * time.Hour)
				m.UpdatedAt = tsPtr(old)
			},
			wantKind: KindCheckpoint,
		},
		{
			name: "repair-after-checkpoint-satisfied",
			mut: func(m *proto.Manifest, eff *config.Config, fsck **proto.FsckReport, present *[]string) {
				m.Checkpoint = &proto.CheckpointRef{Seq: 100, CreatedAt: tsPtr(now)} // fresh cp
				eff.Upstream.Git = "https://upstream.example/acme/widget.git"
				r := &proto.FsckReport{Missing: []string{"abc"}, RepairedSeq: 0}
				*fsck = r
			},
			wantKind: KindRepair,
		},
		{
			name: "repair-disarmed-by-repaired_seq",
			mut: func(m *proto.Manifest, eff *config.Config, fsck **proto.FsckReport, present *[]string) {
				m.Checkpoint = &proto.CheckpointRef{Seq: 100, CreatedAt: tsPtr(now)}
				*fsck = &proto.FsckReport{Missing: []string{"abc"}, RepairedSeq: 99}
			},
			wantKind: KindBundle, // falls through to bundles
		},
		{
			name: "bundles-when-strategies",
			mut: func(m *proto.Manifest, eff *config.Config, fsck **proto.FsckReport, present *[]string) {
				m.Checkpoint = &proto.CheckpointRef{Seq: 100, CreatedAt: tsPtr(now)}
			},
			wantKind: KindBundle,
		},
		{
			name: "compaction-count",
			mut: func(m *proto.Manifest, eff *config.Config, fsck **proto.FsckReport, present *[]string) {
				m.Checkpoint = &proto.CheckpointRef{Seq: 100, CreatedAt: tsPtr(now)}
				eff.Bundles.Strategy = nil
				for i := range 16 {
					m.Packs = append(m.Packs, pack(fmt.Sprintf("p%d", i), uint64(i), 1000, 10, 0))
				}
			},
			wantKind: KindCompact,
		},
		{
			name: "compaction-single-pack-never-folds",
			mut: func(m *proto.Manifest, eff *config.Config, fsck **proto.FsckReport, present *[]string) {
				m.Checkpoint = &proto.CheckpointRef{Seq: 100, CreatedAt: tsPtr(now)}
				eff.Bundles.Strategy = nil
				m.Packs = append(m.Packs, pack("only", 1, 2<<30, 10, 0)) // huge but alone
				*present = []string{"only"}
			},
			wantKind: KindFsck, // falls to fsck (due: never audited)
		},
		{
			name: "compaction-bytes",
			mut: func(m *proto.Manifest, eff *config.Config, fsck **proto.FsckReport, present *[]string) {
				m.Checkpoint = &proto.CheckpointRef{Seq: 100, CreatedAt: tsPtr(now)}
				eff.Bundles.Strategy = nil
				for i := range 2 {
					m.Packs = append(m.Packs, pack(fmt.Sprintf("big%d", i), uint64(i), 1<<30+1, 10, 0))
				}
			},
			wantKind: KindCompact,
		},
		{
			name: "rev-index",
			mut: func(m *proto.Manifest, eff *config.Config, fsck **proto.FsckReport, present *[]string) {
				m.Checkpoint = &proto.CheckpointRef{Seq: 100, CreatedAt: tsPtr(now)}
				eff.Bundles.Strategy = nil
				m.Packs = append(m.Packs, pack("rev", 1, 1000, 300_000, 0)) // ≥ 250k, no rev
			},
			wantKind: KindRevIndex,
		},
		{
			name: "fsck-due-never-audited",
			mut: func(m *proto.Manifest, eff *config.Config, fsck **proto.FsckReport, present *[]string) {
				m.Checkpoint = &proto.CheckpointRef{Seq: 100, CreatedAt: tsPtr(now)}
				eff.Bundles.Strategy = nil
				m.Packs = append(m.Packs, pack("ok", 1, 10, 10, 0))
				*present = []string{"ok"}
			},
			wantKind: KindFsck,
		},
		{
			name: "idle",
			mut: func(m *proto.Manifest, eff *config.Config, fsck **proto.FsckReport, present *[]string) {
				m.Checkpoint = &proto.CheckpointRef{Seq: 100, CreatedAt: tsPtr(now)}
				eff.Bundles.Strategy = nil
				eff.Maintenance.FsckInterval = 0 // off
				m.Packs = append(m.Packs, pack("ok", 1, 10, 10, 0))
				*present = []string{"ok"}
			},
			wantKind: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := base()
			var fsck *proto.FsckReport
			present := []string{}
			tt.mut(m, eff, &fsck, &present)
			snap := snapshotFor(eff, m, fsck, present)
			got := snap.Select(now)
			if got.Kind != tt.wantKind {
				t.Fatalf("Select = %q (%s), want %q", got.Kind, got.Reason, tt.wantKind)
			}
		})
	}
}

// TestSelect_RevIndexTieBreak: oldest seq first among qualifying packs (§4).
func TestSelect_RevIndexTieBreak(t *testing.T) {
	eff := defaultEff()
	eff.Bundles.Strategy = nil
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Compaction.Enabled = false
	m := &proto.Manifest{
		Packs: []*proto.PackRef{
			{Checksum: "new", Seq: 9, ObjectCount: 400_000},
			{Checksum: "old", Seq: 3, ObjectCount: 250_000},
			{Checksum: "small", Seq: 1, ObjectCount: 249_999}, // below threshold
			{Checksum: "done", Seq: 2, ObjectCount: 900_000, HasRev: true},
		},
	}
	got := snapshotFor(eff, m, nil, nil).Select(time.Now())
	if got.Kind != KindRevIndex || wantSubstring(got.Reason, "old") == false {
		t.Fatalf("Select = %q (%s), want rev-index on pack 'old'", got.Kind, got.Reason)
	}
}

// TestSelect_WrongHostGating: units 3–6 are wrong-host when the pack set does
// not fit; checkpoint and repair always run (§4.1).
func TestSelect_WrongHostGating(t *testing.T) {
	now := time.Now()
	eff := defaultEff()
	eff.Maintenance.Checkpoints = false
	eff.Maintenance.FsckInterval = 0
	eff.Bundles.Strategy = nil
	eff.Compaction.Enabled = true
	eff.Maintenance.MaxPackBytes = 1 << 20 // 1 MiB capacity
	eff.Compaction.TriggerPacks = 2
	m := &proto.Manifest{
		Packs: []*proto.PackRef{
			{Checksum: "p1", Seq: 1, PackSize: 2 << 20, ObjectCount: 10, Tier: 0},
			{Checksum: "p2", Seq: 2, PackSize: 2 << 20, ObjectCount: 10, Tier: 0},
		},
	}
	// Compaction triggers but must be gated in the pass (wrong-host), so
	// Select still names it and the pass loop marks wrong-host.
	snap := snapshotFor(eff, m, nil, nil)
	got := snap.Select(now)
	if got.Kind != KindCompact {
		t.Fatalf("Select = %q, want compact (gating happens in the pass)", got.Kind)
	}
	if !wrongHostUnit(got.Kind) {
		t.Fatal("compact must be a wrong-host-capable unit")
	}
	if wrongHostUnit(KindCheckpoint) || wrongHostUnit(KindRepair) {
		t.Fatal("checkpoint and repair must always run (§4.1)")
	}
}

func tsPtr(t time.Time) *proto.Timestamp {
	ts := proto.TimeFromGo(t)
	return &ts
}

func wantSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestCheckpointTrigger_Math: 0 disables a given trigger (§4 row 1).
func TestCheckpointTrigger_Math(t *testing.T) {
	now := time.Now()
	eff := defaultEff()
	m := &proto.Manifest{HeadSeq: 10, Checkpoint: &proto.CheckpointRef{Seq: 10, CreatedAt: tsPtr(now)}}

	if _, ok := checkpointTrigger(eff, m, now); ok {
		t.Fatal("fresh checkpoint must not trigger")
	}
	// entries: head - cp >= 256
	m.HeadSeq = 266
	if got, ok := checkpointTrigger(eff, m, now); !ok || got != "entries" {
		t.Fatalf("trigger = %q ok=%v, want entries", got, ok)
	}
	// entries disabled → tail-bytes still fires
	eff.WAL.SnapshotEveryEntries = 0
	m.HeadSeq = 10
	m.LogSegments = []*proto.LogSegmentRef{{LastSeq: 11, Size: 8 << 20}} // exactly 8 MiB: not >
	if _, ok := checkpointTrigger(eff, m, now); ok {
		t.Fatal("8 MiB tail is not > 8 MiB")
	}
	m.LogSegments[0].Size = (8 << 20) + 1
	if got, ok := checkpointTrigger(eff, m, now); !ok || got != "tail-bytes" {
		t.Fatalf("trigger = %q ok=%v, want tail-bytes", got, ok)
	}
	eff.WAL.CheckpointTailBytes = 0 // disable
	if _, ok := checkpointTrigger(eff, m, now); ok {
		t.Fatal("all triggers disabled must stay quiet")
	}
	// age from created_at; manifest.updated_at when no checkpoint
	eff.WAL.CheckpointInterval = config.Duration(60 * time.Second)
	if _, ok := checkpointTrigger(eff, m, now); ok {
		t.Fatal("fresh checkpoint age must not trigger")
	}
	m.Checkpoint = nil
	if _, ok := checkpointTrigger(eff, m, now); ok {
		t.Fatal("no age base at all must not trigger")
	}
	m.UpdatedAt = tsPtr(now.Add(-2 * time.Minute))
	if got, ok := checkpointTrigger(eff, m, now); !ok || got != "age" {
		t.Fatalf("trigger = %q ok=%v, want age", got, ok)
	}
}

// TestBaseRebuildDue_BarMath: the §6.2 triggers incl. base_seq ≤ max(bar,1).
func TestBaseRebuildDue_BarMath(t *testing.T) {
	window := time.Date(2026, 8, 30, 23, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		packs []*proto.PackRef
		bar   uint64
		want  bool
	}{
		{"no-base-but-packs", []*proto.PackRef{pack("p", 1, 1, 1, 0)}, 100, true},
		{"no-base-no-packs", nil, 100, false},
		{"two-bases", []*proto.PackRef{pack("b1", 1, 1, 1, 2), pack("b2", 2, 1, 1, 2)}, 0, true},
		{"base-lacks-bitmap", []*proto.PackRef{&proto.PackRef{Checksum: "b", Seq: 9, Tier: 2, HasBitmap: false}}, 0, true},
		{"base-after-window", []*proto.PackRef{&proto.PackRef{Checksum: "b", Seq: 50, Tier: 2, HasBitmap: true}}, 49, false},
		{"base-at-window", []*proto.PackRef{&proto.PackRef{Checksum: "b", Seq: 49, Tier: 2, HasBitmap: true}}, 49, true},
		{"empty-repo-bar-0", []*proto.PackRef{&proto.PackRef{Checksum: "b", Seq: 1, Tier: 2, HasBitmap: true}}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &proto.Manifest{Packs: tt.packs}
			got := baseRebuildDue(m, window, func(time.Time) uint64 { return tt.bar })
			if got != tt.want {
				t.Fatalf("baseRebuildDue = %v, want %v", got, tt.want)
			}
			if tt.bar == 0 && tt.want {
				if baseBar(0) != 1 {
					t.Fatal("max(bar,1) must lift bar 0 to 1")
				}
			}
		})
	}
}

// TestPlacementGlobs: "owner/name" exact, "owner/*", "*", maintain_exclude.
func TestPlacementGlobs(t *testing.T) {
	tests := []struct {
		pattern, id string
		want        bool
	}{
		{"*", "acme/widget", true},
		{"acme/widget", "acme/widget", true},
		{"acme/widget", "acme/other", false},
		{"acme/*", "acme/widget", true},
		{"acme/*", "acme/deep/widget", false}, // one segment only
		{"acme/*", "other/widget", false},
	}
	for _, tt := range tests {
		if got := matchGlob(tt.pattern, tt.id); got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.id, got, tt.want)
		}
	}
	if !assigned("acme/widget", []string{"*"}, nil) {
		t.Error("default maintain=[*] must assign")
	}
	if assigned("archive/x", []string{"*"}, []string{"archive/*"}) {
		t.Error("maintain_exclude must veto")
	}
	if assigned("other/x", []string{"acme/*"}, nil) {
		t.Error("non-matching include must not assign")
	}
}

// TestFits: capacity math (§4.1): max_pack_bytes > 0 wins; budget mode uses
// cache.max_bytes; disk mode unlimited.
func TestFits(t *testing.T) {
	eff := defaultEff()
	m := &proto.Manifest{Packs: []*proto.PackRef{pack("p", 1, 100, 1, 0)}}

	eff.Cache.Mode = "auto"
	eff.Maintenance.MaxPackBytes = 50
	if fits(eff, m) {
		t.Fatal("100 bytes over a 50-byte cap must not fit")
	}
	eff.Maintenance.MaxPackBytes = 100
	if !fits(eff, m) {
		t.Fatal("exactly at capacity fits (≤)")
	}
	eff.Maintenance.MaxPackBytes = 0
	eff.Cache.Mode = "disk"
	if !fits(eff, m) {
		t.Fatal("disk mode is unlimited")
	}
	eff.Cache.Mode = "budget"
	eff.Cache.MaxBytes = 50
	if fits(eff, m) {
		t.Fatal("budget mode must honor cache.max_bytes")
	}
	eff.Cache.MaxBytes = 100
	if !fits(eff, m) {
		t.Fatal("budget mode at capacity fits")
	}
}
