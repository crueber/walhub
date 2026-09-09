// web/src/lib/diff-lines.js — commit/PR diff line selection (issue #244).
//
// Pure module (no Solid, no DOM): per-line old/new numbering derived from
// hunk headers, split-view rows carrying those numbers, and the
// file-scoped shareable hash codec. All DOM (gutter anchors, mouse drag,
// scroll-into-view) lives in components/DiffTable.jsx; this module is the
// headless-testable rule.
//
// Numbering: unified lines show the NEW-side number for context (' ') and
// additions ('+'), the OLD-side number for deletions ('-') — GitHub-style.
// newNo increments on every non-'-' line from hunk.newStart; oldNo on every
// non-'+' line from hunk.oldStart.
//
// Hash scheme (file-scoped, GitHub-compatible tail):
//   new-side: #<encodeURIComponent(path)>L12 / #<enc>L12-L20
//   old-side (unified deletions, split left column):
//             #<enc>OL12 / #<enc>OL12-OL20
// The O-prefix side marker is the scheme this module pins — documented on
// issue #244. Split and unified share the same hash: a selection names a
// file line, not a view column. Blob #L<n> hashes and #f-<file> anchors
// parse to null here (different vocabularies, same location.hash).

import { rangeOf } from "./blob-lines.js";
import { splitRows } from "./diff.js";

function startOf(hunk, key) {
  const v = Number(hunk?.[key]);
  return Number.isFinite(v) && v >= 0 ? v : 0;
}

/**
 * Number every line of a parsed hunk from its @@ header.
 * @returns {Array<{t: string, text: string, oldNo: number|null, newNo: number|null}>}
 */
export function annotateHunkLines(hunk) {
  const lines = hunk?.lines ?? [];
  let o = startOf(hunk, "oldStart");
  let n = startOf(hunk, "newStart");
  return lines.map((l) => {
    const t = l?.t;
    const text = l?.text ?? "";
    if (t === " ") {
      const row = { t, text, oldNo: o, newNo: n };
      o++;
      n++;
      return row;
    }
    if (t === "-") {
      const row = { t, text, oldNo: o, newNo: null };
      o++;
      return row;
    }
    if (t === "+") {
      const row = { t, text, oldNo: null, newNo: n };
      n++;
      return row;
    }
    return { t: t ?? " ", text, oldNo: null, newNo: null };
  });
}

/**
 * Split-view rows for one hunk, carrying per-side line numbers.
 * Pairing is splitRows() itself (same input → same rows, guaranteed — the
 * numbers zip back onto the paired texts in run order); the drift-hash
 * inputs (hunk.lines) are never touched.
 * @returns {Array<{left: {t, text, no}|null, right: {t, text, no}|null}>}
 */
export function annotateSplitRows(hunk) {
  const ann = annotateHunkLines(hunk);
  const rows = splitRows(ann.map(({ t, text }) => ({ t, text })));
  let ai = 0;
  let ri = 0;
  while (ai < ann.length) {
    const a = ann[ai];
    if (a.t === " ") {
      const row = rows[ri++];
      if (row?.left) row.left.no = a.oldNo;
      if (row?.right) row.right.no = a.newNo;
      ai++;
      continue;
    }
    const dels = [];
    const adds = [];
    while (ai < ann.length && ann[ai].t === "-") dels.push(ann[ai++]);
    while (ai < ann.length && ann[ai].t === "+") adds.push(ann[ai++]);
    // This run's rows hold exactly dels.length left cells and adds.length
    // right cells (lcsPair consumes every line once, in order) — scan rows
    // until both counts are satisfied.
    let di = 0;
    let gi = 0;
    while (ri < rows.length && (di < dels.length || gi < adds.length)) {
      const row = rows[ri++];
      if (row.left) row.left.no = dels[di++]?.oldNo ?? null;
      if (row.right) row.right.no = adds[gi++]?.newNo ?? null;
    }
  }
  return rows;
}

/** The selectable number of an annotated unified line (GitHub-style). */
export function unifiedNo(line) {
  if (!line) return null;
  return line.t === "-" ? line.oldNo : line.newNo;
}

/** Selection side of an annotated unified line: deletions are old-side. */
export function unifiedSide(line) {
  return line?.t === "-" ? "old" : "new";
}

/**
 * Format a shareable diff hash. Returns "" for anything invalid — the
 * caller keeps the current URL instead of writing garbage.
 */
export function diffLineHash(path, side, start, end) {
  const r = rangeOf(start, end);
  if (typeof path !== "string" || path === "") return "";
  if (!r) return "";
  const s = side === "old" ? "O" : "";
  const p = encodeURIComponent(path);
  return r.start === r.end
    ? `#${p}${s}L${r.start}`
    : `#${p}${s}L${r.start}-${s}L${r.end}`;
}

/**
 * Parse a diff hash into {path, side, start, end}, or null for anything
 * else (blob #L hashes, #f- file anchors, garbage, mixed-side ranges).
 */
export function parseDiffHash(hash) {
  if (typeof hash !== "string") return null;
  // Blob vocabulary takes precedence for its exact shape: #L<n> / #L<n>-L<m>
  // with an empty path is a blob selection, never a diff selection.
  if (/^#L\d+(?:-L\d+)?$/.test(hash)) return null;
  const m = /^#(.+?)(O?)L(\d+)(?:-(O?)L(\d+))?$/.exec(hash);
  if (!m) return null;
  const [, enc, s1, a, s2, b] = m;
  if (b !== undefined && s2 !== s1) return null; // mixed-side range
  let path;
  try {
    path = decodeURIComponent(enc);
  } catch {
    return null;
  }
  if (path === "") return null;
  const r = rangeOf(a, b ?? a);
  if (!r) return null;
  return { path, side: s1 === "O" ? "old" : "new", start: r.start, end: r.end };
}

/** Structural equality for diff selections; guards hash-echo loops. */
export function sameDiffSelection(a, b) {
  if (a == null || b == null) return a == null && b == null;
  return a.path === b.path && a.side === b.side && a.start === b.start && a.end === b.end;
}

/**
 * Indices of the annotated unified lines whose side-number falls in
 * [start, end]. Empty when the range names no line in this hunk (unknown
 * path/out-of-range hashes highlight nothing, never crash).
 */
export function matchAnnotated(ann, side, start, end) {
  const out = [];
  for (let i = 0; i < ann.length; i++) {
    const no = side === "old" ? ann[i].oldNo : ann[i].newNo;
    if (no != null && no >= start && no <= end) out.push(i);
  }
  return out;
}

/**
 * Clamp a drag focus to the anchor's chunk: cross-hunk mouseover is
 * ignored by the caller (the range stops at the chunk edge — GitHub's
 * behavior). Returns the ascending index {start, end}, or null when
 * either endpoint is off-chunk.
 */
export function chunkRange(anchorIdx, focusIdx, lineCount) {
  const r = rangeOf(anchorIdx + 1, focusIdx + 1);
  if (!r) return null;
  if (r.start < 1 || r.end > lineCount) return null;
  return { start: r.start - 1, end: r.end - 1 };
}
