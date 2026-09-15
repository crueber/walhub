// web/test/unit/thread-flash-clear-573.test.js — Forgejo #573: collapsing
// a jumped-to thread card clears its flash highlight and actually collapses.
//
// Root cause: ThreadCard `open = () => getOpen() || flashed()` force-expands
// while flashed, and jumpToThread set flashTid with NOTHING ever clearing it
// (no timeout, no clear on collapse/resolve/reload) — so collapse looked dead
// and the emerald outline stuck forever.
//
// Fix (one shared-lifecycle clear path, reusable by #575's collapse-click
// case): the collapse/expand toggle clears the page-level flash for that tid
// first (ThreadCard `onCollapse` prop → page-level `clearFlashTid`), so
// open() and the outline return to tracking getOpen(). Forced-open while
// flashed is preserved for the jump itself; set paths
// (jumpToThread/jumpToStaged) unchanged; resolve/unresolve + collab-stream
// thread frames deliberately do NOT clear (minimal bar is collapse).
// StagedCard has no collapse control, so it takes no clear wiring.
//
// Pinned here: collapse clears the flash tid (inline + file-end cards),
// forced-open-while-flashed preserved, second-jump overwrite, unflashed cards
// unchanged (targeted clear), resolve/frame decision, staged no-collapse
// state, anchor/hash surface untouched, no new deps, Tailwind-only, law-12
// doc amendment. 390px reasoned: no layout change (same button, same card
// classes).

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");

const PULL = () => read("../../src/pages/Pull.jsx");
const DOC = () => read("../../../docs/go/12_web_ui.md");

const threadCard = () => {
  const s = PULL();
  return s.slice(s.indexOf("function ThreadCard(props)"), s.indexOf("function ThreadIndex(props)"));
};
const stagedCard = () => {
  const s = PULL();
  return s.slice(s.indexOf("function StagedCard(props)"), s.indexOf("/** One thread card:"));
};

// --- 1. collapse clears the page-level flash for that tid -------------------

test("collapse/expand toggle clears the flash for its tid before toggling", () => {
  const card = threadCard();
  assert.match(
    card,
    /onClick=\{\(e\) => \{ e\.stopPropagation\(\); props\.onCollapse\?\.\(t\(\)\.tid\); setOpen\(!getOpen\(\)\); \}\}/,
    "toggle stopPropagations (no card-root re-flash, #575) then calls onCollapse(tid) alongside the existing setOpen toggle",
  );
});

test("DiffFile threads the clear prop to inline + file-end thread cards", () => {
  const s = PULL();
  const at = s.indexOf("threadsAt(hi(), ri())");
  const inlineCard = s.slice(s.indexOf("<ThreadCard", at), s.indexOf("/>", at) + 2);
  assert.ok(inlineCard.includes("onCollapse={props.onCollapse}"), "inline cards receive onCollapse");
  const unplaced = s.slice(s.indexOf("placement().unplaced"), s.indexOf("function StagedCard(props)"));
  assert.ok(unplaced.includes("onCollapse={props.onCollapse}"), "file-end (outdated) cards receive onCollapse");
  const diffCall = s.slice(s.indexOf("<DiffFile"), s.indexOf("/>", s.indexOf("<DiffFile")));
  assert.ok(diffCall.includes("onCollapse={clearFlashTid}"), "page wires clearFlashTid into DiffFile");
});

test("clearFlashTid is targeted: clears only the matching tid", () => {
  const s = PULL();
  assert.match(
    s,
    /const clearFlashTid = \(tid\) => setFlashTid\(\(cur\) => \(cur === tid \? null : cur\)\);/,
    "targeted clear — collapsing an unflashed card never steals another card's flash",
  );
});

// --- 2. forced-open while flashed preserved (the jump still reveals) --------

test("forced-open-while-flashed preserved: open() and outline still track flashed()", () => {
  const card = threadCard();
  assert.ok(card.includes("const flashed = () => props.flashTid?.() === t().tid;"), "flashed() reads the page-level flash tid");
  assert.ok(card.includes("const open = () => getOpen() || flashed();"), "open() force-expands while flashed");
  assert.ok(card.includes("<Show when={open()}>"), "body renders on the forced-open signal");
  assert.ok(card.includes("outline outline-2 outline-emerald-500"), "flashed card keeps the emerald outline");
});

