// web/test/unit/thread-toggle-label-580.test.js — Forgejo #580: thread card
// collapse/expand label goes stale when flash-expanded.
//
// Root cause: ThreadCard `open = () => getOpen() || flashed()` drives the
// body `<Show when={open()}>`, but the toggle label read only `getOpen()` —
// so a flash-expanded card (via #575 card-body click or the #573-era pill
// jump, both of which set the same page-level flashTid) showed body-open
// with label "expand".
//
// Fix (minimal per issue): derive the toggle label from open(), the same
// signal the body renders on. Unflashed behavior is unchanged
// (open()===getOpen() when unflashed; resolved threads still start
// collapsed with label "expand").
//
// Post-click composition (verified here): the toggle runs onCollapse (the
// #573 targeted clearFlashTid) + setOpen(!getOpen()). Trace on a
// flashed-and-collapsed card (getOpen()=false, flash set): label reads
// "collapse" (correct — body open); first click clears the flash and flips
// getOpen to true → open()=true, label "collapse", body open (agree);
// second click flips getOpen to false with flash still clear → open()=false,
// label "expand", body closed (agree). Pinned below as a two-click sequence
// so label and body can never disagree after any click.
//
// StagedCard finding: no collapse/expand toggle exists (plain always-open
// card with edit/remove controls only), so there is no divergent signal to
// fix — pinned in §5.
//
// Pinned here: label/body share open(), flash-expanded → "collapse" for both
// flash sources (#575 click + #573 pill jump set the same signal), the
// two-click agreement sequence incl. the #573 clear path, unflashed label
// truth table, resolved-starts-collapsed unchanged, StagedCard no-toggle
// finding, no new deps, Tailwind untouched, law-12 doc amendment. 390px
// reasoned: text-only change (same button, same card classes).

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

// --- 1. label and body share the effective open() signal ---------------------

test("toggle label reads open(), the same signal the body renders on", () => {
  const card = threadCard();
  assert.ok(card.includes("const open = () => getOpen() || flashed();"), "effective open() still force-expands while flashed");
  assert.ok(card.includes('{open() ? "collapse" : "expand"}'), "toggle label derives from open()");
  assert.ok(card.includes("<Show when={open()}>"), "body renders on open()");
  assert.ok(!card.includes('{getOpen() ? "collapse" : "expand"}'), "no stale getOpen()-only label remains");
});

// --- 2. both flash sources expand with label "collapse" ----------------------

