// bind_wal.go adapts internal/wal's surface onto this package's interfaces —
// the key seam of 07_api.md §1: handler files NEVER import internal/wal; only
// this file may. The wal engine is being built concurrently; this binding
// compiles against the frozen contract shapes in internal/wal/types.go (sync
// levels, ObjectAccess bridging, TaskRecord/Progress conversion) and reports
// every surface the engine does not expose yet as ErrPending (→ 503 plain
// text). The integration pass replaces the ErrPending bodies with the real
// engine calls; the wire shapes and interfaces do not change.
package api

import (
	"context"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/wal"
)

// WalEngine is the subset of internal/wal's engine surface this binding
// needs. It is structural: internal/server passes the engine in at startup
// and the compiler checks the shapes against the engine's concrete type.
type WalEngine interface {
	// Sync materializes the repo to the requested level (§2 of doc 05).
	Sync(ctx context.Context, id git.RepoId, level wal.SyncLevel) error
	// ObjectAccess builds the per-request object reader (local packs or the
	// remote reader) after a sync — the §1 "ObjectAccess bridging" seam.
	ObjectAccess(ctx context.Context, id git.RepoId) (wal.ObjectAccess, error)
	// Revision is the repo's current manifest revision (render-cache stamp).
	Revision(ctx context.Context, id git.RepoId) (uint64, error)
}

// syncLevelToWal mirrors the handler-side enum onto the frozen engine enum.
func syncLevelToWal(l SyncLevel) wal.SyncLevel {
	switch l {
	case SyncServe:
		return wal.LevelServe
	case SyncFull:
		return wal.LevelFull
	default:
		return wal.LevelRefs
	}
}

// taskFromWal converts the engine's frozen TaskRecord onto the wire shape.
// Field-for-field identical (doc 05 §6.8 frozen); the conversion exists so
// the wal import stays in this file alone.
func taskFromWal(t wal.TaskRecord) TaskRecord {
	ok := t.OK
	var progress *Progress
	if t.Progress != nil {
		p := progressFromWal(*t.Progress)
		progress = &p
	}
	tail := t.LogTail
	if tail == nil {
		tail = []string{}
	}
	return TaskRecord{
		ID:        t.ID,
		Kind:      t.Kind,
		Repo:      t.Repo,
		Hostname:  t.Hostname,
		Started:   t.Started,
		Finished:  t.Finished,
		ElapsedMS: t.ElapsedMS,
		OK:        ok,
		Summary:   t.Summary,
		Progress:  progress,
		LogTail:   tail,
		Params:    t.Params,
	}
}

// progressFromWal converts one engine narration packet.
func progressFromWal(p wal.Progress) Progress {
	return Progress{
		Kind:    p.Kind,
		Text:    p.Text,
		Label:   p.Label,
		Done:    p.Done,
		Total:   p.Total,
		Unit:    p.Unit,
		Percent: p.Percent,
	}
}

// walView implements RepoView over the engine. Every method first syncs /
// stamps by the manifest revision; the object renders behind these methods
// are exactly the git recipes of §9 (they land with the engine — until then
// each body is ErrPending).
type walView struct {
	engine WalEngine
}

var _ RepoView = (*walView)(nil)

func (v *walView) Sync(ctx context.Context, id git.RepoId, level SyncLevel) error {
	if v.engine == nil {
		return ErrPending
	}
	return v.engine.Sync(ctx, id, syncLevelToWal(level))
}

func (v *walView) Resolve(ctx context.Context, id git.RepoId, rest string) (Resolution, error) {
	// INTEGRATION: 2k exact lookups in the ref snapshot (branch beats tag),
	// rev-parse fallback via ObjectAccess; revision from engine.Revision.
	return Resolution{}, ErrPending
}

func (v *walView) Head(ctx context.Context, id git.RepoId) (Ref, bool, error) {
	return Ref{}, false, ErrPending
}

func (v *walView) RefList(ctx context.Context, id git.RepoId, ns string, q RefQuery) ([]Ref, bool, error) {
	return nil, false, ErrPending
}

func (v *walView) Tree(ctx context.Context, id git.RepoId, rev, path string) (TreeResult, error) {
	return TreeResult{}, ErrPending
}

func (v *walView) Blob(ctx context.Context, id git.RepoId, rev, path string, raw bool) (BlobResult, error) {
	return BlobResult{}, ErrPending
}

func (v *walView) Commits(ctx context.Context, id git.RepoId, ref, path string, skip, n int) (CommitPage, error) {
	return CommitPage{}, ErrPending
}

func (v *walView) Commit(ctx context.Context, id git.RepoId, sha string) (CommitDetail, error) {
	return CommitDetail{}, ErrPending
}

func (v *walView) Summary(ctx context.Context, id git.RepoId) (SummaryData, error) {
	return SummaryData{}, ErrPending
}

func (v *walView) Overview(ctx context.Context, id git.RepoId) (OverviewData, error) {
	return OverviewData{}, ErrPending
}

func (v *walView) Settings(ctx context.Context, id git.RepoId) (SettingsDoc, error) {
	return SettingsDoc{}, ErrPending
}

func (v *walView) PublishSettings(ctx context.Context, id git.RepoId, body []byte, message, author string) (uint64, error) {
	return 0, ErrPending
}

func (v *walView) SettingsHistory(ctx context.Context, id git.RepoId) (SettingsHistory, error) {
	return SettingsHistory{}, ErrPending
}

func (v *walView) HeadSeq(ctx context.Context, id git.RepoId) (uint64, error) { return 0, ErrPending }

func (v *walView) PushHistory(ctx context.Context, id git.RepoId, last int) ([]PushRecord, error) {
	return nil, ErrPending
}

// NewEnv is the constructor internal/server calls once at startup (§8.10
// order). engine may be nil until the wal engine lands — handlers answer 503
// for the pending surfaces; the Tasks table is bound by the server through
// Env.Tasks (maintain owns the table).
func NewEnv(st interface {
	Backend() string
}, repos RepoRegistry, cfg *config.Config, engine WalEngine, version, hostname string) *Env {
	e := &Env{
		Repos:    repos,
		Cfg:      cfg,
		Version:  version,
		Hostname: hostname,
	}
	e.Ready()
	if engine != nil {
		e.Repo = &walView{engine: engine}
	}
	return e
}