// --- 3. second jump moves flash to the new target only ----------------------

test("jumpToThread still sets (overwrites) the flash tid and scrolls", () => {
  const s = PULL();
  assert.match(
    s,
    /const jumpToThread = \(tid\) => \{\s*setFlashTid\(tid\);\s*document\.getElementById\(`thread-\$\{tid\}`\)\?\.scrollIntoView\(\{ block: "center" \}\);/,
    "jump sets flashTid (overwrite — second jump moves flash) and scrolls to thread-<tid>",
  );
  assert.ok(s.includes("onClick={() => props.onJump(e.target)}"), "index pill still jumps by tid");
});

test("jumpToStaged set path unchanged", () => {
  const s = PULL();
  assert.match(
    s,
    /const jumpToStaged = \(i\) => \{\s*setFlashStaged\(i\);\s*document\.getElementById\(`staged-\$\{i\}`\)\?\.scrollIntoView\(\{ block: "center" \}\);/,
    "staged jump+flash idiom byte-identical (#567)",
  );
});

// --- 4. unflashed cards behave exactly as today ------------------------------

test("unflashed cards: toggle-only behavior intact, label reads getOpen()", () => {
  const card = threadCard();
  assert.ok(card.includes('{getOpen() ? "collapse" : "expand"}'), "toggle label still reads getOpen()");
  assert.ok(card.includes("const [getOpen, setOpen] = createSignal(!t().resolved);"), "initial open state still !resolved");
  // Optional-chaining call: cards rendered without onCollapse (none exist
  // in-tree — both call sites wire it) would toggle exactly as before.
  assert.ok(card.includes("props.onCollapse?.(t().tid)"), "clear is optional-chained — a missing prop toggles as today");
});

// --- 5. resolve/unresolve + collab frames deliberately do NOT clear ---------

test("resolve/unresolve path carries no flash clear (decision pinned)", () => {
  const card = threadCard();
  const toggle = card.slice(card.indexOf("const toggle = async"), card.indexOf("};", card.indexOf("const toggle = async")) + 2);
  assert.ok(!toggle.includes("Flash") && !toggle.includes("flash"), "resolve toggle never touches flash — reload keeps the highlight on the acted card");
});

test("collab-stream thread frames carry no flash clear (decision pinned)", () => {
  const s = PULL();
  const streamLine = s.slice(s.indexOf("useCollabStream"), s.indexOf(");", s.indexOf("useCollabStream")) + 2);
  assert.ok(!streamLine.includes("Flash"), "stream handler invalidates only — no flash clear on live frames");
});

// --- 6. staged: no collapse control → no clear wiring ------------------------

test("StagedCard has no collapse/expand control, so no clear wiring", () => {
  const card = stagedCard();
  assert.ok(!card.includes("collapse") && !card.includes("expand"), "no collapse control on StagedCard");
  assert.ok(!card.includes("onCollapse") && !card.includes("clearFlash"), "no clear prop on StagedCard");
  assert.ok(card.includes("outline outline-2 outline-emerald-500"), "staged flash outline idiom untouched");
});

// --- 7. anchor/drift-hash surface untouched ----------------------------------

test("no anchor/drift-hash surface touched", () => {
  const s = PULL();
  assert.ok(!/anchorContextSha\s*\(/.test(s), "never hashes directly (buildAnchor owns the bytes — the #546 pin holds)");
  assert.ok(s.includes("freshnessOf"), "freshness placement truth untouched");
  assert.ok(s.includes("sortThreadsForIndex"), "index order untouched");
});

// --- 8. laws: no new deps, Tailwind-only, docs change with code --------------

test("no new runtime dependencies, no new styling surface (laws 1 + 11)", () => {
  assert.ok(!PULL().includes("<style"), "no inline <style> in Pull.jsx");
  const pkg = JSON.parse(read("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime deps stay exactly solid-js + @solidjs/router + marked + dompurify",
  );
});

test("law-12: the web-UI decision lands in the same change", () => {
  assert.match(DOC(), /#573/, "12_web_ui.md carries the #573 decision");
});
