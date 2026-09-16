// pushmirror.go — Forgejo #623 composition (docs/features/13_push_mirror.md):
// the push-mirror service over store/registry/config (Seam 5 kind
// registered once — duplicate registration panics, the
// maintain.RegisterKind contract in code terms), the Seam 1 chain (repo
// lanes only — post-hoc config, never at create), the discovery
// entries, the summary projection hook (api.Env.PushMirrorSummary —
// core never imports the feature, law 8), and the server on-push
// fan-out hook.
package main

import (
	"context"
	"net/http"
	"time"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/pushmirror"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// pushMirrorLoopInterval is the scheduled-sync enumeration cadence (the
// follow.go shape: its own ticker, never a maintenance unit). Repos
// with scheduling OFF ("") never fire here — on-push is their only
// trigger. Due mirrors fire; the bucket lease arbitrates across
// maintain hosts.
const pushMirrorLoopInterval = time.Minute

// newPushMirrorService builds the push-mirror surface over st/reg/cfg
// and registers the task kind + discovery templates (called once per
// process from buildCollab — one block for this package). apiEnv may be
// nil in tests that only need the push path (the summary hook is
// skipped then).
func newPushMirrorService(st store.ObjectStore, reg *wal.Registry, cfg *config.Config, apiEnv *api.Env) (*pushmirror.Service, *pushmirror.Handler) {
	pushmirror.RegisterKind(pushmirror.KindPushMirrorSync)
	api.RegisterExposed(pushmirror.ExposedTemplates...)
	svc := pushmirror.New(pushmirror.Deps{
		Store:     st,
		Reg:       reg,
		GitBinary: cfg.Git.Binary,
		CacheDir:  cfg.Cache.Dir,
		Hostname:  instanceID(cfg),
		// The [import] section owns SSRF + timeouts for both mirror
		// directions (one gate, one clock — no new config section).
		PushTimeout:  time.Duration(cfg.Import.CloneTimeout),
		GitTimeout:   time.Duration(cfg.Import.GitTimeout),
		AllowPrivate: cfg.Import.AllowPrivateNetworks,
		Allowlist:    cfg.Import.URLAllowlist,
		AllowFile:    cfg.Import.AllowFileURLs,
	})
	h := &pushmirror.Handler{Svc: svc}
	if apiEnv != nil {
		// The summary projection behind the Env hook (the MirrorSummary
		// shape): api renders PushMirrorView without importing the
		// feature (law 8); the next fire is computed here, at read,
		// from the stored schedule. Secrets never reach the
		// projection — presence + last-4 only.
		apiEnv.PushMirrorSummary = func(ctx context.Context, owner, repo string) (api.PushMirrorView, bool) {
			doc, _, err := pushmirror.Load(ctx, st, owner, repo)
			if err != nil || doc == nil {
				return api.PushMirrorView{}, false
			}
			sec, _, _ := pushmirror.LoadSecret(ctx, st, owner, repo)
			return pushMirrorViewOf(pushmirror.ViewOf(doc, sec, time.Now())), true
		}
	}
	return svc, h
}

// pushMirrorViewOf maps the feature read model onto the wire projection
// (pure: the hook's I/O stays inline above; the field mapping is pinned
// by unit test without touching kind registration, which panics on
// duplicates and belongs to buildCollab alone in this binary).
func pushMirrorViewOf(v pushmirror.View) api.PushMirrorView {
	return api.PushMirrorView{
		UpstreamURL:         v.UpstreamURL,
		AuthKind:            v.AuthKind,
		Username:            v.Username,
		HasSecret:           v.HasSecret,
		SecretHint:          v.SecretHint,
		Schedule:            v.Schedule,
		NextSyncAt:          v.NextSyncAt,
		LastSyncedAt:        v.LastSyncedAt,
		LastResult:          v.LastResult,
		ConsecutiveFailures: v.ConsecutiveFailures,
		Due:                 v.Due,
	}
}

// chainPushMirror fronts the core mux with the push-mirror surface
// (Seam 1); authentication resolves through the server chain (Seam 2).
func chainPushMirror(srv *server.Server, h *pushmirror.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().AuthenticateForwarded(r, srv.Config())
	}
	srv.ChainExtra(h)
}

// pushMirrorOnPushOf builds the server on-push fan-out hook over the
// service: after a successful client push lands, the service probes the
// config sidecar and fires an async sync when configured. Nil service →
// nil hook (no fan-out). The server never imports the feature (law 8).
func pushMirrorOnPushOf(svc *pushmirror.Service) func(id git.RepoId) {
	if svc == nil {
		return nil
	}
	return func(id git.RepoId) {
		svc.EnqueueOnPush(id.Owner, id.Name)
	}
}

// pushMirrorHookOf resolves the OnPush hook from the collab wiring (nil
// collab/service → nil hook — setup-only and bare instances fan out
// nothing).
func pushMirrorHookOf(c *collabWiring) func(id git.RepoId) {
	if c == nil || c.pushMirrorSvc == nil {
		return nil
	}
	return pushMirrorOnPushOf(c.pushMirrorSvc)
}
