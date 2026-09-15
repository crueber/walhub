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
//
// AMENDED by Forgejo #598 (diff gutter rework): the affordances moved —
// the DiffTable lineTap hover-plus now lives in the leftmost sign cell
// (signCell: always-visible sign, button when gated) and the conversation
// gutter "+" is gone entirely (row click is the staging entry point).
// The anchor shapes below are UNCHANGED (same staging payloads, same
// #502 gate, same no-touch-handlers contract); the surface pins assert
// the relocated affordances — see diff-gutter-598.test.js for the full
// row-anatomy pin.

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

// --- Files tab: DiffTable sign-cell staging (relocated by #598) -----------------

test("DiffTable stages single-line selections from the sign cell (same shape)", () => {
  const src = read("../../src/components/DiffTable.jsx");
  assert.ok(!src.includes("const lineTap"), "lineTap helper is gone");
  assert.match(
    src,
    /onClick=\{\(\) => props\.onCommentSelect\(\{ path: path\(\), side, start: no, end: no \}\)\}/,
    "sign button stages {path, side, start: no, end: no} — the #546 single-line shape",
  );
  assert.match(src, /signCell\(l\.t, side, no,/, "unified rows stage their own side + number");
  assert.match(src, /signCell\(row\.left\?\.t, "old", row\.left\?\.no,/, "split left sign stages OLD");
  assert.match(src, /signCell\(row\.right\?\.t, "new", row\.right\?\.no,/, "split right sign stages NEW");
  assert.match(src, /aria-label=\{label\}/, "sign button is labelled");
  assert.match(src, /`Comment on line \$\{no\}/, "unified label names the line");
  assert.match(src, /title="Comment on this line"/, "sign button carries a title hint");
});

test("DiffTable sign target honors the #502 gate (same bar as the range flow)", () => {
  const src = read("../../src/components/DiffTable.jsx");
  assert.match(
    src,
    /props\.onCommentSelect && props\.canComment !== false && no != null/,
    "sign button renders only for gated consumers on real lines",
  );
});

test("DiffTable sign is always visible (no hover/coarse/focus reveal)", () => {
  const src = read("../../src/components/DiffTable.jsx");
  assert.ok(!src.includes("group-hover:inline"), "no hover reveal");
  assert.ok(!src.includes("pointer-coarse:inline"), "no coarse-pointer-only fallback (always rendered)");
  assert.ok(!src.includes("focus-visible:inline"), "no focus-only reveal fallback");
  assert.match(src, /focus-visible:outline/, "keyboard users keep a focus-visible outline");
  assert.match(src, /fallback=\{signChar\(t\)\}/, "ungated viewers still see the plain sign");
});

test("DiffTable never fights text selection: no touch handlers, code text handler-free", () => {
  const src = read("../../src/components/DiffTable.jsx");
  assert.ok(!src.includes("onTouchStart"), "no touchstart (a tap arrives as click)");
  assert.ok(!src.includes("onTouchEnd"), "no touchend (custom handlers would double-stage)");
  assert.equal(
    (src.match(/onMouseDown/g) ?? []).length, 1,
    "exactly one mousedown — the gutter drag anchor; sign + code cells carry none",
  );
  assert.ok(!/<tr[^>]*onClick/.test(src), "no row-click handler (would fire after text-selection drags)");
});

test("DiffTable range flow is untouched (drag/shift + comment-on-selection bar)", () => {
  const src = read("../../src/components/DiffTable.jsx");
  assert.match(src, /comment on selection/, "range bar still stages ranges");
  assert.match(src, /onClick=\{\(\) => props\.onCommentSelect\(sel\(\)\)\}/, "bar still feeds the same staging path");
});

// --- Conversation: Pull.jsx DiffFile (gutter + gone by #598) --------------------

test("conversation stages from the row itself — no gutter trigger (Forgejo #598)", () => {
  const src = read("../../src/pages/Pull.jsx");
  // #598 removed the #560 gutter "+" column: the row leads with its sign
  // and row click (with closest() isolation) is the staging entry point —
  // see inline-composer-560.test.js for the full pin.
  assert.ok(!src.includes('<span class="w-6'), "no left gutter trigger cell");
  assert.match(src, /onClick=\{\(ev\) => onRowClick\(props\.file, hunk, hi\(\), row, ri\(\), ev\)\}/, "row click stages");
  assert.match(src, /<Show when=\{props\.canComment !== false\}>/, "composer + staged cards honor the #502 gate");
  assert.match(src, /canComment=\{canComment\(\)\}/, "call site passes the gate");
});

test("conversation sign keeps no plus-button tokens (themed with the row)", () => {
  const src = read("../../src/pages/Pull.jsx");
  assert.ok(!src.includes("link ml-2 hidden"), "no undefined link class on the affordance");
  assert.ok(!src.includes("text-emerald-600 hover:text-emerald-700"), "no plus-button token string survives");
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
