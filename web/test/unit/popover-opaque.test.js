// web/test/unit/popover-opaque.test.js — Forgejo #405 regression guard.
//
// Root cause: the shared .card utility is translucent in dark mode
// (dark:bg-zinc-900/70 by design) and opacity for floating panels rode a
// hand-maintained enumeration of hook classes (.clone-body, .ref-drop, … in
// web/src/ui.css, from #37/#115/#278). IdentityMenu's dropdown is
// `card absolute …` with none of those classes, so page text bled through it
// in dark mode — the enumeration is exactly what let it slip.
//
// Guard shape (the issue's option 1, structural, PLUS a static test pinning
// it — the safest combination): opacity is the STRUCTURAL default —
// `.card.absolute, .card.fixed` renders opaque in web/src/ui.css — and this
// file fails the build if (a) that structural rule regresses, (b) the
// IdentityMenu panel stops being a floating .card, (c) any floating .card
// panel carries an explicit translucent/transparent bg utility (Tailwind
// utilities live in a later cascade layer than the components-layer
// structural rule, so such a utility would punch through it), or (d) a
// previously-audited popover disappears from the tree.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const path = require("node:path");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const liveCss = () => srcOf("../../src/ui.css");

// Every popover hook class the #37/#115/#278 audits accumulated. They stay in
// the markup as harmless markers (and keep their #278 viewport-bound rule)
// but carry no opacity role since #405 — the structural rule subsumes them.
const LEGACY_PANELS = [
  "clone-body",
  "ref-drop",
  "tasks-drop",
  "notif-drop",
  "reaction-drop",
  "label-drop",
  "milestone-drop",
  "close-drop",
  "tag-drop",
];

function jsxFiles(dirs) {
  const out = [];
  for (const dir of dirs) {
    const abs = new URL(dir, import.meta.url);
    for (const name of fs.readdirSync(abs)) {
      if (name.endsWith(".jsx")) out.push(path.join(abs.pathname, name));
    }
  }
  return out;
}

// Literal class="…" attributes (the tree's popover panels all use literals).
function classStrings(src) {
  const found = [];
  for (const m of src.matchAll(/class="([^"]*)"/g)) found.push(m[1]);
  return found;
}

const isFloatingCard = (cls) =>
  cls.split(/\s+/).includes("card") &&
  (cls.split(/\s+/).includes("absolute") || cls.split(/\s+/).includes("fixed"));

// A bg utility with an opacity modifier (bg-zinc-900/70) or bg-transparent
// would beat the components-layer structural rule (utilities layer wins),
// so no floating .card may carry one.
const TRANSLUCENT_BG = /bg-transparent|bg-\S+\/\d+/;

test("structural opaque rule exists in the live stylesheet", () => {
  const css = liveCss();
  assert.match(
    css,
    /\.card\.absolute, \.card\.fixed \{\s*\n?\s*@apply bg-white dark:bg-zinc-900;/,
    "opaque background is the structural default for every floating .card",
  );
  const rule = css.slice(css.indexOf(".card.absolute"));
  assert.ok(!rule.slice(0, 120).includes("/70"), "structural rule is fully opaque (no alpha modifier)");
  assert.ok(
    css.indexOf(".card.absolute") > css.indexOf(".card {"),
    "structural rule is ordered after the shared .card (same-layer tiebreak agrees with its higher specificity)",
  );
});

test("IdentityMenu dropdown (#405) is opaque via the structural rule", () => {
  const menu = srcOf("../../src/components/IdentityMenu.jsx");
  const panel = classStrings(menu).find((c) => c.includes("card") && c.includes("absolute"));
  assert.ok(panel, "IdentityMenu renders a floating .card panel");
  assert.ok(isFloatingCard(panel), "panel is covered by the structural .card.absolute rule");
  assert.ok(!TRANSLUCENT_BG.test(panel), "panel carries no translucent bg utility that could punch through the rule");
});

test("no floating .card panel ships a translucent bg utility", () => {
  const files = jsxFiles(["../../src/components/", "../../src/pages/"]);
  assert.ok(files.length > 0, "component/page scan is non-vacuous");
  const floating = [];
  for (const f of files) {
    for (const cls of classStrings(fs.readFileSync(f, "utf8"))) {
      if (isFloatingCard(cls)) floating.push(`${path.basename(f)}: ${cls}`);
    }
  }
  // IdentityMenu plus the nine legacy popovers (some hooks appear twice).
  assert.ok(floating.length >= 10, `floating .card scan finds every popover (found ${floating.length})`);
  assert.ok(floating.some((c) => c.startsWith("IdentityMenu.jsx")), "IdentityMenu is among the covered panels");
  for (const hit of floating) {
    assert.ok(!TRANSLUCENT_BG.test(hit), `floating panel stays opaque: ${hit}`);
  }
});

test("no existing popover regresses: legacy hooks still present and subsumed", () => {
  const files = jsxFiles(["../../src/components/", "../../src/pages/"]);
  const all = files.map((f) => fs.readFileSync(f, "utf8")).join("\n");
  for (const hook of LEGACY_PANELS) {
    assert.ok(all.includes(hook), `.${hook} still present in the tree`);
    const onFloatingCard = files.some((f) =>
      classStrings(fs.readFileSync(f, "utf8")).some((cls) => cls.includes(hook) && isFloatingCard(cls)),
    );
    assert.ok(onFloatingCard, `.${hook} still sits on a floating .card, hence subsumed by the structural rule`);
  }
});
