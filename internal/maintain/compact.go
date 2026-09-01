// compact.go — unit 4 (§6.1): the geometric fold under leases/compact.pb,
// published as a tier-1 COMPACT entry, plus the superseded-pack retention GC.
package maintain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// runCompact folds tier-0 packs geometrically. The §6.1 exact argv lives in
// git.Layer.GeometricRepack; the invariant: a fold NEVER touches the base
// (tier 2) or history packs (--keep-pack).
func (m *Maintainer) runCompact(ctx context.Context, rep Repo, snap *Snapshot, t TaskLogger) (Outcome, string) {
	eff := snap.Eff
	// (a) the leases/compact.pb lease is the cross-instance mutex — one fold
	// at a time, CAS ladder, steal only after expires_at + 2s (§6.1).
	release, err := m.leaser().Acquire(ctx, "compact", m.host(),
		"geometric fold: "+rep.ID(), time.Duration(eff.Compaction.LeaseTTL), leaseSkew)
	if err != nil {
		if err == ErrLeaseHeld {
			return OutcomeHeld, "compact lease held by another instance"
		}
		return OutcomeError, "lease: " + err.Error()
	}
	defer release()
	// (b) locally the fold runs under the repo handle's pack_mutex with
	// try-lock semantics: if the publish path holds it, report held and defer
	// to the next pass — readers must never queue behind maintenance.
	unlock, ok := rep.TryLockPacks()
	if !ok {
		return OutcomeHeld, "pack_mutex held by the publish path"
	}
	defer unlock()

	// keep-packs: base (tier 2) and history packs survive every fold; the
	// supersedes list is built from the manifest snapshot taken BEFORE the
	// repack (§6.1 c: packs added after the snapshot stay live; the next
	// trigger folds them).
	var keep, supersedes []string
	for _, p := range snap.Manifest.Packs {
		if p == nil {
			continue
		}
		if p.Tier == 2 || p.Kind == proto.PackKindHistory {
			// The fold NEVER touches the base (tier 2) or history packs —
			// they are kept on the argv and never superseded (§6.1).
			keep = append(keep, "pack-"+p.Checksum+".pack")
			continue
		}
		supersedes = append(supersedes, p.Checksum)
	}

	diff, err := rep.GitOps().GeometricRepack(ctx, rep.Local(), eff.Compaction.Factor, true, keep)
	if err != nil {
		return OutcomeError, "repack: " + err.Error()
	}
	if len(diff.New) == 0 {
		// Nothing folded (the fresh set is already geometric); the trigger
		// re-fires next pass only if the predicate still holds.
		return OutcomeOK, "no fold produced"
	}
	// Publish the new pack(s) as a tier-1 COMPACT entry superseding the
	// folded set (§6.1). PublishCompact uploads + CASes the manifest; on
	// commit the superseded checksums join pending_pack_removals and the
	// publisher's pack sync removes them locally like everyone else's.
	published := 0
	for _, name := range diff.New {
		checksum := checksumFromPackPath(name)
		if _, err := rep.PublishCompact(ctx, &PreparedPack{
			Checksum: checksum,
			PackPath: rep.Local().PackDir() + "/pack-" + checksum + ".pack",
			Tier:     1,
		}, supersedes, map[string]string{"agent": "walgit maintenance compact"}); err != nil {
			return OutcomeError, fmt.Sprintf("publish fold pack %s: %v", checksum, err)
		}
		published++
		supersedes = nil // one fold supersedes the old set exactly once
	}

	// Retention/GC (§6.1): superseded packs are kept
	// compaction.retention_superseded (7 d provenance window); the sweep
	// deletes wal/<checksum>.* older than that AND no longer in any manifest.
	if removed, err := m.gcSuperseded(ctx, rep, snap); err != nil {
		m.logf("%s: retention gc failed: %v", rep.ID(), err)
	} else if removed > 0 {
		m.logf("%s: retention gc removed %d objects", rep.ID(), removed)
	}

	t.Notice(fmt.Sprintf("fold published %d pack(s) superseding %d", published, len(diff.Removed)))
	return OutcomeOK, fmt.Sprintf("%d packs folded → %d new", len(diff.Removed), len(diff.New))
}

// leaser resolves the lease seam (bind_wal injects the store-backed one;
// tests may fake it).
func (m *Maintainer) leaser() Leaser {
	if m.opt.Leaser != nil {
		return m.opt.Leaser
	}
	return StoreLeaser{St: m.store()}
}

// gcSuperseded deletes wal/<checksum>.{pack,idx,rev,bitmap,commit-graph}
// objects that (a) are absent from the current manifest pack set ("no longer
// in any manifest"), and (b) were superseded by a COMPACT entry older than
// compaction.retention_superseded. Candidates with an unknown superseding
// time are kept (conservative — the store contract carries no mtime to fall
// back on; §6.1 retention).
func (m *Maintainer) gcSuperseded(ctx context.Context, rep Repo, snap *Snapshot) (int, error) {
	st := m.store()
	if st == nil {
		return 0, nil
	}
	retention := time.Duration(snap.Eff.Compaction.RetentionSuperseded)
	if retention <= 0 {
		return 0, nil
	}
	live := map[string]bool{}
	for _, p := range snap.Manifest.Packs {
		if p != nil {
			live[p.Checksum] = true
		}
	}
	// supersededAt: checksum → created_at of the COMPACT entry that
	// superseded it (the retained log window is the provenance record).
	supersededAt := map[string]time.Time{}
	entries, err := rep.ReadLog(ctx, snap.Manifest.MinSeq, snap.Manifest.HeadSeq)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if e.Kind != proto.EntryKindCompact || e.CreatedAt == nil {
			continue
		}
		at := e.CreatedAt.Go()
		for _, c := range e.Supersedes {
			if old, ok := supersededAt[c]; !ok || at.Before(old) {
				supersededAt[c] = at
			}
		}
	}
	cutoff := m.now().Add(-retention)
	prefix := rep.Prefix() + store.WalDir
	removed := 0
	err = st.List(ctx, prefix, "", func(meta store.ObjectMeta) error {
		rel := strings.TrimPrefix(meta.Key, prefix)
		checksum, _, ok := strings.Cut(rel, ".")
		if !ok || live[checksum] {
			return nil
		}
		at, ok := supersededAt[checksum]
		if !ok || at.After(cutoff) {
			return nil
		}
		if err := st.Delete(ctx, meta.Key, ""); err != nil {
			return err
		}
		removed++
		return nil
	})
	return removed, err
}
