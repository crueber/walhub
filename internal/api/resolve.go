package api

import (
	"net/http"
)

// --- GET …/resolve[/{rest}] (§9.3) --------------------------------------------------

// resolveBody is the wire shape: {ref, sha, path, kind}.
type resolveBody struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Path string `json:"path"`
	Kind string `json:"kind"`
}

func (h *handlers) resolve(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	rest := r.PathValue("rest") // "" → default branch
	res, err := h.env.Repo.Resolve(r.Context(), RepoOf(r), rest)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	writeCached(w, r, ccSWR, res.SHA, http.StatusOK, resolveBody{
		Ref:  res.Ref,
		SHA:  res.SHA,
		Path: res.Path,
		Kind: res.Kind,
	})
}
