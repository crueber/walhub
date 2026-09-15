// web/test/unit/inline-composer-560.test.js — Forgejo #560: refined inline
// review composer on the conversation-page DiffFile (web/src/pages/Pull.jsx).
//
// Follow-up to #546/#555: the per-line "+" lived in the diff row BODY
// (hover-revealed, shifting row content), a row click staged nothing, and
// drafts were a single per-file `createSignal(null)` — one composer at a
// time, rendered by one <Show> at the file end. The rework moves the "+"
// into the left gutter (one discrete, always-visible button per
// commentable line), stages a composer immediately below the clicked row
// on ANY row click, and holds drafts in a keyed Map (`${hunkIdx}:${rowIdx}`)
// so unlimited composers coexist with text preserved, each submitting
// independently in any order.
//
// Preserved (pinned here): stageLine → buildAnchor
// (web/src/lib/review-anchor.js) → shared CommentComposer → props.onStage;
// byte-identical anchor inputs (the pinned Go-twin vector + single-line
// hash parity); Shift+click range extension; click isolation (composer /
// thread-card clicks spawn nothing); the #502 gate on all three entry
// points; thread placement/resolve/drift behavior; Files-tab gutter
// conventions; no new runtime deps; Tailwind-only composition.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import { parsePatchFiles, anchorContextSha } from "../../src/lib/diff.js";
import { annotateHunkLines, matchAnnotated } from "../../src/lib/diff-lines.js";
import { buildAnchor } from "../../src/lib/review-anchor.js";

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
const src = () => read("../../src/pages/Pull.jsx");

// --- 1. gutter "+" per commentable line, always visible -----------------------

test("gutter trigger lives in the left gutter, ahead of the line numbers", () => {
  const s = src();
  const btn = s.indexOf("Comment on line ${row.newNo ?? row.oldNo} in ${props.file.path}");
  assert.ok(btn > 0, "gutter + button with a line-labelling aria-label");
  const gutterCell = s.indexOf("w-6 shrink-0 select-none text-center");
  assert.ok(gutterCell > 0 && gutterCell < btn, "button sits inside a left gutter cell");
  assert.ok(
    s.indexOf("w-10 shrink-0 select-none text-right", gutterCell) > btn,
    "line-number gutter cells follow the trigger cell",
  );
});

test("gutter trigger is always visible (no hover dependency)", () => {
  const s = src();
  const cellIdx = s.indexOf("w-6 shrink-0 select-none text-center");
  const block = s.slice(cellIdx, s.indexOf("</span>", s.indexOf("</button>", cellIdx)));
  assert.ok(!block.includes("hidden"), "no hidden class on the trigger");
  assert.ok(!block.includes("group-hover:"), "no hover-reveal indirection");
  assert.ok(!block.includes("pointer-coarse:"), "no coarse-pointer-only fallback needed (always rendered)");
  assert.ok(!block.includes("focus-visible:inline"), "no focus-only reveal fallback needed");
  assert.match(block, /focus-visible:outline/, "keyboard users keep a focus-visible affordance");
});

test("row content is shift-free: no trigger inside the row body", () => {
  const s = src();
  assert.ok(!s.includes("ml-2 hidden"), "the old inline-body trigger (ml-2 hidden…) is gone");
  const bodyAfterNumbers = s.slice(
    s.indexOf('<span class="w-4 shrink-0 select-none">{row.line.t}</span>'),
    s.indexOf("</div>", s.indexOf('<span class="w-4 shrink-0 select-none">{row.line.t}</span>')),
  );
  assert.ok(!bodyAfterNumbers.includes("<button"), "no button after the code cell — the body holds text only");
});

test("gutter trigger mirrors the Files-tab conventions (label + title + tokens)", () => {
  const s = src();
  assert.match(s, /aria-label=\{`Comment on line \$\{row\.newNo \?\? row\.oldNo\} in \$\{props\.file\.path\}`\}/, "gutterLink-style line label incl. path");
  assert.match(s, /title="Comment on this line \(Shift\+click another \+ for a range\)"/, "range-hint title kept");
  assert.match(s, /text-emerald-600.*dark:text-emerald-400/, "explicit F2 emerald tokens, both themes");
});

// --- 2. row click stages a composer below that line ----------------------------

