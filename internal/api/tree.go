package api

import (
	"encoding/json"
	"net/http"
)

// --- GET …/tree/{rest} (§9.4: rev may contain slashes — Resolve splits) --------

func (h *handlers) tree(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	// Greedy tail (issue #251): the rev may itself contain slashes, so the
	// handler passes the whole tail to Resolve and lets its longest-prefix
	// match split ref from path — the same treatment resolve already has.
	rest := r.PathValue("rest")
	res, err := h.env.Repo.Resolve(r.Context(), RepoOf(r), rest)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	path := res.Path
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
	if revIsFullSHA(refPartOf(rest, path)) {
		class = ccImmutable
	}
	writeBody(w, r, class, res.SHA, http.StatusOK, body)
}
