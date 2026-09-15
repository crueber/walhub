// web/test/unit/review-anchor-546.test.js — Forgejo #546: select line(s)
// in a PR diff to create inline comment threads + jump-to-comments index.
//
// Pins the headless rule (web/src/lib/review-anchor.js): selection → §4
// anchor conversion (single-line keeps the stageLine shape, multi-line
// becomes a range, NEW/OLD zero-pair convention), byte-identical hash
// inputs (single-line spans hash exactly what stageLine hashed — the
// pinned Go-twin vector is the tripwire), view-time freshness over the
// anchor's own span, unresolved-first index order, and the wiring pins
// (DiffTable affordance → shared CommentComposer, no prompt on the
// comment path; Files-tab threads.create; conversation ThreadIndex).

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import { parsePatchFiles, anchorContextSha } from "../../src/lib/diff.js";
import { annotateHunkLines, matchAnnotated } from "../../src/lib/diff-lines.js";
import {
  findHunkForSelection,
  buildAnchor,
  selectionToAnchor,
  anchorLabel,
  freshnessOf,
  sortThreadsForIndex,
} from "../../src/lib/review-anchor.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");

const PATCH = `diff --git a/x.txt b/x.txt
index 1111111..2222222 100644
--- a/x.txt
+++ b/x.txt
@@ -1,5 +1,6 @@
 ctx1
-old1
+new1
+new2
 ctx2
 ctx3
@@ -20,3 +21,3 @@
 ctxA
-delA
+addA
 ctxB
`;

const HEAD = "abcdef1234567890abcdef1234567890abcdef12";

function files() {
  return parsePatchFiles(PATCH).files;
}
function file0() {
  return files()[0];
}
function hunk0() {
  return file0().hunks[0];
}
function hunk1() {
  return file0().hunks[1];
}

// --- single-line shape (stageLine-compatible) --------------------------------

test("single NEW line keeps the stageLine shape", () => {
  // hunk0: ctx1(1/1), -old1(2/-), +new1(-/2), +new2(-/3), ctx2(3/4), ctx3(4/5)
  const a = buildAnchor({ file: file0(), hunk: hunk0(), side: "NEW", startNo: 2, endNo: 2, head: HEAD });
  assert.deepEqual(a, {
    path: "x.txt", side: "NEW",
    old_start: 0, old_lines: 0, new_start: 2, new_lines: 1,
    commit_sha: HEAD, context_sha: a.context_sha,
  });
  assert.match(a.context_sha, /^[0-9a-f]{64}$/);
});

test("single OLD line keeps the stageLine shape", () => {
  const a = buildAnchor({ file: file0(), hunk: hunk0(), side: "OLD", startNo: 2, endNo: 2, head: HEAD });
  assert.equal(a.side, "OLD");
  assert.deepEqual([a.old_start, a.old_lines], [2, 1]);
  assert.deepEqual([a.new_start, a.new_lines], [0, 0]);
});

test("single NEW context line anchors on the new number", () => {
  const a = buildAnchor({ file: file0(), hunk: hunk0(), side: "NEW", startNo: 4, endNo: 4, head: HEAD });
  assert.deepEqual([a.new_start, a.new_lines], [4, 1]);
});

// --- ranges ------------------------------------------------------------------

test("multi-line NEW selection becomes a range anchor", () => {
  const a = buildAnchor({ file: file0(), hunk: hunk0(), side: "NEW", startNo: 2, endNo: 4, head: HEAD });
  assert.deepEqual([a.new_start, a.new_lines], [2, 3]);
  assert.deepEqual([a.old_start, a.old_lines], [0, 0]);
});

test("multi-line OLD selection becomes a range anchor", () => {
  const a = buildAnchor({ file: file0(), hunk: hunk0(), side: "OLD", startNo: 1, endNo: 3, head: HEAD });
  assert.deepEqual([a.old_start, a.old_lines], [1, 3]);
  assert.deepEqual([a.new_start, a.new_lines], [0, 0]);
});

test("buildAnchor rejects bad input with null (no throw)", () => {
  const ok = { file: file0(), hunk: hunk0(), side: "NEW", startNo: 1, endNo: 1, head: HEAD };
  assert.equal(buildAnchor({ ...ok, side: "MID" }), null);
  assert.equal(buildAnchor({ ...ok, startNo: 0 }), null);
  assert.equal(buildAnchor({ ...ok, endNo: 0 }), null);
  assert.equal(buildAnchor({ ...ok, startNo: 3, endNo: 2 }), null);
  assert.equal(buildAnchor({ ...ok, head: "" }), null);
  assert.equal(buildAnchor({ ...ok, startNo: 99, endNo: 100 }), null);
  assert.equal(
    selectionToAnchor({ file: { path: "other.txt", hunks: [] }, hunk: hunk0(), selection: { path: "x.txt", side: "new", start: 1, end: 1 }, head: HEAD }),
    null
  );
});

