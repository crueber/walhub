// bind_wal.go — the composition seam between internal/events and the concrete
// WAL engine (internal/wal, doc 05). The bridge codes against its own narrow
// interfaces (WalSource / RepoView); this file binds the production surface.
package events

import (
	"context"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/store/proto"
	"git.packden.us/crueber/walhub/internal/wal"
)

// Compile-time shape checks against the concrete engine.
var (
	_ WalSource = (*WALSource)(nil)
	_ RepoView  = (*WALRepo)(nil)
)

// WALSource binds wal.Registry. Compose it only for instances whose roles
// include events (09 §1); NewBridgeWithRegistry wires store + config too.
type WALSource struct {
	reg *wal.Registry
}

// NewRegistrySource binds a live WAL registry.
func NewRegistrySource(reg *wal.Registry) *WALSource { return &WALSource{reg: reg} }

// Repos lists every repo the registry knows (Registry.List is instant, served
// from the cached listing).
func (s *WALSource) Repos(ctx context.Context) ([]string, error) {
	return s.reg.List(), nil
}

// Handle opens the repo; unknown repo surfaces the engine's not-found error.
func (s *WALSource) Handle(ctx context.Context, repo string) (RepoView, error) {
	id, err := git.ParseRepoId(repo)
	if err != nil {
		return nil, err
	}
	h, err := s.reg.Open(ctx, id.String())
	if err != nil {
		return nil, err
	}
	return &WALRepo{h: h}, nil
}

// WALRepo binds wal.RepoHandle.
type WALRepo struct {
	h *wal.RepoHandle
}

// SyncRefs revalidates the manifest fresh: a LevelRefs sync (the manifest
// conditional GET runs before any freshness skip, so the returned head/min_seq
// are current), after which the manifest snapshot is the fresh state.
//
// TODO-INTEGRATION: doc 05's exported surface names this SyncRefs; the current
// engine exposes Sync(ctx, wal.LevelRefs). If the engine renames/adds a
// dedicated SyncRefs, rebind here only.
func (r *WALRepo) SyncRefs(ctx context.Context) (RepoState, error) {
	g, err := r.h.Sync(ctx, wal.LevelRefs)
	if err != nil {
		return RepoState{}, err
	}
	defer g.Release()
	m, _ := r.h.ManifestSnapshot()
	return RepoState{
		HeadSeq: m.HeadSeq,
		MinSeq:  m.MinSeq,
		Sha256:  m.ObjectFormat == "sha256",
	}, nil
}

// ReadLog reads log entries [from, to] from the framed segments.
//
// TODO-INTEGRATION: ReadLog holds syncMu and re-freshens; the engine contract
// (doc 05) matches doc 09 §2's read_log(from, to) semantics including the
// "stops at the first incomplete trailing frame" rule.
func (r *WALRepo) ReadLog(ctx context.Context, from, to uint64) ([]*proto.LogEntry, error) {
	return r.h.ReadLog(ctx, from, to)
}
