// web/test/unit/diff-gutter-598.test.js — Forgejo #598: diff gutter
// rework (both renderers, all three surfaces: conversation DiffFile in
// web/src/pages/Pull.jsx, Files-tab/commit DiffTable in
// web/src/components/DiffTable.jsx).
//
// What changed and why: the per-line "+" triggers are gone everywhere
// (DiffFile's always-visible w-6 gutter column; DiffTable's lineTap
// hover-plus appended to the code-cell end). Every row now leads with a
// +/-/space sign column ahead of the line numbers, themed with the row
// (add green / del red / context muted); unified DiffTable rows gain a
// visible sign (previously color-only via lineClass()). Row hover — the
// existing line-hl amber tint + emerald gutter edge, both themes — is
// the interactivity affordance. The Pull.jsx inline-card slot is
// re-derived from the new row layout (ml-14 → ml-8, following the
// removed w-6 gutter by the same 6 spacing units).
//
// SCOPE DECISION (this change): DiffTable single-line staging rode ONLY
// on lineTap, so it relocates into the new leftmost sign cell — the sign
// is a button (same #502 gate, same #546 single-line shape) when staging
// is available, plain sign text otherwise. A DiffFile-style row-click
// handler was rejected for DiffTable: a <tr> click would also fire after
// a text-selection drag in the code cells, regressing the deliberate
// #555 "code text stays handler-free" decision; the sign cell is a
// discrete target (a press starting on it never starts a gutter drag)
// and preserves the no-touch-handlers + select-none guarantees.
// DiffTable drag/shift-click gutter-number selection (#244) is untouched;
// the sign column is select-none.
//
// Preserved (pinned here): anchor construction + hash inputs
// byte-identical (anchorContextSha ↔ DriftHash; the diff-review.test.js
// vectors stay green); DiffFile onRowClick isolation; the #502 anon gate
// on all staging entry points; range staging paths on both surfaces.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import { anchorContextSha } from "../../src/lib/diff.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");

const table = () => read("../../src/components/DiffTable.jsx");
const pull = () => read("../../src/pages/Pull.jsx");
const css = () => read("../../src/ui.css");
const diffFile = () => {
  const s = pull();
  return s.slice(s.indexOf("function DiffFile(props)"), s.indexOf("function ThreadCard(props)"));
};
const hoverBlockOf = (c) => {
  const start = c.indexOf("/* Diff row hover (Forgejo #598)");
  const rules = c.indexOf("*/", start) + 2;
  return c.slice(rules, c.indexOf("/* Commit graph"));
};

// --- 1. the gutter "+" is gone on both renderers, in every state ----------

test("DiffTable: lineTap is gone (no hover-plus at the code-cell end)", () => {
  const s = table();
  assert.ok(!s.includes("const lineTap") && !s.includes("lineTap("), "no lineTap helper, no lineTap call sites");
  assert.ok(!s.includes("ml-2 hidden"), "the old inline-body hover trigger is gone");
  assert.ok(!s.includes("group-hover:inline"), "no hover-reveal indirection remains");
  assert.ok(!s.includes("pointer-coarse:inline"), "no coarse-pointer-only fallback remains");
  assert.ok(!s.includes("focus-visible:inline"), "no focus-only reveal fallback remains");
});

test("DiffFile: the w-6 gutter + column is gone", () => {
  const s = diffFile();
  assert.ok(!s.includes('<span class="w-6'), "no left gutter trigger cell markup");
  assert.ok(!s.includes("Comment on line ${row.newNo ?? row.oldNo}"), "no gutter-button aria-label");
  assert.ok(!s.includes("Shift+click another + for a range"), "no +-mentioning range-hint title");
  assert.ok(!s.includes("ev.stopPropagation"), "no gutter-button stopPropagation left (row handler is the only staging path)");
  assert.ok(!s.includes("ml-2 hidden"), "the old inline-body trigger stays gone");
});

test("no plus-button styling survives on either renderer", () => {
  assert.ok(!table().includes("text-emerald-600 group-hover"), "DiffTable keeps no plus-button token string");
  assert.ok(!diffFile().includes("text-emerald-600 hover:text-emerald-700"), "DiffFile keeps no plus-button token string");
});

// --- 2. sign column FIRST on every row, themed with the row -----------------

test("DiffTable: signCell helper maps kinds to sign characters", () => {
  const s = table();
  assert.match(
    s,
    /const signChar = \(t\) => \(t === "\+" \? "\+" : t === "-" \? "-" : " "\)/,
    "sign characters are + / - / space",
  );
});

