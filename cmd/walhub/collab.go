// collab.go — Feature 09 integration (docs/features/09_rollout.md §4): the
// ONE place that assembles the collaboration layer (Features 01–08) over
// the frozen seams. serveHTTP calls buildCollab + chainCollab;
// integration tests reuse buildCollab so the measured composition is the
// shipped composition. No new product surface: pure wiring, one block per
// package (09 §4 touch point 3).
package main

import (
	"context"
	"net/http"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/checks"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/issues"
	"git.packden.us/crueber/walhub/internal/notify"
	"git.packden.us/crueber/walhub/internal/pulls"
	"git.packden.us/crueber/walhub/internal/releases"
	"git.packden.us/crueber/walhub/internal/review"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/social"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// collabWiring holds every collaboration service + handler (never built in
// setup-only mode, where there is no store).
type collabWiring struct {
	ident           *identity.Service
	identHandler    *identity.Handler
	issuesSvc       *issues.Service
	issuesHandler   *issues.Handler
	pullsSvc        *pulls.Service
	pullsHandler    *pulls.Handler
	reviewSvc       *review.Service
	reviewHandler   *review.Handler
	checksSvc       *checks.Service
	checksHandler   *checks.Handler
	releasesSvc     *releases.Service
	releasesHandler *releases.Handler
	socialSvc       *social.Service
	socialHandler   *social.Handler
	notifySvc       *notify.Service
	notifyHandler   *notify.Handler
}

// buildCollab assembles every collaboration service + handler over the
// shared store/registry/config and binds the nil-safe cross-package seams
// (fan-out, counters, frames, access bootstrap). apiEnv may be nil in
// tests that only need the push path (back-bindings are skipped then).
func buildCollab(st store.ObjectStore, cfg *config.Config, reg *wal.Registry, apiEnv *api.Env) *collabWiring {
	c := &collabWiring{}
	// Wave A identity (docs/features/01): the access.json/org/team
	// surface (Seam 1, both lanes), the require_read gate (Seam 3
	// expansion wired into dry-run via GroupExpander), and the
	// access-bootstrap op (Seam 5).
	c.ident = identity.New(st, cfg)
	c.identHandler = &identity.Handler{Svc: c.ident}
	if apiEnv != nil {
		apiEnv.Access = c.ident
		apiEnv.GroupExpander = c.ident.PolicyExpander()
		if ot, ok := apiEnv.Tasks.(*opsTasks); ok {
			ot.ident = c.ident
		}
	}
	// Wave B issues (docs/features/02): the thread/event/label/
	// milestone surface (Seam 1, both lanes) over the P6 roles owned
	// by identity. Notifications emit through the 02 §10 seam —
	// nil until wireNotifyFanout binds the real emitter below
	// (best-effort synchronous fan-out contract, P8).
	c.issuesSvc = issues.New(st, c.ident)
	c.issuesHandler = &issues.Handler{Svc: c.issuesSvc}
	// Wave C1 pulls (docs/features/03): PR threads over the shared
	// numbering/thread/index family, pr.json sidecars, the stamped
	// mergeable.json cache, the pull-merge/pull-mergeable/pull-fork
	// tasks, and the pulls event sink. Ref publishes funnel through
	// the WAL publish path (never force); the merge task calls 02's
	// ApplyClosingReferences seam via issuesSvc.
	c.pullsSvc, c.pullsHandler = newPullsService(st, c.ident, c.issuesSvc, reg, cfg.Git.Binary)
	// Wave C2 review (docs/features/04): immutable reviews, CAS'd
	// line-anchored threads, the review-requests index, the
	// review_summary render cache, and the required-reviews
	// merge-time half (consulted by the pull-merge task through
	// pulls' ReviewGate seam — the merge logic is NOT forked).
	// Suggest's commit authors ride pulls' HeadAuthors; the
	// push-time half (policy.RequiredReviewsEffect) enforces at
	// receive-pack with no wiring. Registers NO task kinds.
	c.reviewSvc, c.reviewHandler = newReviewService(st, c.ident, c.pullsSvc)
	// Wave 05 checks (docs/features/05): commit statuses (Create-
	// then-CAS per (sha, context)), the CAS'd checks/index.json
	// projection with inline compaction, wct_ CI tokens (Seam 2
	// shape → unprivileged ci:<id> principal, capability checked
	// handler-side), the combined worst-of view, and the
	// require_checks merge-time half (consulted by the pull-merge
	// task through pulls' ChecksGate seam — the merge logic is NOT
	// forked). The push-time half needs no wiring: protect ignores
	// require_checks on the push path by construction.
	c.checksSvc, c.checksHandler = newChecksService(st, c.ident, c.pullsSvc, reg, cfg.Git.Binary)
	// Feature 07 releases (docs/features/07 §§1–3): release headers,
	// asset bytes (two-step upload, static serving), the monotonic
	// latest pointer, and changelog autodraft. Publish fan-out rides
	// internal/notify through the nil-safe seams bound below (P8).
	c.releasesSvc, c.releasesHandler = newReleasesService(st, c.ident, reg, cfg.Git.Binary, cfg.Cache.Dir, int64(cfg.Releases.MaxAssetBytes))
	// Feature 07 social (docs/features/07 §§4–6): stars, watcher
	// reads, counters, starred lists. Watch mutation stays in
	// internal/notify (06 §6); the fork counter binds onto pulls
	// below (07 §6).
	c.socialSvc, c.socialHandler = newSocialService(st, c.ident)
	// Feature 06 notifications (docs/features/06): the fan-out
	// layer — notification objects, per-user indexes, the activity
	// log, per-user SSE, repo webhooks, retention. The REAL
	// Emitter/Streamer implementations bind onto the nil-safe
	// seams here, so issues/pulls/review/checks mutations fan out
	// synchronously (P8) from this boot forward.
	c.notifySvc, c.notifyHandler = newNotifyService(st, c.ident)
	wireNotifyFanout(c.notifySvc, c.issuesSvc, c.pullsSvc, c.reviewSvc, c.checksSvc)
	wireReleasesFanout(c.releasesSvc, c.notifySvc)
	wireSocialForks(c.socialSvc, c.pullsSvc)
	// Feature 08 §4: access.json CAS commits publish the "access"
	// collab frame (nil-safe seam on the identity service; the doc
	// stays the backfill truth).
	if c.ident != nil {
		c.ident.Stream = func(ctx context.Context, repo string) {
			c.notifySvc.PublishFrame(notify.RepoFrame{Name: "access", Repo: repo})
		}
	}
	return c
}

// chainCollab fronts the server mux with every collab surface (Seam 1,
// one block per package); authentication resolves through the server
// chain (Seam 2). Nil handlers are skipped (tests may wire a subset).
func chainCollab(srv *server.Server, c *collabWiring) {
	if c == nil {
		return
	}
	if c.identHandler != nil {
		// Chain the identity surface in front of the core api mux (Seam 1);
		// authentication resolves through the server chain (Seam 2).
		c.identHandler.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return srv.Auth().Authenticate(r, srv.Config())
		}
		srv.ChainExtra(c.identHandler)
	}
	if c.issuesHandler != nil {
		// Chain the issues surface in front of the core api mux (Seam 1);
		// authentication resolves through the server chain (Seam 2).
		c.issuesHandler.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
			return srv.Auth().Authenticate(r, srv.Config())
		}
		srv.ChainExtra(c.issuesHandler)
	}
	if c.pullsHandler != nil {
		chainPulls(srv, c.pullsHandler)
	}
	if c.reviewHandler != nil {
		chainReview(srv, c.reviewHandler)
	}
	if c.checksHandler != nil {
		chainChecks(srv, c.checksHandler)
	}
	if c.releasesHandler != nil {
		chainReleases(srv, c.releasesHandler)
	}
	if c.socialHandler != nil {
		chainSocial(srv, c.socialHandler)
	}
	if c.notifyHandler != nil {
		chainNotify(srv, c.notifyHandler)
	}
}
