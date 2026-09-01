// plan.go — unit selection per repo (doc 10 §3.1/§4): the desired-state
// snapshot, placement globs, wrong-host planning, and the EXACT 7-row
// priority list. There are no weights, no scoring: order IS priority.
package maintain

import (
	"fmt"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// LocalState is the cheap local-disk view of the serving copy (§3.2 step 6:
// selection reads only manifest/checkpoint refs already synced at refs level
// plus local disk state — never bulk store I/O for unselected repos).
type LocalState struct {
	// Present holds the pack checksums linked in the local objects/pack dir.
	Present map[string]bool
}

// Snapshot is the per-repo desired-state view (§4): manifest, effective
// config (host config ⊕ repo settings, evaluated fresh each pass), the cached
// fsck.pb, and local pack-dir state.
type Snapshot struct {
	ID       string
	Manifest *proto.Manifest
	Eff      *config.Config
	Fsck     *proto.FsckReport // nil when never audited
	Local    LocalState
	// Skip holds the kinds already attempted for this repo this pass (done,
	// held, timed out, or wrong-host): §3.2 step 5 "skipped for those units
	// this pass".
	Skip map[string]bool
}

// Selection is one walked row of the §4 priority list.
type Selection struct {
	Kind   string // "" = idle
	Reason string
}

// matchGlob implements the §3.1 placement globs: "owner/name" exact,
// "owner/*", "*".
func matchGlob(pattern, id string) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasSuffix(pattern, "/*"):
		owner := strings.TrimSuffix(pattern, "/*")
		rest, ok := strings.CutPrefix(id, owner+"/")
		return ok && !strings.Contains(rest, "/")
	default:
		return pattern == id
	}
}

