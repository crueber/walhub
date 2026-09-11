// handle.go — the per-repo handle (doc 05 §5.1.2, §5.1.3, §5.1.4): manifest
// state with the monotonic revision guard, the persisted walgit-state.json,
// the three locks (syncMu → packMu → rw.TryWrite; never the reverse), and the
// ReadGuard returned by Sync.
package wal

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal/rw"
)

// RepoHandle is one open repo (05 §5.1.2).
type RepoHandle struct {
	ID string // "owner/name"

	// Manifest state. Writers hold syncMu; readers snapshot under syncMu too
	// (it is a short critical section; there is no separate RWMutex here —
	// the spec's "read under RLock" maps to the snapshot under syncMu).
	manifest *pbManifest
	version  string    // store version tag for the next CAS
	freshAt  time.Time // last freshness check
	heldRev  uint64    // monotonic revision guard (rule 5.0.4)

	syncMu syncMutex // serializes the refs phase (§5.2); try-lock-first
	packMu syncMutex // serializes pack reconciliation; NEVER held with syncMu
	rw     rw.TryRWMutex

	// serveDegraded gates the serve-health success-clear (servehealth.go):
	// set when this handle's pack phase fails, cleared when a later
	// serve succeeds and deletes the sidecar.
	serveDegraded atomic.Bool

	state   *RepoState
	stateMu sync.Mutex // guards state file read-modify-write

	progress       *Broadcast[Progress]
	remoteIdx      atomic.Pointer[RemotePacks]
	checkpointsMut sync.Mutex
	checkpoints    map[uint64]time.Time // seq -> created_at cache
	effCfg         effectiveConfigCache

	// entry-time bookkeeping for checkpoint provenance (§5.5); under syncMu.
	firstEntryTime time.Time
	lastEntryTime  time.Time

	reg     *Registry
	repo    *git.LocalRepo
	pub     *Publisher
	pubOnce sync.Once
}

// pbManifest aliases the proto type for brevity across this package.
type pbManifest = proto.Manifest

const stateFileName = "walgit-state.json"

func newHandle(reg *Registry, id string, m *pbManifest, version string, repo *git.LocalRepo, st *RepoState) *RepoHandle {
	h := &RepoHandle{
		ID:          id,
		manifest:    m,
		version:     version,
		freshAt:     time.Now(),
		heldRev:     m.Revision,
		state:       st,
		progress:    &Broadcast[Progress]{},
		checkpoints: map[uint64]time.Time{},
		reg:         reg,
		repo:        repo,
	}
	if m.Checkpoint != nil && m.Checkpoint.CreatedAt != nil {
		h.checkpoints[m.Checkpoint.Seq] = m.Checkpoint.CreatedAt.Go()
	}
	return h
}

// Repo returns the local bare repo handle.
func (h *RepoHandle) Repo() *git.LocalRepo { return h.repo }

// Registry returns the owning registry.
func (h *RepoHandle) Registry() *Registry { return h.reg }

// Layer returns the git layer.
func (h *RepoHandle) Layer() *git.Layer { return h.reg.GitLayer() }

// Dir is the local repo path.
func (h *RepoHandle) Dir() string { return h.repo.Path }

// Progress returns the per-repo broadcast (SSE attach point, §5.8).
func (h *RepoHandle) Progress() *Broadcast[Progress] { return h.progress }

// teardown stops the publisher and drops background work (Close/Delete path).
func (h *RepoHandle) teardown() {
	if h.pub != nil {
		h.pub.Close()
	}
}

// ---- manifest access --------------------------------------------------------

// ManifestSnapshot returns a stable copy of the held manifest + its CAS version.
func (h *RepoHandle) ManifestSnapshot() (*pbManifest, string) {
	h.syncMu.Lock()
	defer h.syncMu.Unlock()
	return h.manifest, h.version
}

