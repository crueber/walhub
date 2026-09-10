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
 * filterReleases(rels, filter) → array. Client-side draft/prerelease chips
 * for the releases list (issue #270): the list endpoint hides drafts, so
 * the chips filter the loaded page only — no new endpoint. Unknown filters
 * behave as "all". Non-array input behaves as an empty list. Returns a
 * fresh array, never the input.
 */
export function filterReleases(rels, filter) {
  const list = Array.isArray(rels) ? rels.slice() : [];
  if (filter === "drafts") return list.filter((r) => !!r?.draft);
  if (filter === "prereleases") return list.filter((r) => !!r?.prerelease);
  return list;
}

/**
 * excerptBody(body, max?) → string. One-line plain-text excerpt of release
 * notes for the list rows (issue #270): the first non-blank line with
 * leading markdown markers (# heading, > quote, -/* list, 1. ordered)
 * stripped and internal whitespace collapsed, truncated to `max` chars
 * (default 140) with an ellipsis. Non-string input renders as "".
 */
export function excerptBody(body, max = 140) {
  if (typeof body !== "string") return "";
  const line = body.split("\n").find((l) => l.trim()) ?? "";
  let text = line.trim().replace(/^(#{1,6}\s+|>\s*|[-*+]\s+|\d+[.)]\s+)/, "").trim();
  text = text.replace(/\s+/g, " ");
  const n = Number(max);
  const limit = Number.isFinite(n) && n > 0 ? Math.floor(n) : 140;
  if (text.length <= limit) return text;
  return text.slice(0, Math.max(0, limit - 1)).trimEnd() + "…";
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
