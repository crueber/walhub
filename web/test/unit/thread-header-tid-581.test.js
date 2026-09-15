// web/test/unit/thread-header-tid-581.test.js — Forgejo #581: the ThreadCard
// header renders no visible zero-padded tid. The %08x thread id is an opaque
// internal key, so the mono tid span is gone (plain removal, no tooltip);
// the card root id, aria-label, flashTid mechanics, and the ThreadIndex
// anchorLabel pills are untouched. StagedCard renders the anchorLabel
// (path:line — a meaningful location, not an opaque id), so it is unchanged.
//
// Removal only — no Tailwind change, no new deps (law 1); doc amendment in
// docs/go/12_web_ui.md in the same change (law 12).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");

const PULL = () => read("../../src/pages/Pull.jsx");
const DOC = () => read("../../../docs/go/12_web_ui.md");

const cardOf = (s) => s.slice(s.indexOf("function ThreadCard(props)"), s.indexOf("function ThreadIndex(props)"));
const stagedOf = (s) => s.slice(s.indexOf("function StagedCard(props)"), s.indexOf("function ThreadCard(props)"));
const indexOf = (s) => s.slice(s.indexOf("function ThreadIndex(props)"), s.indexOf("function ThreadComments(props)"));

test("header has no visible tid span (plain removal, no tooltip)", () => {
  const card = cardOf(PULL());
  assert.ok(!card.includes("{t().tid}</span>"), "no span renders the tid as visible text");
  assert.ok(!card.includes("title={t().tid}") && !card.includes("title={`Thread"), "tid was removed, not moved into a title/tooltip");
});

test("root id + aria-label + flash mechanics intact", () => {
  const s = PULL();
  const card = cardOf(s);
  assert.match(card, /id=\{`thread-\$\{t\(\)\.tid\}`\}/, "card root keeps id thread-<tid> (jump target)");
  assert.match(card, /aria-label=\{`Thread \$\{t\(\)\.tid\}`\}/, "screen-reader label kept");
  assert.ok(card.includes("const flashed = () => props.flashTid?.() === t().tid;"), "flashTid comparison kept");
  assert.ok(card.includes("onClick={(e) => cardFlashClick(e, () => props.onFlash?.(t().tid))}"), "body-click flash kept (#575)");
  assert.ok(card.includes("props.onCollapse?.(t().tid)"), "collapse clear kept (#573)");
  assert.match(s, /const jumpToThread = \(tid\) => \{\s*setFlashTid\(tid\);\s*document\.getElementById\(`thread-\$\{tid\}`\)\?\.scrollIntoView\(\{ block: "center" \}\);/, "pill jump still scrolls to thread-<tid> with flash");
});

test("index pills render anchorLabel, not tid", () => {
  const index = indexOf(PULL());
  assert.ok(index.includes("{anchorLabel(e.t.anchor)}"), "posted pills label by anchor");
  assert.ok(index.includes("{anchorLabel(p.anchor)}"), "staged pills label by anchor");
  assert.ok(!index.includes("{e.t.tid}</") && !index.includes("{t().tid}</"), "no pill renders a bare tid");
});

test("StagedCard renders the meaningful anchor label, not an opaque id", () => {
  const staged = stagedOf(PULL());
  assert.ok(staged.includes("{label()}"), "staged header still shows the path:line anchor label");
  assert.ok(!staged.includes("props.index}</span>"), "no visible staged pending-index span");
  assert.match(staged, /id=\{`staged-\$\{props\.index\}`\}/, "staged jump-target id kept");
});

test("no new styling surface, no new runtime dependencies (laws 1 + 11)", () => {
  const card = cardOf(PULL());
  assert.ok(!card.includes("style="), "no inline styles on the ThreadCard");
  const pkg = JSON.parse(read("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime deps stay exactly solid-js + @solidjs/router + marked + dompurify",
  );
});

test("law-12: the web-UI decision lands in the same change", () => {
  assert.match(DOC(), /#581/, "12_web_ui.md carries the #581 decision");
});
