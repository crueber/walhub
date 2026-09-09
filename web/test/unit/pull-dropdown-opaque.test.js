// web/test/unit/pull-dropdown-opaque.test.js — issue #236 regression: the
// head/base pickers on PullNew and the reviewer picker on Pull render their
// option list as an opaque panel. Both dropdowns missed the #115 popover
// sweep (their `ref-list`/`ref-item` classes have no rules in the shipped
// web/src/ui.css — web/css/repo.css is dead), so page text bled through.
// The fix wires the shared #115 opaque-popover classes (`ref-drop card`,
// same pattern as the Repo.jsx picker) onto the dropdown and gives the rows
// the shared hover-highlight utilities — no new CSS, both themes.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const pullNew = () => srcOf("../../src/pages/PullNew.jsx");
const pull = () => srcOf("../../src/pages/Pull.jsx");
const liveCss = () => srcOf("../../src/ui.css");

test("shipped stylesheet is src/ui.css: ref-drop carries the opaque override", () => {
  const src = srcOf("../../src/index.jsx");
  assert.ok(src.includes('import "./ui.css"'), "SPA entry imports the live stylesheet");
  assert.match(
    liveCss(),
    /\.clone-body, \.ref-drop,/,
    "shared opaque-popover rule covers .ref-drop (issues #37, #115)",
  );
});

test("PullNew head/base dropdown is an opaque panel (ref-drop card)", () => {
  const s = pullNew();
  assert.ok(
    s.includes('class="ref-list ref-drop card scroll-slim absolute z-10 mt-1 max-h-48 w-full overflow-y-auto p-1 shadow-lg"'),
    "dropdown wires the shared opaque panel classes, keeps scroll + stacking",
  );
  assert.ok(!s.match(/<ul class="ref-list absolute /), "no bare transparent ref-list remains");
});

test("PullNew option rows are readable with a hover highlight, both themes", () => {
  const s = pullNew();
  assert.ok(s.includes("ref-item flex w-full"), "row keeps the ref-item hook");
  assert.ok(s.includes("hover:bg-zinc-100"), "light-theme hover highlight");
  assert.ok(s.includes("dark:hover:bg-zinc-800"), "dark-theme hover highlight");
  assert.ok(s.includes("dark:text-zinc-200"), "dark-theme readable text");
});

test("Pull reviewer dropdown gets the same opaque treatment", () => {
  const s = pull();
  assert.ok(
    s.includes('class="ref-list ref-drop card scroll-slim absolute z-10 mt-1 max-h-48 w-full overflow-y-auto p-1 shadow-lg"'),
    "reviewer dropdown wires the shared opaque panel classes, keeps scroll + stacking",
  );
  assert.ok(!s.match(/<ul class="ref-list absolute /), "no bare transparent ref-list remains");
  assert.ok(s.includes("ref-item flex w-full"), "reviewer row keeps the ref-item hook");
  assert.ok(s.includes("hover:bg-zinc-100"), "light-theme hover highlight");
  assert.ok(s.includes("dark:hover:bg-zinc-800"), "dark-theme hover highlight");
  assert.ok(s.includes("dark:text-zinc-200"), "dark-theme readable text");
});

test("picker behavior unchanged: debounce, stream/suggest, pick-to-close", () => {
  const n = pullNew();
  assert.ok(n.includes("}, 150);"), "150 ms filter debounce kept");
  assert.ok(n.includes("props.repo.refStream("), "SSE ref stream kept");
  assert.ok(n.includes("props.onPick(r.name);"), "pick still reports the refname");
  const p = pull();
  assert.ok(p.includes("props.client.pulls.suggest("), "review-suggest kept");
  assert.ok(p.includes("}, 150);"), "150 ms suggest debounce kept");
});
