package api

import (
	"errors"
	"hash/fnv"
	"net/http"
	"strconv"

	"git.packden.us/crueber/walhub/internal/config"
	"git.packden.us/crueber/walhub/internal/git"
)

// --- GET/PUT/DELETE {lane} — repo summary, create, delete (§9.1) ----------------------

type summaryBody struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
	// Description is the per-repo short display string (issue #235): "" when
	// unset, rendered by the repo header and set via the General settings tab.
	Description  string           `json:"description"`
	FullName     string           `json:"full_name"`
	Head         *Ref             `json:"head"` // null = unborn (the one sanctioned null)
	Branches     int              `json:"branches"`
	Tags         int              `json:"tags"`
	Health       string           `json:"health"` // empty|healthy|degraded (§9.1; issue #209)
	MissingTotal uint64           `json:"missing_total,omitempty"`
	Placeholder  *PlaceholderInfo `json:"placeholder,omitempty"` // #210 R1 B1: sidecar projection, empty repos only
	// Mirror is the pull-only mirror projection (Forgejo #240): nil on
	// non-mirrors (omitempty — never null).
	Mirror *MirrorView `json:"mirror,omitempty"`
	// PushMirror is the push-mirror projection (Forgejo #623): nil on
	// repos without one (omitempty — never null). Independent of
	// Mirror: either, both, or neither may be present.
	PushMirror *PushMirrorView `json:"push_mirror,omitempty"`
	// OpenIssues/OpenPulls are the tab-badge numerators (issue #319):
	// open kind:"issue" / kind:"pr" cards from the shared P4 index, always
	// present (0 = none — the badge hides at 0 client-side). Old clients
	// ignore them (14 §14.12).
	OpenIssues int `json:"open_issues"`
	OpenPulls  int `json:"open_pulls"`
	// Visibility is the badge source (Forgejo #345, modes split #374):
	// "public"|"authenticated"|"private" when the identity surface is wired, "" when it
	// is not (never null — old clients ignore it, 14 §14.12). Missing
	// access.json resolves public (the §10 legacy default).
	Visibility string `json:"visibility"`
	// ForkParent is the fork parent ("o/r", issue #424): "" when not a
	// fork (omitempty — old clients ignore it, 14 §14.12). Forks is the
	// direct-children count from the meta/forks.json index (always
	// present, 0 = none — the #319 badge discipline).
	ForkParent string `json:"fork_parent,omitempty"`
	Forks      int    `json:"forks"`
	// HasChecks is the Checks-tab visibility flag (issue #505): true
	// once any check status was reported (hot window or backfilled — the
	// index exists in both cases). Always present (false = none — the
	// #319 badge discipline). Old clients ignore it (14 §14.12).
	HasChecks bool `json:"has_checks"`
	// Features are the per-repo feature flags (Forgejo #522): six
	// enabled booleans from the [features] settings section, resolved
	// all-on when unset. Always present (never null — the #319 badge
	// discipline); the tab bar and the star/watch/fork pills gate on
	// this one payload with zero new requests (law 6: a dedicated
	// endpoint would cost an extra request per repo view). Old clients
	// ignore it (14 §14.12).
	Features    config.ResolvedFeatures `json:"features"`
	CloneURL    string                  `json:"clone_url"`
	SSHCloneURL string                  `json:"ssh_clone_url,omitempty"`
	HTMLURL     string                  `json:"html_url"`
	APIURL      string                  `json:"api_url"`
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
	// The mirror projection (Forgejo #240): one exact-key probe behind
	// the Env hook (nil → no probe, +0 round trips — the hook IS the
	// feature; 404s are free per law 4).
	var mirrorView *MirrorView
	if h.env.MirrorSummary != nil {
		if v, ok := h.env.MirrorSummary(r.Context(), id.Owner, id.Name); ok {
			v := v
			mirrorView = &v
		}
	}
	// The push-mirror projection (Forgejo #623): one exact-key probe
	// behind the Env hook (nil → no probe, +0 round trips — the hook IS
	// the feature; 404s are free per law 4). Independent of the pull
	// projection above.
	var pushMirrorView *PushMirrorView
	if h.env.PushMirrorSummary != nil {
		if v, ok := h.env.PushMirrorSummary(r.Context(), id.Owner, id.Name); ok {
			v := v
			pushMirrorView = &v
		}
	}
	if health != RepoHealthEmpty && health != RepoHealthDegraded {
		// Degraded override from the serve-health sidecar (issue #320):
		// the serve path's own sticky failure record. Mirrors ride the
		// hook's verdict (it already probed — DegradedReason, +0 extra
		// round trips here); non-mirrors pay one exact-key probe, the
		// same cost class as the fsck probe above (this path is off the
		// law-6 budgeted paths).
		if mirrorView != nil {
			if mirrorView.DegradedReason != "" {
				health = RepoHealthDegraded
			}
		} else if _, ok := probeServeHealth(r.Context(), h.env.Store, id); ok {
			health = RepoHealthDegraded
		}
	}
	// The open-count projection (Forgejo #319): one exact-key probe behind
	// the Env hook (nil/absent → zeros, +0 round trips — the hook IS the
	// feature; 404s are free per law 4). The counts ride the summary so
	// the tab bar needs zero new requests (law 6: the two-count-endpoints
	// alternative costs two extra requests per repo view).
	var counts CollabCounts
	countsOK := false
	if h.env.CollabCounts != nil {
		if c, ok := h.env.CollabCounts(r.Context(), id.Owner, id.Name); ok {
			counts, countsOK = c, true
		}
	}
	// The visibility projection (Forgejo #345): one LRU-backed
	// conditional access.json GET (usually a version hit, no body —
	// the same trip the dispatch read gate already paid). Unwired →
	// "" (the badge hides, exactly like an absent mirror view).
	visibility, _ := h.env.repoVisibility(r.Context(), id.Owner, id.Name)
	// The fork projection (issue #424): two exact-key probes behind the
	// Env hook (nil/absent → empty, +0 round trips). The parent renders
	// the "forked from" line; the count renders next to the Fork button
	// (same payload, no extra fetch — the mirror badge pattern).
	var forkSum ForkSummary
	forkOK := false
	if h.env.ForkInfo != nil {
		if fs, ok := h.env.ForkInfo(r.Context(), id.Owner, id.Name); ok {
			forkSum, forkOK = fs, true
		}
	}
	// The checks-existence projection (issue #505): one exact-key probe
	// behind the Env hook (nil/absent → false, +0 round trips — the hook
	// IS the feature; 404s are free per law 4). The flag rides the
	// summary so the tab bar needs zero new requests (law 6: a dedicated
	// endpoint would cost an extra request per repo view).
	var checksSum ChecksSummary
	checksOK := false
	if h.env.ChecksSummary != nil {
		if cs, ok := h.env.ChecksSummary(r.Context(), id.Owner, id.Name); ok {
			checksSum, checksOK = cs, true
		}
	}
	// The features projection (Forgejo #522): resolved flags from the
	// view (the walView folds the manifest-inline settings TOML with
	// zero new round trips — the Description precedent). A nil view
	// field fails open to all-enabled, so an unpopulated view can never
	// strand tabs hidden; the wire object itself is always present (the
	// #319 badge discipline, never null).
	features := config.AllFeatures()
	if s.Features != nil {
		features = *s.Features
	}
	body := summaryBody{
		Owner:        id.Owner,
		Name:         id.Name,
		Description:  s.Description,
		FullName:     id.Owner + "/" + id.Name,
		Head:         s.Head,
		Branches:     s.Branches,
		Tags:         s.Tags,
		Health:       health,
		MissingTotal: missingTotal,
		Placeholder:  placeholder,
		Mirror:       mirrorView,
		PushMirror:   pushMirrorView,
		OpenIssues:   counts.OpenIssues,
		OpenPulls:    counts.OpenPulls,
		Visibility:   visibility,
		ForkParent:   forkSum.Parent,
		Forks:        forkSum.Count,
		HasChecks:    checksSum.HasChecks,
		Features:     features,
		CloneURL:     base + "/" + id.Owner + "/" + id.Name + ".git",
		SSHCloneURL:  h.env.sshCloneURL(r, id.Owner, id.Name),
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
	if s.Description != "" {
		// Same trap as the ~degraded suffix (issue #235): a
		// description-only change keeps the head sha, so the ETag must
		// cover the field or a revalidating client 304s and keeps showing
		// the stale text. FNV-1a keeps the suffix short; clearing the
		// description drops the suffix, which busts the cache too.
		etag += "~d" + descriptionHash(s.Description)
	}
	if mirrorView != nil {
		// Same trap once more (Forgejo #240): a sync outcome changes
		// neither the head sha nor the description, so the ETag covers
		// the mirror projection or the badge/next-sync display goes
		// stale behind a 304.
		etag += "~m" + mirrorHash(*mirrorView)
	}
	if pushMirrorView != nil {
		// Same trap once more (Forgejo #623): a push-mirror outcome
		// changes neither the head sha nor the description, so the
		// ETag covers the projection or the status display goes stale
		// behind a 304.
		etag += "~p" + pushMirrorHash(*pushMirrorView)
	}
	if countsOK {
		// Same trap once more (issue #319): a close/reopen moves no ref,
		// so the ETag covers the shared index version or the badges go
		// stale behind a 304.
		etag += "~c" + strconv.Itoa(counts.Version)
	}
	if visibility != "" {
		// Same trap once more (Forgejo #345): a visibility flip moves
		// no ref, so the ETag covers the field or a revalidating
		// client 304s and keeps showing the stale badge.
		etag += "~v" + visibility
	}
	if forkOK {
		// Same trap once more (issue #424): a fork landing moves no
		// parent ref, so the ETag covers the index version + count +
		// parent or the fork count and "forked from" line go stale
		// behind a 304.
		etag += "~f" + strconv.Itoa(forkSum.Version) + "." + strconv.Itoa(forkSum.Count) + forkParentHash(forkSum.Parent)
	}
	if checksOK {
		// Same trap once more (issue #505): the first report creates
		// the checks index and moves no ref, so the ETag covers the
		// index version or a revalidating client 304s and the Checks
		// tab stays hidden after CI reports.
		etag += "~k" + strconv.Itoa(checksSum.Version)
	} else {
		// Forgejo #513: the suffix is unconditional. A client holding
		// a pre-#505 cached summary (no has_checks field at all)
		// revalidates with a bare head-sha ETag; without this suffix
		// the server 304s forever and the client never receives
		// has_checks, so the Checks tab stays visible via the
		// missing-field fail-open. ~k0 can never collide with a real
		// index: updateIndex ++ before the first write, so a present
		// index is always Version >= 1.
		etag += "~k0"
	}
	// Forgejo #522: the features suffix is unconditional, for the same
	// #513 reason — a settings-only flip (no new commit, no ref move)
	// must revalidate, and a pre-#522 cached summary (no features field
	// at all) must never 304-match or the client keeps the stale tab
	// gating forever. ~t carries the six resolved flags as fixed-order
	// bits (config.ResolvedFeatures.ETagBits), so both a flip and the
	// first sighting bust the cache. The summary already serves the
	// mutable-collab class (private, no-cache), so this only sharpens
	// revalidation — same cost class as the ~d/~m/~c/~v/~f/~k suffixes.
	etag += "~t" + features.ETagBits()
	// Forgejo #381: the summary serves the mutable-collab class
	// (private, no-cache), NOT SWR. The ~d/~m/~c/~v/~f/~k suffixes above make
	// *revalidation* correct, but SWR's stale-serve window still licensed
	// the browser to paint the pre-mutation body on the next refresh
	// while revalidating in the background (the #280 flip-flop class —
	// the summary was the surface #280 left on SWR). no-cache keeps the
	// ETag/304 economics (unchanged summaries revalidate to 304 with
	// zero body) and drops only the stale-serve window. Law 6: this
	// path is off the push/sync/checkpoint budgets (those never call
	// here), so no hot-path sequential-trip regression.
	writeCached(w, r, ccMutable, etag, http.StatusOK, body)
}

