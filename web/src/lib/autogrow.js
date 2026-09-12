/**
 * autogrow.js — shared textarea auto-grow (Forgejo #419).
 *
 * One helper consumed by both profile bio textareas (owner form in
 * pages/Repos.jsx, org form in pages/Org.jsx) — no per-page copies.
 *
 * Behavior:
 * - The textarea grows AND shrinks to fit its content, starting from its
 *   `rows` height: `initAutogrow` records the mounted height as the floor
 *   (rows stay as the no-JS fallback), `growTextarea` refits on every
 *   input — including Enter/newline — by resetting `height:auto` before
 *   measuring `scrollHeight` (measuring without the reset is the classic
 *   grows-but-never-shrinks bug).
 * - Caret stays in view: while the content fits, the box IS the content
 *   (no inner scroll, so the caret is always visible). Past the cap the
 *   box scrolls internally and a caret pinned at the end of the text
 *   (the Enter-at-end case) is scrolled into view explicitly; mid-text
 *   carets are left to the browser's native scroll-into-view.
 * - Max bound (exact cap): 50vh (`AUTOGROW_MAX_VH`) — a 60-line bio stops
 *   growing at half the viewport and scrolls internally (`overflow-y:auto`
 *   beyond the cap, `hidden` while fitted to avoid scrollbar flicker).
 *
 * Split per repo convention (lib/attachUpload.js, lib/settingsNav.js):
 * pure, headless-testable height math (`autogrowMaxPx`,
 * `clampAutogrowHeight`) separate from the DOM-touching `initAutogrow` /
 * `growTextarea` (the latter also runs headless against stub elements in
 * `node --test`, pixel scroll handling aside). No Solid, no DOM globals
 * at import time — safe to import in Node.
 */

/** Exact max grown height, in viewport-height percent. Stated cap: 50vh. */
export const AUTOGROW_MAX_VH = 50;

/**
 * Max grown height in px for a viewport of `viewportHeightPx`.
 * Non-finite viewports yield 0 — combined with the rows floor in
 * `clampAutogrowHeight` that collapses to "never below min", the safe
 * direction (never unbounded, never invisible).
 */
export function autogrowMaxPx(viewportHeightPx, vh = AUTOGROW_MAX_VH) {
  const vp = Number(viewportHeightPx);
  const pct = Number(vh);
  if (!Number.isFinite(vp) || vp < 0 || !Number.isFinite(pct) || pct < 0) return 0;
  return Math.floor((vp * pct) / 100);
}

/** Sanitize a px bound: finite and >= 0, else `fallback`. */
function cleanPx(value, fallback) {
  const n = Number(value);
  return Number.isFinite(n) && n >= 0 ? n : fallback;
}

/**
 * Pure height fit: clamp `scrollHeightPx` into [minHeightPx, maxHeightPx].
 * Returns `{ heightPx, overflowing }` — `overflowing` is true exactly when
 * content exceeds the cap and the caller must scroll internally.
 * When min > max (short viewport, tall rows floor) the floor wins and the
 * page scrolls instead of the box collapsing.
 */
export function clampAutogrowHeight(scrollHeightPx, { minHeightPx = 0, maxHeightPx = Infinity } = {}) {
  const need = cleanPx(scrollHeightPx, 0);
  const min = cleanPx(minHeightPx, 0);
  let max = maxHeightPx === Infinity ? Infinity : cleanPx(maxHeightPx, 0);
  if (max < min) max = min;
  const heightPx = Math.min(Math.max(need, min), max);
  return { heightPx, overflowing: need > max };
}

/**
 * Record the textarea's current (rows-driven) height as its grow floor.
 * Call once from the Solid `ref` callback, when the element is mounted
 * and visible. Returns the recorded floor.
 */
export function initAutogrow(el, { minHeightPx } = {}) {
  if (!el) return 0;
  const measured =
    typeof minHeightPx === "number"
      ? minHeightPx
      : cleanPx(el.offsetHeight, 0) || cleanPx(el.clientHeight, 0);
  try {
    el.__autogrowMinPx = measured;
  } catch {
    /* frozen stub in tests — caller passes minHeightPx explicitly instead */
  }
  return measured;
}

/** Resolve the cap: explicit opt wins, else 50vh of the current viewport, else unbounded headless. */
function resolveMaxPx(opts) {
  if (opts && typeof opts.maxHeightPx === "number") return opts.maxHeightPx;
  const w = typeof globalThis !== "undefined" ? globalThis.window : undefined;
  const vp = w && Number(w.innerHeight);
  if (Number.isFinite(vp) && vp > 0) return autogrowMaxPx(vp);
  return Infinity;
}

/** True when the caret sits at (or the element exposes no caret past) the end of the text. */
function caretAtEnd(el) {
  if (typeof el.selectionStart !== "number") return true;
  return el.selectionStart >= String(el.value ?? "").length;
}

/**
 * Refit a textarea to its content. Resolves the floor from
 * `opts.minHeightPx ?? el.__autogrowMinPx` (see `initAutogrow`) and the
 * cap from `opts.maxHeightPx ?? 50vh`. Resets height to auto first so
 * deleting lines shrinks back toward `rows`. Past the cap sets
 * `overflow-y:auto` and pins a trailing caret into view (the
 * Enter-at-end case). Returns `{ heightPx, overflowing }`.
 */
export function growTextarea(el, opts = {}) {
  if (!el) return { heightPx: 0, overflowing: false };
  const min =
    typeof opts.minHeightPx === "number"
      ? opts.minHeightPx
      : cleanPx(el.__autogrowMinPx, 0);
  const max = resolveMaxPx(opts);
  el.style.height = "auto";
  // Measure AFTER the reset: without it scrollHeight never drops and the
  // box grows but never shrinks. (On real elements assigning style can
  // change scrollHeight; stubs keep the preset value — same result.)
  const { heightPx, overflowing } = clampAutogrowHeight(el.scrollHeight, {
    minHeightPx: min,
    maxHeightPx: max,
  });
  el.style.height = `${heightPx}px`;
  el.style.overflowY = overflowing ? "auto" : "hidden";
  if (overflowing && caretAtEnd(el)) {
    try {
      el.scrollTop = el.scrollHeight;
    } catch {
      /* headless stub without scroll */
    }
  }
  return { heightPx, overflowing };
}
