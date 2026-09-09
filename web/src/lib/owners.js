// web/src/lib/owners.js — pure helpers for the owners (/) page (issue #117).
// Headless-testable: no Solid, no DOM — importable in Node.

/**
 * Max owners rendered on `/`. One `owners.detailed(owner)` GET fans out per
 * shown owner, so the cap bounds the page to 1 + MAX_OWNERS store reads.
 */
export const MAX_OWNERS = 50;

/**
 * Max repos rendered inside one owner's section. Overflow folds behind a
 * "+N more →" link to `/:owner`, which lists that owner without a cap.
 */
export const MAX_REPOS_PER_OWNER = 10;

/**
 * newestFirst(names) → a reversed copy: owners newest-first.
 *
 * Ordering key, documented honestly: the owners listing path exposes NO
 * creation timestamp — `GET /api/v1/owners` returns a store-sorted
 * (ascending) name list of plain strings. Reverse-lexicographic stays the
 * deterministic newest-first proxy for OWNER sections. (Repo rows no longer
 * use this: Forgejo #247 serves them pre-ordered by most recent commit via
 * `GET /api/v1/owners/{owner}/repos/detailed?sort=activity&order=desc`, and
 * `orderByActivity` below stabilizes that order client-side.)
 */
export function newestFirst(names) {
  const list = Array.isArray(names) ? names : [];
  return [...list].reverse();
}

/**
 * orderByActivity(rows) → a sorted copy, newest commit first (Forgejo #247).
 *
 * Total order over detailed listing rows (`{name, last_commit_time?...}`):
 * known `last_commit_time` (RFC 3339 — lexicographic order IS chronological
 * order) descending, unknown (null/missing) always last, ties broken
 * deterministically on `(name, owner)`. The server already sorts
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
    const na = nameOf(a);
    const nb = nameOf(b);
    if (na !== nb) return na < nb ? -1 : 1;
    const oa = ownerOf(a);
    const ob = ownerOf(b);
    if (oa !== ob) return oa < ob ? -1 : 1;
    return 0;
  });
  return list;
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
