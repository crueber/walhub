// web/src/lib/blob-lines.js — blob line-selection model (issue #243).
//
// Pure module (no Solid, no DOM): line splitting, the GitHub-compatible
// #L12 / #L12-L20 hash codec, and the range helpers. All DOM (per-line
// rows, gutter anchors, mouse drag, scroll-into-view) lives in Blob.jsx;
// this module is the headless-testable rule, the same split settingsNav.js
// uses for the settings sidebar hashes.

/**
 * Split blob text into display lines. Pops the single trailing "" that a
 * final newline produces, so "a\n" is one line, not two.
 */
export function splitLines(text) {
  const ls = String(text ?? "").split("\n");
  if (ls.length > 1 && ls[ls.length - 1] === "") ls.pop();
  return ls;
}

function toLineNo(v) {
  if (typeof v === "number") {
    if (!Number.isFinite(v)) return null;
  } else if (typeof v === "string") {
    if (!/^\d+$/.test(v)) return null;
    v = Number(v);
  } else {
    return null;
  }
  if (!Number.isInteger(v)) return null;
  return v >= 1 ? v : null;
}

/**
 * Normalize two line endpoints to an ascending {start, end} range, or null
 * when either endpoint is not a positive integer line number. Dragging
 * upward (focus < anchor) normalizes to ascending order.
 */
export function rangeOf(a, b) {
  const x = toLineNo(a);
  const y = toLineNo(b);
  if (x == null || y == null) return null;
  return x <= y ? { start: x, end: y } : { start: y, end: x };
}

/**
 * The drag/shift-click range from an anchor line to a focus line. A missing
 * anchor (plain click, keyboard activation with no prior selection) falls
 * back to the focus line itself — a single-line range.
 */
export function dragRange(anchor, focus) {
  return rangeOf(anchor ?? focus, focus);
}

/**
 * Format a range as a shareable hash: "#L12" for a single line,
 * "#L12-L20" for a range (ascending). Returns "" for anything invalid —
 * the caller keeps the current URL instead of writing garbage.
 */
export function lineHash(start, end) {
  const r = rangeOf(start, end);
  if (!r) return "";
  return r.start === r.end ? `#L${r.start}` : `#L${r.start}-L${r.end}`;
}

/**
 * Parse a URL hash into a {start, end} range (single line → start == end),
 * or null for anything that is not exactly #L<n> or #L<n>-L<m> with
 * positive integer line numbers. Reversed ranges normalize to ascending.
 */
export function parseLineHash(hash) {
  if (typeof hash !== "string") return null;
  const m = /^#L(\d+)(?:-L(\d+))?$/.exec(hash);
  if (!m) return null;
  return rangeOf(m[1], m[2] ?? m[1]);
}

/** True when line n falls inside sel (inclusive); a null sel selects nothing. */
export function inSelection(n, sel) {
  const line = toLineNo(n);
  if (line == null || sel == null) return false;
  const s = toLineNo(sel.start);
  const e = toLineNo(sel.end);
  if (s == null || e == null) return false;
  return line >= Math.min(s, e) && line <= Math.max(s, e);
}

/** Structural equality for selections (null == null); guards hash-echo loops. */
export function sameSelection(a, b) {
  if (a == null || b == null) return a == null && b == null;
  return a.start === b.start && a.end === b.end;
}
