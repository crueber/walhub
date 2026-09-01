// reconcile.go — the pack phase of the read path (doc 05 §5.2 step 3): the
// serve plan tiering, one-round 8-way downloads with striped .pack fetch,
// side-file retrofit, history-pack deferral, superseded removal via the rw
// try-write rule, commit-graph fold, and the SyncObjects remote-reader swap.
package wal

import (
	"context"
	"os"
	"path/filepath"

	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// servePlan is the tiering decision for one manifest (§5.2 level table).
type servePlan struct {
	local     []*proto.PackRef // tier < 2 and HISTORY packs: fully local
	sideFiles []*proto.PackRef // tier-2 base: side-files local, .pack remote-served/mounted
}

// servePlanOf tiers the live pack set. Full overrides: every live pack local.
func servePlanOf(m *pbManifest, lvl SyncLevel) *servePlan {
	p := &servePlan{}
	for _, pack := range m.Packs {
		if lvl >= LevelFull || pack.Tier < 2 || pack.Kind == proto.PackKindHistory {
			p.local = append(p.local, pack)
		} else {
			p.sideFiles = append(p.sideFiles, pack)
		}
	}
	return p
}

// planBytes sums the pack sizes in the plan (checkFits budget test).
func planBytes(packs []*proto.PackRef) int64 {
	var n int64
	for _, p := range packs {
		n += int64(p.PackSize)
	}
	return n
}

// checkFits refuses with ErrTooLarge when the set exceeds the cache budget
// (surfaced as HTTP 503 with the bundle-uri fix text per doc 07).
func (h *RepoHandle) checkFits(m *pbManifest, lvl SyncLevel) error {
	if lvl >= LevelFull {
		if need := planBytes(m.Packs); h.reg.vals.cacheMaxBytes > 0 && need > h.reg.vals.cacheMaxBytes {
			return ErrTooLarge(need, h.reg.vals.cacheMaxBytes)
		}
	}
	return nil
}

// reconcilePacks materializes the pack set for lvl. Caller holds packMu and
// NOT syncMu (refs requests must never queue behind a multi-GB materialization).
func (h *RepoHandle) reconcilePacks(ctx context.Context, lvl SyncLevel) error {
	m := h.manifest
	if m == nil {
		return nil
	}
	if err := h.checkFits(m, lvl); err != nil {
		return err
	}

	// The whole phase is one task; concurrent callers join it (§5.8).
	if lvl >= LevelServe {
		_, err := h.reg.tasks.Run(ctx, h.ID, "materialize", map[string]string{"level": lvl.String()},
			func(tctx context.Context, t *Task) error {
				return h.materialize(tctx, t, m, lvl)
			})
		if err != nil {
			return err
		}
		// Refresh the local repo state the git layer caches.
		if _, err := h.Layer().Snapshot(h.repo); err != nil {
			return &WalError{Kind: WalErrGit, Detail: "post-materialize snapshot", Wrapped: err}
		}
	}
	return nil
}

// materialize downloads and reconciles the pack set for the plan.
func (h *RepoHandle) materialize(ctx context.Context, t *Task, m *pbManifest, lvl SyncLevel) error {
	plan := servePlanOf(m, lvl)
	packDir := h.repo.PackDir()
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		return &WalError{Kind: WalErrIo, Detail: packDir, Wrapped: err}
	}

	// Missing local packs: one round of downloads, 8-way (13 §4).
	type need struct {
		pack *proto.PackRef
		why  string // "pack" | "idx" | "side"
	}
	var missing []need
	present := localPacks(packDir)
	for _, p := range plan.local {
		_, havePack := present[p.Checksum+".pack"]
		_, haveIdx := present[p.Checksum+".idx"]
		if !havePack {
			missing = append(missing, need{p, "pack"})
		}
		if !haveIdx {
			missing = append(missing, need{p, "idx"})
		}
	}
	for _, p := range plan.sideFiles {
		for _, suf := range sideFileSuffixes(p) {
			if _, ok := present[p.Checksum+suf]; !ok {
				missing = append(missing, need{p, suf})
			}
		}
	}
	if t != nil && len(missing) > 0 {
		t.Progress("packs", 0, uint64(len(missing)), "files")
	}
	if len(missing) > 0 {
		g, gctx := store.WithContext(ctx)
		g.SetLimit(8)
		for i, n := range missing {
			i, n := i, n
			g.Go(func() error {
				err := h.fetchPackFile(gctx, n.pack, n.why)
				if err == nil && t != nil {
					t.Progress("packs", uint64(i+1), uint64(len(missing)), "files")
				}
				return err
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
	}

	// Tier-2 base .pack: link from the store mount when present, else the
	// pack is remote-served (no local copy; stock git serving is deferred).
	for _, p := range plan.sideFiles {
		h.retrofitBasePack(p)
	}

	// Superseded removal: try-write only — readers (a streaming clone) are
	// NEVER blocked; losing the race leaves the pending list for next sync.
	h.removeSuperseded()

	// packs_ready bookkeeping.
	if err := h.updateState(func(st *RepoState) {
		if lvl >= LevelServe && st.PacksRevision != m.Revision {
			st.PacksRevision = m.Revision
		}
	}); err != nil {
		logWarnf("%s: state persist after materialize: %v", h.ID, err)
	}
	return nil
}

// fetchPackFile downloads one file of a pack. The .pack goes through the
// striped downloader (16 × 32 MiB stripes, §5.2/03 §5.2); side-files are
// small single GETs.
func (h *RepoHandle) fetchPackFile(ctx context.Context, p *proto.PackRef, what string) error {
	packDir := h.repo.PackDir()
	switch what {
	case "pack":
		dst := filepath.Join(packDir, "pack-"+p.Checksum+".pack")
		key := h.repoKey(store.PackKey(p.Checksum))
		size := int64(p.PackSize)
		if size == 0 {
			if meta, err := h.reg.st.Head(ctx, key); err == nil && meta != nil {
				size = meta.Size
			}
		}
		f, err := os.OpenFile(dst+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return &WalError{Kind: WalErrIo, Detail: dst, Wrapped: err}
		}
		defer f.Close()
		if err := store.DownloadFileParallel(ctx, h.reg.st, key, f, size); err != nil {
			return &WalError{Kind: WalErrStore, Detail: key, Wrapped: err}
		}
		f.Close()
		return os.Rename(dst+".tmp", dst)
	case "idx":
		return h.fetchSideFile(ctx, p.Checksum, ".idx", store.IdxKey(p.Checksum), int64(p.IdxSize))
	default:
		for _, suf := range []string{".rev", ".bitmap", ".commit-graph"} {
			if what == suf {
				key := sideKeyOf(suf, p.Checksum)
				return h.fetchSideFile(ctx, p.Checksum, suf, key, 0)
			}
		}
	}
	return nil
}

func sideKeyOf(suffix, checksum string) string {
	switch suffix {
	case ".rev":
		return store.RevKey(checksum)
	case ".bitmap":
		return store.BitmapKey(checksum)
	case ".commit-graph":
		return store.CommitGraphKey(checksum)
	}
	return store.IdxKey(checksum)
}

// fetchSideFile GETs one side-file into objects/pack (tmp+rename).
func (h *RepoHandle) fetchSideFile(ctx context.Context, checksum, suffix, key string, size int64) error {
	body, _, err := store.GetBytes(ctx, h.reg.st, h.repoKey(key), store.GetOptions{})
	if err != nil {
		return &WalError{Kind: WalErrStore, Detail: key, Wrapped: err}
	}
	if body == nil {
		// A missing optional side-file is not an error (flags may lag).
		if suffix == ".idx" {
			return &WalError{Kind: WalErrCorrupt, Detail: "idx absent: " + key}
		}
		return nil
	}
	dst := filepath.Join(h.repo.PackDir(), "pack-"+checksum+suffix)
	return atomicWriteFile(dst, body)
}

// sideFileSuffixes lists the side-files a pack's flags claim.
func sideFileSuffixes(p *proto.PackRef) []string {
	var out []string
	if p.IdxSize > 0 || true { // idx always expected
		out = append(out, ".idx")
	}
	if p.HasRev {
		out = append(out, ".rev")
	}
	if p.HasBitmap {
		out = append(out, ".bitmap")
	}
	if p.HasCommitGraph {
		out = append(out, ".commit-graph")
	}
	return out
}

// retrofitBasePack links a tier-2 base .pack from cache.store_mount, or marks
// it remote-served in the persisted state (no local .pack copy).
func (h *RepoHandle) retrofitBasePack(p *proto.PackRef) {
	mount := h.reg.vals.storeMount
	if mount != "" {
		src := filepath.Join(mount, h.repoKey(store.PackKey(p.Checksum)))
		if _, err := os.Stat(src); err == nil {
			dst := filepath.Join(h.repo.PackDir(), "pack-"+p.Checksum+".pack")
			if _, err := os.Lstat(dst); err != nil {
				_ = os.Symlink(src, dst)
			}
			return
		}
	}
	h.stateMu.Lock()
	has := false
	for _, s := range h.state.RemoteServed {
		if s == p.Checksum {
			has = true
			break
		}
	}
	if !has {
		h.state.RemoteServed = append(h.state.RemoteServed, p.Checksum)
	}
	h.stateMu.Unlock()
}

// removeSuperseded deletes pending pack files under the rw TRY-WRITE rule
// (13 §2.1): any active reader wins — checksums stay pending and the next
// sync retries. Never a blocking write, ever.
func (h *RepoHandle) removeSuperseded() {
	h.stateMu.Lock()
	pending := append([]string(nil), h.state.PendingPackRemovals...)
	h.stateMu.Unlock()
	if len(pending) == 0 {
		return
	}
	if !h.rw.TryWriteLock() { // readers active → defer, never wait
		return
	}
	defer h.rw.WriteUnlock()

	packDir := h.repo.PackDir()
	var stillPending []string
	for _, cs := range pending {
		removed := true
		for _, suf := range []string{".pack", ".idx", ".rev", ".bitmap"} {
			if err := os.Remove(filepath.Join(packDir, "pack-"+cs+suf)); err != nil && !os.IsNotExist(err) {
				removed = false
			}
		}
		if !removed {
			stillPending = append(stillPending, cs)
		}
	}
	h.updateState(func(st *RepoState) {
		st.PendingPackRemovals = stillPending
	})
}

// localPacks lists the checksum-suffixed files in objects/pack.
func localPacks(packDir string) map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(packDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			out[e.Name()] = true
		}
	}
	return out
}

func atomicWriteFile(dst string, data []byte) error {
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// remoteReaderFor builds/refreshes the per-revision remote reader when the
// pack set does not fit the budget (SyncObjects fallback). Built by the
// `remote-index` task; concurrent openers join it (§5.7).
func (h *RepoHandle) remoteReaderFor(ctx context.Context, m *pbManifest) (*RemoteReader, error) {
	if cur := h.remoteIdx.Load(); cur != nil && cur.Revision == m.Revision {
		return &RemoteReader{Revision: m.Revision, eng: h.engineFor(cur)}, nil
	}
	_, err := h.reg.tasks.Run(ctx, h.ID, "remote-index", map[string]string{"revision": itoa(m.Revision)},
		func(tctx context.Context, t *Task) error {
			return h.buildRemoteIndex(tctx, t, m)
		})
	if err != nil {
		return nil, err
	}
	cur := h.remoteIdx.Load()
	if cur == nil || cur.Revision != m.Revision {
		return nil, &WalError{Kind: WalErrRetry, Detail: "remote index build raced a revision change"}
	}
	return &RemoteReader{Revision: m.Revision, eng: h.engineFor(cur)}, nil
}

// engineFor binds a RemotePacks revision to the shared block cache and the
// repo's object store (per-revision engine, §5.7).
func (h *RepoHandle) engineFor(rp *RemotePacks) *remoteEngine {
	return &remoteEngine{
		packs:  rp,
		blocks: h.reg.blocks,
		st:     h.reg.st,
		repoID: h.ID,
		objCap: h.reg.vals.remoteObjectBytes,
	}
}

// buildRemoteIndex downloads the .idx files for every non-history pack into
// remote-idx/ (or hard-links the local copy when Serve installed it), 4
// concurrent downloads, then opens them (§5.7). objects/pack stays untouched.
func (h *RepoHandle) buildRemoteIndex(ctx context.Context, t *Task, m *pbManifest) error {
	idxDir := filepath.Join(h.repo.Path, "remote-idx")
	if err := os.MkdirAll(idxDir, 0o755); err != nil {
		return &WalError{Kind: WalErrIo, Detail: idxDir, Wrapped: err}
	}
	var packs []*proto.PackRef
	for _, p := range m.Packs {
		if p.Kind != proto.PackKindHistory {
			packs = append(packs, p)
		}
	}
	g, gctx := store.WithContext(ctx)
	g.SetLimit(4)
	for _, p := range packs {
		p := p
		g.Go(func() error {
			dst := filepath.Join(idxDir, p.Checksum+".idx")
			if _, err := os.Stat(dst); err == nil {
				return nil
			}
			// Hard-link the local idx when the pack is materialized.
			local := filepath.Join(h.repo.PackDir(), "pack-"+p.Checksum+".idx")
			if _, err := os.Stat(local); err == nil {
				if os.Link(local, dst) == nil {
					return nil
				}
			}
			body, _, err := store.GetBytes(gctx, h.reg.st, h.repoKey(store.IdxKey(p.Checksum)), store.GetOptions{})
			if err != nil {
				return &WalError{Kind: WalErrStore, Detail: store.IdxKey(p.Checksum), Wrapped: err}
			}
			if body == nil {
				return &WalError{Kind: WalErrCorrupt, Detail: "idx absent: " + p.Checksum}
			}
			return atomicWriteFile(dst, body)
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	idxs := make([]*packIndex, 0, len(packs))
	for _, p := range packs {
		ix, err := openPackIndex(filepath.Join(idxDir, p.Checksum+".idx"))
		if err != nil {
			return err
		}
		idxs = append(idxs, ix)
	}
	rp := &RemotePacks{Revision: m.Revision, packs: packs, idxs: idxs}
	h.remoteIdx.Store(rp)
	if t != nil {
		t.Notice("remote index ready: " + itoa(uint64(len(idxs))) + " packs at revision " + itoa(m.Revision))
	}
	return nil
}