// freshenManifest is the ONE sanctioned store-call-under-syncMu (13 §2.2):
// the conditional GET of manifest.pb that serializes the refs phase.
func (h *RepoHandle) freshenManifest(ctx context.Context) error {
	ttl := time.Duration(h.reg.cfg.WAL.FreshnessTTL)
	if !h.freshAt.IsZero() && ttl > 0 && time.Since(h.freshAt) < ttl {
		return nil
	}
	key := manifestKey(h.ID)
	var body []byte
	var version string
	if h.version != "" {
		res, err := h.reg.st.Get(ctx, key, store.GetOptions{IfNoneMatch: store.Version(h.version)})
		if err != nil {
			return &WalError{Kind: WalErrStore, Detail: key, Wrapped: err}
		}
		switch r := res.(type) {
		case store.NotModified:
			h.freshAt = time.Now()
			return nil
		case store.Object:
			b, err := readAll(r.Body)
			if err != nil {
				return &WalError{Kind: WalErrIo, Detail: key, Wrapped: err}
			}
			body, version = b, string(r.Meta.Version)
		}
	} else {
		b, meta, err := store.GetBytes(ctx, h.reg.st, key, store.GetOptions{})
		if err != nil {
			return &WalError{Kind: WalErrStore, Detail: key, Wrapped: err}
		}
		if b == nil {
			return ErrNotFound(h.ID)
		}
		body, version = b, string(meta.Version)
	}

	m, err := proto.UnmarshalManifest(body)
	if err != nil {
		return &WalError{Kind: WalErrCorrupt, Detail: key, Wrapped: err}
	}
	// Monotonic revision guard (rule 5.0.4): a stale cached read from after
	// our own publish is discarded — only the freshness stamp moves.
	if m.Revision <= h.heldRev {
		h.freshAt = time.Now()
		return nil
	}
	h.manifest = m
	h.version = version
	h.heldRev = m.Revision
	h.freshAt = time.Now()
	if m.Checkpoint != nil && m.Checkpoint.CreatedAt != nil {
		h.noteCheckpoint(m.Checkpoint.Seq, m.Checkpoint.CreatedAt.Go())
	}
	return nil
}

// noteCheckpoint records a checkpoint's created_at (§5.2 bump).
func (h *RepoHandle) noteCheckpoint(seq uint64, at time.Time) {
	h.checkpointsMut.Lock()
	h.checkpoints[seq] = at
	h.checkpointsMut.Unlock()
}

// ---- persisted state (05 §5.1.4) --------------------------------------------

// loadState reads walgit-state.json; corrupt/missing → zero-value defaults.
// Never fails: the bucket is the truth, the state file is only an accelerator.
func loadState(dir string) *RepoState {
	st := &RepoState{}
	data, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil || json.Unmarshal(data, st) != nil {
		return &RepoState{}
	}
	return st
}

// saveState writes the state atomically: tmp file + rename in the same dir.
// Callers must hold stateMu (or be before any concurrent section exists).
func saveState(dir string, st *RepoState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, stateFileName+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, stateFileName))
}

// updateState runs fn under stateMu and persists (syncMu → stateMu nesting).
func (h *RepoHandle) updateState(fn func(st *RepoState)) error {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	fn(h.state)
	return saveState(h.repo.Path, h.state)
}

// packsReady reports whether the local pack set is current and clean.
func (h *RepoHandle) packsReady() bool {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	return h.state.PacksReady()
}

// ---- catch-up and Sync (05 §5.2) --------------------------------------------

// catchUp is the open-path applyDelta: the manifest is already in hand
// (no second GET), only the refs phase runs.
func (h *RepoHandle) catchUp(ctx context.Context) error {
	if err := h.syncMu.LockMeasured(ctx, "sync_mutex", h.ID); err != nil {
		return err
	}
	defer h.syncMu.Unlock()
	return h.applyDelta(ctx)
}

// ReadGuard holds the rw read guard: packs cannot be removed while held.
type ReadGuard struct{ h *RepoHandle }

// Release drops the read guard (caller's defer).
func (g *ReadGuard) Release() {
	if g != nil && g.h != nil {
		g.h.rw.RUnlock()
		g.h = nil
	}
}