// --- hash parity: byte-identical inputs --------------------------------------

test("single-line hash equals the old stageLine computation", () => {
  // stageLine hashed {path, lines} + {start: row.idx, count: 1}; the
  // selection path must feed anchorContextSha byte-identically.
  const ann = annotateHunkLines(hunk0());
  const idxs = matchAnnotated(ann, "new", 2, 2);
  assert.equal(idxs.length, 1);
  const direct = anchorContextSha({ path: "x.txt", lines: hunk0().lines }, { start: idxs[0], count: 1 });
  const a = buildAnchor({ file: file0(), hunk: hunk0(), side: "NEW", startNo: 2, endNo: 2, head: HEAD });
  assert.equal(a.context_sha, direct);
});

test("range hash equals a direct span call (no new hash path)", () => {
  const ann = annotateHunkLines(hunk0());
  const idxs = matchAnnotated(ann, "new", 2, 4);
  const lo = Math.min(...idxs);
  const hi = Math.max(...idxs);
  const direct = anchorContextSha({ path: "x.txt", lines: hunk0().lines }, { start: lo, count: hi - lo + 1 });
  const a = buildAnchor({ file: file0(), hunk: hunk0(), side: "NEW", startNo: 2, endNo: 4, head: HEAD });
  assert.equal(a.context_sha, direct);
});

test("pinned Go-twin vector unchanged (hash inputs never changed)", () => {
  const hunk = {
    path: "src/main.go",
    lines: [
      { t: " ", text: "a" },
      { t: " ", text: "b" },
      { t: "+", text: "NEW" },
      { t: " ", text: "c" },
    ],
  };
  assert.equal(
    anchorContextSha(hunk, { start: 2, count: 1 }),
    "89e40705caa54ab6ad1deb39b0de14005dad1361f2289531ed05af1a065f7610"
  );
});

// --- selection → anchor + hunk lookup ----------------------------------------

test("selectionToAnchor maps the diff-lines vocabulary to the wire side", () => {
  const n = selectionToAnchor({
    file: file0(), hunk: hunk0(),
    selection: { path: "x.txt", side: "new", start: 4, end: 5 }, head: HEAD,
  });
  assert.deepEqual([n.side, n.new_start, n.new_lines], ["NEW", 4, 2]);
  const o = selectionToAnchor({
    file: file0(), hunk: hunk0(),
    selection: { path: "x.txt", side: "old", start: 1, end: 1 }, head: HEAD,
  });
  assert.deepEqual([o.side, o.old_start, o.old_lines], ["OLD", 1, 1]);
  assert.equal(selectionToAnchor({ file: file0(), hunk: hunk0(), selection: null, head: HEAD }), null);
  assert.equal(
    selectionToAnchor({ file: file0(), hunk: hunk0(), selection: { path: "x.txt", side: "mid", start: 1, end: 1 }, head: HEAD }),
    null
  );
});

test("findHunkForSelection resolves the holding hunk, null otherwise", () => {
  const f = file0();
  assert.equal(findHunkForSelection(f, { path: "x.txt", side: "new", start: 2, end: 3 }).hunkIndex, 0);
  assert.equal(findHunkForSelection(f, { path: "x.txt", side: "new", start: 21, end: 22 }).hunkIndex, 1);
  assert.equal(findHunkForSelection(f, { path: "other.txt", side: "new", start: 1, end: 1 }), null);
  assert.equal(findHunkForSelection(f, { path: "x.txt", side: "new", start: 999, end: 999 }), null);
  assert.equal(findHunkForSelection(f, null), null);
});

// --- labels ------------------------------------------------------------------

test("anchorLabel: single vs range, NEW vs OLD", () => {
  assert.equal(anchorLabel({ path: "x.txt", side: "NEW", new_start: 12, new_lines: 1 }), "x.txt:12");
  assert.equal(anchorLabel({ path: "x.txt", side: "NEW", new_start: 12, new_lines: 4 }), "x.txt:12-15");
  assert.equal(anchorLabel({ path: "x.txt", side: "OLD", old_start: 7, old_lines: 2 }), "x.txt:7-8");
});

