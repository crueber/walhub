// owners_activity.go — Forgejo #283 server-side owner activity ordering.
//
// The v1 string lists stay byte-identical by default (14 §14.12): the rollup
// is served two additive ways over the same derived fold —
//   - GET /api/v1/owners?sort=activity&order=desc → the frozen []string,
//     activity-ordered (this is what fixes the >50-owner cap: the server
//     ranks over ALL owners before the client slices);
//   - GET /api/v1/owners/detailed?sort=&order= → {owners: [{name,
//     last_commit_sha|null, last_commit_time|null}]} object rows carrying the
//     per-owner max (max over the owner's repos; null when the owner has no
//     commits), triple twins + discovery + SDK, the #248 detailed precedent.
//
// Both read the aggregate catalog in ONE exact-key GET regardless of owner
// count (law 6; probe, don't list — law 4); the rollup itself is derived at
// request time (sizecatalog.OwnerRollups — one comparison per repo in
// memory), so there is no stored per-owner aggregate to backfill and no
// proto change. A new push updates its owner's rollup the moment its
// per-repo entry folds (the #247 incremental contract), without a rescan.
// Absent catalog degrades to name order / null rows (optional/rebuildable
// contract, same as /repos/detailed); a corrupt catalog is a 503 — the
// bucket is wrong (same mapping as ownerReposDetailed via mapViewErr).
package api

import (
	"net/http"
	"strings"

	"git.packden.us/crueber/walhub/internal/sizecatalog"
)

// OwnerActivityRow is one owners/detailed row (additive shape, 14 §14.12
// field rule: consumers ignore unknown fields; arrays stay [] never null;
// nulls stay explicit — no omitempty — so unknown is distinguishable from
// empty, mirroring RepoSizeRow).
type OwnerActivityRow struct {
	Name string `json:"name"`
	// LastCommitSHA is the tip sha of the owner's newest committed repo
	// (nil = unknown/unbackfilled — the owner has no repo with a known tip).
	LastCommitSHA *string `json:"last_commit_sha"`
	// LastCommitTime is the max last_commit_time over the owner's repos,
	// RFC 3339 UTC (nil = unknown/unbackfilled — sorts last, renders
	// without a stamp; never a fake epoch).
	LastCommitTime *string `json:"last_commit_time"`
}

// ownerSortParams parses the shared listing query: sort=name|activity
// (default name — the legacy store order), order=asc|desc (default asc).
// Unknown values degrade to the defaults, never a 400 (the /repos/detailed
// sort/order rule).
func ownerSortParams(r *http.Request) (sortKey, order string) {
	sortKey = strings.ToLower(r.URL.Query().Get("sort"))
	if sortKey != "activity" {
		sortKey = "name"
	}
	order = strings.ToLower(r.URL.Query().Get("order"))
	if order != "desc" {
		order = "asc"
	}
	return sortKey, order
}

// ownerRollups reads the aggregate catalog for an owners-listing query.
// Absent catalog → empty rollups (degraded name order / null rows, never an
// error); corrupt/unreadable → the mapped store error (503 via mapViewErr).
// Nil store → empty rollups (tests/handlers without a bucket).
func ownerRollups(r *http.Request, h *handlers) (map[string]sizecatalog.OwnerRollup, error) {
	rollups := map[string]sizecatalog.OwnerRollup{}
	if h.env.Store == nil {
		return rollups, nil
	}
	cat, err := sizecatalog.ReadCatalog(r.Context(), h.env.Store)
	if err != nil {
		return nil, err
	}
	return sizecatalog.OwnerRollups(cat), nil
}

// ownersDetailed serves the per-owner activity rows with the shared
// sort/order query. Membership is the registry Owners() list (the source of
// truth — the catalog never invents owners); activity rides the derived
// rollup. Cache class: SWR (ref-dependent listing, same as owners).
func (h *handlers) ownersDetailed(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthRead) {
		return
	}
	if h.env.Repos == nil {
		writePlain(w, http.StatusServiceUnavailable, "repo registry not configured")
		return
	}
	names, err := h.env.Repos.Owners(r.Context())
	if err != nil {
		mapViewErr(w, err)
		return
	}
	sortKey, order := ownerSortParams(r)
	rollups, err := ownerRollups(r, h)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	out := make([]OwnerActivityRow, 0, len(names))
	for _, n := range sizecatalog.SortOwners(names, rollups, sortKey, order) {
		row := OwnerActivityRow{Name: n}
		if rl, ok := rollups[n]; ok && rl.LastCommitTime != "" {
			ct := rl.LastCommitTime
			row.LastCommitTime = &ct
			if rl.LastCommitSHA != "" {
				sha := rl.LastCommitSHA
				row.LastCommitSHA = &sha
			}
		}
		out = append(out, row)
	}
	writeCached(w, r, ccSWR, "", http.StatusOK, struct {
		Owners []OwnerActivityRow `json:"owners"`
	}{Owners: out})
}