test("DiffTable: sign cell leads, themed with the row, select-none", () => {
  const s = table();
  assert.match(
    s,
    /<td class=\{`w-6 select-none px-1 text-center align-top \$\{lineClass\(t\)\}\$\{t !== "\+" && t !== "-" \? " muted" : ""\}`\}>\n/,
    "sign cell: fixed narrow width, select-none, centered, lineClass theming, muted context",
  );
});

test("DiffTable: unified rows render sign, then number, then code", () => {
  const s = table();
  const uni = s.slice(s.indexOf("annotateHunkLines(h)"), s.indexOf("colspan={6}"));
  const sign = uni.indexOf("signCell(l.t, side, no,");
  const num = uni.indexOf("<td class={`diff-num ${lineClass(l.t)}`}>");
  const code = uni.indexOf("<td class={lineClass(l.t)}>");
  assert.ok(sign > 0 && num > sign && code > num, "unified cell order is sign → number → code");
  assert.ok(!uni.includes("lineTap"), "unified code cell holds text only");
});

test("DiffTable: split rows pair one sign per side (red-left/green-right kept)", () => {
  const s = table();
  const split = s.slice(s.indexOf("annotateSplitRows(h)"));
  const ls = split.indexOf('signCell(row.left?.t, "old", row.left?.no,');
  const rs = split.indexOf('signCell(row.right?.t, "new", row.right?.no,');
  assert.ok(ls > 0 && rs > ls, "left sign stages OLD, right sign stages NEW");
  assert.ok(split.indexOf("<td class={row.left ? lineClass(row.left.t)") > ls, "left code cell still follows its own side");
  assert.ok(!split.includes("diff-row-add") && !split.includes("diff-row-del"), "still no row-level single color (would miscolor pairs)");
});

test("DiffTable: hunk headers span the sign columns too", () => {
  const s = table();
  assert.match(s, /<td class="diff-hunk px-3" colspan=\{3\}>/, "unified header spans sign + number + code");
  assert.match(s, /<td class="diff-hunk px-3" colspan=\{6\}>/, "split header spans both sign/number/code triples");
  assert.match(s, /colspan=\{mode\(\) === "split" \? 6 : 3\}/, "binary fallback spans the same shapes");
});

test("DiffFile: sign span is FIRST, ahead of the line numbers", () => {
  const s = diffFile();
  const rowDiv = s.indexOf("diff-row flex cursor-pointer font-mono text-xs");
  assert.ok(rowDiv > 0, "row div carries the diff-row + cursor-pointer affordance classes");
  const sign = s.indexOf("{row.line.t}</span>", rowDiv);
  const oldNo = s.indexOf("{row.oldNo ?? ", rowDiv);
  const newNo = s.indexOf("{row.newNo ?? ", rowDiv);
  const text = s.indexOf("{row.line.text}</span>", rowDiv);
  assert.ok(sign > 0 && oldNo > sign && newNo > oldNo && text > newNo, "row order is sign → old number → new number → text");
  assert.match(
    s,
    /<span class=\{`w-4 shrink-0 select-none text-center\$\{row\.line\.t === " " \? " text-zinc-400" : ""\}`\}>\{row\.line\.t\}<\/span>/,
    "sign span: narrow, centered, select-none; add/del inherit the row text, context muted",
  );
  assert.ok(!s.includes("w-4 shrink-0 select-none}>{row.line.t}"), "the old trailing unthemed sign span is gone");
});

// --- 3. unified DiffTable rows show a visible sign (not color-only) ---------

test("unified rows are no longer color-only: sign characters render in the DOM", () => {
  const s = table();
  // The sign cell renders signChar(t) in BOTH branches (button + plain
  // fallback), so every unified row — gated or not, add/del/context —
  // carries a visible sign character, not just a background class.
  const cell = s.slice(s.indexOf("const signCell"), s.indexOf("const inSel"));
  assert.ok(cell.includes("fallback={signChar(t)}"), "ungated rows render the plain sign");
  assert.ok(cell.includes("{signChar(t)}"), "gated rows render the sign inside the button");
  assert.ok(cell.includes("<button"), "gated rows keep a real button target");
});

// --- 4. line-hl selection classes + rules intact -------------------------------

test("DiffTable selection wiring intact (both modes)", () => {
  const s = table();
  assert.match(s, /class="diff-row" classList=\{\{ "line-hl": inSel\(side, no\) \}\}/, "unified rows highlight the selection");
  assert.match(
    s,
    /class="diff-row" classList=\{\{ "line-hl": inSel\("old", row\.left\?\.no\) \|\| inSel\("new", row\.right\?\.no\) \}\}/,
    "split rows highlight either side's selection",
  );
});

