// mirror.go — Forgejo #240 composition (docs/features/11_mirror.md):
// the mirror service over store/registry/config (Seam 5 kind registered
// once — duplicate registration panics, the maintain.RegisterKind
// contract in code terms), the Seam 1 chain (repo lanes + the
// create-from-URL top-level twin), the discovery entry, the summary
// projection hook (api.Env.MirrorSummary — core never imports the
// feature, law 8), and the server push-refusal guard.
package main

import (
	"context"
	"net/http"
	"time"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/mirror"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// mirrorLoopInterval is the scheduled-sync enumeration cadence (the
// follow.go shape: its own ticker, never a maintenance unit). Due
// mirrors fire; the bucket lease arbitrates across maintain hosts.
// No new knob: schedules are presets, and daily-default fleets probe
// one tiny sidecar per repo per minute (404s are free).
const mirrorLoopInterval = time.Minute

// newMirrorService builds the mirror surface over st/reg/cfg and
// registers the task kind + discovery templates (called once per
// process from buildCollab — one block for this package). apiEnv may
// be nil in tests that only need the push path (the summary hook is
// skipped then).
func newMirrorService(st store.ObjectStore, reg *wal.Registry, cfg *config.Config, apiEnv *api.Env) (*mirror.Service, *mirror.Handler) {
	mirror.RegisterKind(mirror.KindMirrorSync)
	api.RegisterExposed(mirror.ExposedTemplates...)
	svc := mirror.New(mirror.Deps{
		Store:     st,
		Reg:       reg,
		GitBinary: cfg.Git.Binary,
		CacheDir:  cfg.Cache.Dir,
		Hostname:  instanceID(cfg),
		// The [import] section owns SSRF + timeouts + size caps for both
		// flows (one gate, one clock — no new config section).
		CloneTimeout: time.Duration(cfg.Import.CloneTimeout),
		GitTimeout:   time.Duration(cfg.Import.GitTimeout),
		AllowPrivate: cfg.Import.AllowPrivateNetworks,
		Allowlist:    cfg.Import.URLAllowlist,
		AllowFile:    cfg.Import.AllowFileURLs,
		MaxBytes:     int64(cfg.Import.MaxBytes),
	})
	h := &mirror.Handler{
		Svc: svc,
		CreateRepo: func(ctx context.Context, owner, name string) error {
			_, err := reg.Create(ctx, owner+"/"+name, git.Sha1)
			return err
		},
		AuthMode: func() string { return cfg.Server.Auth.Mode },
	}
	if apiEnv != nil {
		// The summary projection behind the Env hook (the ReadGate/
		// OrgGate shape): api renders MirrorView without importing
		// the feature (law 8); the next fire is computed here, at
		// read, from the stored preset (R1 (b)).
		apiEnv.MirrorSummary = func(ctx context.Context, owner, repo string) (api.MirrorView, bool) {
			doc, _, err := mirror.Load(ctx, st, owner, repo)
			if err != nil || doc == nil {
				return api.MirrorView{}, false
			}
			v := mirror.ViewOf(doc, time.Now())
			return api.MirrorView{
				UpstreamURL:         v.UpstreamURL,
				Schedule:            v.Schedule,
				NextSyncAt:          v.NextSyncAt,
				LastSyncedAt:        v.LastSyncedAt,
				LastResult:          v.LastResult,
				ConsecutiveFailures: v.ConsecutiveFailures,
				Due:                 v.Due,
			}, true
		}
	}
	return svc, h
}

// chainMirror fronts the core mux with the mirror surface (Seam 1);
// authentication resolves through the server chain (Seam 2), including
// the §8.6 broker-forwarding rule — the forwarded principal replaces
// the broker's, never the broker itself.
func chainMirror(srv *server.Server, h *mirror.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().AuthenticateForwarded(r, srv.Config())
	}
	srv.ChainExtra(h)
}

// mirrorGuardOf builds the server push-refusal predicate (Forgejo #240
// R1 (c)) over the same store: true iff a live meta/mirror.json
// exists. Nil store (setup-only) → refuse nothing.
func mirrorGuardOf(st store.ObjectStore) func(ctx context.Context, id git.RepoId) bool {
	if st == nil {
		return nil
	}
	return func(ctx context.Context, id git.RepoId) bool {
		return mirror.IsMirror(ctx, st, id.Owner, id.Name)
	}
}
