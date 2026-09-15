// web/test/unit/tap-comment-555.test.js — Forgejo #555: per-line
// tap/click-to-comment affordance on diff rows.
//
// Follow-up to #546: the DiffTable comment affordance required an active
// drag/shift SELECTION, so a tap (touch) or plain click (mouse) on code
// text staged nothing and no affordance ever appeared; the conversation
// "+" was hover-only, invisible on touch. The fix puts an inline "+"
// target at the end of every diff code line (Files tab DiffBody +
// conversation DiffFile) staging a single-line draft through the EXISTING
// anchor model (single-line shape, side convention, chunk clamping via
// lib/review-anchor.js — REUSED, never forked) and the #502 gate.
//
// These tests pin the tap-selection anchor shape, the per-line affordance
// wiring + gate + visibility on both surfaces, the deliberate no-touch-
// handlers decision (a tap arrives as click; code text stays handler-free
// so text selection is never fought), the untouched range flow, and the
// law-11/law-12 doc entries.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import { parsePatchFiles } from "../../src/lib/diff.js";
import {
  findHunkForSelection,
  selectionToAnchor,
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

function file0() {
  return parsePatchFiles(PATCH).files[0];
}
function hunk0() {
  return file0().hunks[0];
}

// --- tap-selection anchor shape (stageLine-compatible, never forked) ----------

test("tap on a NEW line stages the single-line stageLine shape", () => {
  // What lineTap feeds onCommentSelect for new1 (new-side line 2).
  const sel = { path: "x.txt", side: "new", start: 2, end: 2 };
  const found = findHunkForSelection(file0(), sel);
  assert.equal(found.hunkIndex, 0);
  const a = selectionToAnchor({ file: file0(), hunk: found.hunk, selection: sel, head: HEAD });
  assert.deepEqual(
    { path: a.path, side: a.side, old_start: a.old_start, old_lines: a.old_lines, new_start: a.new_start, new_lines: a.new_lines },
    { path: "x.txt", side: "NEW", old_start: 0, old_lines: 0, new_start: 2, new_lines: 1 },
  );
});

test("tap on an OLD line stages the single-line OLD shape", () => {
  // What lineTap feeds onCommentSelect for -old1 (old-side line 2).
  const sel = { path: "x.txt", side: "old", start: 2, end: 2 };
  const found = findHunkForSelection(file0(), sel);
  assert.equal(found.hunkIndex, 0);
  const a = selectionToAnchor({ file: file0(), hunk: found.hunk, selection: sel, head: HEAD });
  assert.deepEqual([a.side, a.old_start, a.old_lines], ["OLD", 2, 1]);
  assert.deepEqual([a.new_start, a.new_lines], [0, 0]);
});

test("tap chunk clamping is inherent: one line names its own hunk", () => {
  const f = parsePatchFiles(PATCH).files[0];
  const first = findHunkForSelection(f, { path: "x.txt", side: "new", start: 2, end: 2 });
  const second = findHunkForSelection(f, { path: "x.txt", side: "new", start: 22, end: 22 });
  assert.equal(first.hunkIndex, 0);
  assert.equal(second.hunkIndex, 1);
  assert.equal(first.hunk, f.hunks[0]);
});

// --- Files tab: DiffTable lineTap --------------------------------------------

test("DiffTable renders a per-line tap target staging a single-line selection", () => {
  const src = read("../../src/components/DiffTable.jsx");
  assert.match(src, /lineTap\(/, "lineTap helper");
  assert.match(
    src,
    /onCommentSelect\(\{ path: path\(\), side, start: no, end: no \}\)/,
    "tap stages {path, side, start: no, end: no} — the #546 single-line shape",
  );
  assert.match(src, /lineTap\(side, no,/, "unified rows tap their own side + number");
  assert.match(src, /lineTap\("old", row\.left\?\.no,/, "split left gutter taps OLD");
  assert.match(src, /lineTap\("new", row\.right\?\.no,/, "split right gutter taps NEW");
  assert.match(src, /aria-label=\{label\}/, "tap target is labelled");
  assert.match(src, /`Comment on line \$\{no\}/, "unified label names the line");
  assert.match(src, /title="Comment on this line"/, "tap target carries a title hint");
});

test("DiffTable tap target honors the #502 gate (same bar as the range flow)", () => {
  const src = read("../../src/components/DiffTable.jsx");
  assert.match(
    src,
    /props\.onCommentSelect && props\.canComment !== false && no != null/,
    "tap renders only for gated consumers on real lines",
  );
});

test("DiffTable tap target is visible without hover (coarse pointers + keyboard)", () => {
  const src = read("../../src/components/DiffTable.jsx");
  assert.match(src, /group-hover:inline/, "hover reveal on fine pointers");
  assert.match(src, /pointer-coarse:inline/, "always visible on touch");
  assert.match(src, /focus-visible:inline/, "keyboard focus reveals (a11y floor)");
  assert.match(src, /class="diff-row group"/, "rows carry the group anchor");
});

test("DiffTable never fights text selection: no touch handlers, code text handler-free", () => {
  const src = read("../../src/components/DiffTable.jsx");
  assert.ok(!src.includes("onTouchStart"), "no touchstart (a tap arrives as click)");
  assert.ok(!src.includes("onTouchEnd"), "no touchend (custom handlers would double-stage)");
  assert.equal(
    (src.match(/onMouseDown/g) ?? []).length, 1,
    "exactly one mousedown — the gutter drag anchor; code cells carry none",
  );
});

test("DiffTable range flow is untouched (drag/shift + comment-on-selection bar)", () => {
  const src = read("../../src/components/DiffTable.jsx");
  assert.match(src, /comment on selection/, "range bar still stages ranges");
  assert.match(src, /onClick=\{\(\) => props\.onCommentSelect\(sel\(\)\)\}/, "bar still feeds the same staging path");
});

// --- Conversation: Pull.jsx DiffFile ------------------------------------------

test("conversation + is visible without hover and gated on canComment", () => {
  const src = read("../../src/pages/Pull.jsx");
  assert.match(src, /pointer-coarse:inline/, "always visible on touch");
  assert.match(src, /focus-visible:inline/, "keyboard focus reveals");
  assert.match(src, /group-hover:inline/, "hover reveal kept");
  assert.match(src, /<Show when=\{props\.canComment !== false\}>/, "+ honors the #502 gate");
  assert.match(src, /canComment=\{canComment\(\)\}/, "call site passes the gate");
});

test("conversation + drops the undefined link class for theme tokens", () => {
  const src = read("../../src/pages/Pull.jsx");
  assert.ok(!src.includes("link ml-2 hidden"), "no undefined link class on the affordance");
  assert.match(src, /text-emerald-600.*dark:text-emerald-400/, "explicit F2 token pair, both themes");
});

test("conversation adds no touch handlers (tap arrives as click)", () => {
  const src = read("../../src/pages/Pull.jsx");
  assert.ok(!src.includes("onTouchStart"), "no touchstart on the conversation diff");
  assert.ok(!src.includes("onTouchEnd"), "no touchend on the conversation diff");
});

// --- invariants -----------------------------------------------------------------

test("no new runtime dependencies (law 1)", () => {
  const pkg = JSON.parse(read("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime deps stay exactly solid-js + @solidjs/router + marked + dompurify",
  );
});

test("law-11/law-12: guideline idiom + web-UI decision land in the same change", () => {
  const guide = read("../../../docs/style-guideline.md");
  assert.match(guide, /tap-to-comment|tap affordance/i, "guideline names the per-line affordance idiom");
  const doc = read("../../../docs/go/12_web_ui.md");
  assert.match(doc, /#555/, "12_web_ui.md carries the #555 decision");
});
