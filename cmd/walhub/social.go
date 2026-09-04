// social.go — Feature 07 composition (docs/features/07 §§4–6): the social
// service (Seam 1, both lanes: stars, GET social, starred twins) over the
// P6 roles owned by identity. Watch mutation stays in internal/notify
// (06 §6 routes — single HTTP owner; social.json converges through the
// field-scoped CAS loops). The fork counter rides pulls' ForksCounter
// seam, wired here.
package main

import (
	"net/http"

	"git.packden.us/crueber/walhub/internal/identity"
	"git.packden.us/crueber/walhub/internal/pulls"
	"git.packden.us/crueber/walhub/internal/server"
	"git.packden.us/crueber/walhub/internal/server/auth"
	"git.packden.us/crueber/walhub/internal/social"
	"git.packden.us/crueber/walhub/internal/store"
)

// newSocialService builds the social service over st/ident.
func newSocialService(st store.ObjectStore, ident *identity.Service) (*social.Service, *social.Handler) {
	svc := social.New(st, ident)
	h := &social.Handler{Svc: svc}
	return svc, h
}

// chainSocial fronts the core mux with the social surface (Seam 1);
// authentication resolves through the server chain (Seam 2).
func chainSocial(srv *server.Server, h *social.Handler) {
	h.Auth = func(r *http.Request) (auth.Principal, *auth.AuthError) {
		return srv.Auth().Authenticate(r, srv.Config())
	}
	srv.ChainExtra(h)
}

// wireSocialForks binds the social fork counter onto pulls' ForksCounter
// seam (07 §6: the fork task's completion step increments the parent's
// social.json forks). Nil-safe: without a social service the fork task
// simply skips the increment.
func wireSocialForks(svc *social.Service, pullsSvc *pulls.Service) {
	if svc == nil || pullsSvc == nil {
		return
	}
	pullsSvc.Forks = svc
}

// compile-time seam assertions: composition consumes exactly the narrow
// interfaces the social package defines (core never imports social; pulls
// never imports social — the seam is the ForksCounter interface).
var (
	_ social.RoleService = (*identity.Service)(nil)
	_ pulls.ForksCounter = (*social.Service)(nil)
	_ server.ExtraRoutes = (*social.Handler)(nil)
)