// --- freshness (placement truth) ----------------------------------------------

test("freshnessOf: just-built anchors are fresh", () => {
  const f = files();
  const single = buildAnchor({ file: f[0], hunk: f[0].hunks[0], side: "NEW", startNo: 2, endNo: 2, head: HEAD });
  assert.equal(freshnessOf(single, f).fresh, true);
  const range = buildAnchor({ file: f[0], hunk: f[0].hunks[0], side: "NEW", startNo: 2, endNo: 4, head: HEAD });
  const r = freshnessOf(range, f);
  assert.equal(r.fresh, true);
  assert.ok(r.count > 1);
});

test("freshnessOf: edited context / deleted lines go stale, never relocate", () => {
  const f = files();
  // OLD 2 (-old1) has ctx1 as hashed before-context.
  const single = buildAnchor({ file: f[0], hunk: f[0].hunks[0], side: "OLD", startNo: 2, endNo: 2, head: HEAD });
  assert.equal(freshnessOf(single, f).fresh, true);
  const drifted = structuredClone(f);
  drifted[0].hunks[0].lines[0] = { t: " ", text: "ctx1 edited" };
  assert.equal(freshnessOf(single, drifted).fresh, false);
  const gone = structuredClone(f);
  // Removing a context neighbor changes the hashed context (removing one
  // of two adjacent additions would not — identical neighbors hash the
  // same, a known context-hash property, not a bug).
  gone[0].hunks[0].lines = gone[0].hunks[0].lines.filter((l, i) => i !== 0);
  assert.equal(freshnessOf(single, gone).fresh, false);
  assert.equal(freshnessOf(single, []).fresh, false);
  assert.equal(freshnessOf({ path: "x.txt", side: "NEW", new_start: 999, new_lines: 1, context_sha: single.context_sha }, f).fresh, false);
});

// --- index order ---------------------------------------------------------------

test("sortThreadsForIndex: unresolved-first, stable", () => {
  const inOrder = [
    { tid: "a", resolved: true },
    { tid: "b", resolved: false },
    { tid: "c", resolved: false },
    { tid: "d", resolved: true },
  ];
  assert.deepEqual(sortThreadsForIndex(inOrder).map((t) => t.tid), ["b", "c", "a", "d"]);
  assert.deepEqual(sortThreadsForIndex([]), []);
});

// --- wiring pins -----------------------------------------------------------------

test("DiffTable exposes comment-on-selection (shared composer path, no prompt)", () => {
  const src = read("../../src/components/DiffTable.jsx");
  assert.match(src, /onCommentSelect/, "affordance prop");
  assert.match(src, /comment on selection/, "affordance button");
  assert.match(src, /aria-label=\{`Comment on selected lines/, "labelled affordance");
  assert.ok(!src.includes("window.prompt"), "never prompts");
  assert.ok(!src.includes("CommentComposer.jsx"), "composer lives with the consumer (one composer owner)");
});

test("PullFiles stages selections into threads.create (no new wire)", () => {
  const src = read("../../src/pages/PullFiles.jsx");
  assert.match(src, /onCommentSelect/, "wires the DiffBody affordance");
  assert.match(src, /pulls\.threads\.create/, "reuses the threads SDK surface");
  assert.match(src, /from "\.\.\/components\/CommentComposer\.jsx"/, "shared composer");
  assert.match(src, /selectionToAnchor/, "anchor via the shared helper");
  assert.ok(!src.includes("window.prompt"), "never prompts");
  assert.ok(!/anchorContextSha\s*\(/.test(src), "never hashes directly (helper owns the bytes)");
});

test("Pull conversation: range anchors, composer draft, thread index", () => {
  const src = read("../../src/pages/Pull.jsx");
  assert.ok(!src.includes("window.prompt(`Comment on"), "prompt replaced by the composer draft");
  assert.match(src, /shiftKey/, "Shift+click extends a range");
  assert.match(src, /buildAnchor\(/, "anchors via the shared helper");
  assert.match(src, /ThreadIndex/, "jump-to-comments index rendered");
  assert.match(src, /anchorLabel\(/, "index + pending list share the label");
  assert.match(src, /id=\{`thread-\$\{t\(\)\.tid\}`\}/, "cards carry jump targets");
  assert.match(src, /freshnessOf/, "index reflects placement truth");
  assert.match(src, /scrollIntoView/, "jump scrolls to the card");
  assert.match(src, /· outdated/, "drifted entries marked");
});
