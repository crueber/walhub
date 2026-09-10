// web/src/lib/owners.js — pure helpers for the owners (/) page (issue #117).
// Headless-testable: no Solid, no DOM — importable in Node.

/**
 * Max owner SECTIONS rendered on `/explore` (Forgejo #295: the top 5 most
 * active owners — not the 50 name-sorted sections the page used to render).
 * One `owners.detailed(owner)` GET fans out per shown owner, so the cap
 * bounds the page to 1 + MAX_OWNERS store reads (a cold load drops from ~51
 * to ~6). The section list comes server-ranked from
 * `GET /api/v1/owners/detailed?sort=activity&order=desc` (#283 rollup) and
 * is filtered to active owners (see activeOwnerNames) BEFORE this slice, so
 * the cap keeps the server's top-N.
 */
export const MAX_OWNERS = 5;

/**
 * Max repos rendered inside one owner's section. Overflow folds behind a
 * "+N more →" link to `/:owner`, which lists that owner without a cap.
 */
export const MAX_REPOS_PER_OWNER = 10;

/**
 * newestFirst(names) → a reversed copy: reverse-lexicographic name order.
 *
 * LEGACY proxy, retained for compatibility (Forgejo #283): the owners
 * listing path exposes NO creation timestamp — `GET /api/v1/owners`
 * returns a store-sorted (ascending) name list of plain strings, so this
 * was the deterministic newest-first stand-in for OWNER sections. The
 * explore page no longer uses it: owner sections now order by most recent
 * commit via `orderOwnersByActivity` below (per-owner times come from the
 * #247 detailed listing rows the page already fetches — zero extra GETs).
 * New callers want `orderOwnersByActivity`, not this.
 */
export function newestFirst(names) {
  const list = Array.isArray(names) ? names : [];
  return [...list].reverse();
}

/**
 * orderByActivity(rows) → a sorted copy, newest commit first (Forgejo #247).
 *
 * Total order over detailed listing rows (`{owner?, name, last_commit_time?...}`):
 * known `last_commit_time` (RFC 3339 — lexicographic order IS chronological
 * order) descending, unknown (null/missing) always last, ties broken
 * deterministically on `(owner, name)` — the same key order as the server's
 * FilterSort, so mixed-owner lists agree with the catalog order. The server
 * (`sort=activity&order=desc`); this stabilizes mixed shapes (rows without
 * times fall back to name order instead of server order) so every caller
 * renders deterministically. Non-array input behaves as an empty list.
 */
export function orderByActivity(rows) {
  const list = Array.isArray(rows) ? [...rows] : [];
  const timeOf = (r) =>
    r && typeof r.last_commit_time === "string" ? r.last_commit_time : null;
  const nameOf = (r) => (r && typeof r.name === "string" ? r.name : "");
  const ownerOf = (r) => (r && typeof r.owner === "string" ? r.owner : "");
  list.sort((a, b) => {
    const ta = timeOf(a);
    const tb = timeOf(b);
    if (ta !== tb) {
      if (ta === null) return 1; // unknowns always last
      if (tb === null) return -1;
      return ta < tb ? 1 : -1; // newest first
    }
    const oa = ownerOf(a);
    const ob = ownerOf(b);
    if (oa !== ob) return oa < ob ? -1 : 1;
    const na = nameOf(a);
    const nb = nameOf(b);
    if (na !== nb) return na < nb ? -1 : 1;
    return 0;
  });
  return list;
}

/**
 * ownerActivity(docOrRows) → newest `last_commit_time` across one owner's
 * detailed listing rows, or null when the owner has no known activity
 * (Forgejo #283).
 *
 * Accepts either the `owners.detailed()` doc (`{repos: [...]}`) or a bare
 * rows array; non-array/missing input is null. Comparison is lexicographic
 * on RFC 3339 strings — chronological order, same as `orderByActivity`.
 * Non-string times (null/missing) are skipped, never selected.
 */
