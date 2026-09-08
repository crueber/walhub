package api

import (
	"errors"
	"net/http"

	"git.packden.us/crueber/walhub/internal/git"
)

// --- GET/PUT/DELETE {lane} — repo summary, create, delete (§9.1) ----------------------

type summaryBody struct {
	Owner        string           `json:"owner"`
	Name         string           `json:"name"`
	FullName     string           `json:"full_name"`
	Head         *Ref             `json:"head"` // null = unborn (the one sanctioned null)
	Branches     int              `json:"branches"`
	Tags         int              `json:"tags"`
	Health       string           `json:"health"` // empty|healthy|degraded (§9.1; issue #209)
	MissingTotal uint64           `json:"missing_total,omitempty"`
	Placeholder  *PlaceholderInfo `json:"placeholder,omitempty"` // #210 R1 B1: sidecar projection, empty repos only
	CloneURL     string           `json:"clone_url"`
	HTMLURL      string           `json:"html_url"`
	APIURL       string           `json:"api_url"`
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
	var placeholder *PlaceholderInfo
	if health == RepoHealthEmpty && s.Head == nil && s.Branches == 0 && s.Tags == 0 {
		// The placeholder projection (R1 B1): one exact-key probe, ONLY
		// when the in-hand data already says empty — real repos pay +0
		// round trips. Affordances key on refs==0 && marker, never marker
		// alone (a stale marker on a real repo renders as real).
		if doc, ok := probePlaceholder(r.Context(), h.env.Store, id); ok && doc != nil {
			placeholder = &PlaceholderInfo{CreatedBy: doc.CreatedBy, CreatedAt: doc.CreatedAt, ExpiresAt: doc.ExpiresAt}
		}
	}
	body := summaryBody{
		Owner:        id.Owner,
		Name:         id.Name,
		FullName:     id.Owner + "/" + id.Name,
		Head:         s.Head,
		Branches:     s.Branches,
		Tags:         s.Tags,
		Health:       health,
		MissingTotal: missingTotal,
		Placeholder:  placeholder,
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

// repoPut creates a repo (?object_format=sha1|sha256, ?placeholder=true);
// 201/409 (§9.1, #210 §3).
//
// ?placeholder=true selects placeholder create-semantics on the existing
// PUT (R1 B3: the flag AS the shape, not a shim — justified in Decisions):
// the manifest is created as today plus the meta/placeholder.json sidecar
// and the eager access.json default. Without the flag the path is
// byte-identical to today. Idempotency (§6): same-principal re-create of an
// unborn placeholder → 200 already:true; different principal (or PUT
// without the flag on a placeholder) → legacy 409 plain-text.
func (h *handlers) repoPut(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthWrite) {
		return
	}
	id := RepoOf(r)
	placeholder := r.URL.Query().Get("placeholder") == "true"
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
	principal := h.env.PrincipalOf(r).Name
	if h.checkOrgGate(w, r, id, principal) {
		return
	}
	if err := h.env.Repos.Create(r.Context(), id, format); err != nil {
		if errors.Is(err, ErrExists) {
			if placeholder {
				if doc, ok := probePlaceholder(r.Context(), h.env.Store, id); ok && doc != nil {
					if samePrincipal(doc.CreatedBy, principal) && h.stillUnborn(r.Context(), id) {
						h.writeCreateJSON(w, r, id, true, http.StatusOK)
						return
					}
				}
				writePlain(w, http.StatusConflict, h.conflictBody(r, id))
				return
			}
			writePlain(w, http.StatusConflict, "repository already exists")
			return
		}
		mapViewErr(w, err)
		return
	}
	if placeholder {
		// The PUT-flag path has no visibility concept: the eager default
		// is always public (the POST twin's visibility toggle is the
		// private path).
		if err := h.createPlaceholder(r, id, format, principal, ""); err != nil {
			mapViewErr(w, err)
			return
		}
		h.writeCreateJSON(w, r, id, false, http.StatusCreated)
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
