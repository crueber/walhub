// rebuild.go — unit 3b (§6.2): the resumable base-rebuild phase machine.
// Scratch copy → repack → history pack → commit-graph, with a JSON phase
// marker and the started_head_seq resume rule; publish is idempotent
// (create-if-absent uploads + CAS COMPACT entry), and a kill between ANY two
// phases resumes with exactly one full git repack across all attempts.
package maintain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// Marker phases (§6.2 step 3) — the durable evidence is the pack files
// themselves; the marker only names the next step.
const (
	phaseCopied      = "copied"
	phaseRepacked    = "repacked"
	phaseHistory     = "history_pack"
	phaseCommitGraph = "commit_graph"
)

// rebuildMarker is the §6.2 step 2 phase marker, written after EACH phase
// completes (atomic write-temp + rename).
type rebuildMarker struct {
	StartedHeadSeq uint64   `json:"started_head_seq"`
	Phase          string   `json:"phase"` // copied → repacked → history_pack → commit_graph
	NewPacks       []string `json:"new_packs,omitempty"`
	History        string   `json:"history,omitempty"`
	CommitGraph    string   `json:"commit_graph,omitempty"`
}

// rebuildScratch returns the §6.2 scratch copy dir and its marker path:
// <cache.dir>/_rebuild/<owner>/<repo>.git and <owner>/<repo>.json.
func rebuildScratch(cacheDir string, id git.RepoId) (string, string) {
	base := filepath.Join(cacheDir, "_rebuild", id.Owner)
	return filepath.Join(base, id.Name+".git"), filepath.Join(base, id.Name+".json")
}

// runRebuild executes the base rebuild phase machine. The caller (bundles
// unit) has already established the trigger + ssd gating; the compact lease
// guards base rebuild and geometric fold alike (§6.2 Concurrency c).
func (m *Maintainer) runRebuild(ctx context.Context, rep Repo, snap *Snapshot, t TaskLogger) (Outcome, string) {
	eff := snap.Eff
	id, err := git.ParseRepoId(rep.ID())
	if err != nil {
		return OutcomeError, err.Error()
	}
	cacheDir := eff.Cache.Dir
	if cacheDir == "" {
		return OutcomeError, "cache.dir empty; rebuild pre-flight impossible"
	}
	release, err := m.leaser().Acquire(ctx, "compact", m.host(),
		"base rebuild: "+rep.ID(), time.Duration(eff.Compaction.LeaseTTL), leaseSkew)
	if err != nil {
		if err == ErrLeaseHeld {
			return OutcomeHeld, "compact lease held by another instance"
		}
		return OutcomeError, "lease: " + err.Error()
	}
	defer release()

	// Pre-flight (§6.2): disk free under cache.dir must exceed the live pack
	// set; otherwise report blocked and try the next pass.
	free, err := m.freeSpace(cacheDir)
	if err != nil {
		return OutcomeError, "statfs: " + err.Error()
	}
	if free < packSetBytes(snap.Manifest) {
		return OutcomeHeld, fmt.Sprintf("state=blocked free=%d pack_set=%d", free, packSetBytes(snap.Manifest))
	}

	mst, _ := rep.Manifest()
	scratch, markerPath := rebuildScratch(cacheDir, id)

	// Resume rule (§6.2 step 4): continue from the recorded phase IFF
	// manifest.head_seq == started_head_seq AND the scratch has an objects
	// dir. Otherwise (a push landed — the repacked pack would be missing the
	// new objects) delete the scratch and the marker, start over. Head-seq
	// equality, not "marker exists": the scratch is a snapshot of one
	// manifest generation.
	marker, resumable := readRebuildMarker(markerPath)
	if resumable && marker.StartedHeadSeq == mst.HeadSeq && hasObjectsDir(scratch) {
		t.Notice(fmt.Sprintf("resuming rebuild at phase %s (head %d)", marker.Phase, marker.StartedHeadSeq))
	} else {
		if resumable {
			t.Notice("head moved since rebuild start; scratch invalidated")
		}
		_ = os.RemoveAll(scratch)
		_ = os.Remove(markerPath)
		if err := copyDir(scratch, rep.Dir()); err != nil {
			return OutcomeError, "scratch copy: " + err.Error()
		}
		marker = &rebuildMarker{StartedHeadSeq: mst.HeadSeq, Phase: phaseCopied}
		if err := writeRebuildMarker(markerPath, marker); err != nil {
			return OutcomeError, "marker write: " + err.Error()
		}
	}
	phaseHook := m.opt.RebuildPhaseHook // kill-between-phases injection (tests)

	// Phase: repacked (§6.2 step 3) — stray *.keep markers deleted, then
	// git repack -a -d --threads=0 --write-bitmap-index --write-midx
	// [--keep-pack …]. Record new pack checksums (pack-dir diff).
	if marker.Phase == phaseCopied {
		diff, err := rep.GitOps().FullRepack(ctx, scratchRepo(cacheDir, id, scratch), rebuildKeepPacks(mst))
		if err != nil {
			return OutcomeError, "full repack: " + err.Error()
		}
		marker.NewPacks = idxChecksums(diff.New)
		marker.Phase = phaseRepacked
		if err := writeRebuildMarker(markerPath, marker); err != nil {
			return OutcomeError, "marker write: " + err.Error()
		}
		if err := runPhaseHook(phaseHook, phaseRepacked); err != nil {
			return OutcomeError, "hook: " + err.Error()
		}
	}

	// Phase: history_pack (§6.2 step 3) — when git.history_pack (default
	// true): the blobless history pack over all ref oids; "" = no refs.
	if marker.Phase == phaseRepacked && eff.Git.HistoryPack {
		hist, err := rep.GitOps().HistoryPack(ctx, scratchRepo(cacheDir, id, scratch), "")
		if err != nil {
			return OutcomeError, "history pack: " + err.Error()
		}
		marker.History = checksumFromPackPath(hist)
		marker.Phase = phaseHistory
		if err := writeRebuildMarker(markerPath, marker); err != nil {
			return OutcomeError, "marker write: " + err.Error()
		}
		if err := runPhaseHook(phaseHook, phaseHistory); err != nil {
			return OutcomeError, "hook: " + err.Error()
		}
	}

	// Phase: commit_graph (§6.2 step 3) — the trailing chain layer is copied
	// out as wal/<checksum>.commit-graph by the git helper.
	if marker.Phase == phaseHistory {
		cg, err := rep.GitOps().WriteCommitGraph(ctx, scratchRepo(cacheDir, id, scratch),
			eff.Git.CommitGraphChangedPath, cacheDir)
		if err != nil {
			return OutcomeError, "commit-graph: " + err.Error()
		}
		marker.CommitGraph = cg
		marker.Phase = phaseCommitGraph
		if err := writeRebuildMarker(markerPath, marker); err != nil {
			return OutcomeError, "marker write: " + err.Error()
		}
		if err := runPhaseHook(phaseHook, phaseCommitGraph); err != nil {
			return OutcomeError, "hook: " + err.Error()
		}
	}

	// Publish (§6.2 step 5, idempotent).
	outcome, detail := m.publishRebuild(ctx, rep, snap, scratch, marker, t)
	if outcome == OutcomeOK {
		_ = os.Remove(markerPath) // consumed; the next trigger starts fresh
	}
	return outcome, detail
}

