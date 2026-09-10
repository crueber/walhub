// web/test/unit/card-meta.test.js — issue #277 regression: the pull-list
// and review-card meta rows rendered their spans concatenated ("this is a
// title9 hours ago1 assets") because .card-meta was referenced in
// Pulls.jsx/Pull.jsx but had zero CSS rules anywhere. The fix is a real
// .card-meta rule (flex row, gap, wrap, muted, both themes) in the LIVE
// stylesheet web/src/ui.css — web/css/repo.css is dead/unbundled — plus ·
// separators in the markup, consistent with the repo-header meta language.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const pulls = () => srcOf("../../src/pages/Pulls.jsx");
const pull = () => srcOf("../../src/pages/Pull.jsx");
const liveCss = () => srcOf("../../src/ui.css");
const deadCss = () => srcOf("../../css/repo.css");

test("shipped stylesheet is src/ui.css: .card-meta rule lives there, not in dead css", () => {
  const entry = srcOf("../../src/index.jsx");
  assert.ok(entry.includes('import "./ui.css"'), "SPA entry imports the live stylesheet");
  assert.match(liveCss(), /\.card-meta \{ @apply [^}]*flex/, ".card-meta rule exists in the live stylesheet");
  assert.ok(!deadCss().includes(".card-meta"), "no .card-meta rule in dead web/css/repo.css");
});

test(".card-meta separates spans on wrap: flex row, gap, wrap, muted both themes", () => {
  const m = liveCss().match(/\.card-meta \{ @apply ([^}]*)\}/);
  assert.ok(m, ".card-meta @apply rule found");
  const rule = m[1];
  for (const token of ["flex", "flex-wrap", "gap-x-2", "text-xs", "text-zinc-500", "dark:text-zinc-400"]) {
    assert.ok(rule.includes(token), `rule carries ${token}`);
  }
});

test("Pulls list card-meta separates ref-range, author, date with · separators", () => {
  const s = pulls();
  assert.ok(s.includes('class="card-meta"'), "Pulls list keeps the card-meta hook");
  assert.ok(s.includes('{" · "}'), "text items joined with · separators");
  const meta = s.slice(s.indexOf('class="card-meta"'));
  let at = -1;
  for (const t of ["base_ref", "·", "pr.author", "·", "updated_at"]) {
    const i = meta.indexOf(t, at + 1);
    assert.ok(i > at, `${t} follows in separator order ref-range · author · date`);
    at = i;
  }
});

test("Pull ReviewsList card-meta separates author, badge, date with · separators", () => {
  const s = pull();
  assert.ok(s.includes('class="card-meta"'), "ReviewsList keeps the card-meta hook");
  const meta = s.slice(s.indexOf('class="card-meta"'));
  let at = -1;
  for (const t of ["rv.by", "·", "decisionBadge", "·", "rv.at"]) {
    const i = meta.indexOf(t, at + 1);
    assert.ok(i > at, `${t} follows in separator order author · badge · date`);
    at = i;
  }
});