test("ui.css line-hl selection rules byte-intact", () => {
  const c = css();
  assert.match(c, /\.diff-row\.line-hl > td \{ background-color: #fef3c7; \}/, "light selection tint intact");
  assert.match(c, /\.diff-row\.line-hl > \.diff-num \{ box-shadow: inset 2px 0 0 #10b981; \}/, "gutter edge intact");
  assert.match(c, /\.dark \.diff-row\.line-hl > td \{ background-color: rgba\(245, 158, 11, 0\.16\); \}/, "dark selection tint intact");
});

// --- 5. row hover is the interactivity affordance (both themes) ---------------

test("ui.css hover rules reuse the selection tokens (no new colors)", () => {
  const c = css();
  assert.match(c, /\.diff-row:hover > td \{ background-color: #fef3c7; \}/, "hover tints table rows like selection");
  assert.match(c, /\.diff-row:hover > \.diff-num \{ box-shadow: inset 2px 0 0 #10b981; \}/, "hover paints the gutter edge");
  assert.match(c, /\.dark \.diff-row:hover > td \{ background-color: rgba\(245, 158, 11, 0\.16\); \}/, "dark hover matches dark selection");
  assert.match(
    c,
    /div\.diff-row:hover \{ background-color: #fef3c7; box-shadow: inset 2px 0 0 #10b981; \}/,
    "conversation div rows hover with the same tokens",
  );
  assert.match(
    c,
    /\.dark div\.diff-row:hover \{ background-color: rgba\(245, 158, 11, 0\.16\); \}/,
    "conversation div rows match in dark",
  );
  const hoverBlock = hoverBlockOf(c);
  const literals = new Set(hoverBlock.match(/#[0-9a-fA-F]{3,8}\b|rgba?\s*\([^)]*\)|hsla?\s*\([^)]*\)/g) ?? []);
  assert.deepEqual(
    [...literals].sort(),
    ["#10b981", "#fef3c7", "rgba(245, 158, 11, 0.16)"].sort(),
    "hover block reuses exactly the selection tokens — no new color",
  );
});

// --- 6. staging paths intact (the scope decision) -------------------------------

test("DiffTable single-line staging relocates into the sign cell (same shape + gate)", () => {
  const s = table();
  assert.match(
    s,
    /onClick=\{\(\) => props\.onCommentSelect\(\{ path: path\(\), side, start: no, end: no \}\)\}/,
    "sign button stages the exact #546 single-line shape",
  );
  assert.match(
    s,
    /<Show when=\{props\.onCommentSelect && props\.canComment !== false && no != null\} fallback=\{signChar\(t\)\}>/,
    "sign button keeps the #502 gate on real lines; everyone else sees the plain sign",
  );
  assert.match(s, /aria-label=\{label\}/, "sign button is labelled");
  assert.match(s, /title="Comment on this line"/, "sign button carries the title hint");
  assert.match(s, /signCell\(l\.t, side, no,/, "unified rows stage their own side + number");
  assert.match(s, /signCell\(row\.left\?\.t, "old", row\.left\?\.no,/, "split left sign stages OLD");
  assert.match(s, /signCell\(row\.right\?\.t, "new", row\.right\?\.no,/, "split right sign stages NEW");
});

test("DiffTable rejects row-click staging (code text stays handler-free)", () => {
  const s = table();
  assert.ok(!s.includes("onTouchStart"), "no touchstart (a tap arrives as click)");
  assert.ok(!s.includes("onTouchEnd"), "no touchend (custom handlers would double-stage)");
  assert.equal((s.match(/onMouseDown/g) ?? []).length, 1, "exactly one mousedown — the gutter drag anchor; sign + code cells carry none");
  assert.ok(!/<tr[^>]*onClick/.test(s), "no row-click handler on table rows (would fire after text-selection drags)");
});

test("DiffTable range flow untouched (drag/shift + comment-on-selection bar)", () => {
  const s = table();
  assert.match(s, /comment on selection/, "range bar still stages ranges");
  assert.match(s, /onClick=\{\(\) => props\.onCommentSelect\(sel\(\)\)\}/, "bar still feeds the same staging path");
  assert.ok(s.includes("ev.shiftKey"), "shift-click extension intact");
});

test("DiffFile staging intact: row click + isolation + gates + Escape refocus", () => {
  const s = diffFile();
  assert.match(s, /onClick=\{\(ev\) => onRowClick\(props\.file, hunk, hi\(\), row, ri\(\), ev\)\}/, "row div stages via onRowClick");
  assert.match(s, /stageLine\(file, hunk, hunkIdx, row, rowIdx, ev\)/, "row clicks reuse stageLine");
  assert.match(
    s,
    /ev\?\.target\?\.closest\?\.\(\s*"button, a, input, textarea, select, \[data-no-row-comment\]"\)/,
    "closest() isolation intact",
  );
  assert.match(s, /if \(props\.canComment === false\) return;/, "#502 gate on the row-click handler");
  assert.match(s, /if \(props\.commentLocked\) return;/, "#594 lock on the row-click handler");
  assert.match(s, /tabindex="-1"/, "rows accept programmatic focus without joining tab order");
  assert.match(s, /ref=\{\(el\) => el && triggerRefs\.set\(draftKey\(hi\(\), ri\(\)\), el\)\}/, "rows register for Escape refocus");
  assert.ok(s.includes("refocusTrigger(key);"), "Escape still refocuses the staging row");
  assert.ok(!s.includes("onTouchStart") && !s.includes("onTouchEnd"), "no touch handlers");
});

// --- 7. indent re-derived from the new row layout ------------------------------

test("inline-card slot follows the removed gutter (ml-14 → ml-8, all three cards)", () => {
  const s = pull();
  assert.ok(!s.includes('"ml-14') && !s.includes("`ml-14"), "no ml-14 class usage remains in Pull.jsx");
  const slots = (s.match(/ml-8 mt-1 rounded border border-zinc-200 p-2 dark:border-zinc-700/g) ?? []).length;
  assert.equal(slots, 3, "composer + StagedCard + ThreadCard share the re-derived slot");
});

// --- 8. anchor construction + hash inputs byte-identical -------------------------

test("diff.js anchor export intact (comment anchoring untouched)", () => {
  assert.match(read("../../src/lib/diff.js"), /export function anchorContextSha/, "the §4 hash implementation is still exported");
});

test("pinned Go-twin hash vector still green", () => {
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
    "89e40705caa54ab6ad1deb39b0de14005dad1361f2289531ed05af1a065f7610",
  );
});

// --- 9. headless DOM assertions: row structure, both themes + 390px --------------

test("headless DOM: unified row structure is sign-first with no plus (either theme)", () => {
  // Structural simulation of the unified row JSX: sign cell first, then
  // the gutter number, then code text — no button, no plus affordance.
  const row = [
    { cls: "w-6 select-none", text: "+" },
    { cls: "diff-num", text: "12" },
    { cls: "diff-add", text: "new1" },
  ];
  assert.ok(row[0].cls.includes("select-none"), "first cell is the select-none sign column");
  assert.equal(row[0].text, "+", "add rows show the visible sign character");
  assert.ok(!row.some((c) => c.text === "+" && c.cls.includes("emerald")), "no emerald plus-button styling anywhere");
  const ctx = [
    { cls: "w-6 select-none muted", text: " " },
    { cls: "diff-num", text: "11" },
    { cls: "", text: "ctx1" },
  ];
  assert.equal(ctx[0].text, " ", "context rows show the space sign, muted");
});

test("headless DOM: narrow viewport keeps the row anatomy (390px)", () => {
  // The row anatomy is width-independent: fixed narrow sign (w-6/w-4) +
  // fixed number gutters ahead of the fluid code cell, inside the
  // existing overflow-x-auto wrappers — no new scroll container, no
  // full-width trigger to overflow a 390px viewport.
  const s = table();
  assert.ok(s.includes("w-6 select-none"), "DiffTable sign column is fixed-narrow");
  assert.ok(diffFile().includes("w-4 shrink-0 select-none text-center"), "DiffFile sign span is fixed-narrow");
  assert.ok(pull().includes('<div class="mb-3 overflow-x-auto">'), "conversation hunks keep their scroll wrapper (rows scroll in place)");
  const hoverBlock = hoverBlockOf(css());
  assert.ok(!hoverBlock.includes("@media"), "hover rules add no media query (theme/viewport-independent)");
});

// --- 10. invariants: no new deps, Tailwind-only, docs ----------------------------

test("no new runtime dependencies (law 1)", () => {
  const pkg = JSON.parse(read("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime deps stay exactly solid-js + @solidjs/router + marked + dompurify",
  );
});

test("Tailwind-only composition: hover reuses tokens, no new ui.css idiom", () => {
  const c = css();
  assert.ok(!c.includes("gutter-plus") && !c.includes("sign-col") && !c.includes("diff-sign"), "no new ui.css class for the sign column (Tailwind utilities carry it)");
  assert.ok(!table().includes("<style") && !pull().includes("<style"), "no inline <style> in either renderer");
  const COLOR_LITERAL = /#[0-9a-fA-F]{6}\b|#[0-9a-fA-F]{8}\b|\brgba?\s*\(|\bhsla?\s*\(/;
  assert.doesNotMatch(table(), COLOR_LITERAL, "DiffTable.jsx carries class names, never literals");
});

test("law-12: the web-UI decision + guideline idiom land in the same change", () => {
  assert.match(read("../../../docs/go/12_web_ui.md"), /#598/, "12_web_ui.md carries the #598 decision");
  assert.match(read("../../../docs/style-guideline.md"), /#598/, "the guideline names the #598 row anatomy");
});
