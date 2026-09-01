package api

import (
	"encoding/json"
	"net/http"
)

// --- GET …/tree/{rev}[/{path}] (§9.4) -----------------------------------------------

func (h *handlers) tree(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	rev := r.PathValue("rev")
	path := r.PathValue("path") // "" at the repo root
	res, err := h.env.Repo.Resolve(r.Context(), RepoOf(r), rev)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	ctx := r.Context()
	render := func() ([]byte, error) {
		tr, err := h.env.Repo.Tree(ctx, RepoOf(r), res.SHA, path)
		if err != nil {
			return nil, err
		}
		if path == "" {
			tr.Commit = nil // `commit` present only when path is non-empty
		}
		tr.Ref, tr.SHA, tr.Path = res.Ref, res.SHA, path
		tr.Entries = nonNil(tr.Entries)
		return json.Marshal(tr)
	}
	body, err := h.env.renderImmutable(ctx, "tree/"+res.SHA+"/"+path, res.Revision, res.SHA, render)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	class := ccSWR
	if revIsFullSHA(rev) {
		class = ccImmutable
	}
	writeBody(w, r, class, res.SHA, http.StatusOK, body)
}