// assigned applies any [placement] maintain glob and none of maintain_exclude.
func assigned(repo string, include, exclude []string) bool {
	matched := false
	for _, g := range include {
		if matchGlob(g, repo) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	for _, g := range exclude {
		if matchGlob(g, repo) {
			return false
		}
	}
	return true
}

// packSetBytes sums the live pack bytes (§4.1 fits math).
func packSetBytes(m *proto.Manifest) uint64 {
	total := uint64(0)
	for _, p := range m.Packs {
		total += p.PackSize
	}
	return total
}

// capacity returns the host capacity in bytes (§2): maintenance.max_pack_bytes
// when > 0; budget mode → cache.max_bytes; disk mode → 0 (unlimited).
func capacity(eff *config.Config) int64 {
	if eff.Maintenance.MaxPackBytes > 0 {
		return int64(eff.Maintenance.MaxPackBytes)
	}
	if eff.Cache.Mode == "disk" {
		return 0
	}
	return int64(eff.Cache.MaxBytes)
}

// fits reports whether the live pack set fits this host (§4.1).
func fits(eff *config.Config, m *proto.Manifest) bool {
	cap := capacity(eff)
	return cap == 0 || int64(packSetBytes(m)) <= cap
}

// checkpointTrigger evaluates the §6.5 triggers (any; a 0 value disables a
// given trigger): entries since last cp ≥ wal.snapshot_every_entries; tail
// bytes (segments with last_seq > cp.seq) > wal.checkpoint_tail_bytes; age ≥
// wal.checkpoint_interval measured from checkpoint.created_at, or
// manifest.updated_at when no checkpoint.
func checkpointTrigger(eff *config.Config, m *proto.Manifest, now time.Time) (trigger string, ok bool) {
	cpSeq := uint64(0)
	var cpAt time.Time
	if m.Checkpoint != nil {
		cpSeq = m.Checkpoint.Seq
		if m.Checkpoint.CreatedAt != nil {
			cpAt = m.Checkpoint.CreatedAt.Go()
		}
	}
	if eff.WAL.SnapshotEveryEntries > 0 && m.HeadSeq-cpSeq >= eff.WAL.SnapshotEveryEntries {
		return "entries", true
	}
	if eff.WAL.CheckpointTailBytes > 0 {
		var tail uint64
		for _, s := range m.LogSegments {
			if s.LastSeq > cpSeq {
				tail += s.Size
			}
		}
		if tail > uint64(eff.WAL.CheckpointTailBytes) {
			return "tail-bytes", true
		}
	}
	ageBase := cpAt
	if ageBase.IsZero() && m.UpdatedAt != nil {
		ageBase = m.UpdatedAt.Go()
	}
	if eff.WAL.CheckpointInterval > 0 && !ageBase.IsZero() && now.Sub(ageBase) >= time.Duration(eff.WAL.CheckpointInterval) {
		return "age", true
	}
	return "", false
}

// compactionTrigger evaluates the §4 unit-4 predicate: compaction.enabled AND
// (tier-0 pack count ≥ compaction.trigger_packs OR tier-0 bytes >
// compaction.trigger_bytes) AND ≥ 2 fresh packs (a single pack never folds
// into itself). Returns the fresh-pack count and bytes for the reason line.
func compactionTrigger(eff *config.Config, m *proto.Manifest) (fresh int, bytes uint64, ok bool) {
	if !eff.Compaction.Enabled {
		return 0, 0, false
	}
	for _, p := range m.Packs {
		if p.Tier == 0 {
			fresh++
			bytes += p.PackSize
		}
	}
	if eff.Compaction.TriggerPacks > 0 && fresh >= eff.Compaction.TriggerPacks {
		ok = true
	}
	if eff.Compaction.TriggerBytes > 0 && bytes > uint64(eff.Compaction.TriggerBytes) {
		ok = true
	}
	if !ok || fresh < 2 {
		return 0, 0, false
	}
	return fresh, bytes, true
}

// revIndexCandidate picks the oldest live pack with !has_rev and
// object_count ≥ 250 000 (§10; tie-break oldest seq first). Push packs below
// the threshold intentionally stay rev-less.
func revIndexCandidate(m *proto.Manifest) *proto.PackRef {
	var best *proto.PackRef
	for _, p := range m.Packs {
		if p == nil || p.HasRev || p.ObjectCount < revIndexThreshold {
			continue
		}
		if best == nil || p.Seq < best.Seq {
			best = p
		}
	}
	return best
}

// fsckDue evaluates the §4 unit-6 interval predicate: never audited ∥
// repaired_seq != 0 since last audit ∥ now − report.at ≥ fsck_interval.
// maintenance.fsck_interval ≤ 0 turns the unit off.
func fsckDue(eff *config.Config, rep *proto.FsckReport, now time.Time) bool {
	d := time.Duration(eff.Maintenance.FsckInterval)
	if d <= 0 {
		return false
	}
	if rep == nil || rep.At == nil {
		return true
	}
	if rep.RepairedSeq != 0 {
		return true
	}
	return now.Sub(rep.At.Go()) >= d
}

// fullCopy reports whether the local copy holds the whole live pack set
// (§9.1: the audit runs over a complete local copy only — never over a
// linked/remote base).
func fullCopy(m *proto.Manifest, local LocalState) bool {
	for _, p := range m.Packs {
		if p == nil || !local.Present[p.Checksum] {
			return false
		}
	}
	return true
}

// baseBar is the §6.2 bar math: rebuild iff base_seq ≤ max(bar, 1) — an
// empty-at-slot-time repo (bar 0) is still rebuildable at seq 1.
func baseBar(bar uint64) uint64 {
	if bar == 0 {
		return 1
	}
	return bar
}

// baseRebuildDue evaluates the §6.2 trigger predicates (any):
//   - no base exists (no tier-2 pack) but packs do; OR
//   - more than one tier-2 pack exists; OR
//   - the base lacks a bitmap; OR
//   - the base predates the window: bar = WAL head_seq at windowStart;
//     rebuild iff base_seq ≤ max(bar, 1).
func baseRebuildDue(m *proto.Manifest, windowStart time.Time, headSeqAt func(time.Time) uint64) bool {
	var bases []*proto.PackRef
	for _, p := range m.Packs {
		if p != nil && p.Tier == 2 {
			bases = append(bases, p)
		}
	}
	if len(bases) == 0 {
		return len(m.Packs) > 0
	}
	if len(bases) > 1 {
		return true
	}
	base := bases[0]
	if !base.HasBitmap {
		return true
	}
	return base.Seq <= baseBar(headSeqAt(windowStart))
}

// Select walks the EXACT §4 priority order top-down and returns the first
// unit whose trigger predicate holds. Units already attempted for this repo
// this pass (snapshot.Skip) are passed over.
func (s *Snapshot) Select(now time.Time) Selection {
	if s.Skip == nil {
		s.Skip = map[string]bool{}
	}
	// 1. Checkpoint (§11): always fits-capable — refs-level by design.
	if !s.Skip[KindCheckpoint] && s.Eff.Maintenance.Checkpoints {
		if trig, ok := checkpointTrigger(s.Eff, s.Manifest, now); ok {
			return Selection{KindCheckpoint, trig}
		}
	}
	// 2. Repair (§9.2): due right after checkpoint when fsck.pb lists missing
	// oids, repaired_seq == 0, and upstream.git is configured.
	if !s.Skip[KindRepair] && s.Fsck != nil && s.Fsck.RepairedSeq == 0 &&
		(len(s.Fsck.Missing) > 0 || s.Fsck.MissingTotal > 0) && s.Eff.Upstream.Git != "" {
		return Selection{KindRepair, fmt.Sprintf("%d missing oids", s.Fsck.MissingTotal)}
	}
	// 3. Bundles (§5): planning lives in the unit; the trigger here is
	// "strategies configured". The unit decides build vs BaseRebuild fork.
	if !s.Skip[KindBundle] && len(s.Eff.Bundles.Strategy) > 0 {
		return Selection{KindBundle, "bundle planning"}
	}
	// 4. Compaction (§6.1).
	if !s.Skip[KindCompact] {
		if fresh, bytes, ok := compactionTrigger(s.Eff, s.Manifest); ok {
			return Selection{KindCompact, fmt.Sprintf("%d fresh packs (%d bytes)", fresh, bytes)}
		}
	}
	// 5. Rev-index (§10).
	if !s.Skip[KindRevIndex] {
		if p := revIndexCandidate(s.Manifest); p != nil {
			return Selection{KindRevIndex, fmt.Sprintf("pack %s (%d objects)", shortChecksum(p.Checksum), p.ObjectCount)}
		}
	}
	// 6. Fsck audit (§9.1): runs over a complete local copy only.
	if !s.Skip[KindFsck] && fsckDue(s.Eff, s.Fsck, now) && fullCopy(s.Manifest, s.Local) {
		return Selection{KindFsck, "audit due"}
	}
	return Selection{}
}

// wrongHostUnits are the units that require the object bytes locally (§4.1).
// Checkpoint and repair always run: they must not require the object bytes.
func wrongHostUnit(kind string) bool {
	switch kind {
	case KindBundle, KindCompact, KindRevIndex, KindFsck:
		return true
	}
	return false
}

func shortChecksum(c string) string {
	if len(c) > 8 {
		return c[:8]
	}
	return c
}
