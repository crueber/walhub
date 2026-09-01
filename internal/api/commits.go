package api

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// --- GET …/commits?ref=&path=&skip=&n= (§9.6) ----------------------------------------

func (h *handlers) commits(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	qs := r.URL.Query()
	ref := qs.Get("ref")
	if ref == "" {
		ref = "HEAD"
	}
	path := qs.Get("path")
	skip := 0
	if s := qs.Get("skip"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			writePlain(w, http.StatusBadRequest, "invalid skip")
			return
		}
		skip = v
	}
	n := 35
	if s := qs.Get("n"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 {
			writePlain(w, http.StatusBadRequest, "invalid n")
			return
		}
		if v > 200 {
			v = 200
		}
		n = v
	}
	res, err := h.env.Repo.Resolve(r.Context(), RepoOf(r), ref)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	ctx := r.Context()
	render := func() ([]byte, error) {
		page, err := h.env.Repo.Commits(ctx, RepoOf(r), res.SHA, path, skip, n)
		if err != nil {
			return nil, err
		}
		page.Ref, page.SHA = res.Ref, res.SHA
		page.Commits = nonNil(page.Commits)
		return json.Marshal(page)
	}
	body, err := h.env.renderImmutable(ctx,
		"commits/"+res.SHA+"/"+path+"/"+strconv.Itoa(skip)+"/"+strconv.Itoa(n),
		res.Revision, res.SHA, render)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	class := ccSWR
	if revIsFullSHA(ref) {
		class = ccImmutable
	}
	writeBody(w, r, class, res.SHA, http.StatusOK, body)
}
