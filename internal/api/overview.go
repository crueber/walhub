package api

import (
	"encoding/json"
	"net/http"
)

// --- GET …/overview (§12.1, no-store) -------------------------------------------------

func (h *handlers) overview(w http.ResponseWriter, r *http.Request) {
	if !h.open(w, r, AuthRead) {
		return
	}
	id := RepoOf(r)
	ov, err := h.env.Repo.Overview(r.Context(), id)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	ov.Repo = id.Owner + "/" + id.Name
	ov.CloneURL = h.env.baseURL(r) + "/" + id.Owner + "/" + id.Name + ".git"
	if ov.Hostname == "" {
		ov.Hostname = h.env.Hostname
	}
	ov.Health.Issues = nonNil(ov.Health.Issues)
	ov.Health.Suggestions = nonNil(ov.Health.Suggestions)
	ov.Manifest.Segments = nonNil(ov.Manifest.Segments)
	ov.Bundles = nonNil(ov.Bundles)
	ov.BundlePlan.Slots = nonNil(ov.BundlePlan.Slots)
	ov.BundlePlan.Upcoming = nonNil(ov.BundlePlan.Upcoming)
	ov.BundlePlan.Maintainers = nonNil(ov.BundlePlan.Maintainers)
	ov.BundlePlan.Orphaned = nonNil(ov.BundlePlan.Orphaned)
	ov.Compactions = nonNil(ov.Compactions)
	if ov.Node.Counters == nil {
		ov.Node.Counters = map[string]uint64{}
	}
	raw, err := json.Marshal(ov)
	if err != nil {
		writePlain(w, http.StatusInternalServerError, "encode: "+err.Error())
		return
	}
	w.Header().Set("Cache-Control", ccNoStore)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
