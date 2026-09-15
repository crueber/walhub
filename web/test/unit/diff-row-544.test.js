// web/test/unit/diff-row-544.test.js — Forgejo #544: added/removed diff
// lines carry a full-row light green / light red background (gutter cells
// included) on every diff surface, in both themes.
//
// Shape under test (styling only — no wire/parser change):
// - DiffBody (DiffTable.jsx, serves Commit.jsx + PullFiles.jsx): lineClass()
//   applies per CELL — unified rows color gutter + code, split rows color
//   per side (a paired change row is red-left/green-right, never one color
//   across).
// - Pull.jsx DiffFile (the div-based conversation diff): the row div carries
//   the shared lineClass(), so the main PR surface matches.
// - ui.css: .diff-add/.diff-del carry both themes; the compound
//   .diff-num.diff-add/.diff-num.diff-del rules beat the plain .diff-num
//   background at any order (equal-specificity single-class tie would lose
//   — .diff-num is written later); the unlayered .diff-row.line-hl rule
//   still wins over all of them (selection > add/del).
// - lib/diff.js untouched by styling: no CSS class literals in the parser,
//   anchorContextSha still exported (the diff-review.test.js hash vectors
//   are the byte-identical tripwire and run in the same suite).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const diffTable = () => srcOf("../../src/components/DiffTable.jsx");
const pull = () => srcOf("../../src/pages/Pull.jsx");
const css = () => srcOf("../../src/ui.css");
const parser = () => srcOf("../../src/lib/diff.js");

const COLOR_LITERAL = /#[0-9a-fA-F]{6}\b|#[0-9a-fA-F]{8}\b|\brgba?\s*\(|\bhsla?\s*\(/;

test("lineClass maps +/- to the shared tokens, context to none", () => {
  const src = diffTable();
  assert.match(
    src,
    /export const lineClass = \(t\) => \(t === "\+" \? "diff-add" : t === "-" \? "diff-del" : ""\)/,
    "the +/-/context mapping is unchanged",
  );
});

test("DiffBody unified row colors gutter + code cells", () => {
  const src = diffTable();
  assert.match(src, /<td class=\{`diff-num \$\{lineClass\(l\.t\)\}`\}>/, "unified gutter carries lineClass(l.t)");
  assert.match(src, /<td class=\{lineClass\(l\.t\)\}>/, "unified code cell keeps lineClass(l.t)");
});

test("DiffBody split row colors per side (paired rows stay red-left/green-right)", () => {
  const src = diffTable();
  assert.match(
    src,
    /<td class=\{`diff-num \$\{row\.left \? lineClass\(row\.left\.t\) : ""\}`\}>/,
    "split left gutter follows the left cell type",
  );
  assert.match(
    src,
    /<td class=\{`diff-num \$\{row\.right \? lineClass\(row\.right\.t\) : ""\}`\}>/,
    "split right gutter follows the right cell type",
  );
  assert.ok(!src.includes("diff-row-add") && !src.includes("diff-row-del"), "no row-level single color (would miscolor pairs)");
});

test("Pull.jsx DiffFile reuses the shared lineClass on the row div", () => {
  const src = pull();
  assert.match(
    src,
    /import \{ lineClass \} from "\.\.\/components\/DiffTable\.jsx"/,
    "DiffFile imports the shared helper (no forked mapping)",
  );
  assert.match(
    src,
    /diff-row flex font-mono text-xs \$\{lineClass\(row\.line\.t\)\}\$\{props\.canComment !== false && !props\.commentLocked \? " cursor-pointer" : ""\}/,
    "the div row carries the add/del background + the #598 hover affordance classes, cursor gated (Forgejo #560 adds the row-click staging handler on the same div)",
  );
  assert.match(
    src,
    /onClick=\{\(ev\) => onRowClick\(props\.file, hunk, hi\(\), row, ri\(\), ev\)\}/,
    "the row-click staging handler is still on the same div",
  );
});

test("ui.css: add/del tokens cover both themes", () => {
  const src = css();
  assert.match(src, /\.diff-add \{ @apply bg-emerald-50 text-emerald-900 dark:bg-emerald-950\/60 dark:text-emerald-200; \}/, "add: light base + dark tint");
  assert.match(src, /\.diff-del \{ @apply bg-red-50 text-red-900 dark:bg-red-950\/60 dark:text-red-200; \}/, "del: light base + dark tint");
});

test("ui.css: compound gutter rules beat .diff-num at any order", () => {
  const src = css();
  const num = src.indexOf("  .diff-num { @apply");
  const add = src.indexOf("  .diff-num.diff-add { @apply");
  const del = src.indexOf("  .diff-num.diff-del { @apply");
  assert.ok(num >= 0 && add >= 0 && del >= 0, "base gutter rule + both compound rules present");
  assert.match(src, /\.diff-num\.diff-add \{ @apply bg-emerald-50 [^}]*dark:bg-emerald-950\/60[^}]*\}/, "gutter add: both themes");
  assert.match(src, /\.diff-num\.diff-del \{ @apply bg-red-50 [^}]*dark:bg-red-950\/60[^}]*\}/, "gutter del: both themes");
});

test("ui.css: unlayered line-hl selection still wins over add/del", () => {
  const src = css();
  const layeredEnd = src.indexOf("}", src.lastIndexOf(".diff-num.diff-del"));
  const hl = src.indexOf(".diff-row.line-hl > td");
  assert.ok(hl > layeredEnd, "selection rule lives outside @layer (unlayered beats layered at any specificity)");
  assert.match(src, /\.diff-row\.line-hl > td \{ background-color: #fef3c7; \}/, "light selection tint intact");
  assert.match(src, /\.dark \.diff-row\.line-hl > td \{ background-color: rgba\(245, 158, 11, 0\.16\); \}/, "dark selection tint intact");
});

test("no hardcoded color literals in the diff JSX (colors live in ui.css)", () => {
  assert.doesNotMatch(diffTable(), COLOR_LITERAL, "DiffTable.jsx carries class names, never literals");
  assert.doesNotMatch(parser(), /diff-add|diff-del|diff-num/, "the parser knows no CSS classes");
  assert.doesNotMatch(parser(), COLOR_LITERAL, "the parser knows no colors");
});

test("diff.js anchor export intact (comment anchoring untouched)", () => {
  const src = parser();
  assert.match(src, /export function anchorContextSha/, "the §4 hash implementation is still exported");
});
