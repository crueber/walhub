// web/test/unit/flash-ring-574.test.js — Forgejo #574: the flashed-card
// highlight ring (emerald, on ThreadIndex pill jump) painted OUTSIDE the
// card's border box (`outline outline-2 outline-emerald-500` with the default
// 0 offset) while every hunk renders inside `<div class="mb-3
// overflow-x-auto">` (Pull.jsx DiffFile — overflow-x:auto implies
// overflow-y:auto). Long code lines put the card's right edge exactly at the
// wrapper edge — where the outside ring paints — so the ring clipped at the
// wrapper edge / contributed to scroll extent.
//
// Fix (per the issue): inset ring geometry on the flashed state —
// `outline-offset-[-2px]` alongside the existing outline — so the ring paints
// inside the border box, hugging the rounded corners. ONE shared treatment:
// StagedCard (~:696) and ThreadCard (~:764) carry the byte-identical flash
// fragment, and the draft composer card in the same slot (:583, no flash
// state) keeps its slot classes with no divergent outline/ring. The a11y
// :focus-visible outline (ui.css:18-21) is deliberately untouched.
//
// Pinned here: inset geometry on both flash paths (no bare outside
// `outline-2` without offset/inset on any flash path), byte-identical shared
// fragment across StagedCard/ThreadCard, composer-slot consistency (no
// outline/ring there, base slot classes unchanged), focus-visible rule +
// gutter-trigger classes byte-identical, cards still inside the existing
// overflow wrapper (no new scroll container), no new ui.css rules / deps,
// law-12 doc amendment. CSS geometry (no layout-box delta — outline-offset
// is paint-only) + both-themes token (emerald-500 both themes, the #567
// idiom) + 390px reasoned: the fragment adds zero layout utilities, and the
// ring can no longer reach the wrapper edge by construction.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");

const PULL = () => read("../../src/pages/Pull.jsx");
const CSS = () => read("../../src/ui.css");
const DOC = () => read("../../../docs/go/12_web_ui.md");

// The ONE shared flashed geometry (Tailwind-only composition — the existing
// emerald outline idiom plus the inset offset; identical in both cards).
const FLASH = " outline outline-2 outline-emerald-500 outline-offset-[-2px]";

const stagedCard = () => {
  const s = PULL();
  return s.slice(s.indexOf("function StagedCard(props)"), s.indexOf("function ThreadCard(props)"));
};
const threadCard = () => {
  const s = PULL();
  return s.slice(s.indexOf("function ThreadCard(props)"), s.indexOf("function ThreadIndex(props)"));
};
const flashFragment = (card, sig) => {
  const m = card.match(new RegExp("\\$\\{" + sig + ' \\? "([^"]+)" : ""\\}'));
  assert.ok(m, `flash ternary (${sig}) present`);
  return m[1];
};

// --- 1. inset geometry on both flash paths ----------------------------------

test("StagedCard flash carries the inset ring geometry", () => {
  assert.equal(flashFragment(stagedCard(), "props\\.flashed"), FLASH, "staged flash = outline + emerald + inset -2px offset");
});

test("ThreadCard flash carries the inset ring geometry", () => {
  assert.equal(flashFragment(threadCard(), "flashed\\(\\)"), FLASH, "thread flash = outline + emerald + inset -2px offset");
});

// --- 2. one shared treatment, no bare outside ring anywhere ------------------

test("flash fragments are byte-identical across ThreadCard + StagedCard", () => {
  assert.equal(
    flashFragment(threadCard(), "flashed\\(\\)"),
    flashFragment(stagedCard(), "props\\.flashed"),
    "one shared geometry — the two cards cannot drift apart",
  );
});