// Sync brings the local view to lvl and returns a read guard (05 §5.2).
// The refs phase runs under syncMu (try-lock first, measured); the pack phase
// runs under packMu — syncMu is NEVER held with packMu, so refs requests
// never queue behind a multi-GB materialization.
//
// The pack phase is BOUNDED (issue #320): packMu acquisition and the
// materialize single-flight join/leader-wait run under a timeout
// (server.serve_sync_timeout, default 45s). Exceeding it fails the Sync
// with WalErrTimeout and marks the serve-health sidecar — the request
// answers 503 + degraded instead of hanging past the proxy — while the
// detached materialize body keeps warming the cache in the background
// (capped separately by server.serve_materialize_timeout). A client
// disconnect (parent ctx canceled, bound not fired) is NOT a serve
// failure and marks nothing. Levels below LevelServe never touch packMu
// and are never bounded by this timeout.
func (h *RepoHandle) Sync(ctx context.Context, lvl SyncLevel) (*ReadGuard, error) {
	if err := h.syncMu.LockMeasured(ctx, "sync_mutex", h.ID); err != nil {
		return nil, err
	}
	if err := h.freshenManifest(ctx); err != nil {
		h.syncMu.Unlock()
		return nil, err
	}
	m := h.manifest
	// Freshness skip: nothing new could have arrived since the last check.
	needDelta := true
	if ttl := time.Duration(h.reg.cfg.WAL.FreshnessTTL); ttl > 0 && !h.freshAt.IsZero() &&
		time.Since(h.freshAt) < ttl && h.state.AppliedSeq >= m.HeadSeq {
		needDelta = false
	}
	var derr error
	if needDelta {
		derr = h.applyDelta(ctx)
	}
	h.syncMu.Unlock()
	if derr != nil {
		return nil, derr
	}

	if lvl >= LevelServe {
		wait := h.reg.vals.serveSyncTimeout
		if wait <= 0 {
			wait = DefaultServeSyncTimeout
		}
		pctx, cancel := context.WithTimeout(ctx, wait)
		if err := h.packMu.LockMeasured(pctx, "pack_mutex", h.ID); err != nil {
			cancel()
			return nil, h.packPhaseError(pctx, ctx, err, "serve sync wait")
		}
		err := h.reconcilePacks(pctx, lvl)
		h.packMu.Unlock()
		cancel()
		if err != nil {
			return nil, h.packPhaseError(pctx, ctx, err, "serve sync")
		}
		h.clearServeDegraded()
	}

	h.rw.RLock()
	return &ReadGuard{h: h}, nil
}

// packPhaseError maps a pack-phase failure onto the caller's error and
// the serve-health sidecar (issue #320): our bound firing (pctx deadline
// exceeded) or any other materialize failure marks degraded; a pure
// client disconnect (parent canceled, bound not fired) marks nothing.
func (h *RepoHandle) packPhaseError(pctx, ctx context.Context, err error, what string) error {
	if pctx.Err() == context.DeadlineExceeded {
		h.markServeDegraded(what + " timed out: objects not servable")
		return &WalError{Kind: WalErrTimeout, Detail: what + " timed out: objects not servable"}
	}
	if ctx.Err() != nil {
		return ctx.Err() // client went away — not a serve failure
	}
	h.markServeDegraded(ShortErr(err))
	return err
}

// ShortErr renders a one-liner for sidecar/task reasons (bounded
// length, single line — markers are log/UI-visible). Exported for the
// mirror feature's probe/heal reasons (same bound, one function).
func ShortErr(err error) string {
	s := err.Error()
	for i, c := range s {
		if c == '\n' || c == '\r' {
			s = s[:i]
			break
		}
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// ---- effective config cache (§5.3.3 settings invalidation) ------------------

type effectiveConfigCache struct {
	mu       sync.Mutex
	revision uint64 // settings revision the cache was built for
	valid    bool
	toml     string
}

// invalidateSettings drops the effective-config cache (publish_settings).
func (h *RepoHandle) invalidateSettings() {
	h.effCfg.mu.Lock()
	h.effCfg.valid = false
	h.effCfg.mu.Unlock()
}

// readAll drains and closes a store object body.
func readAll(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	return io.ReadAll(rc)
}