func runPhaseHook(hook func(phase string) error, phase string) error {
	if hook == nil {
		return nil
	}
	return hook(phase)
}

// publishRebuild (§6.2 step 5): upload pack+idx+rev+bitmap+commit-graph
// create-if-absent (duplicate creates are success; a pack already live under
// the same checksum is skipped), then PublishCompact superseding the pack set
// as it existed at rebuild start (superseding already-superseded packs is
// harmless — the manifest CAS removes what it finds). Only at publish are the
// new files linked into the serving copy. A crash mid-publish is safe:
// create-if-absent + CAS = retriable exactly.
func (m *Maintainer) publishRebuild(ctx context.Context, rep Repo, snap *Snapshot, scratch string, marker *rebuildMarker, t TaskLogger) (Outcome, string) {
	mst, _ := rep.Manifest()
	scratchPackDir := scratchRepo(snap.Eff.Cache.Dir, mustID(rep), scratch).PackDir()
	servingPackDir := rep.Local().PackDir()

	// Supersedes: the pack set as it existed at rebuild start, minus the new
	// packs — equivalent under the resume rule (the scratch is a snapshot of
	// one manifest generation).
	newSet := map[string]bool{}
	for _, c := range marker.NewPacks {
		newSet[c] = true
	}
	if marker.History != "" {
		newSet[marker.History] = true
	}
	supersedes := make([]string, 0, len(mst.Packs))
	for _, p := range mst.Packs {
		if p != nil && !newSet[p.Checksum] {
			supersedes = append(supersedes, p.Checksum)
		}
	}

	main := ""
	for _, c := range marker.NewPacks {
		if main == "" {
			main = c // the full-repack output is the base (tier 2)
		}
		if err := m.uploadPackFiles(ctx, rep.Prefix(), scratchPackDir, c, servingPackDir); err != nil {
			return OutcomeError, "upload: " + err.Error()
		}
	}
	if main == "" {
		return OutcomeError, "rebuild produced no packs"
	}
	if marker.CommitGraph != "" {
		data, ok, err := readPackFile(scratchPackDir, marker.CommitGraph, ".commit-graph")
		if err != nil {
			return OutcomeError, "read commit-graph: " + err.Error()
		}
		if ok {
			if err := putCreateIfAbsent(ctx, m.store(), rep.Prefix()+store.CommitGraphKey(marker.CommitGraph), data); err != nil {
				return OutcomeError, "upload commit-graph: " + err.Error()
			}
		}
	}

	// The COMPACT entry: tier-2 base pack superseding the old set. A push
	// landing DURING the rebuild does not re-trigger it (its seqs are above
	// bar) and is NOT in supersedes.
	if _, err := rep.PublishCompact(ctx, &PreparedPack{
		Checksum: main,
		PackPath: filepath.Join(servingPackDir, "pack-"+main+".pack"),
		IdxPath:  filepath.Join(servingPackDir, "pack-"+main+".idx"),
		Tier:     2,
	}, supersedes, map[string]string{"agent": "walgit maintenance base-rebuild"}); err != nil {
		return OutcomeError, "publish base: " + err.Error()
	}

	// Annotate the new base: bitmap from the repack, commit-graph side-file,
	// .rev produced by index-pack during the repack (manifest-only CAS).
	if err := rep.AnnotatePack(ctx, main, true, true, marker.CommitGraph != ""); err != nil {
		return OutcomeError, "annotate base: " + err.Error()
	}

	// History pack (D18): files uploaded + installed above; the HISTORY-kind
	// manifest entry needs the publisher to carry Kind/DerivedFrom.
	// TODO-INTEGRATION: wal.PublishCompact carries a single PackRef without
	// Kind/DerivedFrom; wire the HISTORY entry when the publisher grows the
	// fields (files are durable; a later rebuild re-uploads idempotently).
	if marker.History != "" {
		if err := m.uploadPackFiles(ctx, rep.Prefix(), scratchPackDir, marker.History, servingPackDir); err != nil {
			return OutcomeError, "upload history: " + err.Error()
		}
		t.Notice("history pack staged (manifest HISTORY entry pending wal publisher field)")
	}
	t.Notice(fmt.Sprintf("base rebuilt (head at copy %d, %d packs superseded)", marker.StartedHeadSeq, len(supersedes)))
	return OutcomeOK, fmt.Sprintf("phase=commit_graph base=%s superseded=%d", main, len(supersedes))
}

