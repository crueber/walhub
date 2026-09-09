// web/test/unit/diff-lines.test.js — issue #244: commit/PR diff line
// selection with shareable #<path>L links. Pins the headless-testable rule
// (web/src/lib/diff-lines.js: @@-derived numbering, split rows carrying
// numbers, file-scoped hash codec, chunk clamp) and the DiffTable.jsx
// wiring shared by Commit.jsx + PullFiles.jsx. Row highlight, drag,
// scroll, and both themes are covered by the real-Chromium pass, not
// node --test. The anchorContextSha pinned vector is the drift-hash
// tripwire: numbering is display-only and must never perturb it.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import {
  annotateHunkLines,
  annotateSplitRows,
  unifiedNo,
  unifiedSide,
  diffLineHash,
  parseDiffHash,
  sameDiffSelection,
  matchAnnotated,
  chunkRange,
} from "../../src/lib/diff-lines.js";
import { splitRows, parsePatchFiles, anchorContextSha } from "../../src/lib/diff.js";

const require = createRequire(import.meta.url);

const PATCH = `diff --git a/x.txt b/x.txt
index 1111111..2222222 100644
--- a/x.txt
+++ b/x.txt
@@ -1,3 +1,4 @@
 a
-b
+c
+d
 e
@@ -10,2 +11,1 @@
 ctx
-del
`;

function hunk0() {
  return parsePatchFiles(PATCH, "s").files[0].hunks[0];
}
function hunk1() {
  return parsePatchFiles(PATCH, "s").files[0].hunks[1];
}

test("annotateHunkLines derives per-side numbers from the @@ header", () => {
  const ann = annotateHunkLines(hunk0());
  // lines: ' 'a, '-'b, '+'c, '+'d, ' 'e — old 1..3, new 1..4
  assert.deepEqual(ann.map((l) => l.oldNo), [1, 2, null, null, 3]);
  assert.deepEqual(ann.map((l) => l.newNo), [1, null, 2, 3, 4]);
});

test("annotateHunkLines continues numbering into later hunks", () => {
  const ann = annotateHunkLines(hunk1());
  // lines: ' 'ctx, '-'del — old 10..11, new 11
  assert.deepEqual(ann.map((l) => l.oldNo), [10, 11]);
  assert.deepEqual(ann.map((l) => l.newNo), [11, null]);
});

test("annotateHunkLines handles added/deleted files (0 start)", () => {
  const added = parsePatchFiles(`diff --git a/n b/n
new file mode 100644
--- /dev/null
+++ b/n
@@ -0,0 +1,2 @@
+x
+y
`, "s").files[0].hunks[0];
  assert.deepEqual(annotateHunkLines(added).map((l) => l.newNo), [1, 2]);
  const deleted = parsePatchFiles(`diff --git a/g b/g
deleted file mode 100644
--- a/g
+++ /dev/null
@@ -1,1 +0,0 @@
+no
`.replace("+no", "-no"), "s").files[0].hunks[0];
  assert.deepEqual(annotateHunkLines(deleted).map((l) => l.oldNo), [1]);
});

test("unifiedNo/unifiedSide: context+adds read new-side, deletions old-side", () => {
  const ann = annotateHunkLines(hunk0());
  assert.deepEqual(ann.map(unifiedNo), [1, 2, 2, 3, 4]);
  assert.deepEqual(ann.map(unifiedSide), ["new", "old", "new", "new", "new"]);
});

test("annotateSplitRows pairs exactly like splitRows, carrying numbers", () => {
  const h = { oldStart: 1, newStart: 1, lines: [
    { t: " ", text: "a" },
    { t: "-", text: "b" },
    { t: "+", text: "b" },
    { t: "+", text: "c" },
    { t: " ", text: "e" },
  ] };
  const plain = splitRows(h.lines.map(({ t, text }) => ({ t, text })));
  const numbered = annotateSplitRows(h);
  assert.equal(numbered.length, plain.length);
  assert.deepEqual(
    numbered.map((r) => ({ l: r.left?.text ?? null, r: r.right?.text ?? null })),
    plain.map((r) => ({ l: r.left?.text ?? null, r: r.right?.text ?? null })),
  );
  // context row carries both numbers; paired change row carries old left, new right
  assert.deepEqual([numbered[0].left.no, numbered[0].right.no], [1, 1]);
  const pair = numbered.find((r) => r.left && r.right && r.left.t === "-");
  assert.deepEqual([pair.left.no, pair.right.no], [2, 2]);
  const rightOnly = numbered.find((r) => !r.left && r.right);
  assert.equal(rightOnly.right.no, 3);
});

