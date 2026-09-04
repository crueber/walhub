// web/src/lib/sort.js — list display order (02 §2 Decisions, issue #48):
// issue/PR lists ALWAYS render newest-first by number descending (#N…#1),
// regardless of filter. The bucket index stays newest-activity-first; the
// render sorts by num at every layer (backend ListIssues/ListPRs AND here,
// so a stale cache window or an SSE refetch can never show activity order).

/** Non-mutating number-descending sort of `{num}` rows (null-safe). */
export function sortByNumDesc(rows) {
  return [...(rows ?? [])].sort((a, b) => (b?.num ?? 0) - (a?.num ?? 0));
}
