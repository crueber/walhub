package server

import (
	"context"
	"net/http"

	"git.packden.us/crueber/walhub/internal/api"
	"git.packden.us/crueber/walhub/internal/git"
	"git.packden.us/crueber/walhub/internal/server/auth"
)

// RouteProvider is the internal/api seam (§2.4, 14_extensibility.md §14.3).
// The landed internal/api surface is api.Mount(env) — a self-contained mux
// whose ServeMux patterns cover the non-repo routes (/api/v1, /api-browser/v1,
// /services/api twins) and whose fallback is api.Dispatch, which strips the
// lane, strips .git, parses the repo id, matches the route table, and enforces
// 405/404 itself. The integration pass reconciles drift if the export surface
// moves to api.Build(env) per the doc.
type RouteProvider interface {
	// Serve answers one request whose path is the full request path
	// (/api*, /api-browser*, /services/api*, or a repo lane).
	Serve(w http.ResponseWriter, r *http.Request)
	// Owners lists owner names for the SPA text home (§3.3 ?format=text).
	Owners(r *http.Request) ([]string, error)
}

// apiProvider adapts *api.Env through api.Mount (§14.3 seam).
type apiProvider struct {
	env *api.Env
	mux http.Handler
}

// NewAPIProvider builds the production RouteProvider over the api.Env
// constructed by the composition (bind_wal.go provides the production
// RepoView/RepoRegistry bindings).
func NewAPIProvider(env *api.Env) RouteProvider {
	return &apiProvider{env: env, mux: api.Mount(env)}
}

func (p *apiProvider) Serve(w http.ResponseWriter, r *http.Request) {
	p.mux.ServeHTTP(w, r)
}

// Owners lists owner names from the registry (07_api.md §8: from the STORE,
// never a disk directory), visibility-filtered exactly like the
// /api/v1/owners surface (Forgejo #345): owners with no repo readable by
// the request caller are omitted, so the /explore text twin never leaks a
// private-only owner. The principal rides the injected request identity
// (the explore page is gated, so it is always resolved here).
func (p *apiProvider) Owners(r *http.Request) ([]string, error) {
	return p.env.VisibleOwners(r.Context(), p.env.PrincipalOf(r))
}

// ExtraRoutes is implemented by feature packages' HTTP surfaces
// (docs/features/01 §8, Seam 1): Handle answers the request when the path
// is the feature's, and reports false so the core mux answers otherwise.
// Registered in composition order; the first match wins.
type ExtraRoutes interface {
	Handle(w http.ResponseWriter, r *http.Request) bool
}

// ReadGate is the require_read hook consulted on the git/LFS read paths
// after principal resolution (docs/features/01 §4.1). Implemented by
// internal/identity; nil → legacy flag-only read gating.
type ReadGate interface {
	CheckRead(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError
}

// chainedAPI fronts the core api provider with feature surfaces.
type chainedAPI struct {
	primary RouteProvider
	extras  []ExtraRoutes
}

// ChainAPI builds the production RouteProvider: feature surfaces first,
// then the core api mux. Additive — core routes are untouched.
func ChainAPI(primary RouteProvider, extras ...ExtraRoutes) RouteProvider {
	return &chainedAPI{primary: primary, extras: extras}
}

func (c *chainedAPI) Serve(w http.ResponseWriter, r *http.Request) {
	for _, x := range c.extras {
		if x.Handle(w, r) {
			return
		}
	}
	c.primary.Serve(w, r)
}

func (c *chainedAPI) Owners(r *http.Request) ([]string, error) {
	return c.primary.Owners(r)
}

// ChainExtra fronts the api seam with feature surfaces (Seam 1
// registration, one block per package). Nil-safe: without an api seam
// there is nothing to front.
func (s *Server) ChainExtra(extras ...ExtraRoutes) {
	if s.api == nil {
		return
	}
	s.api = ChainAPI(s.api, extras...)
}

// checkReadGate consults the identity require_read hook (01 §4.1) on read
// paths. A nil gate allows (legacy behavior, unchanged for instances
// without the identity surface wired).
func (s *Server) checkReadGate(ctx context.Context, owner, repo string, p auth.Principal) *auth.AuthError {
	if s.readGate == nil {
		return nil
	}
	return s.readGate.CheckRead(ctx, owner, repo, p)
}

// gateRepoRead enforces the repo read gate on the git/LFS/bundle paths
// (Forgejo #345): a wired ReadGate is the authority — visibility decides,
// so anonymous callers reach public repos even with anonymous_read=false
// — while a nil gate keeps the legacy anonymous_read flag check. Reports
// through the §4.2 git failure mapping; false = response written.
func (s *Server) gateRepoRead(w http.ResponseWriter, r *http.Request, svc git.Service, id git.RepoId, p auth.Principal) bool {
	if s.readGate != nil {
		if aerr := s.checkReadGate(r.Context(), id.Owner, id.Name, p); aerr != nil {
			s.gitAuthFailure(w, r, svc, aerr)
			return false
		}
		return true
	}
	if aerr := requireRead(p, s.cfg.Server.Auth.AnonymousRead); aerr != nil {
		s.gitAuthFailure(w, r, svc, aerr)
		return false
	}
	return true
}
