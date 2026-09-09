// web/src/lib/thread-order.js — thread display order (issue #225):
// threads read chronologically, oldest at top → newest at the bottom
// (GitHub convention). The wire stays newest-first (02 §7 Decisions:
// event windows return arrays newest-first; `after_seq=` pages strictly
// below toward older) — the render sorts by seq at the shared
// ThreadTimeline, so a stale cache window or an SSE refetch can never
// show wire order. Headless-testable: ThreadTimeline.jsx applies these,
// `node --test` pins them here.

/**
 * chronological(events) → a new array sorted ascending by `seq`
 * (oldest first, newest last). Stable: equal or missing seqs keep
 * their input relative order (seq-less rows sort before seq 0 —
 * they predate the log, never interleave it). Never mutates input.
 */
export function chronological(events) {
  return [...(events ?? [])]
    .map((ev, i) => [ev, i])
    .sort(([a, ai], [b, bi]) => {
      const d = (a?.seq ?? Number.NEGATIVE_INFINITY) - (b?.seq ?? Number.NEGATIVE_INFINITY);
      return d !== 0 ? d : ai - bi;
    })
    .map(([ev]) => ev);
}

/**
 * appendOlderWindow(current, page) → newest-first assembly extended
 * with one older window: the wire order is newest-first at every
 * layer (view page, then each `after_seq` page older than the last),
 * so plain concatenation stays newest-first and `chronological()`
 * of the whole assembly is the gap-free thread. Never mutates input.
 */
export function appendOlderWindow(current, page) {
  return [...(current ?? []), ...(page ?? [])];
}

/**
 * olderCursor(newestFirst) → the `after_seq` cursor for the next
 * older window: the seq of the oldest loaded event (arrays are
 * newest-first, so the tail). 0 when empty (pages from the newest).
 * Callers pass the UNFILTERED assembly: `after_seq` is a seq cursor,
 * not a count, so filtering (`reaction_changed` folds into the
 * summary, never renders) can only overlap already-loaded seqs —
 * never skip older ones, never duplicate a visible row.
 */
export function olderCursor(newestFirst) {
  const all = newestFirst ?? [];
  if (!all.length) return 0;
  return all[all.length - 1]?.seq ?? 0;
}

/**
 * anchorScrollTop(top, oldHeight, newHeight) → the scrollTop that
 * keeps the content under the viewport fixed when older rows prepend
 * ABOVE it: the height delta joins the previous offset. Pure math —
 * the caller reads `scrollingElement` before the prepend and applies
 * the result after (Issue.jsx `loadOlder`).
 */
export function anchorScrollTop(top, oldHeight, newHeight) {
  return top + (newHeight - oldHeight);
}
