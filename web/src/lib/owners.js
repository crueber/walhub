// web/src/lib/owners.js — pure helpers for the owners (/) page (issue #117).
// Headless-testable: no Solid, no DOM — importable in Node.

/**
 * Max owners rendered on `/`. One `owners.repos(owner)` GET fans out per
 * shown owner, so the cap bounds the page to 1 + MAX_OWNERS store reads.
 */
export const MAX_OWNERS = 50;

/**
 * Max repos rendered inside one owner's section. Overflow folds behind a
 * "+N more →" link to `/:owner`, which lists that owner without a cap.
 */
export const MAX_REPOS_PER_OWNER = 10;

/**
 * newestFirst(names) → a reversed copy: owners (and their repos) newest-first.
 *
 * Ordering key, documented honestly: the bucket stores NO creation timestamp
 * for owners or repos, and `GET /api/v1/owners` (+ `/{owner}/repos`) return
 * store-sorted (ascending) name lists — so the page reverses the server
 * order. Reverse-lexicographic is the closest deterministic newest-first
 * proxy available; if the backend ever carries creation times, this is the
 * single function to replace (callers pass server order straight through).
 */
export function newestFirst(names) {
  const list = Array.isArray(names) ? names : [];
  return [...list].reverse();
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
