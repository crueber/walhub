// web/test/unit/thread-index-heading-572.test.js — Forgejo #572: the
// ThreadIndex panel heading renders just "Comments" — no parenthetical
// thread/staged counts in any state.
//
// Display-only change inside ThreadIndex (Pull.jsx): the pills, jump/flash,
// and the visibility gate (panel appears when threads or pending non-empty)
// are byte-identical; staged entries stay discoverable via their staged
// pills. Composition only — guideline §2 .card/.card-header by reference,
// no new pattern; no new deps (law 1); doc amendment in docs/go/12_web_ui.md
// in the same change (law 12).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");

const PULL = () => read("../../src/pages/Pull.jsx");
const DOC = () => read("../../../docs/go/12_web_ui.md");

const indexOf = (s) => s.slice(s.indexOf("function ThreadIndex(props)"), s.indexOf("function ThreadComments(props)"));

test("heading renders exactly Comments with no parenthetical", () => {
  const index = indexOf(PULL());
  assert.ok(index.includes('<h2 class="card-header">'), "card-header title idiom kept (#521/#557)");
  assert.match(index, /<h2 class="card-header">\s*Comments\s*<\/h2>/, "heading is exactly Comments — no count interpolation");
});

test("no parens in any state: threads only, threads+staged, staged only, gone-marked", () => {
  const index = indexOf(PULL());
  // The heading is static JSX, so every visibility state renders the same
  // text — but pin each state's ingredients still exist around it.
  assert.ok(!index.includes("threads ?? []).length}"), "no posted-thread count in the heading");
  assert.ok(!index.includes("pending ?? []).length}"), "no staged count in the heading");
  assert.ok(!index.includes("+ ${"), "no staged suffix interpolation remains");
  assert.ok(!index.includes("Comments ("), "no parenthetical after Comments");
  // All four states still have their content paths:
  assert.ok(index.includes("sortThreadsForIndex(props.threads"), "threads-only state: posted entries listed");
  assert.ok(index.includes("props.pending ?? []"), "threads+staged / staged-only states: staged pills listed");
  assert.ok(index.includes("is not in the current diff"), "gone-marked state: fallback pill kept");
});

test("visibility gate is unchanged and count-independent", () => {
  const index = indexOf(PULL());
  assert.ok(
    index.includes("<Show when={(props.threads ?? []).length > 0 || (props.pending ?? []).length > 0}>"),
    "panel appears when threads or pending non-empty — the #567 gate byte-identical",
  );
});

test("pills, jump, and flash are unchanged", () => {
  const s = PULL();
  const index = indexOf(s);
  assert.ok(index.includes("onClick={() => props.onJump(e.target)}"), "posted pill jump kept");
  assert.ok(index.includes("onClick={() => props.onJumpStaged(i())}"), "staged pill jump kept");
  assert.ok(index.includes("<span> · staged</span>"), "staged pills still marked staged");
  assert.ok(index.includes("anchorLabel(e.t.anchor)"), "posted pill labels kept");
  assert.ok(index.includes("anchorLabel(p.anchor)"), "staged pill labels kept");
  assert.match(s, /const jumpToStaged = \(i\) => \{\s*setFlashStaged\(i\);\s*document\.getElementById\(`staged-\$\{i\}`\)\?\.scrollIntoView\(\{ block: "center" \}\);/, "staged jump+flash idiom kept");
  assert.ok(s.includes("pending={getPending()} onJumpStaged={jumpToStaged}"), "page wiring kept");
});

test("no new styling surface, no new runtime dependencies (laws 1 + 11)", () => {
  const index = indexOf(PULL());
  assert.ok(!index.includes("style="), "no inline styles on the ThreadIndex nav");
  const pkg = JSON.parse(read("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime deps stay exactly solid-js + @solidjs/router + marked + dompurify",
  );
});

test("law-12: the web-UI decision lands in the same change", () => {
  assert.match(DOC(), /#572/, "12_web_ui.md carries the #572 decision");
});