test("annotateSplitRows parity holds with duplicate texts and long runs", () => {
  const lines = [];
  for (let i = 0; i < 25; i++) lines.push({ t: "-", text: "same" });
  for (let i = 0; i < 25; i++) lines.push({ t: "+", text: "same" });
  const h = { oldStart: 5, newStart: 7, lines };
  const plain = splitRows(lines);
  const numbered = annotateSplitRows(h);
  assert.equal(numbered.length, plain.length);
  const leftNos = numbered.filter((r) => r.left).map((r) => r.left.no);
  const rightNos = numbered.filter((r) => r.right).map((r) => r.right.no);
  assert.deepEqual(leftNos, Array.from({ length: 25 }, (_, i) => 5 + i));
  assert.deepEqual(rightNos, Array.from({ length: 25 }, (_, i) => 7 + i));
});

test("diffLineHash formats new-side, old-side, and ranges", () => {
  assert.equal(diffLineHash("x.txt", "new", 12, 12), "#x.txtL12");
  assert.equal(diffLineHash("x.txt", "new", 12, 20), "#x.txtL12-L20");
  assert.equal(diffLineHash("x.txt", "new", 20, 12), "#x.txtL12-L20");
  assert.equal(diffLineHash("x.txt", "old", 12, 12), "#x.txtOL12");
  assert.equal(diffLineHash("x.txt", "old", 12, 20), "#x.txtOL12-OL20");
  assert.equal(diffLineHash("src/a b/c#.js", "new", 3, 3), "#src%2Fa%20b%2Fc%23.jsL3");
  for (const bad of [["", "new", 1, 1], [null, "new", 1, 1], ["x", "new", 0, 0], ["x", "new", "z", 2]]) {
    assert.equal(diffLineHash(bad[0], bad[1], bad[2], bad[3]), "", `invalid: ${JSON.stringify(bad)}`);
  }
});

test("parseDiffHash reads the file-scoped scheme, ignores everything else", () => {
  assert.deepEqual(parseDiffHash("#x.txtL12"), { path: "x.txt", side: "new", start: 12, end: 12 });
  assert.deepEqual(parseDiffHash("#x.txtL12-L20"), { path: "x.txt", side: "new", start: 12, end: 20 });
  assert.deepEqual(parseDiffHash("#x.txtOL12-OL20"), { path: "x.txt", side: "old", start: 12, end: 20 });
  assert.deepEqual(parseDiffHash("#src%2Fa%20b%2Fc%23.jsL3"), { path: "src/a b/c#.js", side: "new", start: 3, end: 3 });
  assert.deepEqual(parseDiffHash("#x.txtL20-L12"), { path: "x.txt", side: "new", start: 12, end: 20 });
  for (const bad of ["", "#", "#L12", "#L12-L20", "#f-x-txt", "#x.txtL0",
    "#x.txtL12-OL20", "#x.txtOL12-L20", "#x.txtL12-", "#x.txtL", "#x.txtOL",
    "#x.txtL1.5", "#x.txtL12x", undefined, null, 0, {}, []]) {
    assert.equal(parseDiffHash(bad), null, `ignored: ${String(bad)}`);
  }
});

test("parseDiffHash tolerates file-anchor-shaped collisions by path mismatch", () => {
  // "#f-aL12" technically parses as path "f-a" line 12 — harmless: the
  // renderer only highlights when the path names a file in the diff, and
  // native anchor scroll is untouched (DiffTable never preventDefaults
  // the pill links).
  assert.deepEqual(parseDiffHash("#f-aL12"), { path: "f-a", side: "new", start: 12, end: 12 });
});

test("hash round-trips: format(parse(x)) is stable", () => {
  for (const h of ["#x.txtL1", "#a%2Fb.jsL12-L20", "#x.txtOL12", "#x.txtOL12-OL20"]) {
    const sel = parseDiffHash(h);
    assert.equal(diffLineHash(sel.path, sel.side, sel.start, sel.end), h, h);
  }
});

test("sameDiffSelection compares path+side+range", () => {
  const a = { path: "x", side: "new", start: 1, end: 2 };
  assert.equal(sameDiffSelection(a, { ...a }), true);
  assert.equal(sameDiffSelection(a, { ...a, end: 3 }), false);
  assert.equal(sameDiffSelection(a, { ...a, side: "old" }), false);
  assert.equal(sameDiffSelection(a, { ...a, path: "y" }), false);
  assert.equal(sameDiffSelection(null, null), true);
  assert.equal(sameDiffSelection(a, null), false);
});

