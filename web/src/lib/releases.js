// web/src/lib/releases.js — pure helpers for the releases UI (issue #35).
// Headless-testable: no Solid, no DOM — importable in Node.

export const LATEST_ASSET_LIMIT = 3;

/**
 * keyAssets(assets, limit?) → {shown, extra}. Split a release's asset list
 * into the key assets the Latest panel links directly (the first `limit`,
 * in server order) and the overflow count the panel folds behind the
 * "+N more" link to the release detail page. Non-array input behaves as an
 * empty list; a non-positive or non-integer limit shows none (all extra).
 */
export function keyAssets(assets, limit = LATEST_ASSET_LIMIT) {
  const list = Array.isArray(assets) ? assets : [];
  const n = Number(limit);
  if (!Number.isFinite(n) || n <= 0) return { shown: [], extra: list.length };
  const k = Math.floor(n);
  return { shown: list.slice(0, k), extra: Math.max(0, list.length - k) };
}

/**
 * filterTagNames(names, query) → string[]. Client-side substring filter
 * for the new-release tag combobox (issue #254): the option source is the
 * page-owned `tags:{full}` cache entry (most-recent-first stream order,
 * preserved — filtering never re-sorts), so this helper performs no
 * fetch. Empty/blank query returns every name; matching is
 * case-insensitive; non-array input behaves as an empty list.
 */
export function filterTagNames(names, query) {
  const list = Array.isArray(names) ? names.map(String) : [];
  const q = String(query ?? "").trim().toLowerCase();
  if (!q) return list;
  return list.filter((n) => n.toLowerCase().includes(q));
}
