package server

import (
	"net/http"

	"git.packden.us/crueber/walhub/internal/git"
)

// repo_extra.go — the repo-subpath seam (14_extensibility.md §14.3 routing
// note, as required by docs/features/07 §1.2): feature packages serving
// bytes OUTSIDE the api lanes (a new `/{o}/{r}/<sub>` family) register a
// RepoRoutes here; repoDispatch consults it on paths the core switch does
// not claim. Bytes served through this seam are never compressed (no
// compress group wraps repoDispatch) and carry the static contract
// (immutable, ETag, Range) from the feature handler itself.

// RepoRoutes answers one repo-subpath request; false when the sub-path is
// not the feature's (the server falls through to its 404). Registered in
// composition order; the first match wins. Implementations authenticate in
// the handler (the same principal injection the lane handlers see is NOT
// applied here — the byte route is outside the api seam).
type RepoRoutes interface {
	HandleRepo(w http.ResponseWriter, r *http.Request, id git.RepoId, sub []string) bool
}

// ChainRepo fronts repoDispatch with feature repo-subpath surfaces (Seam 1
// registration for non-lane families). Nil-safe.
func (s *Server) ChainRepo(extras ...RepoRoutes) {
	s.repoExtras = append(s.repoExtras, extras...)
}

// repoDispatchRepoExtras runs the repo-subpath chain; false when unclaimed.
func (s *Server) repoDispatchRepoExtras(w http.ResponseWriter, r *http.Request, id git.RepoId, sub []string) bool {
	for _, x := range s.repoExtras {
		if x.HandleRepo(w, r, id, sub) {
			return true
		}
	}
	return false
}
