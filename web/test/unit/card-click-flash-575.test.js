// web/test/unit/card-click-flash-575.test.js — Forgejo #575: clicking
// inside a thread / staged card highlights it like a ThreadIndex pill jump
// does.
//
// Before the fix, the flash lifecycle (#573) existed ONLY on the pill-jump
// path (jumpToThread/jumpToStaged → flashTid/flashStaged, consumed at
// ThreadCard/StagedCard): clicking a card body did nothing.
//
// Fix (CONSUMES the #573 mechanics — no second highlight lifecycle): the
// card roots carry an onClick that sets the SAME page-level flash for that
// tid/index (same emerald ring, same #574 inset geometry) through a shared
// cardFlashClick guard. The page passes the raw setters
// (onFlashTid={setFlashTid}, onFlashStaged={setFlashStaged}), so a body
// click overwrites exactly like a pill jump but never scrolls.
//
// Decisions (implementer's call, pinned here + the law-12 amendment):
// (1) trigger scope — any body click flashes, but clicks originating from
// interactive controls (button/a/input/textarea/select/form, via the
// closest() check — the DiffFile row-handler convention) never flash, and
// a click ending a text selection (window.getSelection non-empty) never
// re-flashes, so selecting comment text to copy it stays calm;
// (2) single-flash model — the card click calls the same setter the pill
// jump calls, so clicking one card overwrites a previous flash (pill-jump
// overwrite semantics; cross-kind flashes are as independent as the pill
// paths leave them); (3) StagedCard joins — same guard + same setter
// terms as ThreadCard.
//
// Collapse interplay (#573): the collapse control stopPropagations AND the
// root guard ignores control-originated clicks, so a collapse click clears
// via onCollapse and can never re-flash — it ends collapsed +
// unhighlighted (the #573 review advisory, pinned as a regression).
//
// Pinned here: shared guard shape, card-root wiring both cards (inline +
// file-end threading), collapse-cleared regression, selection-drag ignore,
// single-flash overwrite, pill-jump paths byte-identical, anchor/hash
// surface untouched, no-new-deps, Tailwind-only, law-12 doc amendment.
// 390px reasoned: behavior-only change — one onClick per card root, zero
// layout utilities added.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");

const PULL = () => read("../../src/pages/Pull.jsx");
const DOC = () => read("../../../docs/go/12_web_ui.md");

const stagedCard = () => {
  const s = PULL();
  return s.slice(s.indexOf("function StagedCard(props)"), s.indexOf("function ThreadCard(props)"));
};
const threadCard = () => {
  const s = PULL();
  return s.slice(s.indexOf("function ThreadCard(props)"), s.indexOf("function ThreadIndex(props)"));
};

// --- 1. one shared guard, consumed (not forked) lifecycle --------------------

test("cardFlashClick guard: ignores control clicks + selection drags, else fires", () => {
  const s = PULL();
  const guard = s.slice(s.indexOf("const cardFlashClick"), s.indexOf("};", s.indexOf("const cardFlashClick")) + 2);
  assert.match(
    guard,
    /e\.target\.closest\?\.\("button, a, input, textarea, select, form"\)/,
    "control-originated clicks never flash (the DiffFile row-handler closest() convention)",
  );
  assert.match(
    guard,
    /window\.getSelection\?\.\(\)\?\.toString\(\)/,
    "a click ending a text selection never re-flashes",
  );
  assert.ok(guard.includes("fire()"), "anything else fires the card's flash setter");
  assert.ok(!guard.includes("setFlashTid") && !guard.includes("setFlashStaged"), "guard owns no flash state — the #573 signals are reused, not forked");
});

test("ThreadCard root click sets the same page-level flash as the pill jump", () => {
  const card = threadCard();
  assert.ok(
    card.includes("onClick={(e) => cardFlashClick(e, () => props.onFlash?.(t().tid))}"),
    "root click fires onFlash(tid) through the shared guard",
  );
  assert.ok(card.includes("const flashed = () => props.flashTid?.() === t().tid;"), "ring still reads the #573 flashTid signal — same emerald ring, same #574 inset geometry");
});

test("StagedCard root click joins on the same terms (parity)", () => {
  const card = stagedCard();
  assert.ok(
    card.includes("onClick={(e) => cardFlashClick(e, () => props.onFlash?.(props.index))}"),
    "root click fires onFlash(index) through the shared guard",
  );
  assert.ok(card.includes("outline outline-2 outline-emerald-500 outline-offset-[-2px]"), "staged ring keeps the #574 inset geometry");
});

// --- 2. threading: page setters reach inline + file-end cards ----------------

