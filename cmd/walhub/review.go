// review.go — Wave C2 composition (docs/features/04): the review service
// (Seam 1, both lanes) over the P6 roles owned by identity, the shared
// thread family owned by issues, and the pr.json sidecars owned by pulls.
// Review state never touches the WAL; the merge-time gate is consulted by
// 03's merge task through pulls' ReviewGate seam (see
// internal/pulls/review.go — the merge logic is NOT forked).
package main

import (
	"context"
	"net/http"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/pulls"
	"git.packden.us/crueber/walhub/internal/review"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
	"git.packden.us/crueber/walhub/internal/wal"
)

// newReviewService builds the review service over st/ident/pullsSvc.
// Notify/Stream stay nil until internal/notify lands (documented no-op,
// P8 backfill via the timeline). Suggest's team expansion rides
// identity's ExpandGroups; its commit authors ride pulls' HeadAuthors.
// The self-approval policy (Forgejo #586) rides the WAL manifest: the
// seam opens the handle (the production precedent — every engine op
// opens; the warm path for an active PR is a mutex hit plus an
// in-memory snapshot, no store call) and parses the settings TOML's
// [review] section, failing open to the default on any unparseable body
// (the config.AllowSelfApprovalOf contract — unpublishable bodies never
// reach here anyway). An unopenable repo fails the submit closed (503),
// never silently allowed or denied: no locks are held across the call
// (13 §2 rule 4 — SubmitReview holds none), and submits are
// control-plane-sized, off the push/sync hot-path budgets (the same cost
// class as the gate's policy.json read).
func newReviewService(st store.ObjectStore, ident *identity.Service, pullsSvc *pulls.Service, reg *wal.Registry) (*review.Service, *review.Handler) {
	api.RegisterExposed(review.ExposedTemplates...)
	svc := review.New(st, ident)
	if ident != nil {
		svc.Expander = ident
	}
	if pullsSvc != nil {
		svc.Authors = pullsSvc
		pullsSvc.Reviews = svc
	}
	if reg != nil {
		svc.Settings = func(ctx context.Context, owner, repo string) (review.ReviewSettings, error) {
			h, err := reg.Open(ctx, owner+"/"+repo)
			if err != nil {
				return review.ReviewSettings{}, err
			}
			if m, _ := h.ManifestSnapshot(); m != nil && m.Settings != nil {
				return review.ReviewSettings{
					AllowSelfApproval: config.AllowSelfApprovalOf([]byte(m.Settings.Toml)),
				}, nil
			}
			return review.DefaultReviewSettings(), nil
		}
	}
	h := &review.Handler{Svc: svc}
	return svc, h
}

// chainReview fronts the core mux with the review surface (Seam 1);
// authentication resolves through the server chain (Seam 2), including the
// §8.6 broker-forwarding rule — the forwarded principal replaces the
// broker's, never the broker itself.
func chainReview(srv *server.Server, h *review.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().AuthenticateForwarded(r, srv.Config())
	}
	srv.ChainExtra(h)
}