// descriptionHash is the short ETag suffix covering the summary description
// (issue #235): FNV-1a/32 hex of the text, so a description-only change busts
// the SWR cache without growing the ETag by the full text.
func descriptionHash(d string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(d))
	return strconv.FormatUint(uint64(h.Sum32()), 16)
}

// mirrorHash is the short ETag suffix covering the mirror projection
// (Forgejo #240): same FNV-1a discipline as descriptionHash. It covers
// DegradedReason too (issue #320): a serve-health flip changes neither
// the head sha nor the sync outcome, so without it a revalidating
// client would 304 and keep showing the stale verdict.
func mirrorHash(v MirrorView) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(v.UpstreamURL + "\x00" + v.Schedule + "\x00" + v.NextSyncAt + "\x00" + v.LastSyncedAt + "\x00" + v.LastResult + "\x00" + v.DegradedReason))
	return strconv.FormatUint(uint64(h.Sum32()), 16)
}

// pushMirrorHash is the short ETag suffix covering the push-mirror
// projection (Forgejo #623): same FNV-1a discipline as mirrorHash. It
// covers only display fields (never secret material — secrets never
// reach the projection).
func pushMirrorHash(v PushMirrorView) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(v.UpstreamURL + "\x00" + v.AuthKind + "\x00" + v.Schedule + "\x00" + v.NextSyncAt + "\x00" + v.LastSyncedAt + "\x00" + v.LastResult))
	return strconv.FormatUint(uint64(h.Sum32()), 16)
}

// forkParentHash is the short ETag suffix covering the fork parent
// (issue #424): the parent pointer never changes post-create, but the
// ETag must still cover the field — same FNV-1a discipline as
// descriptionHash, so a revalidating client never 304s a changed
// "forked from" line.
func forkParentHash(parent string) string {
	if parent == "" {
		return ".0"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(parent))
	return "." + strconv.FormatUint(uint64(h.Sum32()), 16)
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
	principal := h.env.PrincipalOf(r)
	if h.checkCreateOwner(w, r, id, principal) {
		return
	}
	principalName := principal.Name
	if err := h.env.Repos.Create(r.Context(), id, format); err != nil {
		if errors.Is(err, ErrExists) {
			if placeholder {
				if doc, ok := probePlaceholder(r.Context(), h.env.Store, id); ok && doc != nil {
					if samePrincipal(doc.CreatedBy, principalName) && h.stillUnborn(r.Context(), id) {
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
		if err := h.createPlaceholder(r, id, format, principalName, ""); err != nil {
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
