package api

import (
	"encoding/json"
	"net/http"
)

// --- GET …/commit/{sha} (§9.8) --------------------------------------------------------

func (h *handlers) commitDetail(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	shaIn := r.PathValue("sha")
	res, err := h.env.Repo.Resolve(r.Context(), RepoOf(r), shaIn)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	ctx := r.Context()
	render := func() ([]byte, error) {
		detail, err := h.env.Repo.Commit(ctx, RepoOf(r), res.SHA)
		if err != nil {
			return nil, err
		}
		detail.Stats = nonNil(detail.Stats)
		return json.Marshal(detail)
	}
	// A short sha resolving here renders under the full-sha key (§9.8).
	body, err := h.env.renderImmutable(ctx, "commit/"+res.SHA, res.Revision, res.SHA, render)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	class := ccSWR
	if revIsFullSHA(shaIn) {
		class = ccImmutable
	}
	writeBody(w, r, class, res.SHA, http.StatusOK, body)
}
