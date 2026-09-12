// owners_activity.go — Forgejo #283 server-side owner activity ordering.
//
// The v1 string lists stay byte-identical by default (14 §14.12): the rollup
// is served two additive ways over the same derived fold —
//   - GET /api/v1/owners?sort=activity&order=desc → the frozen []string,
//     activity-ordered (this is what fixes the >50-owner cap: the server
//     ranks over ALL owners before the client slices);
//   - GET /api/v1/owners/detailed?sort=&order= → {owners: [{name,
//     is_org, repo_count, last_commit_sha|null, last_commit_time|null}]}
//     object rows carrying the org marker (Forgejo #348 — one OrgLister
//     call, fail-open) plus the per-owner max (max over the owner's repos;
//     null when the owner has no commits) plus the manifest-gated
//     live-repo count (Forgejo #307 — the instance repo-total rail,
//     ghost-filtered like liveRepos), triple twins + discovery + SDK,
//     the #248 detailed precedent.
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
	"git.packden.us/crueber/walhub/internal/store/proto"
)

// splitRepoID cuts a catalog "owner/name" id (the catalog never invents
// repos, but a malformed row must not panic the fold — it is skipped).
func splitRepoID(id string) (owner, name string, ok bool) {
	owner, name, ok = strings.Cut(id, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return owner, name, true
}

// OwnerActivityRow is one owners/detailed row (additive shape, 14 §14.12
// field rule: consumers ignore unknown fields; arrays stay [] never null;
// nulls stay explicit — no omitempty — so unknown is distinguishable from
// empty, mirroring RepoSizeRow).
type OwnerActivityRow struct {
	Name string `json:"name"`
	// IsOrg marks org-owned namespaces (Forgejo #348 — the explore
	// badge rail: the UI links org rows to the org profile instead of
	// a bare repo list). Always present (never null): false for users
	// and for instances without the identity surface wired. Old
	// clients ignore it (14 §14.12).
	IsOrg bool `json:"is_org"`
	// RepoCount is the owner's live-repo count (Forgejo #307 — the instance
	// repo-total rail: the page sums this field over the uncapped payload,
	// never a capped slice and never a per-owner listing walk). Always
	// present (never null): membership implies at least one live repo.
	// Ghost-filtered exactly like liveRepos (manifest-backed repos only).
	RepoCount int `json:"repo_count"`
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
// allow (Forgejo #345) restricts the fold to repos readable by the caller:
// map owner → set of visible repo names; nil = no restriction. A private
// repo's commit time must never lift its owner's activity row.
func ownerRollups(r *http.Request, h *handlers, allow map[string]map[string]bool) (map[string]sizecatalog.OwnerRollup, error) {
	rollups := map[string]sizecatalog.OwnerRollup{}
	if h.env.Store == nil {
		return rollups, nil
	}
	cat, err := sizecatalog.ReadCatalog(r.Context(), h.env.Store)
	if err != nil {
		return nil, err
	}
	if allow != nil {
		cat = filterCatalog(cat, allow)
	}
	return sizecatalog.OwnerRollups(cat), nil
}

// filterCatalog returns a catalog copy holding only allowlisted repos
// (owner → visible names). The aggregate catalog is shared across owners,
// so the listing folds its private rows out before deriving maxima.
func filterCatalog(cat *proto.RepoCatalog, allow map[string]map[string]bool) *proto.RepoCatalog {
	if cat == nil {
		return nil
	}
	out := *cat
	out.Entries = out.Entries[:0:0]
	for _, e := range cat.Entries {
		if e == nil {
			continue
		}
		owner, name, ok := splitRepoID(e.Repo)
		if !ok {
			continue
		}
		if allow[owner][name] {
			out.Entries = append(out.Entries, e)
		}
	}
	return &out
}

// ownersDetailed serves the per-owner activity rows with the shared
// sort/order query. Membership AND repo counts come from the registry's
// OwnerRepoCounts (one manifest-gated walk — the same trip profile as the
// Owners call it replaces, law 6; the catalog never invents owners and
// never sizes the counts, because the sweep never prunes deleted rows —
// catalog counts would resurrect ghosts); activity rides the derived
// rollup. Cache class: SWR (ref-dependent listing, same as owners).
func (h *handlers) ownersDetailed(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthRead) {
		return
	}
	if h.env.Repos == nil {
		writePlain(w, http.StatusServiceUnavailable, "repo registry not configured")
		return
	}
	// Forgejo #345: membership, counts, and activity all derive from the
	// repos readable by this caller. Unfiltered callers (nil Access, host
	// admin) keep the single OwnerRepoCounts walk, byte-identical
	// trips; filtered callers trade one Repos walk per owner (the same
	// manifest-gated rule, shared with the allowlist below, law 6).
	p := h.env.PrincipalOf(r)
	counts := map[string]int{}
	var allow map[string]map[string]bool
	if h.env.unfiltered(p) {
		var err error
		counts, err = h.env.Repos.OwnerRepoCounts(r.Context())
		if err != nil {
			mapViewErr(w, err)
			return
		}
	} else {
		byOwner, berr := h.env.VisibleReposByOwner(r.Context(), p)
		if berr != nil {
			mapViewErr(w, berr)
			return
		}
		allow = make(map[string]map[string]bool, len(byOwner))
		for owner, repos := range byOwner {
			counts[owner] = len(repos)
			set := make(map[string]bool, len(repos))
			for _, n := range repos {
				set[n] = true
			}
			allow[owner] = set
		}
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sortKey, order := ownerSortParams(r)
	rollups, err := ownerRollups(r, h, allow)
	if err != nil {
		mapViewErr(w, err)
		return
	}
	// Forgejo #348: the org set behind one OrgLister call (nil → no
	// orgs; error → fail open to all-false — display metadata must
	// never fail the listing, the CollabCounts precedent).
	isOrg := map[string]bool{}
	if h.env.Orgs != nil {
		if names, lerr := h.env.Orgs.ListOrgs(r.Context()); lerr == nil {
			for _, n := range names {
				isOrg[n] = true
			}
		}
	}
	out := make([]OwnerActivityRow, 0, len(names))
	for _, n := range sizecatalog.SortOwners(names, rollups, sortKey, order) {
		row := OwnerActivityRow{Name: n, IsOrg: isOrg[n], RepoCount: counts[n]}
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
