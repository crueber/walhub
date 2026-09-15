// web/test/unit/mergebox-headline-592.test.js — Forgejo #592:
// "Able to be Merged" rendered twice: the sidebar MERGEABILITY value
// (Pull.jsx mergeabilityView via mergeabilityDisplay, kept) and the
// MergeBox state line (the disp() <p>, removed). Removal only — the
// sidebar headline, the amber "blocking merge: …" reasons line, the
// tooltip, mergeState, the enabled() gate, and strategy/buttons are
// untouched; the mergeabilityDisplay helper itself stays (sidebar use).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const PULL = srcOf("../../src/pages/Pull.jsx");
const MERGEBOX = srcOf("../../src/components/MergeBox.jsx");
const LIB = srcOf("../../src/lib/pull-state.js");

function mergeFormOf(src) {
  const start = src.indexOf('<form onSubmit={merge} aria-label="Merge">');
  assert.ok(start > 0, "merge form found");
  return src.slice(start, src.indexOf("</form>", start));
}

test("MERGE section has no status headline in any state", () => {
  const form = mergeFormOf(MERGEBOX);
  assert.ok(!form.includes("{disp().text}"), "no headline phrase in the form");
  assert.ok(!form.includes("{disp().sub}"), "no behind sub-line in the form");
  assert.ok(!form.includes("mergeabilityDisplay("), "no helper call in the form");
  assert.ok(!MERGEBOX.includes("disp()"), "the disp() memo is gone file-wide");
  assert.ok(!MERGEBOX.includes("Able to be Merged"), "no hardcoded headline copy");
  assert.ok(!MERGEBOX.includes("Checking mergeability"), "no hardcoded pending copy");
});

test("blocked reasons still render in the box", () => {
  assert.ok(MERGEBOX.includes("blocking merge: {blockers().join"), "amber reasons line kept");
  assert.ok(MERGEBOX.includes("update branch"), "update-branch remediation kept");
});

test("tooltip, mergeState, and the enabled() gate are untouched", () => {
  assert.ok(MERGEBOX.includes('if (props.mergeable?.state === "dirty") return "blocked"'), "mergeState dirty arm intact");
  assert.ok(
    MERGEBOX.includes('props.mergeable?.state === "clean" || props.mergeable?.state === "behind"'),
    "mergeState clean/behind arm intact",
  );
  assert.ok(MERGEBOX.includes('state() === "mergeable" && canMerge()'), "merge-button enable logic untouched");
  assert.ok(MERGEBOX.includes("blocked: ${b.join"), "disabled tooltip keeps machine wording");
  assert.ok(MERGEBOX.includes('type="submit"'), "merge submit button kept");
  assert.ok(mergeFormOf(MERGEBOX).includes("<select"), "strategy select kept");
});

test("helper retained with its single sidebar call site", () => {
  const defs = LIB.match(/function mergeabilityDisplay\(/g) ?? [];
  assert.equal(defs.length, 1, "exactly one mapping definition, no fork");
  assert.ok(PULL.includes("mergeabilityView(mergeable(),"), "sidebar still renders the headline");
  assert.ok(PULL.includes("mergeabilityDisplay(m?.state"), "sidebar maps through the shared helper");
});

test("removal only: no new deps, no new CSS", () => {
  const pkg = JSON.parse(srcOf("../../package.json"));
  for (const k of ["solid-js", "@solidjs/router", "marked", "dompurify"]) {
    assert.ok(pkg.dependencies?.[k], `runtime dep ${k} kept`);
  }
  assert.equal(Object.keys(pkg.dependencies ?? {}).length, 4, "no new runtime deps");
  assert.ok(!MERGEBOX.includes("style="), "MergeBox carries no inline style");
});