// uploadPackFiles uploads the side files of one checksum create-if-absent
// (wal/<checksum>.{pack,idx,rev,bitmap}) and installs them into the serving
// copy's pack dir (§6.2 step 5: "Only at publish are the new files linked
// into the serving copy").
func (m *Maintainer) uploadPackFiles(ctx context.Context, prefix, scratchPackDir, checksum, servingPackDir string) error {
	st := m.store()
	for _, spec := range []struct {
		ext string
		key func(string) string
	}{
		{".pack", store.PackKey},
		{".idx", store.IdxKey},
		{".rev", store.RevKey},
		{".bitmap", store.BitmapKey},
	} {
		data, ok, err := readPackFile(scratchPackDir, checksum, spec.ext)
		if err != nil {
			return fmt.Errorf("%s%s: %w", checksum, spec.ext, err)
		}
		if !ok {
			continue
		}
		if err := putCreateIfAbsent(ctx, st, prefix+spec.key(checksum), data); err != nil {
			return fmt.Errorf("%s%s: %w", checksum, spec.ext, err)
		}
		if err := installSideFile(servingPackDir, filepath.Join(scratchPackDir, "pack-"+checksum+spec.ext)); err != nil {
			return fmt.Errorf("install %s%s: %w", checksum, spec.ext, err)
		}
	}
	return nil
}

// putCreateIfAbsent uploads with PutCreate; a duplicate create is success
// (§6.2 step 5: "duplicate creates are success; a pack already live under the
// same checksum is skipped").
func putCreateIfAbsent(ctx context.Context, st store.ObjectStore, key string, data []byte) error {
	if st == nil {
		return nil
	}
	_, err := st.Put(ctx, key, store.PutBody{Bytes: data},
		store.PutOptions{Mode: store.PutCreate, ContentType: "application/octet-stream", Immutable: true})
	if err != nil && store.IsPreconditionFailed(err) {
		return nil
	}
	return err
}

// ---- marker helpers -------------------------------------------------------------

func readRebuildMarker(path string) (*rebuildMarker, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	m := &rebuildMarker{}
	if json.Unmarshal(data, m) != nil {
		return nil, false
	}
	return m, true
}

func writeRebuildMarker(path string, m *rebuildMarker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

func rebuildKeepPacks(m *proto.Manifest) []string {
	var keep []string
	for _, p := range m.Packs {
		if p != nil && (p.Tier == 2 || p.Kind == proto.PackKindHistory) {
			keep = append(keep, "pack-"+p.Checksum+".pack")
		}
	}
	return keep
}

func idxChecksums(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, checksumFromPackPath(n))
	}
	return out
}

func scratchRepo(cacheDir string, id git.RepoId, path string) *git.LocalRepo {
	return &git.LocalRepo{Root: cacheDir, ID: id, Path: path}
}

func mustID(rep Repo) git.RepoId {
	id, _ := git.ParseRepoId(rep.ID())
	return id
}
