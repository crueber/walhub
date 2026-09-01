package server

import (
	"context"

	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/wal"
)

// Engine is the internal/server seam over the WAL engine (internal/wal, doc
// 05; §2.4). The server composes git-layer calls directly for smart HTTP and
// calls the engine only through this narrow surface: sync levels, publish,
// registry open, placement, and bundle listing. The integration pass binds the
// production engine (bindEngine below).
type Engine interface {
	// Sync materializes the repo to the given level (per-request refs sync).
	Sync(ctx context.Context, id git.RepoId, level wal.SyncLevel) error

	// Repo opens the local repo for git operations; create=true auto-creates
	// with the given object format (server.auto_create_on_push). Unknown repo
	// with create=false → a *wal.WalError with Kind wal.WalErrNotFound.
	Repo(ctx context.Context, id git.RepoId, create bool, format git.ObjectFormat) (*git.LocalRepo, error)

	// Publish applies a parsed receive-pack request (commands already parsed,
	// pack already ingested + connectivity-checked by the git layer) to the
	// WAL; seq is the commit point. Ref conflicts surface in the result.
	Publish(ctx context.Context, id git.RepoId, req *git.PushRequest, principal string, access wal.ObjectAccess) (wal.PublishResult, error)

	// Placement answers where the repo is served/maintained (§4.3).
	Placement(ctx context.Context, id git.RepoId) (Placement, error)

	// Bundles answers the bundle list for the repo (fulls + chain), already
	// filtered. filter != "" and not accepted → the handler 400s first.
	Bundles(ctx context.Context, id git.RepoId, filter string) (BundleList, error)

	// AutoCreateOnPush reports the effective flag for this repo.
	AutoCreate(ctx context.Context, id git.RepoId) bool
}

// Placement is the maintainer-heartbeat-derived placement decision (§4.3,
// 10_maintenance.md). servedBy is the maintaining host when !serve.
type Placement struct {
	Serve           bool
	ServeExclude    bool
	Maintain        bool
	MaintainExclude bool
	ServedBy        string
}

// BundleEntry is one bundle object in a bundle list.
type BundleEntry struct {
	Strategy string // e.g. "full" | chain name
	Name     string
}

// BundleList is the git-config bundle list: fulls + chain (D17).
type BundleList struct {
	Fulls []BundleEntry
	Chain []BundleEntry
}

// Filter catches list requests with a filter other than blob:none (§3.3).
func (bl BundleList) Clone() BundleList {
	out := BundleList{Fulls: make([]BundleEntry, len(bl.Fulls)), Chain: make([]BundleEntry, len(bl.Chain))}
	copy(out.Fulls, bl.Fulls)
	copy(out.Chain, bl.Chain)
	return out
}

// walObjectAccessNoLocal is the zero ObjectAccess (local disk absent).
// bindEngine converts server-side needs onto wal's CONTRACT shapes
// (internal/wal/types.go); drift is reconciled by the integration pass.
type bindEngine struct {
	inner Engine // the production engine, injected at composition
}

// Compile-time shape checks against wal's frozen types (types.go).
var (
	_ = wal.LevelRefs
	_ = wal.LevelServe
	_ = wal.LevelFull
	_ = wal.PublishResult{}
	_ = wal.RefResult{}
	_ = wal.ErrNotFound
)

// bundleKeys maps a BundleEntry to its store key (§5 static path).
func bundleKey(id git.RepoId, e BundleEntry) string {
	return id.StorePrefix() + "bundles/" + e.Strategy + "/" + e.Name
}

// lfsKey is the store key for one LFS object.
func lfsKey(id git.RepoId, oid string) string { return id.StorePrefix() + "lfs/objects/" + oid }