test("row click wires a row-level handler into stageLine", () => {
  const s = src();
  assert.match(s, /onClick=\{\(ev\) => onRowClick\(props\.file, hunk, hi\(\), row, ri\(\), ev\)\}/, "row div carries the click handler with hunk+row identity");
  assert.match(s, /const onRowClick = \(file, hunk, hunkIdx, row, rowIdx, ev\)/, "handler resolves the clicked row");
  assert.match(s, /stageLine\(file, hunk, hunkIdx, row, rowIdx, ev\)/, "row clicks reuse stageLine (same anchor path as +)");
});

test("composer renders inline below its row, never at the file end", () => {
  const s = src();
  const forRows = s.indexOf("<For each={rows}>");
  const draftShow = s.indexOf("draftAt(hi(), ri())");
  assert.ok(forRows > 0 && draftShow > forRows, "draft <Show> lives inside the row <For>");
  const threadsFor = s.indexOf("threadsAt(hi(), ri())");
  assert.ok(threadsFor > draftShow, "draft composer sits directly below its row, above the thread cards");
  assert.ok(!s.includes("getDraft()"), "no single-draft signal remains");
  assert.ok(!s.includes("setDraft(null)"), "no single-draft close remains");
});

// --- 3. keyed multi-draft collection --------------------------------------------

test("draft state is a keyed Map, not a single signal", () => {
  const s = src();
  assert.match(s, /const \[getDrafts, setDrafts\] = createSignal\(new Map\(\)\)/, "signal holds a Map");
  const diffFile = s.slice(s.indexOf("function DiffFile(props)"), s.indexOf("function ThreadCard(props)"));
  assert.ok(!/\bgetDraft\b/.test(diffFile) && !/\bsetDraft\b/.test(diffFile), "no single-draft signal in DiffFile (getLast's null signal is the range origin, not a draft)");
  assert.match(s, /const draftKey = \(hunkIdx, rowIdx\) => `\$\{hunkIdx\}:\$\{rowIdx\}`/, "key is hunk+row");
  assert.match(s, /const draftAt = \(hunkIdx, rowIdx\) => getDrafts\(\)\.get\(draftKey\(hunkIdx, rowIdx\)\)/, "keyed lookup per row");
  assert.match(s, /new Map\(prev\)\.set\(key, \{ anchor \}\)/, "staging adds without clobbering siblings");
});

test("each draft submits and cancels independently", () => {
  const s = src();
  assert.match(s, /props\.onStage\(\{ anchor: d\(\)\.anchor, body \}\)/, "submit still stages {anchor, body} into the finish-review modal");
  assert.match(s, /closeDraft\(draftKey\(hi\(\), ri\(\)\)\)/, "submit/cancel close only their own key");
  assert.match(s, /const closeDraft = \(key\) =>/, "per-key close helper");
  const closes = (s.match(/closeDraft\(draftKey\(hi\(\), ri\(\)\)\)/g) ?? []).length;
  assert.ok(closes >= 2, "both submit and cancel close per-key (independent, any order)");
});

test("shift+click range extension still works through the gutter buttons", () => {
  const s = src();
  assert.match(s, /ev\?\.shiftKey && last && last\.file === file\.path && last\.side === side && last\.hunkIdx === hunkIdx/, "same-file+side+hunk clamp");
  assert.match(s, /startNo: Math\.min\(last\.no, no\)/, "range anchors from the last staged line");
  assert.match(s, /endNo: Math\.max\(last\.no, no\)/, "range end at the clicked line");
  assert.match(s, /setLast\(\{ file: file\.path, side, no, hunkIdx \}\)/, "plain stages record the range origin");
});

// --- click isolation --------------------------------------------------------------

test("gutter clicks never double-stage through the row handler", () => {
  const s = src();
  assert.match(s, /ev\.stopPropagation\(\);/, "gutter button stops propagation");
});

test("row clicks ignore interactive content (composer/thread-card safe)", () => {
  const s = src();
  assert.match(
    s,
    /ev\?\.target\?\.closest\?\.\(\s*"button, a, input, textarea, select, \[data-no-row-comment\]"\)/,
    "closest() guard over buttons/links/inputs",
  );
  assert.ok(!s.includes("onTouchStart"), "no touchstart (a tap arrives as click)");
  assert.ok(!s.includes("onTouchEnd"), "no touchend (custom handlers would double-stage)");
});

