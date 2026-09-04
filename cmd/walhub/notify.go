// notify.go — Feature 06 composition (docs/features/06): the notify
// service (Seam 1, both lanes) over the P6 roles owned by identity, and
// the REAL Emitter/Streamer implementations for the nil-safe seams the
// 02/03/04/05 waves left behind. Mutations now actually fan out:
// comments/assigns/reviews/checks write notification objects, append the
// activity log, and publish per-user SSE frames synchronously (P8).
// Watched repos resolve through the social.json watcher_list that the
// notify watch endpoints maintain until 07 lands.
package main

import (
	"context"
	"net/http"

	"git.packden.us/crueber/walhub/internal/checks"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/issues"
	"git.packden.us/crueber/walhub/internal/notify"
	"git.packden.us/crueber/walhub/internal/pulls"
	"git.packden.us/crueber/walhub/internal/review"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// newNotifyService builds the notify service over st/ident. Profiles and
// Teams ride identity (mention validation probes + @org/team expansion);
// nil ident leaves a valid service whose mention/team recipients drop
// (documented fail-closed for emission, fail-open nowhere).
func newNotifyService(st store.ObjectStore, ident *identity.Service) (*notify.Service, *notify.Handler) {
	svc := notify.New(st, ident)
	if ident != nil {
		svc.Profiles = ident
		svc.Teams = ident
	}
	h := &notify.Handler{Svc: svc}
	return svc, h
}

// chainNotify fronts the core mux with the notify surface (Seam 1);
// authentication resolves through the server chain (Seam 2).
func chainNotify(srv *server.Server, h *notify.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().Authenticate(r, srv.Config())
	}
	srv.ChainExtra(h)
}

// wireNotifyFanout binds the REAL emission implementations onto the
// nil-safe Emitter/Streamer seams the 02/03/04/05 waves left behind (P8:
// synchronous fan-out after each CAS commit). Every adapter is a thin
// type translation in composition — all fan-out logic lives in
// internal/notify, which never imports the feature packages (09 §2).
// Emission never blocks the mutating handler past the 5 s fan-out budget
// (overflow and shortfalls defer to the notify-fanout task); stream
// publishes are always non-blocking (drop-oldest).
func wireNotifyFanout(svc *notify.Service, issuesSvc *issues.Service, pullsSvc *pulls.Service, reviewSvc *review.Service, checksSvc *checks.Service) {
	if issuesSvc != nil {
		issuesSvc.Notify = func(ctx context.Context, ev issues.NotifyEvent) {
			svc.EmitIssue(ctx, ev.Repo, ev.IssueNum, ev.Class, ev.Actor, ev.At, ev.Action, ev.Recipients)
		}
		issuesSvc.Stream = func(ctx context.Context, ev issues.StreamEvent) {
			svc.PublishFrame(notify.RepoFrame{Name: ev.Name, Repo: ev.Repo, Num: ev.IssueNum, Seq: ev.Seq})
		}
	}
	if pullsSvc != nil {
		pullsSvc.Notify = func(ctx context.Context, ev pulls.NotifyEvent) {
			svc.EmitPull(ctx, ev.Repo, ev.PullNum, ev.Class, ev.Actor, ev.At, ev.Recipients)
		}
		pullsSvc.Stream = func(ctx context.Context, ev pulls.StreamEvent) {
			svc.PublishFrame(notify.RepoFrame{Name: ev.Name, Repo: ev.Repo, Action: ev.Action, Num: ev.Num, Title: ev.Title, State: ev.State, Sha: ev.HeadSHA})
		}
	}
	if reviewSvc != nil {
		reviewSvc.Notify = func(ctx context.Context, ev review.NotifyEvent) {
			svc.EmitReview(ctx, ev.Repo, ev.PullNum, ev.Class, ev.Actor, ev.At, ev.Recipients)
		}
		reviewSvc.Stream = func(ctx context.Context, ev review.StreamEvent) {
			svc.PublishFrame(notify.RepoFrame{Name: ev.Name, Repo: ev.Repo, Action: ev.Action, Num: ev.Num, Tid: ev.TID})
		}
	}
	if checksSvc != nil {
		checksSvc.Notify = func(ctx context.Context, ev checks.NotifyEvent) {
			svc.EmitCheck(ctx, ev.Repo, ev.SHA, ev.Context, ev.State, ev.Description, ev.TargetURL, ev.Actor, ev.At, ev.PR)
		}
		checksSvc.Stream = func(ctx context.Context, ev checks.StreamEvent) {
			svc.PublishFrame(notify.RepoFrame{Name: ev.Name, Repo: ev.Repo, Sha: ev.SHA, Context: ev.Context, State: ev.State, Combined: ev.CombinedState, At: ev.UpdatedAt})
		}
	}
}

// compile-time seam assertions: composition consumes exactly the narrow
// interfaces the feature packages define (core never imports upward).
var (
	_ notify.RoleService   = (*identity.Service)(nil)
	_ notify.ProfileProber = (*identity.Service)(nil)
	_ notify.TeamReader    = (*identity.Service)(nil)
	_ server.ExtraRoutes   = (*notify.Handler)(nil)
)