test("DiffFile threads the flash setters to inline + file-end cards", () => {
  const s = PULL();
  const at = s.indexOf("threadsAt(hi(), ri())");
  const inlineThread = s.slice(s.indexOf("<ThreadCard", at), s.indexOf("/>", at) + 2);
  assert.ok(inlineThread.includes("onFlash={props.onFlashTid}"), "inline thread cards receive onFlashTid");
  const unplaced = s.slice(s.indexOf("stagedByKey().unplaced"), s.indexOf("function StagedCard(props)"));
  assert.ok(unplaced.includes("onFlash={props.onFlashTid}"), "file-end (outdated) thread cards receive onFlashTid");
  assert.ok((unplaced.match(/onFlash=\{props\.onFlashStaged\}/g) ?? []).length >= 1, "file-end staged cards receive onFlashStaged");
  const stagedInline = s.slice(s.indexOf("stagedAt(hi(), ri())"), s.indexOf("threadsAt(hi(), ri())"));
  assert.ok(stagedInline.includes("onFlash={props.onFlashStaged}"), "inline staged cards receive onFlashStaged");
});

test("page passes the raw flash setters (same overwrite, no scroll)", () => {
  const s = PULL();
  const diffCall = s.slice(s.indexOf("<DiffFile"), s.indexOf("/>", s.indexOf("<DiffFile")));
  assert.ok(diffCall.includes("onFlashTid={setFlashTid}"), "thread card clicks call the same setter the pill jump calls — single-flash overwrite");
  assert.ok(diffCall.includes("onFlashStaged={setFlashStaged}"), "staged card clicks call the same setter the staged pill jump calls");
});

// --- 3. collapse interplay: ends collapsed + unhighlighted (regression) ------

test("collapse click cannot re-flash: stopPropagation + control-ignore + clear-before-toggle", () => {
  const card = threadCard();
  assert.match(
    card,
    /onClick=\{\(e\) => \{ e\.stopPropagation\(\); props\.onCollapse\?\.\(t\(\)\.tid\); setOpen\(!getOpen\(\)\); \}\}/,
    "collapse stopPropagations, then clears via onCollapse before toggling",
  );
  assert.ok(PULL().includes("const cardFlashClick"), "the root guard's closest() check ignores the collapse button even without the stop");
});

// --- 4. pill-jump paths unchanged --------------------------------------------

test("jumpToThread set + scroll idiom byte-identical", () => {
  assert.match(
    PULL(),
    /const jumpToThread = \(tid\) => \{\s*setFlashTid\(tid\);\s*document\.getElementById\(`thread-\$\{tid\}`\)\?\.scrollIntoView\(\{ block: "center" \}\);/,
    "pill jump still sets flashTid and scrolls to thread-<tid>",
  );
});

test("jumpToStaged set + scroll idiom byte-identical", () => {
  assert.match(
    PULL(),
    /const jumpToStaged = \(i\) => \{\s*setFlashStaged\(i\);\s*document\.getElementById\(`staged-\$\{i\}`\)\?\.scrollIntoView\(\{ block: "center" \}\);/,
    "staged pill jump still sets flashStaged and scrolls to staged-<i>",
  );
});

test("card click never scrolls (jump owns the scroll)", () => {
  const guard = PULL().slice(PULL().indexOf("const cardFlashClick"), PULL().indexOf("};", PULL().indexOf("const cardFlashClick")) + 2);
  assert.ok(!guard.includes("scrollIntoView"), "card flash path carries no scrollIntoView — the card is already under the cursor");
});

// --- 5. anchor/drift-hash surface untouched -----------------------------------

test("no anchor/drift-hash surface touched", () => {
  const s = PULL();
  assert.ok(!/anchorContextSha\s*\(/.test(s), "never hashes directly (the #546 pin holds)");
  assert.ok(s.includes("freshnessOf"), "freshness placement truth untouched");
  assert.ok(s.includes("sortThreadsForIndex"), "index order untouched");
});

// --- 6. laws: no new deps, Tailwind-only, docs change with code ---------------

test("no new runtime dependencies, no new styling surface (laws 1 + 11)", () => {
  assert.ok(!PULL().includes("<style"), "no inline <style> in Pull.jsx");
  const css = read("../../src/ui.css");
  assert.ok(!css.includes("575") && !css.includes("flash"), "no new ui.css rule — behavior-only change composes the #574 fragment as-is");
  const pkg = JSON.parse(read("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime deps stay exactly solid-js + @solidjs/router + marked + dompurify",
  );
});

test("law-12: the web-UI decision lands in the same change", () => {
  assert.match(DOC(), /#575/, "12_web_ui.md carries the #575 decision");
});
