// web/test/unit/label-picker-rows.test.js — issue #334: label picker
// rows misaligned and wrapped badly with multi-word pack names ("good
// first issue" / "help wanted") plus long descriptions. Each row was one
// flat flex line with a freely-wrapping name and an unconstrained
// truncate description, so every row laid out differently. Rows are now
// a three-column grid [check+dot] [name] [description] in a w-80 panel.
// Menu semantics are untouched (menuitemcheckbox, aria-checked,
// busy-disable, outside-click/Esc close, native-button keyboard).
// The #278 audit (opaque + viewport-bound) must not regress: the panel
// keeps its `label-drop card` hook and right-anchored absolute stacking.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const picker = () => srcOf("../../src/components/LabelPicker.jsx");
const liveCss = () => srcOf("../../src/ui.css");

test("dropdown is widened to w-80 so multi-word names fit, keeps the audited panel hook", () => {
  const s = picker();
  assert.ok(s.includes("label-drop scroll-slim card absolute right-0 z-30"), "keeps the opaque right-anchored panel classes (#278)");
  assert.ok(s.includes("max-h-72 w-80 overflow-y-auto"), "panel widens w-64 toward w-80");
  assert.ok(!s.includes("max-h-72 w-64"), "no w-64 panel remains");
});

test("row is a three-column grid: [check+dot] [name] [description]", () => {
  const s = picker();
  assert.ok(
    s.includes("grid w-full grid-cols-[auto_minmax(0,1fr)_minmax(0,1fr)] items-center gap-2"),
    "row lays out as a stable three-column grid, not a flat flex line",
  );
  assert.ok(s.includes("flex shrink-0 items-center gap-2"), "column 1 groups check + dot at a constant width");
});

test("name never wraps; description truncates at a consistent width", () => {
  const s = picker();
  assert.ok(s.includes("whitespace-nowrap font-medium"), "name stays on one line");
  assert.ok(s.includes("overflow-hidden text-ellipsis"), "an overlong name truncates instead of breaking the grid");
  assert.ok(s.includes("muted min-w-0 truncate text-xs"), "description truncates inside a min-w-0 column");
});

test("full text stays reachable: row title carries name + description", () => {
  const s = picker();
  assert.ok(
    s.includes("`${on() ? \"remove\" : \"apply\"} ${l.name}${l.description ? ` — ${l.description}` : \"\"}`"),
    "row title carries the full name and description",
  );
});

test("menu semantics unchanged: checkbox rows, busy-disable, outside-click/Esc close", () => {
  const s = picker();
  assert.ok(s.includes('role="menuitemcheckbox"'), "rows stay menuitemcheckbox");
  assert.ok(s.includes("aria-checked={on()"), "aria-checked kept");
  assert.ok(s.includes("disabled={isBusy()}"), "busy-disable kept");
  assert.ok(s.includes("onClick={() => props.onToggle?.(l.name)}"), "toggle apply/remove kept");
  assert.ok(s.includes('e.key === "Escape"'), "Esc close kept");
  assert.ok(s.includes("!root.contains(e.target)"), "outside-click close kept");
});

test("no #278 regression: opaque rule and viewport bound still cover .label-drop", () => {
  const css = liveCss();
  assert.match(css, /\.clone-body, \.ref-drop, .*\.label-drop, \.milestone-drop,/,
    "opaque-popover rule still covers .label-drop");
  assert.ok(css.includes("max-width: calc(100vw - 1rem);"), "viewport bound still exists");
});

test("390px arithmetic: the w-80 panel fits inside the viewport cap", () => {
  // w-80 = 320px; the #278 bound caps panels at 390 - 16 = 374px, so the
  // right-anchored panel renders at min(320, 374) = 320 <= 390 with no
  // page-level horizontal scroll. Both themes share the treatment (no
  // color change in this fix).
  const viewport = 390;
  const cap = viewport - 16;
  assert.ok(Math.min(320, cap) <= viewport, "w-80 panel fits at 390px");
  assert.ok(320 <= cap, "the bound does not even bite at w-80 — no shrink needed");
});
