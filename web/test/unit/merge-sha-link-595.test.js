// web/test/unit/merge-sha-link-595.test.js — Forgejo #595:
// the merge commit is a child of base, so it never appears in the PR
// commits tab (base…head by documented design, 03_pull_requests.md:349).
// The MERGE box showed the SHA as dead plain text; it now links to the
// commit page (full SHA in href, 12-char text unchanged). Reasons line,
// tooltip, gate, and buttons are untouched.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const MERGEBOX = srcOf("../../src/components/MergeBox.jsx");
const PULL = srcOf("../../src/pages/Pull.jsx");
const CSS = srcOf("../../src/ui.css");

function mergedLineOf(src) {
  const start = src.indexOf("merged as");
  assert.ok(start > 0, "merged line found");
  return src.slice(start, src.indexOf("</p>", start));
}

test("merged SHA links to the commit page (full SHA href, short text)", () => {
  const line = mergedLineOf(MERGEBOX);
  assert.match(
    line,
    /<A[^>]*href=\{`\/\$\{props\.full\}\/commit\/\$\{props\.pr\?\.merge_commit_sha \?\? ""\}`\}/,
    "href is /<full>/commit/<full-sha>"
  );
  assert.ok(line.includes('{(props.pr?.merge_commit_sha ?? "").slice(0, 12)}'), "12-char text unchanged");
  assert.ok(line.includes("by {props.pr?.merged_by}"), "merger credit unchanged");
});

test("link uses the canonical affordance (no new CSS)", () => {
  const line = mergedLineOf(MERGEBOX);
  assert.ok(line.includes('class="link'), "shared .link rule (#568), visible both themes");
  assert.ok(CSS.includes(".link"), "shared rule still present");
  assert.ok(!MERGEBOX.includes("style="), "no inline styles added");
});

test("MergeBox receives the repo path; reasons/gate/buttons untouched", () => {
  assert.ok(PULL.includes("full={ctx.full}"), "call site passes full");
  assert.ok(MERGEBOX.includes("blocking merge: {blockers().join"), "amber reasons line kept");
  assert.ok(MERGEBOX.includes("mergeState("), "machine untouched");
  assert.ok(MERGEBOX.includes('aria-label="Merge"'), "merge form intact");
});

test("no new runtime dependencies (law 1)", () => {
  assert.ok(MERGEBOX.includes('from "@solidjs/router"'), "router import only");
  const pkg = srcOf("../../package.json");
  for (const dep of ["solid-js", "@solidjs/router", "marked", "dompurify"]) {
    assert.ok(pkg.includes(`"${dep}"`), `runtime dep ${dep} still declared`);
  }
});
