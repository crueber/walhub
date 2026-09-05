// review.go — Wave C2 composition (docs/features/04): the review service
// (Seam 1, both lanes) over the P6 roles owned by identity, the shared
// thread family owned by issues, and the pr.json sidecars owned by pulls.
// Review state never touches the WAL; the merge-time gate is consulted by
// 03's merge task through pulls' ReviewGate seam (see
// internal/pulls/review.go — the merge logic is NOT forked).
package main

import (
	"net/http"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/pulls"
	"git.packden.us/crueber/walhub/internal/review"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/store"
)

// newReviewService builds the review service over st/ident/pullsSvc.
// Notify/Stream stay nil until internal/notify lands (documented no-op,
// P8 backfill via the timeline). Suggest's team expansion rides
// identity's ExpandGroups; its commit authors ride pulls' HeadAuthors.
func newReviewService(st store.ObjectStore, ident *identity.Service, pullsSvc *pulls.Service) (*review.Service, *review.Handler) {
	svc := review.New(st, ident)
	if ident != nil {
		svc.Expander = ident
	}
	if pullsSvc != nil {
		svc.Authors = pullsSvc
		pullsSvc.Reviews = svc
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
