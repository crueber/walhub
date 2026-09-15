// web/src/lib/review-anchor.js — Forgejo #546: diff selection → review anchor.
//
// Headless (no Solid, no DOM): converts a DiffTable/diff-lines selection
// ({path, side: "new"|"old", start, end} — the file-scoped #<path>L vocabulary
// from lib/diff-lines.js) into a §4 ThreadAnchor, plus the jump-to-comments
// index helpers. Both conversation surfaces (Pull.jsx DiffFile, PullFiles.jsx
// DiffBody) build anchors ONLY through here, so single-line keeps the exact
// stageLine shape and range anchors stay clamped to one chunk + one side.
//
// Hash contract: context_sha is ALWAYS anchorContextSha (lib/diff.js — the
// ONLY §4 implementation, twin DriftHash in internal/review/model.go) fed
// byte-identically: {path, lines: hunk.lines} + the contiguous hunk-lines
// span covering the selection. Single-line spans are {start: idx, count: 1},
// i.e. exactly what Pull.jsx stageLine hashed before — the pinned vectors in
// diff-review.test.js / diff-lines.test.js are the tripwire. Range anchors
// widen only the span (count = endIdx - startIdx + 1); the function, the
// byte layout, and the single-line inputs never change.

import {
  annotateHunkLines,
  matchAnnotated,
} from "./diff-lines.js";
import { anchorContextSha } from "./diff.js";

/**
 * Locate which hunk of a parsed file holds a selection: the first hunk
 * whose annotated lines cover the selection's [start, end] on its side.
 * Returns {hunk, hunkIndex} or null (unknown path, bad range, or no line
 * of the range lives in any hunk — e.g. a stale shared URL).
 */
export function findHunkForSelection(file, selection) {
  if (!file || file.path !== selection?.path) return null;
  const { side, start, end } = selection ?? {};
  if ((side !== "new" && side !== "old") || !Number.isInteger(start) || !Number.isInteger(end)) return null;
  if (start < 1 || end < start) return null;
  const hunks = file.hunks ?? [];
  for (let hi = 0; hi < hunks.length; hi++) {
    const ann = annotateHunkLines(hunks[hi]);
    const idxs = matchAnnotated(ann, side, start, end);
    if (idxs.length) return { hunk: hunks[hi], hunkIndex: hi };
  }
  return null;
}

/**
 * Build a §4 anchor for a line-number range on one side of one hunk.
 * side is the wire spelling ("NEW"|"OLD"); startNo/endNo are ascending
 * file line numbers on that side (endNo === startNo for a single line).
 * Returns null when the range names no line in the hunk.
 *
 * Shape convention (stageLine-compatible): NEW-side anchors zero the
 * old_* pair, OLD-side anchors zero the new_* pair; single-line keeps
 * new_lines/old_lines === 1 exactly.
 */
export function buildAnchor({ file, hunk, side, startNo, endNo, head }) {
  if (!file?.path || !hunk || (side !== "NEW" && side !== "OLD")) return null;
  if (!Number.isInteger(startNo) || !Number.isInteger(endNo)) return null;
  if (startNo < 1 || endNo < startNo) return null;
  if (typeof head !== "string" || head === "") return null;
  const selSide = side === "NEW" ? "new" : "old";
  const ann = annotateHunkLines(hunk);
  const idxs = matchAnnotated(ann, selSide, startNo, endNo);
  if (!idxs.length) return null;
  const lo = Math.min(...idxs);
  const hi = Math.max(...idxs);
  const context_sha = anchorContextSha(
    { path: file.path, lines: hunk.lines },
    { start: lo, count: hi - lo + 1 },
  );
  const span = endNo - startNo + 1;
  return side === "NEW"
    ? { path: file.path, side, old_start: 0, old_lines: 0, new_start: startNo, new_lines: span, commit_sha: head, context_sha }
    : { path: file.path, side, old_start: startNo, old_lines: span, new_start: 0, new_lines: 0, commit_sha: head, context_sha };
}

/**
 * Build a §4 anchor from a diff-lines selection + the hunk holding it.
 * The selection is already clamped to one chunk + one side by the
 * DiffTable drag logic; this resolves the hunk span + hash only.
 */
export function selectionToAnchor({ file, hunk, selection, head }) {
  if (!selection || (selection.side !== "new" && selection.side !== "old")) return null;
  if (!file || file.path !== selection.path) return null;
  return buildAnchor({
    file,
    hunk,
    side: selection.side === "old" ? "OLD" : "NEW",
    startNo: selection.start,
    endNo: selection.end,
    head,
  });
}

/** Anchor line label: "path:12" single, "path:12-15" range (side-aware). */
export function anchorLabel(anchor) {
  const a = anchor ?? {};
  const start = a.side === "OLD" ? a.old_start : a.new_start;
  const lines = a.side === "OLD" ? a.old_lines : a.new_lines;
  if (!a.path || !Number.isInteger(start)) return String(a.path ?? "");
  return lines > 1 ? `${a.path}:${start}-${start + lines - 1}` : `${a.path}:${start}`;
}

/**
 * View-time freshness of one anchor against the CURRENT parsed files —
 * the same placement truth DiffFile renders (anchors never relocate).
 * Recomputes the hash over the anchor's OWN span: single-line anchors
 * hash {start: idx, count: 1} exactly as before (pinned vectors hold);
 * range anchors hash their full span. Returns {fresh, hunkIndex, startIdx,
 * count}; fresh is false when either endpoint no longer locates.
 */
export function freshnessOf(anchor, files) {
  const a = anchor ?? {};
  const side = a.side === "OLD" ? "old" : a.side === "NEW" ? "new" : null;
  if (!a.path || !side) return { fresh: false, hunkIndex: -1, startIdx: -1, count: 0 };
  const start = side === "new" ? a.new_start : a.old_start;
  const lines = side === "new" ? a.new_lines : a.old_lines;
  if (!Number.isInteger(start) || !Number.isInteger(lines) || start < 1 || lines < 1) {
    return { fresh: false, hunkIndex: -1, startIdx: -1, count: 0 };
  }
  for (const f of files ?? []) {
    if (f?.path !== a.path) continue;
    const hunks = f.hunks ?? [];
    for (let hi = 0; hi < hunks.length; hi++) {
      const ann = annotateHunkLines(hunks[hi]);
      const loIdx = ann.findIndex((r) => (side === "new" ? r.newNo : r.oldNo) === start);
      if (loIdx < 0) continue;
      const hiIdx = ann.findIndex((r) => (side === "new" ? r.newNo : r.oldNo) === start + lines - 1);
      if (hiIdx < 0) return { fresh: false, hunkIndex: hi, startIdx: loIdx, count: 0 };
      const fresh = anchorContextSha(
        { path: f.path, lines: hunks[hi].lines },
        { start: Math.min(loIdx, hiIdx), count: Math.abs(hiIdx - loIdx) + 1 },
      ) === a.context_sha;
      return { fresh, hunkIndex: hi, startIdx: loIdx, count: Math.abs(hiIdx - loIdx) + 1 };
    }
  }
  return { fresh: false, hunkIndex: -1, startIdx: -1, count: 0 };
}

/**
 * Unresolved-first stable order for the jump-to-comments index (the same
 * ordering the inline ThreadCards use — resolve/unresolve behavior
 * unchanged).
 */
export function sortThreadsForIndex(threads) {
  return [...(threads ?? [])]
    .map((t, i) => ({ t, i }))
    .sort((a, b) => Number(a.t?.resolved) - Number(b.t?.resolved) || a.i - b.i)
    .map(({ t }) => t);
}
