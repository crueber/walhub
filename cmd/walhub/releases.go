// releases.go — Feature 07 composition (docs/features/07): the releases
// service (Seam 1, both lanes + the repo-subpath byte route) over the P6
// roles owned by identity, stock git through the bounded pool, repo dirs
// through the WAL registry, and the asset cap/spool from config. Publish
// fan-out rides internal/notify through the nil-safe Emitter/Streamer
// seams (P8); the social fork counter is wired in social.go.
package main

import (
	"context"
	"net/http"
	"path/filepath"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/notify"
	"git.packden.us/crueber/walhub/internal/releases"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// newReleasesService builds the releases service over st/ident. Git, Dirs,
// Notify, and Stream are wired by the caller (serveHTTP); the asset cap
// comes from releases.max_asset_bytes (0 = the 2 GiB default inside the
// package) and the spool stages uploads under cacheDir (LFS §6.2 pattern).
func newReleasesService(st store.ObjectStore, ident *identity.Service, reg *wal.Registry, gitBinary, cacheDir string, maxAssetBytes int64) (*releases.Service, *releases.Handler) {
	svc := releases.New(st, ident)
	svc.Git = releases.NewSubprocessGit(gitBinary)
	svc.Dirs = &pullsDirs{reg: reg}
	svc.MaxAssetBytes = maxAssetBytes
	if cacheDir != "" {
		svc.SpoolDir = filepath.Join(cacheDir, "release-spool")
	}
	h := &releases.Handler{Svc: svc}
	return svc, h
}

// chainReleases fronts the core mux with the releases lane surface (Seam 1)
// and registers the byte route on the repo-subpath chain (the static
// uncompressed group per the 14.3 routing note); authentication resolves
// through the server chain (Seam 2).
func chainReleases(srv *server.Server, h *releases.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().Authenticate(r, srv.Config())
	}
	srv.ChainExtra(h)
	srv.ChainRepo(h)
}

// wireReleasesFanout binds the REAL emission implementations onto the
// nil-safe Emitter/Streamer seams (P8: synchronous fan-out after each
// publish commit). The adapter is a thin type translation in composition —
// all fan-out logic lives in internal/notify, which never imports the
// feature packages (09 §2).
func wireReleasesFanout(svc *releases.Service, notifySvc *notify.Service) {
	if svc == nil || notifySvc == nil {
		return
	}
	svc.Notify = func(ctx context.Context, ev releases.NotifyEvent) {
		notifySvc.EmitRelease(ctx, ev.Repo, ev.Tag, ev.Actor, ev.At)
	}
	svc.Stream = func(ctx context.Context, ev releases.StreamEvent) {
		notifySvc.PublishStream(ev.Name, ev.Repo, ev.Action, ev.Tag, "", 0)
	}
}

// compile-time seam assertions: composition consumes exactly the narrow
// interfaces the releases package defines (core never imports releases).
var (
	_ releases.RoleService = (*identity.Service)(nil)
	_ releases.RepoDirs    = (*pullsDirs)(nil)
	_ server.ExtraRoutes   = (*releases.Handler)(nil)
	_ server.RepoRoutes    = (*releases.Handler)(nil)
)