export function ownerActivity(docOrRows) {
  const rows = Array.isArray(docOrRows)
    ? docOrRows
    : docOrRows && Array.isArray(docOrRows.repos)
      ? docOrRows.repos
      : [];
  let newest = null;
  for (const r of rows) {
    const t = r && typeof r.last_commit_time === "string" ? r.last_commit_time : null;
    if (t !== null && (newest === null || t > newest)) newest = t;
  }
  return newest;
}

/**
 * orderOwnersByActivity(names, activityByOwner) → a sorted copy of owner
 * names, most-recently-committed owner first (Forgejo #283).
 *
 * `activityByOwner` maps owner name → newest `last_commit_time` string (as
 * computed by `ownerActivity` over the #247 detailed rows the explore page
 * already fetches per section — zero extra GETs, no per-row commit fetch).
 * Total order, mirroring `orderByActivity`: known times descending,
 * unknown (null/missing/unreported — fetch pending or owner without
 * commits) always last, ties broken deterministically on name ascending.
 * Non-array input behaves as an empty list; a non-object activity map
 * behaves as all-unknown (name order — the page's pre-activity paint).
 */
export function orderOwnersByActivity(names, activityByOwner) {
  const list = Array.isArray(names) ? [...names] : [];
  const byOwner =
    activityByOwner && typeof activityByOwner === "object" ? activityByOwner : {};
  const timeOf = (n) => {
    const t = byOwner[n];
    return typeof t === "string" ? t : null;
  };
  list.sort((a, b) => {
    const ta = timeOf(a);
    const tb = timeOf(b);
    if (ta !== tb) {
      if (ta === null) return 1; // unknowns always last
      if (tb === null) return -1;
      return ta < tb ? 1 : -1; // newest first
    }
    return a < b ? -1 : a > b ? 1 : 0; // deterministic name tiebreak
  });
  return list;
}

/**
 * hasKnownActivity(activityByOwner) → whether any owner has a known commit
 * time string (Forgejo #283 follow-up).
 *
 * The explore page fetches server-ordered names (`sort=activity&order=desc`)
 * and must NOT re-sort until at least one section has reported a real time:
 * re-running `orderOwnersByActivity` over an all-unknown map would resort to
 * plain name order and discard the server ranking BEFORE the MAX_OWNERS
 * slice (an active owner past the name cap would never mount). Null/missing/
 * non-string values are all "not yet known" — a page where every section
 * settles null keeps the server order (which for all-unknown servers already
 * degrades to name order, so both agree).
 */
export function hasKnownActivity(activityByOwner) {
  if (!activityByOwner || typeof activityByOwner !== "object") return false;
  return Object.values(activityByOwner).some((t) => typeof t === "string");
}

/**
 * activeOwnerNames(detailRows) → the names of owners with known commit
 * activity, in the caller's (server-ranked) order (Forgejo #295 over the
 * #283 `owners/detailed` rows).
 *
 * "Active" = the row carries a `last_commit_time` string (the server's
 * per-owner max-commit rollup; null = the owner has no commits). Owners
 * without activity are dropped — they are the opposite of active and never
 * appear in the top-5 page. Non-array input behaves as an empty list; rows
 * without a string name are skipped.
 */
export function activeOwnerNames(detailRows) {
  const rows = Array.isArray(detailRows) ? detailRows : [];
  const out = [];
  for (const r of rows) {
    if (!r || typeof r.name !== "string") continue;
    if (typeof r.last_commit_time !== "string") continue;
    out.push(r.name);
  }
  return out;
}

/**
 * pageSlice(list, limit?) → {shown, extra}. Split a name list into the rows
 * the page renders (the first `limit`, in the caller's order) and the
 * overflow count folded behind the "+N more" link. Non-array input behaves
 * as an empty list; a non-positive or non-integer limit shows none.
 */
export function pageSlice(list, limit) {
  const names = Array.isArray(list) ? list : [];
  const n = Number(limit);
  if (!Number.isFinite(n) || n <= 0) return { shown: [], extra: names.length };
  const k = Math.floor(n);
  return { shown: names.slice(0, k), extra: Math.max(0, names.length - k) };
}