// --- #502 anon gate on all three entry points --------------------------------------

test("#502 gate covers gutter trigger, row-click handler, and composer", () => {
  const s = src();
  const diffFile = s.slice(s.indexOf("function DiffFile(props)"), s.indexOf("function ThreadCard(props)"));
  const gates = (diffFile.match(/props\.canComment (?:!== false|=== false)/g) ?? []).length;
  assert.ok(gates >= 3, `all three entry points gated (found ${gates})`);
  assert.match(s, /if \(props\.canComment === false\) return;/, "row-click handler bails for anonymous viewers");
});

// --- anchor-building inputs byte-identical ------------------------------------------

test("buildAnchor inputs untouched: single-line shapes", () => {
  const n = buildAnchor({ file: file0(), hunk: hunk0(), side: "NEW", startNo: 2, endNo: 2, head: HEAD });
  assert.deepEqual(
    { path: n.path, side: n.side, old_start: n.old_start, old_lines: n.old_lines, new_start: n.new_start, new_lines: n.new_lines },
    { path: "x.txt", side: "NEW", old_start: 0, old_lines: 0, new_start: 2, new_lines: 1 },
  );
  const o = buildAnchor({ file: file0(), hunk: hunk0(), side: "OLD", startNo: 2, endNo: 2, head: HEAD });
  assert.deepEqual([o.side, o.old_start, o.old_lines, o.new_start, o.new_lines], ["OLD", 2, 1, 0, 0]);
});

test("buildAnchor inputs untouched: single-line hash parity + pinned Go-twin vector", () => {
  const ann = annotateHunkLines(hunk0());
  const idxs = matchAnnotated(ann, "new", 2, 2);
  assert.equal(idxs.length, 1);
  const direct = anchorContextSha({ path: "x.txt", lines: hunk0().lines }, { start: idxs[0], count: 1 });
  const a = buildAnchor({ file: file0(), hunk: hunk0(), side: "NEW", startNo: 2, endNo: 2, head: HEAD });
  assert.equal(a.context_sha, direct);
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

test("stageLine still builds only through buildAnchor (no forked hash)", () => {
  const s = src();
  const diffFile = s.slice(s.indexOf("function DiffFile(props)"), s.indexOf("function ThreadCard(props)"));
  assert.ok(/buildAnchor\(\{/.test(diffFile), "anchors via the shared helper");
  assert.ok(!/anchorContextSha\s*\(/.test(diffFile), "never hashes directly (helper owns the bytes)");
  assert.ok(!diffFile.includes("window.prompt"), "never prompts");
});

// --- thread placement / resolve / drift unchanged -------------------------------------

test("thread placement, resolve, and drift behavior unchanged", () => {
  const s = src();
  assert.match(s, /const placement = \(\) =>/, "placement derived fresh every render");
  assert.match(s, /placement\(\)\.byKey\.get\(`\$\{hi\}:\$\{ri\}`\)/, "inline unresolved-first placement key");
  assert.match(s, /placement\(\)\.unplaced/, "drifted anchors still render once at the file end");
  assert.match(s, /threadFreshness\(t, props\.files\)/, "freshness still delegates per thread");
  assert.match(s, /list\.sort\(\(x, y\) => Number\(x\.resolved\) - Number\(y\.resolved\)\)/, "unresolved-first order kept");
  assert.match(s, /from "\.\.\/components\/CommentComposer\.jsx"/, "shared CommentComposer kept");
});

// --- invariants --------------------------------------------------------------------------

test("no new runtime dependencies (law 1)", () => {
  const pkg = JSON.parse(read("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime deps stay exactly solid-js + @solidjs/router + marked + dompurify",
  );
});

test("Tailwind-only composition: no new CSS patterns for the composer", () => {
  const css = read("../../src/ui.css");
  assert.ok(!css.includes("composer-560") && !css.includes("inline-draft") && !css.includes("gutter-plus"), "no new ui.css rules for #560");
  assert.ok(!src().includes("<style"), "no inline <style> in Pull.jsx");
});

test("law-12: the web-UI decision lands in the same change", () => {
  const doc = read("../../../docs/go/12_web_ui.md");
  assert.match(doc, /#560/, "12_web_ui.md carries the #560 decision");
});
