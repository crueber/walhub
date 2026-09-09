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
      const sa = a?.seq ?? Number.NEGATIVE_INFINITY;
      const sb = b?.seq ?? Number.NEGATIVE_INFINITY;
      // NaN-safe: missing-vs-missing (both -Inf) falls to the index
      // tiebreak instead of returning NaN (which sort coerces to 0 —
      // same result, but implicit). Numeric subtraction, never
      // lexicographic; kind-agnostic (system rows are seq-ordered).
      return sa === sb ? ai - bi : sa - sb;
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
 * reconcilePinnedWindow(prevView, nextView, extras) → the extras array
 * reconciled against a slid newest-first view (issue #227): a remote
 * SSE event refetches only the newest-50 page, so the view slides
 * forward while pinned older windows stay put — the evicted boundary
 * row(s) would fall in the gap between the two (rendered 0..64
 * missing seq 14; `more()` then reads false so the hole persists
 * until reload). Own mutations take `reload()` (drop extras); the
 * live path instead carries the evicted tail — rows of the previous
 * view older than the new view's floor and absent from both — onto
 * the extras head. Zero round trips (the rows are already in hand);
 * the assembly stays contiguous with no duplication, and the
 * extras-tail `more` flag keeps its meaning (the tail does not move,
 * so the `more()` predicate cannot lie afterward). Same-reference
 * no-op when nothing is pinned or nothing was evicted. Never mutates
 * input.
 */
export function reconcilePinnedWindow(prevView, nextView, extras) {
  if (!extras || extras.length === 0) return extras ?? [];
  if (!prevView || prevView.length === 0 || !nextView || nextView.length === 0) return extras;
  let floor = Infinity;
  const nextSeqs = new Set();
  for (const ev of nextView) {
    const s = ev?.seq;
    if (typeof s !== "number" || !Number.isFinite(s)) continue;
    nextSeqs.add(s);
    if (s < floor) floor = s;
  }
  if (nextSeqs.size === 0) return extras;
  const extraSeqs = new Set();
  for (const ev of extras) {
    const s = ev?.seq;
    if (typeof s === "number" && Number.isFinite(s)) extraSeqs.add(s);
  }
  // prevView is newest-first, so the filter preserves newest-first
  // order for the carried head.
  const carry = [];
  for (const ev of prevView) {
    const s = ev?.seq;
    if (typeof s !== "number" || !Number.isFinite(s)) continue;
    if (s >= floor || nextSeqs.has(s) || extraSeqs.has(s)) continue;
    extraSeqs.add(s);
    carry.push(ev);
  }
  if (carry.length === 0) return extras;
  return [...carry, ...extras];
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
