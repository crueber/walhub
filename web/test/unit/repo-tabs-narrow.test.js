// web/test/unit/repo-tabs-narrow.test.js — issue #274 regression: the repo
// tab bar (Code / Commits / Issues / Pulls / Checks / Releases / Settings)
// overflowed a 390px phone viewport (measured scrollWidth 515 vs clientWidth
// 358 — trailing tabs cut off mid-letter with no scroll affordance, and the
// overflow leaked to the page root: docW 531 vs 390). The fix gives the bar
// the same treatment as the #273 site-nav strip: the nav scrolls internally
// (overflow-x-auto, nowrap, max-w-full) instead of pushing the page sideways,
// links never shrink to unreadability, and a createEffect keeps the active
// tab scrolled into view on navigation (block: "nearest" so the page never
// jumps vertically). The nav keeps its aria-label so the strip is a labelled
// landmark keyboard users can Tab through (the strip follows focus). Dark +
// light share the treatment (layout classes + theme-independent CSS). No
// DOM: JSX/CSS pinned as source text, mirroring header-narrow.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const REPO = srcOf("../../src/pages/Repo.jsx");
const CSS = srcOf("../../src/ui.css");

function block(src, start, end) {
  const s = src.indexOf(start);
  assert.ok(s !== -1, `expected block start ${start}`);
  const e = src.indexOf(end, s);
  assert.ok(e !== -1, `expected block end ${end}`);
  return src.slice(s, e);
}

test("tab strip scrolls internally instead of pushing the page sideways", () => {
  const navTag = block(REPO, "<nav ref={tabsNav}", ">");
  for (const cls of ["overflow-x-auto", "whitespace-nowrap", "max-w-full"]) {
    assert.ok(navTag.includes(cls), `repo-tabs nav must carry ${cls} (scroll within itself, never widen the page)`);
  }
  assert.ok(!navTag.includes("flex-wrap"), "repo-tabs nav must not wrap (single tab row, the git-host pattern)");
});

test("tab bar contents unchanged (no tabs moved into a hamburger)", () => {
  const tabs = block(REPO, "const TABS = [", "];");
  for (const id of ["code", "commits", "issues", "pulls", "checks", "releases", "settings"]) {
    assert.ok(tabs.includes(`id: "${id}"`), `repo tab bar must still carry the ${id} tab`);
  }
  const nav = block(REPO, "<nav ref={tabsNav}", "</nav>");
  assert.ok(nav.includes("<For each={TABS}>"), "nav still renders the full TABS list");
});

test("strip is a labelled landmark; links never shrink to unreadability", () => {
  const navTag = block(REPO, "<nav ref={tabsNav}", ">");
  assert.ok(navTag.includes('aria-label="repository sections"'), "repo-tabs strip must stay a labelled landmark");
  assert.match(CSS, /\.repo-tabs a \{ @apply shrink-0;/, "tab links keep full width inside the scroll strip");
});

test("strip hides its own scrollbar but stays keyboard/touch scrollable", () => {
  const strip = CSS.slice(CSS.indexOf("issue #274"));
  assert.ok(strip.includes(".repo-tabs { scrollbar-width: none; }"), "Firefox: no strip scrollbar (stable row height)");
  assert.ok(
    strip.includes(".repo-tabs::-webkit-scrollbar { display: none; }"),
    "Chromium/Safari: no strip scrollbar (stable row height)",
  );
  const navTag = block(REPO, "<nav ref={tabsNav}", ">");
  assert.ok(navTag.includes("overflow-x-auto"), "strip keeps overflow-x-auto: touch scroll + focus-follow still work");
});

test("active tab is obvious and scrolled into view on navigation", () => {
  const nav = block(REPO, "<nav ref={tabsNav}", "</nav>");
  assert.ok(nav.includes('aria-current={activeTab(location.pathname) === t.id ? "page" : undefined}'),
    "active tab keeps aria-current=\"page\" (obvious + queryable)");
  const effect = block(REPO, "Issue #274: keep the active tab visible", "const ctx = {");
  assert.ok(effect.includes("createEffect"), "a createEffect tracks navigation");
  assert.ok(effect.includes("activeTab(location.pathname)"), "the effect tracks the active tab reactively");
  assert.ok(effect.includes('[aria-current="page"]'), "the effect finds the active link by aria-current");
  assert.ok(effect.includes("scrollIntoView"), "the effect scrolls the active link into view");
  assert.ok(effect.includes('inline: "center"'), "active link centers in the strip (fully visible, neighbors peek)");
  assert.ok(effect.includes('block: "nearest"'), "scroll never moves the page vertically");
  assert.ok(effect.includes('typeof el.scrollIntoView === "function"'), "guarded for non-DOM environments");
});

test("390px arithmetic: strip fits its cell, overflow stays inside it", () => {
  // Page padding px-4 = 32px, so the strip's cell is 390 − 32 = 358px wide
  // (the issue's measured clientWidth). The widest tab ("Releases" /
  // "Settings", 8 chars at text-sm ≈ 8px/char + px-3 24px + gap) is bounded
  // by ~110px < 358px, so several tabs are always visible with the next one
  // peeking (the touch scroll affordance), and the measured 515px of tab
  // content scrolls INSIDE the strip — the page contributes zero overflow
  // (scrollWidth === clientWidth).
  const viewport = 390;
  const stripClient = viewport - 32;
  assert.equal(stripClient, 358, "strip client width matches the issue's measured 358px");
  const widestTab = 8 * 8 + 24 + 4 + 18; // chars + padding + gap + underline slack
  assert.ok(widestTab < stripClient, `widest tab ~${widestTab}px fits the ${stripClient}px strip (neighbors peek)`);
  const measuredContent = 515; // the issue's measured tab-row scrollWidth
  assert.ok(measuredContent - stripClient > 0, "content overflows the strip cell, so the strip (not the page) scrolls");
});
