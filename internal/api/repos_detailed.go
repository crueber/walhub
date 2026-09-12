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
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"git.packden.us/crueber/walhub/internal/sizecatalog"
	"git.packden.us/crueber/walhub/internal/store"
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
	// Mirror is true iff the pull-only mirror sidecar
	// (repos/<o>/<r>/meta/mirror.json) exists (Forgejo #281). Always
	// present (never null) so listing rows can render the mirror
	// indicator without a per-row summary fetch.
	Mirror bool `json:"mirror"`
	// MirrorUpstream is the sidecar's canonical upstream URL, present
	// only when the sidecar exists and parses (the row's accessible
	// label names the upstream). Corrupt-but-present still reports
	// Mirror=true with no upstream (fail closed).
	MirrorUpstream string `json:"mirror_upstream,omitempty"`
	// Visibility is the badge source (Forgejo #345, modes split #374):
	// "public"|"authenticated"|"private" when the identity surface is wired, "" when
	// it is not (never null — consumers ignore unknown fields, but an
	// explicit empty reads as "unknown", not "public").
	Visibility string `json:"visibility"`
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
	// Forgejo #345: the candidate names are visibility-filtered BEFORE
	// the catalog fold, so private rows (names, sizes, activity) never
	// enter the response.
	names, err := h.env.VisibleRepos(r.Context(), owner, h.env.PrincipalOf(r))
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
	// Mirror flags ride the same response (Forgejo #281): the sidecar
	// probe per row is the only source (the aggregate catalog carries
	// no mirror state), so the indicator costs no per-row summary
	// fetch on the client. See fillMirrorFlags for the trip budget.
	fillMirrorFlags(r.Context(), h.env.Store, owner, out)
	// Visibility flags ride the same response (Forgejo #345): one
	// LRU-backed conditional access.json GET per row (usually a version
	// hit, no body — the same trip the read gate already paid).
	fillVisibilityFlags(r.Context(), h.env, owner, out)
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

// fillVisibilityFlags stamps the visibility spelling onto every listing
// row (the badge source without a per-row summary fetch).
//
// ### Concurrency: one goroutine per row, at most 8 probes in flight;
// each goroutine writes only its own slice index, so there is no shared
// mutable state and no lock to order — the fillMirrorFlags shape.
//
// Trip budget (law 6): one conditional access.json GET per row behind the
// access LRU (version hits carry no body); unwired hook → no trips, the
// field stays "".
func fillVisibilityFlags(ctx context.Context, env *Env, owner string, out []RepoSizeRow) {
	if env == nil || env.RepoVisibility == nil || len(out) == 0 {
		return
	}
	const maxInFlight = 8
	sem := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	for i := range out {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if vis, ok := env.RepoVisibility(ctx, owner, out[i].Name); ok {
				out[i].Visibility = vis
			}
		}(i)
	}
	wg.Wait()
}

// probeMirrorRow reports the mirror flag for one listing row: true iff
// the mirror sidecar exists (probe, don't list — law 4). A present but
// corrupt sidecar still counts as a mirror (fail closed — the
// mirror.IsMirror parity — without ever importing the feature package
// from core, law 8); the canonical upstream URL rides along only when
// the body parses. Store errors and absent bodies degrade to
// non-mirror (the listing degrades, never 500s, on a sick store).
func probeMirrorRow(ctx context.Context, st store.ObjectStore, owner, name string) (bool, string) {
	body, _, err := store.GetBytes(ctx, st, store.MirrorKey(owner, name), store.GetOptions{})
	if err != nil || body == nil {
		return false, ""
	}
	var doc struct {
		UpstreamURL string `json:"upstream_url"`
	}
	if jerr := json.Unmarshal(body, &doc); jerr != nil {
		return true, ""
	}
	return true, doc.UpstreamURL
}

// fillMirrorFlags stamps the mirror flag (+ upstream when known) onto
// every listing row.
//
// ### Concurrency: one goroutine per row, at most 8 probes in flight;
// each goroutine writes only its own slice index, so there is no
// shared mutable state and no lock to order. No lock is held across
// the store calls (there is none here at all). The sender-owns rule
// is trivially satisfied (no channels carry results — WaitGroup joins
// the disjoint writes before the response encodes).
//
// Trip budget (law 6): the catalog read stays ONE object read; the
// sidecar probes are independent GETs of ~200-byte bodies issued in
// parallel, so they add request count but no sequential depth. A
// cancelled request stops launching (in-flight probes drain via ctx)
// and still joins before return, so the response never races a probe.
func fillMirrorFlags(ctx context.Context, st store.ObjectStore, owner string, out []RepoSizeRow) {
	if st == nil || len(out) == 0 {
		return
	}
	const maxInFlight = 8
	sem := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	for i := range out {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m, up := probeMirrorRow(ctx, st, owner, out[i].Name)
			out[i].Mirror = m
			out[i].MirrorUpstream = up
		}(i)
	}
	wg.Wait()
}
