package server

import (
	"net/http"

	"git.packden.us/crueber/walhub/internal/api"
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
// never a disk directory).
func (p *apiProvider) Owners(r *http.Request) ([]string, error) {
	return p.env.Repos.Owners(r.Context())
}