test("no bare outside outline-2 on any flash path (JSX-wide sweep)", () => {
  const s = PULL();
  // Every `outline-2` outside a focus-visible context must compose the inset
  // offset (or an inset ring) — a plain outside outline-2 is the #574 bug.
  const hits = [...s.matchAll(/outline-2[^"]*/g)];
  assert.ok(hits.length > 0, "outline-2 sites exist to sweep");
  for (const h of hits) {
    const lineStart = s.lastIndexOf("\n", h.index) + 1;
    const line = s.slice(lineStart, s.indexOf("\n", h.index));
    if (line.includes("focus-visible")) continue; // a11y precedent — §4 pins it
    assert.ok(
      h[0].includes("outline-offset-[-2px]") || line.includes("ring-inset"),
      `outside outline-2 without inset geometry: ${line.trim().slice(0, 120)}`,
    );
  }
});

// --- 3. composer slot stays consistent (no divergent treatment) ---------------

test("draft composer card shares the slot with no outline/ring of its own", () => {
  const s = PULL();
  const composer = s.slice(s.indexOf("aria-label={`Draft comment on"), s.indexOf("aria-label={`Draft comment on") + 400);
  const divStart = s.lastIndexOf("<div", s.indexOf("aria-label={`Draft comment on"));
  const divTag = s.slice(divStart, s.indexOf(">", divStart) + 1);
  assert.ok(
    divTag.includes("ml-8 mt-1 rounded border border-zinc-200 p-2 dark:border-zinc-700"),
    "composer keeps the exact shared slot classes",
  );
  assert.ok(!divTag.includes("outline") && !divTag.includes("ring-"), "composer carries no outline/ring — no divergent flash treatment");
  assert.ok(!composer.includes("outline"), "composer subtree adds no outline treatment");
});

// --- 4. a11y :focus-visible precedent untouched --------------------------------

test("ui.css :focus-visible rule byte-identical (the #574 do-not-touch)", () => {
  const css = CSS();
  assert.match(
    css,
    /:focus-visible \{\s*outline: 2px solid var\(--color-emerald-500\);\s*outline-offset: 1px;\s*\}/,
    "base-layer focus ring keeps its outside 1px offset — #574 insets only the flash ring",
  );
});

test("sign-button focus-visible classes carry the keyboard ring (#598 relocation)", () => {
  // The #560 keyboard ring lived on the conversation gutter "+"; #598
  // removes that button, and the ring relocates to the DiffTable sign
  // button (same classes — the a11y floor for the single-line staging
  // target). Conversation rows take programmatic focus only
  // (tabindex="-1" for Escape refocus), so they carry no ring.
  const dt = read("../../src/components/DiffTable.jsx");
  assert.ok(
    dt.includes("focus-visible:outline focus-visible:outline-2 focus-visible:outline-emerald-500"),
    "the keyboard ring stays as-is on the sign button",
  );
});

// --- 5. still inside the existing scroll wrapper, no layout delta -------------

test("flashed cards still render inside the existing overflow-x-auto hunk wrapper", () => {
  const s = PULL();
  const diffFile = s.slice(s.indexOf("function DiffFile(props)"), s.indexOf("function StagedCard(props)"));
  const overflow = diffFile.indexOf("mb-3 overflow-x-auto");
  assert.ok(overflow > 0, "hunk wrapper keeps mb-3 overflow-x-auto");
  assert.ok(diffFile.indexOf("<For each={threadsAt(hi(), ri())}>") > overflow, "thread cards render inside that wrapper");
  assert.ok(diffFile.indexOf("<For each={stagedAt(hi(), ri())}>") > overflow, "staged cards render inside that wrapper");
  assert.ok(!/overflow-(x|y)-(auto|scroll)/.test(stagedCard() + threadCard()), "cards add no scroll container of their own");
});

test("flash fragment is paint-only: no layout utilities, both-themes token", () => {
  for (const frag of [flashFragment(stagedCard(), "props\\.flashed"), flashFragment(threadCard(), "flashed\\(\\)")]) {
    assert.ok(!/(^|\s)(ml-|mt-|mb-|p-|m-|w-|h-|block|flex|rounded|border)/.test(frag), "no box/slot utilities in the flash fragment — zero layout delta at desktop + 390px");
    assert.ok(frag.includes("outline-emerald-500"), "same emerald-500 token both themes (the #567 idiom — no per-theme flash code)");
  }
});

// --- 6. laws: Tailwind-only, no new deps, docs change with code ----------------

test("no new styling surface: no ui.css flash rule, no inline <style> (laws 1 + 11)", () => {
  assert.ok(!PULL().includes("<style"), "no inline <style> in Pull.jsx");
  assert.ok(!CSS().includes("flash") && !CSS().includes("574"), "no ui.css flash rule — Tailwind-only composition, so no style-guideline extension needed");
  const pkg = JSON.parse(read("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime deps stay exactly solid-js + @solidjs/router + marked + dompurify",
  );
});

test("law-12: the web-UI decision lands in the same change", () => {
  assert.match(DOC(), /#574/, "12_web_ui.md carries the #574 decision");
});