test("matchAnnotated selects by side number; unknown ranges match nothing", () => {
  const ann = annotateHunkLines(hunk0());
  assert.deepEqual(matchAnnotated(ann, "new", 2, 3), [2, 3]);
  assert.deepEqual(matchAnnotated(ann, "old", 2, 2), [1]);
  assert.deepEqual(matchAnnotated(ann, "new", 99, 100), []);
  assert.deepEqual(matchAnnotated([], "new", 1, 1), []);
});

test("chunkRange normalizes drag direction, rejects off-chunk endpoints", () => {
  assert.deepEqual(chunkRange(2, 5, 10), { start: 2, end: 5 });
  assert.deepEqual(chunkRange(5, 2, 10), { start: 2, end: 5 });
  assert.deepEqual(chunkRange(4, 4, 10), { start: 4, end: 4 });
  assert.equal(chunkRange(0, 10, 10), null);
  assert.equal(chunkRange(-1, 2, 10), null);
});

test("drift-hash contract: parser output gains no numbering fields", () => {
  const { files } = parsePatchFiles(PATCH, "s");
  for (const h of files[0].hunks) {
    for (const l of h.lines) {
      assert.deepEqual(Object.keys(l).sort(), ["t", "text"]);
    }
  }
});

test("drift-hash contract: anchorContextSha pinned vector unchanged", () => {
  // Mirrors diff-review.test.js §8 dogfood vector (Go twin: DriftHash in
  // internal/review/model.go) — numbering changes are display-only.
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

test("Commit.jsx renders the shared selectable DiffBody, not the number-less table", () => {
  const fs = require("node:fs");
  const src = fs.readFileSync(new URL("../../src/pages/Commit.jsx", import.meta.url), "utf8");
  assert.match(src, /from "\.\.\/components\/DiffTable\.jsx"/, "imports the shared renderer");
  assert.match(src, /<DiffBody file=\{f\(\)\} mode=\{getMode\(\)\} \/>/, "delegates per-file rendering");
  assert.ok(!src.includes("splitRows"), "no local split pairing (per-hunk rows live in DiffTable)");
});

test("PullFiles.jsx shares the same renderer with a unified/split toggle", () => {
  const fs = require("node:fs");
  const src = fs.readFileSync(new URL("../../src/pages/PullFiles.jsx", import.meta.url), "utf8");
  assert.match(src, /from "\.\.\/components\/DiffTable\.jsx"/, "imports the shared renderer");
  assert.match(src, /<DiffBody file=\{[^}]*\} mode=\{getMode\(\)\} \/>/, "renders DiffBody with a mode toggle");
});

test("DiffTable.jsx carries gutters, drag handlers, hash sync, and highlight", () => {
  const fs = require("node:fs");
  const src = fs.readFileSync(new URL("../../src/components/DiffTable.jsx", import.meta.url), "utf8");
  assert.match(src, /from "\.\.\/lib\/diff-lines\.js"/, "imports the selection model");
  assert.match(src, /class="diff-num"/, "gutter cells in both modes");
  assert.match(src, /aria-label=\{label\}/, "gutter anchors are labelled");
  assert.match(src, /`Diff line \$\{no\}/, "labels name the line");
  assert.match(src, /onMouseDown.*begin/, "mousedown sets the drag anchor");
  assert.match(src, /onMouseOver.*extend/, "mouseover extends the drag");
  assert.match(src, /onClick.*activate/, "click/keyboard pushes the hash");
  assert.match(src, /ev\.shiftKey && anchor/, "shift extends, plain jumps");
  assert.match(src, /anchor\.hunk === h && anchor\.side === side/, "drag clamps to chunk + side");
  assert.match(src, /hashchange/, "back/forward + pasted URLs re-highlight");
  assert.match(src, /scrollIntoView/, "shared URLs scroll to the target");
  assert.match(src, /replaceState/, "drag frames replace, not push");
  assert.match(src, /"line-hl"/, "selection highlight class");
  assert.match(src, /colspan=\{4\}/, "split hunk headers span both column pairs");
  assert.ok(!/anchorContextSha\s*\(/.test(src), "never calls the drift hash (comment mention only)");
});

test("ui.css carries the diff gutter + highlight rules for both themes", () => {
  const fs = require("node:fs");
  const css = fs.readFileSync(new URL("../../src/ui.css", import.meta.url), "utf8");
  assert.match(css, /\.diff-num/, "gutter column");
  assert.match(css, /\.diff-num a/, "gutter anchors");
  assert.match(css, /\.diff-row\.line-hl > td/, "row highlight");
  assert.match(css, /\.diff-row\.line-hl > \.diff-num/, "gutter edge");
  assert.match(css, /\.dark \.diff-row\.line-hl > td/, "dark-theme highlight");
});
