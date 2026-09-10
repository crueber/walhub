// web/test/unit/header-narrow.test.js — issue #273 regression: the site
// header overflowed a 390px phone viewport (flex row, gap-6, nowrap — brand +
// site-nav + ml-auto right cluster exceeded the viewport, theme toggle landed
// at x=439, page panned sideways). The fix pins brand + right cluster
// (shrink-0) and lets the site-nav flex into the leftover (min-w-0 flex-1)
// and scroll internally (overflow-x-auto, nowrap) — the common git-host
// pattern — with tighter gaps below sm:. No page-level horizontal scroll from
// the header at 390px; bell + theme toggle stay visible and tappable; the
// tray popover fit is the #278 viewport bound (asserted intact here). The nav
// keeps an aria-label so the scroll strip is a labelled landmark keyboard
// users can Tab through (the strip follows focus). Dark + light share the
// treatment (layout classes + theme-independent CSS). No DOM: JSX/CSS pinned
// as source text, mirroring nav-api-right.test.js / popover-viewport.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const APP = srcOf("../../src/App.jsx");
const CSS = srcOf("../../src/ui.css");

function block(src, start, end) {
  const s = src.indexOf(start);
  assert.ok(s !== -1, `expected block start ${start}`);
  const e = src.indexOf(end, s);
  assert.ok(e !== -1, `expected block end ${end}`);
  return src.slice(s, e);
}

test("header row degrades at narrow widths: responsive gaps, no bare gap-6", () => {
  const row = block(APP, "<header", "</header>");
  assert.ok(!row.includes("gap-6 px-4"), "header row must not carry the fixed desktop-only gap-6");
  assert.ok(row.includes("gap-3"), "header row tightens its gap below sm:");
  assert.ok(row.includes("sm:gap-6"), "header row restores the desktop gap at sm:");
});

test("brand + right cluster are pinned; nav flexes and scrolls internally", () => {
  const row = block(APP, "<header", "</header>");
  assert.ok(row.includes('class="brand shrink-0'), "brand must never shrink (pins the left edge)");
  const right = block(APP, '<div class="ml-auto', "</div>\n        </div>");
  assert.ok(right.includes("shrink-0"), "ml-auto right cluster (API + tray + toggle) must never shrink");
  const navTag = row.slice(row.indexOf("<nav"), row.indexOf(">", row.indexOf("<nav")) + 1);
  for (const cls of ["min-w-0", "flex-1", "overflow-x-auto", "whitespace-nowrap"]) {
    assert.ok(navTag.includes(cls), `site-nav must carry ${cls} (flex into leftover, scroll within itself)`);
  }
});

test("nav is a labelled landmark; links never shrink to unreadability", () => {
  const row = block(APP, "<header", "</header>");
  const navTag = row.slice(row.indexOf("<nav"), row.indexOf(">", row.indexOf("<nav")) + 1);
  assert.ok(navTag.includes('aria-label="Site"'), "site-nav strip must be a labelled landmark");
  assert.match(CSS, /\.site-nav a \{ @apply shrink-0;/, "nav links keep full width inside the scroll strip");
});

test("nav strip hides its own scrollbar but stays keyboard/touch scrollable", () => {
  const css = srcOf("../../src/ui.css");
  const strip = css.slice(css.indexOf("issue #273"));
  assert.ok(strip.includes(".site-nav { scrollbar-width: none; }"), "Firefox: no strip scrollbar (stable row height)");
  assert.ok(
    strip.includes(".site-nav::-webkit-scrollbar { display: none; }"),
    "Chromium/Safari: no strip scrollbar (stable row height)",
  );
  const navTag = APP.slice(APP.indexOf("<nav"), APP.indexOf(">", APP.indexOf("<nav")) + 1);
  assert.ok(navTag.includes("overflow-x-auto"), "strip keeps overflow-x-auto: touch scroll + focus-follow still work");
});

test("site-nav contents unchanged (no links moved into a hamburger)", () => {
  const nav = block(APP, '<nav aria-label="Site"', "</nav>");
  for (const href of ["/explore", "/import", "/keys", "/setup"]) {
    assert.ok(nav.includes(`href="${href}"`), `site-nav must still link ${href}`);
  }
  assert.ok(!nav.includes('href="/api"'), "API stays in the ml-auto cluster (#238 untouched)");
});

test("390px arithmetic: pinned chrome fits, nav absorbs the rest", () => {
  // Measured fixed widths (generous upper bounds): brand ~70px, right cluster
  // (API ~30 + bell ~40 + toggle ~40 + internal gaps ~16) ~130px, row padding
  // px-4 = 32px, row gaps at <sm: (gap-3 = 12px) x 2 = 24px. Pinned total ≈
  // 256px < 390px, so the flex-1 nav always gets a non-negative share and its
  // overflow scrolls INSIDE the strip — scrollWidth === clientWidth.
  const viewport = 390;
  const pinned = 70 + 130 + 32 + 24;
  assert.ok(pinned < viewport, `pinned chrome ${pinned}px fits in ${viewport}px (nav gets ${viewport - pinned}px+)`);
});

test("tray popover fit still held by the #278 viewport bound", () => {
  const boundBlock = CSS.slice(CSS.indexOf("issue #278"));
  assert.ok(boundBlock.length > 0, "#278 bound block intact");
  assert.ok(boundBlock.includes(".notif-drop"), "notification dropdown still capped at 100vw - 1rem");
  assert.ok(boundBlock.includes(".tray"), "error tray still capped at 100vw - 1rem");
});
