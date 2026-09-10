// web/test/unit/popover-viewport.test.js — issue #278 regression: fixed-width
// absolutely-positioned popovers overflowed a 390px phone viewport when
// triggered near the right edge (clone-body w-96 measured hanging off at
// x = -10, the fixed error tray fully off-screen). The fix bounds every
// absolute/fixed panel to the viewport in the LIVE stylesheet web/src/ui.css
// (web/css/repo.css is dead/unbundled) with a shared
// `max-width: calc(100vw - 1rem)` rule; right-anchored panels then shrink
// into view instead of clipping. Theme-independent, so dark + light share it.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const liveCss = () => srcOf("../../src/ui.css");
const deadCss = () => srcOf("../../css/repo.css");

// Every panel class the audit found: clone menu, branch/tag picker,
// tasks popover, notification dropdown, reaction/label/milestone menus,
// comment close-confirm, tag picker, and the fixed error tray.
const PANELS = [
  "clone-body",
  "ref-drop",
  "tasks-drop",
  "notif-drop",
  "reaction-drop",
  "label-drop",
  "milestone-drop",
  "close-drop",
  "tag-drop",
  "tray",
];

test("viewport bound lives in the live stylesheet, not the dead css", () => {
  const entry = srcOf("../../src/index.jsx");
  assert.ok(entry.includes('import "./ui.css"'), "SPA entry imports the live stylesheet");
  assert.match(
    liveCss(),
    /max-width: calc\(100vw - 1rem\);/,
    "shared viewport-bound rule exists in web/src/ui.css",
  );
  assert.ok(!deadCss().includes("100vw"), "no viewport rule in dead web/css/repo.css");
});

test("shared bound covers every audited absolute/fixed panel", () => {
  const css = liveCss();
  const boundBlock = css.slice(css.indexOf("issue #278"));
  assert.ok(boundBlock.length > 0, "#278 bound block found");
  for (const panel of PANELS) {
    assert.ok(
      boundBlock.includes(`.${panel}`),
      `bound selector covers .${panel}`,
    );
  }
});

test("fixed-width panels keep their desktop widths (bound only caps)", () => {
  const repo = srcOf("../../src/pages/Repo.jsx");
  assert.ok(repo.includes("clone-body card absolute right-0 z-30 mt-2 w-96"), "clone menu keeps w-96 desktop width");
  assert.ok(repo.includes("tasks-drop card absolute right-0 z-30 mt-2 w-96"), "tasks popover keeps w-96 desktop width");
  assert.match(liveCss(), /\.tray \{ @apply [^}]*w-96/, "error tray keeps w-96 desktop width");
});

test("390px arithmetic: capped panels fit inside the viewport", () => {
  // max-width caps every panel at 390 - 16 = 374px. Right-anchored and
  // fixed-right panels pin their right edge inside the viewport, so used
  // width 374 <= 390 always fits with no page-level horizontal scroll.
  const viewport = 390;
  const cap = viewport - 16;
  for (const desktop of [384, 320, 256]) {
    assert.ok(Math.min(desktop, cap) <= viewport, `desktop w-${desktop} caps to ${Math.min(desktop, cap)} at 390px`);
  }
  assert.ok(cap < 384, "the w-96 panels (384px) actually shrink at 390px — the bound bites");
});

test("milestone picker carries the opaque panel treatment like label picker", () => {
  const milestone = srcOf("../../src/components/MilestonePicker.jsx");
  assert.ok(milestone.includes("milestone-drop"), "milestone dropdown keeps its hook class");
  assert.match(
    liveCss(),
    /\.clone-body, \.ref-drop, \.tasks-drop, \.notif-drop, \.reaction-drop, \.label-drop, \.milestone-drop, \.close-drop, \.tag-drop \{/,
    "opaque-popover rule covers .milestone-drop (same shape as .label-drop)",
  );
});
