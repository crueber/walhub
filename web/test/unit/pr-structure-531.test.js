// web/test/unit/pr-structure-531.test.js — Forgejo #531: the PR surfaces
// converge on the issues-surface idioms (structure only — every gate,
// fetch, and cache key is untouched).
//
// - PR page right column: ONE card divide-y sectioned panel in the
//   Issue.jsx:549 idiom (supersedes the #521 stacked sibling cards).
//   Mergeability is a section VALUE under an uppercase micro-label (the
//   Pull.jsx mergeabilityView display phrase + the base/head branch lines,
//   pending-branch warning, and commits/files links stay as secondary
//   value detail).
//   Review summary composes first; Reviewers / Checks / Merge follow as
//   sections with the issues-sidebar section anatomy (label + value +
//   "none" fallbacks).
// - PR list: flat border-t divider rows in one container (the
//   Issues.jsx:351–358 idiom) — title link, state chip, refs inline,
//   right meta ml-auto, truncation safety min-w-0/max-w-full.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const PULL = srcOf("../../src/pages/Pull.jsx");
const PULLS = srcOf("../../src/pages/Pulls.jsx");
const MERGEBOX = srcOf("../../src/components/MergeBox.jsx");

function asideOf(src) {
  const start = src.indexOf('<aside aria-label="Details"');
  assert.ok(start > 0, "aside found");
  return src.slice(start, src.indexOf("</aside>", start));
}

test("aside holds ONE card divide-y sectioned panel, no stacked sibling cards", () => {
  const aside = asideOf(PULL);
  assert.match(aside, /<section class="card divide-y divide-zinc-200 text-sm dark:divide-zinc-800" aria-label="Pull request metadata">/);
  assert.ok(!aside.includes('<div class="card"'), "no stacked .card blocks remain in the aside");
  assert.ok(!aside.includes("card-list"), "no card-list inside the aside");
});

test("no card-header headings remain in the sidebar", () => {
  const aside = asideOf(PULL);
  assert.ok(!aside.includes("card-header"), "sections use micro-labels, never card-header");
  assert.ok(!MERGEBOX.includes("<h2"), "MergeBox carries no heading element of its own");
  assert.ok(!MERGEBOX.includes('class="card"'), "MergeBox carries no .card wrapper");
});

test("sections follow the issues-sidebar anatomy in order: summary, mergeability, reviewers, checks, merge", () => {
  const aside = asideOf(PULL);
  const order = ["Review summary", "Mergeability", "Reviewers", "Checks", "MergeBox"];
  let at = -1;
  for (const t of order) {
    const i = aside.indexOf(t, at + 1);
    assert.ok(i > at, `${t} follows in section order`);
    at = i;
  }
  const micro = "text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400";
  assert.ok(aside.split(micro).length - 1 >= 5, "every section carries the uppercase micro-label");
});

test("mergeability is a value: micro-label + mergeableText line + kept sub-lines", () => {
  const aside = asideOf(PULL);
  assert.ok(aside.includes(">Mergeability</span>"), "Mergeability is a label span, not a heading");
  assert.ok(aside.includes("mergeabilityView(mergeable(),"), "value line renders the #588 shared display phrase");
  assert.ok(aside.includes("head_ref_ok"), "pending-branch warning stays in the section");
  assert.ok(aside.includes("pr()?.base?.ref") && aside.includes("pr()?.head?.ref"), "base/head refs stay as secondary detail");
  assert.ok(aside.includes("pr()?.fork?.repo"), "cross-repo fork line stays (#328)");
  assert.ok(aside.includes("/commits") && aside.includes("/files"), "commits/files links stay in the section");
});

test("sections keep none-as-value fallbacks (Issue.jsx:549 rationale)", () => {
  assert.ok(PULL.includes("no reviews yet"), "review summary none reads as the section value");
  assert.ok(PULL.includes("none requested"), "reviewers none reads as the section value");
  assert.ok(PULL.includes("<ZeroChecksBlock"), "checks empty state stays in the section");
  assert.ok(PULL.includes("requesting reviewers needs the write role"), "reviewer gate note stays a value, not a card");
});

test("checks section keeps pill + rows + required/blocking sub-lines, logic untouched", () => {
  const aside = asideOf(PULL);
  assert.ok(aside.includes("<CheckPill"), "pill rides the section header row");
  assert.ok(aside.includes("<ContextRows"), "per-context rows kept");
  assert.ok(aside.includes("requiredChecks()") && aside.includes("checksBlockers()"), "advisory sub-lines kept");
  assert.ok(PULL.includes("const checksKey = () => `checks:${ctx.full}:${head()}`"), "checks fetch key untouched");
  assert.ok(PULL.includes("ctx.repoClient.checks.combined(head())"), "combined fetch untouched");
});

test("merge section composes MergeBox with every gate prop intact", () => {
  const aside = asideOf(PULL);
  assert.ok(aside.includes(">Merge</span>"), "Merge is a label span, not a heading");
  for (const p of ["checksBlockers={checksBlockers}", "reviewDecision={() => summary()?.decision}", "role={role}", "canUpdate={canUpdateBranch}"]) {
    assert.ok(aside.includes(p), `MergeBox keeps ${p}`);
  }
  assert.ok(MERGEBOX.includes("{disp().text}"), "display phrase still renders as a value line (#588)");
  assert.ok(MERGEBOX.includes("merge pull request"), "merge affordance kept");
});

test("card-list ban extends to the PR review surface + Checks page (Forgejo #545)", () => {
  assert.ok(!PULL.includes('class="card-list"') && !PULL.includes("card-list"), "no card-list anywhere in Pull.jsx");
  assert.ok(!srcOf("../../src/pages/Checks.jsx").includes("card-list"), "no card-list on the Checks page either");
});

test("PR list rows are flat border-t dividers in one container, never boxed", () => {
  assert.ok(!PULLS.includes('class="card-list"'), "no card-list container");
  assert.ok(!PULLS.includes('<li class="card">'), "no per-PR boxes");
  assert.match(PULLS, /<li class="border-t border-zinc-200 py-3 first:border-t-0 first:pt-0 dark:border-zinc-800">/);
});

test("PR row anatomy mirrors Issues.jsx:359+: title, chip, refs inline, right meta", () => {
  const row = PULLS.slice(PULLS.indexOf("sortByNumDesc(getPage().pulls)"));
  let at = -1;
  for (const t of ["pr.num", "pullListChip(pr).cls", "pullListChip(pr).text", "pr.base_ref", "pr.head_ref", "ml-auto", "pr.author", "pr.updated_at"]) {
    const i = row.indexOf(t, at + 1);
    assert.ok(i > at, `${t} follows in row anatomy order`);
    at = i;
  }
  assert.ok(PULLS.includes("min-w-0 max-w-full truncate font-medium"), "title truncates with min-w-0/max-w-full safety");
  assert.ok(PULLS.includes("flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1"), "chips wrap in the truncating flex row");
  assert.ok(PULLS.includes("ml-auto shrink-0"), "right meta pins right via ml-auto");
});

test("no wire/API change: list chip still reads PROut.merged, no new fetch", () => {
  assert.match(PULLS, /import \{[^}]*pullListChip[^}]*\} from "\.\.\/lib\/pull-state\.js"/);
  assert.doesNotMatch(PULLS, /chip chip-\$\{pr\.state\}/);
  assert.match(PULLS, /ctx\.repoClient\.pulls\.list\(query\(\)\)/);
});
