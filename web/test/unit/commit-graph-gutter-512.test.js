// web/test/unit/commit-graph-gutter-512.test.js — Forgejo #512: the
// commit-graph lane gutter must read as one continuous line while the graph
// is ON. Each row draws only its own rail segment, so consecutive row boxes
// must abut edge-to-edge: rows margin-free (padding-only separation) and no
// per-row divider touching the rail column. Graph OFF keeps its dividers;
// the ≤480px fallback is unchanged. Source pins, mirroring
// commit-graph-506.test.js (no Solid, no DOM).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const COMMITS = srcOf("../../src/pages/Commits.jsx");
const CSS = srcOf("../../src/ui.css");

test("#512: .commit-row carries no margins — separation is padding only", () => {
  const rowClass = COMMITS.match(/class="commit-row ([^"]*)"/)?.[1] ?? "";
  assert.ok(rowClass.includes("py-2"), "row spacing comes from py-2 cell padding");
  const marginUtil = rowClass.split(/\s+/).find((t) => /^(m[xytrbl]?|space-[xy])-/.test(t));
  assert.equal(marginUtil, undefined, `no margin utility on the row (found: ${marginUtil})`);
  assert.ok(
    /\.commit-row[^{]*\{[^}]*margin:\s*0/.test(CSS),
    "ui.css pins the margin-free convention against future mb-* reaches",
  );
});

test("#512: divide-y applies only while the graph is OFF", () => {
  for (const util of ["divide-y", "divide-zinc-100", "dark:divide-zinc-800/60"]) {
    assert.ok(COMMITS.includes(`"${util}"`), `divider utility still present: ${util}`);
  }
  assert.ok(
    COMMITS.includes('"divide-y": !graphOn()'),
    "divide-y is gated on the graph being OFF",
  );
  assert.ok(
    COMMITS.includes('"graph-on": graphOn()'),
    "graph-on marker kept (#506 feature preserved)",
  );
  const listClass = COMMITS.match(/class="commit-list ([^"]*)"/)?.[1] ?? "";
  assert.ok(!/(^|\s)divide-/.test(listClass), "base list class carries no unconditional divider");
});

test("#512: graph-on separation never touches the rail column", () => {
  assert.ok(
    CSS.includes(".graph-on > .commit-row + .commit-row .commit-main"),
    "graph-on divider rides .commit-main (right of the rail)",
  );
  const sep = CSS.match(/\.graph-on > \.commit-row \+ \.commit-row \.commit-main \{([^}]*)\}/)?.[1] ?? "";
  assert.ok(/box-shadow:\s*inset 0 1px/.test(sep), "separation is an inset top edge, not a row border");
  assert.ok(
    CSS.includes(".dark .graph-on > .commit-row + .commit-row .commit-main"),
    "dark theme carries its own inset edge",
  );
  // The rail column itself stays border/margin-free: no rule may put a
  // border, margin, or shadow on the graph-on row box or its rail cell.
  for (const sel of [".graph-on .commit-row {", ".graph-on .commit-rail {"]) {
    const body = CSS.slice(CSS.indexOf(sel)).match(/\{([^}]*)\}/)?.[1] ?? "";
    assert.ok(!/border|margin|box-shadow/.test(body), `${sel} stays border/margin-free`);
  }
  assert.ok(!CSS.includes(".graph-on .commit-row + .commit-row {"), "no bare inter-row rule on the row box");
});

test("#512: empty-row placeholder follows the same margin-free standard", () => {
  assert.ok(COMMITS.includes("commit-empty"), "empty placeholder carries the commit-empty class");
  assert.ok(
    /\.commit-empty[^{]*\{[^}]*margin:\s*0/.test(CSS) || CSS.includes(".commit-row, .commit-empty { margin: 0; }"),
    "placeholder is margin-free like the rows",
  );
});

test("#512: mobile fallback unchanged (rail hidden, 3-column row)", () => {
  assert.ok(CSS.includes("@media (max-width: 480px)"), "phone collapse defined");
  const collapse = CSS.slice(CSS.indexOf("@media (max-width: 480px)"));
  assert.ok(collapse.includes(".commit-rail") && collapse.includes("display: none"), "rail hides on phones");
  assert.ok(
    collapse.includes("minmax(0, 1fr) auto auto"),
    "row falls back to its 3-column shape on phones",
  );
});
