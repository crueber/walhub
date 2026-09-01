// revindex.go — unit 5 (§10): retrofit the .rev onto a live pack that lacks
// one, threshold ≥ 250 000 objects, oldest seq first, one pack per run.
// Built file-locally from the .idx alone (rev.go — byte-identical to git);
// install: temp file in objects/pack + rename, create-if-absent store put,
// then annotate_pack (manifest-only CAS). If the pack vanished meanwhile
// (superseded by a fold), abandon silently — outcome ok with a notice.
package maintain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

func (m *Maintainer) runRevIndex(ctx context.Context, rep Repo, snap *Snapshot, t TaskLogger) (Outcome, string) {
	p := revIndexCandidate(snap.Manifest)
	if p == nil {
		return OutcomeOK, "no candidate"
	}
	repo := rep.Local()
	idxPath := filepath.Join(repo.PackDir(), "pack-"+p.Checksum+".idx")
	idx, err := os.ReadFile(idxPath)
	if err != nil {
		if os.IsNotExist(err) {
			return OutcomeOK, "pack not local; skipping"
		}
		return OutcomeError, "read idx: " + err.Error()
	}
	oidLen := 20
	if repo.Format().String() == "sha256" {
		oidLen = 32
	}
	rev, err := buildRevFile(idx, oidLen)
	if err != nil {
		return OutcomeError, "build .rev: " + err.Error()
	}

	// Install: write to a temp file in objects/pack, rename to
	// pack-<checksum>.rev (§10: rev-index never touches pack bytes).
	revPath := filepath.Join(repo.PackDir(), "pack-"+p.Checksum+".rev")
	if _, err := os.Stat(revPath); err == nil {
		// Already present (previous run died before annotate); proceed to the
		// upload + annotate — idempotent.
	} else {
		tmp := revPath + ".maintain-tmp"
		if err := os.WriteFile(tmp, rev, 0o644); err != nil {
			return OutcomeError, "write temp .rev: " + err.Error()
		}
		if err := os.Rename(tmp, revPath); err != nil {
			_ = os.Remove(tmp)
			return OutcomeError, "install .rev: " + err.Error()
		}
	}

	// Store put create-if-absent of wal/<checksum>.rev (Create-immutable).
	if st := m.store(); st != nil {
		if err := putCreateIfAbsent(ctx, st, rep.Prefix()+storeRevKey(p.Checksum), rev); err != nil {
			return OutcomeError, "upload .rev: " + err.Error()
		}
	}

	// If the pack vanished meanwhile (superseded by a fold), abandon
	// silently — outcome ok with a notice (§10).
	mst, _ := rep.Manifest()
	live := false
	for _, q := range mst.Packs {
		if q != nil && q.Checksum == p.Checksum {
			live = true
			break
		}
	}
	if !live {
		t.Notice(fmt.Sprintf("pack %s superseded during rev-index; abandoning", p.Checksum))
		return OutcomeOK, "pack superseded during build"
	}

	// annotate_pack: manifest-only CAS — has_rev = true, no log entry,
	// head_seq unchanged.
	if err := rep.AnnotatePack(ctx, p.Checksum, true, p.HasBitmap, p.HasCommitGraph); err != nil {
		return OutcomeError, "annotate: " + err.Error()
	}
	return OutcomeOK, fmt.Sprintf("pack %s (%d objects)", p.Checksum, p.ObjectCount)
}

// storeRevKey aliases store.RevKey so the unit file stays self-documenting.
func storeRevKey(checksum string) string { return "wal/" + checksum + ".rev" }
