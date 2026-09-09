// repos_detailed.go — GET /api/v1/owners/{owner}/repos/detailed (Forgejo #248,
// extended by Forgejo #247).
//
// NEW endpoint alongside the frozen v1 string lists (14 §14.12): the v1
// `ownerRepos` shape (bare strings) is untouched; this object-row surface
// carries queryable size + activity state (size_bytes/object_count per row,
// last_commit_sha/time + last_push_at per row, sort=size|activity, min/max
// byte filters) served from the aggregate catalog in ONE object read
// regardless of repo count. Deleting the catalog degrades to null rows,
// never an error (optional/rebuildable contract).
//
// Size semantic: stored-object size (packs+idx), not checkout/LFS/bundles;
// rows with null size_bytes are unknown/unbackfilled (UI hides), 0 is a
// verified-empty repo. Activity semantic (#247): HEAD-tip commit with
// commit-date semantics; nulls = unknown/unbackfilled (UI keeps current
// behavior — stamps fall back to the per-row fetch), except verified-empty
// repos (size 0) which render "no commits yet" without a fetch.
package api

import (
	"net/http"
	"strconv"
	"strings"

	"git.packden.us/crueber/walhub/internal/sizecatalog"
)

// RepoSizeRow is one detailed listing row (additive shape, 14 §14.12 field
// rule: consumers ignore unknown fields; arrays stay [] never null; nulls
// stay explicit — no omitempty — so unknown is distinguishable).
type RepoSizeRow struct {
	Name           string  `json:"name"`
	SizeBytes      *uint64 `json:"size_bytes"` // nil = unknown/unbackfilled
	ObjectCount    *uint64 `json:"object_count,omitempty"`
	HeadSeq        *uint64 `json:"head_seq,omitempty"`
	UpdatedAt      *string `json:"updated_at,omitempty"`
	LastCommitSHA  *string `json:"last_commit_sha"`  // nil = unknown/unbackfilled
	LastCommitTime *string `json:"last_commit_time"` // RFC 3339 UTC, nil = unknown
	LastPushAt     *string `json:"last_push_at"`     // RFC 3339 UTC, nil = unknown
}

// ownerReposDetailed serves the object-row listing with size + activity.
// Query: sort=name|size|activity (default name), order=asc|desc (default
// asc), min_bytes=/max_bytes= (uint64; unknown rows never match a bound).
// sort=activity orders by last_commit_time (unknowns always last, ties on
// (owner,name)); the explore page uses sort=activity&order=desc.
// Cache class: SWR (ref-dependent listing, same as ownerRepos).
func (h *handlers) ownerReposDetailed(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthRead) {
		return
	}
	if h.env.Repos == nil {
		writePlain(w, http.StatusServiceUnavailable, "repo registry not configured")
		return
	}
	owner := r.PathValue("owner")
	// 200 {repos:[]} for an unknown owner — never 404 (§8, same as v1).
	names, err := h.env.Repos.Repos(r.Context(), owner)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	sortKey := strings.ToLower(r.URL.Query().Get("sort"))
	if sortKey != "size" && sortKey != "activity" {
		sortKey = "name"
	}
	order := strings.ToLower(r.URL.Query().Get("order"))
	if order != "desc" {
		order = "asc"
	}
	minBytes, err := parseBytesParam(r.URL.Query().Get("min_bytes"))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "invalid min_bytes")
		return
	}
	maxBytes, err := parseBytesParam(r.URL.Query().Get("max_bytes"))
	if err != nil {
		writePlain(w, http.StatusBadRequest, "invalid max_bytes")
		return
	}
	if minBytes != nil && maxBytes != nil && *minBytes > *maxBytes {
		writePlain(w, http.StatusBadRequest, "min_bytes exceeds max_bytes")
		return
	}

	// ONE object read regardless of repo count (law 6): the aggregate
	// catalog. Absent/corrupt-catalog → null rows (degraded, never 500 for
	// a missing optional object; corrupt IS a 500 — the bucket is wrong).
	rows := make([]sizecatalog.Row, 0, len(names))
	if h.env.Store != nil {
		cat, cerr := sizecatalog.ReadCatalog(r.Context(), h.env.Store)
		if cerr != nil {
			mapViewErr(w, cerr)
			return
		}
		rows = sizecatalog.RowsForOwner(owner, names, cat)
	} else {
		for _, n := range names {
			rows = append(rows, sizecatalog.Row{Owner: owner, Name: n})
		}
	}
	rows = sizecatalog.FilterSort(rows, sortKey, order, minBytes, maxBytes)
	out := make([]RepoSizeRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, RepoSizeRow{
			Name: row.Name, SizeBytes: row.SizeBytes,
			ObjectCount: row.ObjectCount, HeadSeq: row.HeadSeq, UpdatedAt: row.UpdatedAt,
			LastCommitSHA: row.LastCommitSHA, LastCommitTime: row.LastCommitTime, LastPushAt: row.LastPushAt,
		})
	}
	writeCached(w, r, ccSWR, "", http.StatusOK, struct {
		Repos []RepoSizeRow `json:"repos"`
	}{Repos: out})
}

func parseBytesParam(s string) (*uint64, error) {
	if s == "" {
		return nil, nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return nil, err
	}
	return &n, nil
}
