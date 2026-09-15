// web/test/unit/merge-strategy-select-547.test.js — Forgejo #547: the
// merge-strategy <select> on the PR page joins the system select idiom.
// Styling/markup only — strategy values, merge/update-branch flows,
// gates, and polling stay byte-identical.
//
// - MergeBox strategy select: the canonical `.input w-full` (the
//   same-page finish-review-verdict sibling, Pull.jsx:678) — bordered
//   input look + emerald focus ring in both themes, explicit width so
//   the control fills the 16rem sidebar row.
// - Label: the #479 shape (`label.grid.gap-1` + `text-sm font-medium`
//   span) — the undefined `field` class (zero CSS rules) is gone from
//   this form. The #545-noted `field` labels in PullNew.jsx stay: a
//   separate page, a separate issue.
// - Dropdown panel: `color-scheme: light` on `:root`, `dark` under
//   `.dark` (ui.css base layer) so the NATIVE popup panel and option
//   highlight follow the theme — deliberately no appearance-none
//   anywhere (the OwnerNameRow precedent: native arrow + keyboard stay).
// - Siblings: every other web/src select already rides `.input` —
//   pinned by sweep so none regresses; the legacy web/css/base.css:74
//   element rule is unbundled (index.jsx imports ui.css only) and stays
//   untouched.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const path = require("node:path");
const { fileURLToPath } = require("node:url");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const MERGE = srcOf("../../src/components/MergeBox.jsx");
const CSS = srcOf("../../src/ui.css");
const PULL = srcOf("../../src/pages/Pull.jsx");
const BASE = srcOf("../../css/base.css");
const SRC_DIR = path.dirname(fileURLToPath(new URL("../../src/components/MergeBox.jsx", import.meta.url)));

test("strategy select uses the .input idiom with an explicit width", () => {
  assert.ok(
    MERGE.includes('<select class="input w-full" value={getStrategy()}'),
    "strategy select rides .input w-full (the finish-review-verdict sibling)",
  );
  for (const v of ["merge", "squash", "rebase"]) {
    assert.ok(MERGE.includes(`<option value="${v}">${v}</option>`), `strategy option ${v} kept`);
  }
  assert.ok(
    MERGE.includes("onInput={(e) => setStrategy(e.target.value)}"),
    "strategy state binding kept",
  );
  assert.ok(MERGE.includes("disabled={getMerging()}"), "merging disable kept");
});

test("no bare selects or undefined label classes in the MergeBox form", () => {
  const bare = MERGE.match(/<select(?![^>]*class=)[^>]*>/g) ?? [];
  assert.deepEqual(bare, [], "every MergeBox <select> carries a class");
  assert.ok(!MERGE.includes('class="field"'), "label.field gone — the idiom is label.grid.gap-1");
  assert.ok(!MERGE.includes("appearance-none"), "native select chrome kept (no appearance-none)");
});

test("label follows the #479 sibling shape with room before the button row", () => {
  assert.ok(MERGE.includes('<label class="grid gap-1 mt-2">'), "label is a spaced block grid");
  assert.ok(MERGE.includes('<span class="text-sm font-medium">Strategy</span>'), "caption is the font-medium span");
  const labelIdx = MERGE.indexOf('<label class="grid gap-1 mt-2">');
  const rowIdx = MERGE.indexOf('<div class="mt-2 flex flex-wrap gap-2">');
  assert.ok(labelIdx > 0 && rowIdx > labelIdx, "merge / update-branch button row follows the label with mt-2 (no overlap)");
  assert.ok(MERGE.includes('class="btn btn-primary px-3 py-1"'), "merge button idiom kept");
  assert.ok(MERGE.includes('title="update the PR branch from base"'), "update-branch affordance kept");
});

test("color-scheme themes the native dropdown panel in both themes", () => {
  assert.match(CSS, /:root\s*\{\s*color-scheme:\s*light;\s*\}/, ":root declares light");
  assert.match(CSS, /\.dark\s*\{\s*color-scheme:\s*dark;\s*\}/, ".dark declares dark");
  const base = CSS.slice(0, CSS.indexOf("@layer components"));
  assert.ok(base.includes("color-scheme: light") && base.includes("color-scheme: dark"), "both declarations live in the base layer");
  assert.ok(
    CSS.includes(".input { @apply w-full rounded-md border border-zinc-300 bg-white"),
    ".input idiom intact (border, radius, light surface)",
  );
  assert.ok(
    CSS.includes("dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-100"),
    ".input dark surface intact",
  );
  assert.ok(CSS.includes("focus:ring-emerald-500"), ".input emerald focus ring intact");
  assert.ok(!/^select\s*[{,]/m.test(CSS), "no bare-select element rule added to the shipped stylesheet");
});

test("sibling selects unregressed: every web/src select rides .input", () => {
  const files = [];
  const walk = (dir) => {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.name.endsWith(".jsx")) files.push(p);
    }
  };
  walk(SRC_DIR);
  assert.ok(files.length > 10, `sweep covers the src tree (saw ${files.length} jsx files)`);
  const offenders = [];
  for (const f of files) {
    const raw = fs.readFileSync(f, "utf8");
    const code = raw
      .replace(/^\s*\/\/.*$/gm, "") // line comments (e.g. VisSelect.jsx prose "<select>")
      .replace(/\{\/\*[\s\S]*?\*\/\}/g, ""); // JSX block comments
    for (const m of code.match(/<select[\s>][^>]*>/g) ?? []) {
      if (!/class="[^"]*\binput\b/.test(m)) offenders.push(`${path.basename(f)}: ${m.slice(0, 60)}`);
    }
  }
  assert.deepEqual(offenders, [], "no bare <select> controls remain anywhere in web/src");
  // Named sibling pins (the issue's precedent list + the same-page verdict):
  assert.ok(srcOf("../../src/components/VisSelect.jsx").includes('<select\n      class="input"'), "VisSelect keeps bare .input");
  assert.ok(srcOf("../../src/pages/Wal.jsx").includes('<select class="input inline-block w-44"'), "Wal keeps its width");
  assert.ok(PULL.includes('<select id="finish-review-verdict" class="input w-full"'), "same-page verdict select intact");
  assert.ok(
    BASE.includes("input[type=\"text\"], input[type=\"search\"], input[type=\"number\"], input[type=\"password\"], select, textarea {"),
    "legacy web/css/base.css:74 element rule untouched (unbundled — index.jsx imports ui.css only)",
  );
});

test("no behavior change: merge machine, task attach, guards intact", () => {
  assert.ok(MERGE.includes("props.client.pulls.merge(props.num, { strategy: getStrategy() })"), "merge posts the strategy");
  assert.ok(MERGE.includes("props.client.pulls.updateBranch(props.num)"), "update-branch flow kept");
  assert.ok(MERGE.includes("const enabled = () => state() === \"mergeable\" && canMerge() && !getMerging()"), "enable gate kept");
  assert.ok(MERGE.includes("if (getMerging() || !enabled()) return;"), "double-submit guard kept");
  assert.ok(MERGE.includes('aria-label="Merge"'), "form accessible name kept");
  assert.ok(MERGE.includes("blocking merge: {blockers().join"), "blocker line kept");
});