test("pill-jump flash source (#573) still sets the page-level flash tid", () => {
  assert.match(
    PULL(),
    /const jumpToThread = \(tid\) => \{\s*setFlashTid\(tid\);\s*document\.getElementById\(`thread-\$\{tid\}`\)\?\.scrollIntoView\(\{ block: "center" \}\);/,
    "jumpToThread sets flashTid and scrolls — a jumped-to collapsed card is flash-expanded",
  );
});

test("card-body click flash source (#575) still sets the same flash tid", () => {
  const card = threadCard();
  assert.ok(
    card.includes("onClick={(e) => cardFlashClick(e, () => props.onFlash?.(t().tid))}"),
    "root body click fires onFlash(tid) — same signal the pill jump sets, so the same label fix covers it",
  );
});

test("model: flash-expanded (either source) labels 'collapse' with body open", () => {
  // Pure model of the card signals: label + body both derive from open().
  const view = (getOpen, flashed) => {
    const open = getOpen || flashed;
    return { label: open ? "collapse" : "expand", bodyOpen: open };
  };
  for (const source of ["pill-jump", "card-body-click"]) {
    const v = view(false, true);
    assert.equal(v.label, "collapse", `${source}: flashed-and-collapsed card labels "collapse"`);
    assert.equal(v.bodyOpen, true, `${source}: flashed-and-collapsed card body is open`);
  }
});

// --- 3. two-click label/body agreement incl. the #573 clear path -------------

test("toggle clears the page-level flash before flipping getOpen (#573 path)", () => {
  const card = threadCard();
  assert.match(
    card,
    /onClick=\{\(e\) => \{ e\.stopPropagation\(\); props\.onCollapse\?\.\(t\(\)\.tid\); setOpen\(!getOpen\(\)\); \}\}/,
    "toggle stopPropagations, clears via onCollapse(tid), then flips getOpen",
  );
  assert.match(
    PULL(),
    /const clearFlashTid = \(tid\) => setFlashTid\(\(cur\) => \(cur === tid \? null : cur\)\);/,
    "page-level clear is targeted — the click clears exactly this card's flash",
  );
});

test("model: two-click sequence on a flashed-and-collapsed card keeps label/body in agreement", () => {
  // Mirrors the card: flashed = flashTid===tid, open = getOpen||flashed,
  // label/body from open(), click = targeted clear + setOpen(!getOpen).
  const tid = "t1";
  let getOpen = false;
  let flashTid = tid; // flashed-and-collapsed start (jump or body-click flash)
  const flashed = () => flashTid === tid;
  const open = () => getOpen || flashed();
  const label = () => (open() ? "collapse" : "expand");
  const agree = (where) => assert.equal(label() === "collapse", open(), `${where}: label/body agree`);
  const click = () => {
    if (flashTid === tid) flashTid = null; // clearFlashTid(tid)
    getOpen = !getOpen; // setOpen(!getOpen())
  };
  assert.equal(label(), "collapse", "start: flashed card labels 'collapse' (body open)");
  assert.equal(open(), true, "start: flashed card body is open");
  agree("start");
  click(); // first click: labeled "collapse"
  assert.equal(flashTid, null, "first click clears the flash (#573)");
  assert.equal(getOpen, true, "first click flips getOpen false→true");
  assert.equal(open(), true, "after first click: card open");
  assert.equal(label(), "collapse", "after first click: label still 'collapse'");
  agree("after first click");
  click(); // second click: labeled "collapse"
  assert.equal(getOpen, false, "second click flips getOpen true→false");
  assert.equal(open(), false, "after second click: card closed");
  assert.equal(label(), "expand", "after second click: label 'expand'");
  agree("after second click");
});

// --- 4. unflashed behavior unchanged -----------------------------------------

test("model: unflashed label truth table (open()===getOpen())", () => {
  const view = (getOpen, flashed) => {
    const open = getOpen || flashed;
    return { label: open ? "collapse" : "expand", bodyOpen: open };
  };
  assert.deepEqual(view(false, false), { label: "expand", bodyOpen: false }, "unflashed collapsed → 'expand', body closed");
  assert.deepEqual(view(true, false), { label: "collapse", bodyOpen: true }, "unflashed expanded → 'collapse', body open");
});

test("resolved threads still start collapsed (initial signal untouched)", () => {
  const card = threadCard();
  assert.ok(card.includes("const [getOpen, setOpen] = createSignal(!t().resolved);"), "initial open state still !resolved");
  // Model the initial render unflashed: resolved → getOpen false → label "expand".
  const getOpen = !true;
  const open = getOpen || false;
  assert.equal(open ? "collapse" : "expand", "expand", "resolved thread starts collapsed with label 'expand'");
});

// --- 5. StagedCard finding: no collapse/expand toggle ------------------------

test("StagedCard has no collapse/expand toggle — nothing to fix", () => {
  const card = stagedCard();
  assert.ok(!card.includes("collapse") && !card.includes("expand"), "no collapse control on StagedCard (edit/remove only, always open)");
  assert.ok(!card.includes("setOpen") && !card.includes("getOpen"), "no open signal on StagedCard to diverge from");
});

// --- 6. laws: no new deps, Tailwind untouched, docs change with code ---------

test("no new runtime dependencies, no styling change (laws 1 + 11)", () => {
  assert.ok(!PULL().includes("<style"), "no inline <style> in Pull.jsx");
  const css = read("../../src/ui.css");
  assert.ok(!css.includes("580"), "no new ui.css rule — text-only label change");
  const pkg = JSON.parse(read("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime deps stay exactly solid-js + @solidjs/router + marked + dompurify",
  );
});

test("law-12: the web-UI decision lands in the same change", () => {
  assert.match(DOC(), /#580/, "12_web_ui.md carries the #580 decision");
});
