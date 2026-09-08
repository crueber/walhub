package api

import (
	"errors"
	"net/http"

	"git.packden.us/crueber/walhub/internal/git"
)

// --- GET/PUT/DELETE {lane} — repo summary, create, delete (§9.1) ----------------------

type summaryBody struct {
	Owner        string `json:"owner"`
	Name         string `json:"name"`
	FullName     string `json:"full_name"`
	Head         *Ref   `json:"head"` // null = unborn (the one sanctioned null)
	Branches     int    `json:"branches"`
	Tags         int    `json:"tags"`
	Health       string `json:"health"` // empty|healthy|degraded (§9.1; issue #209)
	MissingTotal uint64 `json:"missing_total,omitempty"`
	CloneURL     string `json:"clone_url"`
	HTMLURL      string `json:"html_url"`
	APIURL       string `json:"api_url"`
}

func (h *handlers) summary(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	id := RepoOf(r)
	s, err := h.env.Repo.Summary(r.Context(), id)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	health := s.Health
	if health == "" {
		health = classifyHealth(s) // views that predate the field (test fakes)
	}
	var missingTotal uint64
	if health != RepoHealthEmpty {
		// Degraded override from the cached fsck.pb report: one conditional
		// GET, off the law-6 budgeted paths (push/sync/checkpoint never call
		// here; R1-B1 cost class). Empty repos skip the probe — the branch
		// is on data in hand, so the waveB path costs +0 round trips.
		if rep, ok := probeFsck(r.Context(), h.env.Store, id); ok && fsckHasMissing(rep) {
			health = RepoHealthDegraded
			missingTotal = fsckMissingTotal(rep)
		}
	}
	base := h.env.baseURL(r)
	body := summaryBody{
		Owner:        id.Owner,
		Name:         id.Name,
		FullName:     id.Owner + "/" + id.Name,
		Head:         s.Head,
		Branches:     s.Branches,
		Tags:         s.Tags,
		Health:       health,
		MissingTotal: missingTotal,
		CloneURL:     base + "/" + id.Owner + "/" + id.Name + ".git",
		HTMLURL:      base + "/" + id.Owner + "/" + id.Name,
		APIURL:       base + "/" + id.Owner + "/" + id.Name + "/api",
	}
	etag := ""
	if s.Head != nil {
		etag = s.Head.SHA
	}
	if health == RepoHealthDegraded {
		// The degraded flip must bust the SWR cache: same head sha, new
		// state — without the suffix a revalidating client would 304 and
		// keep showing healthy (review S1: ETag covers the health field).
		etag += "~degraded"
	}
	writeCached(w, r, ccSWR, etag, http.StatusOK, body)
}

// repoPut creates a repo (?object_format=sha1|sha256); 201/409 (§9.1).
func (h *handlers) repoPut(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthWrite) {
		return
	}
	id := RepoOf(r)
	format := git.Sha1
	if s := r.URL.Query().Get("object_format"); s != "" {
		f, err := git.ObjectFormatFrom(s)
		if err != nil {
			writePlain(w, http.StatusBadRequest, err.Error())
			return
		}
		format = f
	}
	if h.env.Repos == nil {
		writePlain(w, http.StatusServiceUnavailable, "repo registry not configured")
		return
	}
	if err := h.env.Repos.Create(r.Context(), id, format); err != nil {
		if errors.Is(err, ErrExists) {
			writePlain(w, http.StatusConflict, "repository already exists")
			return
		}
		mapViewErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"owner":     id.Owner,
		"name":      id.Name,
		"full_name": id.Owner + "/" + id.Name,
	})
}

// repoDelete removes a repo (admin) → 204.
func (h *handlers) repoDelete(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthAdmin) {
		return
	}
	if h.env.Repos == nil {
		writePlain(w, http.StatusServiceUnavailable, "repo registry not configured")
		return
	}
	if err := h.env.Repos.Delete(r.Context(), RepoOf(r)); err != nil {
		mapViewErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
