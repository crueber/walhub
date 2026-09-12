// web/src/lib/issueFilters.js — issues-list filter-bar param helpers (issue #416).
//
// Pure presentation logic over docs/features/02 §7 filter params: `labels`
// is a comma-separated name list (AND), `milestone` is an id or the `none`
// sentinel, both riding the URL as search params. Headless (no Solid, no
// DOM) so node --test covers it. Selection toggling reuses toggleLabel
// from labels.js (names are unique case-insensitively per 02 §3.1).

/** Wire sentinel selecting issues with no milestone (02 §7). */
export const NO_MILESTONE = "none";

/**
 * parseLabelsParam(raw) → string[]: split the `?labels=` param on commas,
 * trimming whitespace and dropping empties. Stored spellings preserved;
 * exact duplicates dropped (first wins). Unknown names (deleted after a
 * deep link was shared) are KEPT — the dropdown renders them as bare rows
 * (same self-heal stance as LabelChip, 02 §3.1) rather than silently
 * dropping the filter.
 */
export function parseLabelsParam(raw) {
  const seen = new Set();
  const out = [];
  for (const part of String(raw ?? "").split(",")) {
    const name = part.trim();
    if (!name || seen.has(name)) continue;
    seen.add(name);
    out.push(name);
  }
  return out;
}

/**
 * serializeLabelsParam(names) → string: join the selected set back into
 * the `?labels=` param value the list endpoint accepts. Empty → "" (the
 * caller clears the param, same URL-honest idiom as the state filter).
 */
export function serializeLabelsParam(names) {
  return (names ?? []).join(",");
}

/**
 * resolveMilestoneFilter(raw, milestones) → {value, unknown, pending}:
 * the select binding for the raw `?milestone=` param against the cached
 * `milestones:{o}/{r}` set. "" and "none" are the clear / No-milestone
 * options; a known id binds verbatim; an unknown id (deleted milestone in
 * a deep link) binds to the RAW value so the filter is shown, not dropped
 * (pending while the set loads — the select disables, never flashes).
 * `milestones === undefined` means the cache has not settled yet.
 */
export function resolveMilestoneFilter(raw, milestones) {
  const value = String(raw ?? "");
  if (value === "" || value === NO_MILESTONE) return { value, unknown: false, pending: false };
  if (milestones === undefined) return { value, unknown: false, pending: true };
  const known = (milestones ?? []).some((m) => m?.id === value);
  return { value, unknown: !known, pending: false };
}
