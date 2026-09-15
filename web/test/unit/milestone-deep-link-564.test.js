import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

// Forgejo #564: the "View N issues" deep link must land on the set the
// label promised (open + closed total), with the #416 milestone dropdown
// showing the milestone selected, surviving a refresh (all state rides
// the URL — no in-memory filter state).
//
// Round trip pinned here, stage by stage:
//   milestoneFilterHref → URL search params → Issues.jsx query
//   (resolveIssueState → issueListState) → wire semantics
//   ("" state omitted → server returns both) + #416 dropdown binding
//   (resolveMilestoneFilter).

import { milestoneFilterHref, milestoneTotal } from "../../src/lib/milestones.js";
import { resolveIssueState, issueListState } from "../../src/lib/issueState.js";
import { resolveMilestoneFilter } from "../../src/lib/issueFilters.js";

const SET = [
  { id: "000001", title: "v1.1" },
  { id: "000002", title: "v1.2" },
];

// The Issues.jsx query builder, mirrored verbatim (Issues.jsx:204-211):
// state rides through resolveIssueState → issueListState, every other
// filter param verbatim off the search params.
function issuesQuery(search) {
  return {
    state: issueListState(resolveIssueState(search.state)),
    milestone: search.milestone || "",
  };
}

test("deep link carries the state the count implies (#564 choice: state=all)", () => {
  // milestoneTotal sums open + closed (1 open + 1 closed → "View 2 issues").
  const m = { open_issues: 1, closed_issues: 1 };
  assert.equal(milestoneTotal(m), 2);
  const href = milestoneFilterHref("o/r", "000001");
  const url = new URL(href, "https://x.test");
  assert.equal(url.searchParams.get("milestone"), "000001");
  assert.equal(url.searchParams.get("state"), "all");
});

test("landing resolves to the unfiltered (both-state) wire query", () => {
  const url = new URL(milestoneFilterHref("o/r", "000001"), "https://x.test");
  const search = Object.fromEntries(url.searchParams.entries());
  const q = issuesQuery(search);
  // "all" maps to the omitted wire param; the SDK qs() skips "" values,
  // so the server sees no state filter and returns open + closed — the
  // promised 2, not the #323 open-only subset.
  assert.equal(q.state, "");
  assert.equal(q.milestone, "000001");
});

test("bare visits keep the #323 open-only default (only the explicit link opts into both)", () => {
  // No ?state= → resolveIssueState defaults to "open" → wire "open".
  assert.equal(issuesQuery({}).state, "open");
  assert.equal(issuesQuery({ milestone: "000001" }).state, "open");
});

test("#416 dropdown binds the deep-linked milestone on landing", () => {
  const url = new URL(milestoneFilterHref("o/r", "000001"), "https://x.test");
  const bound = resolveMilestoneFilter(url.searchParams.get("milestone"), SET);
  assert.deepEqual(bound, { value: "000001", unknown: false, pending: false });
});

test("deep link survives refresh (all filter state rides the URL)", () => {
  const href = milestoneFilterHref("o/r", "000001");
  // Refresh = re-parse the same URL: identical query, identical binding.
  for (const landing of [href, href]) {
    const url = new URL(landing, "https://x.test");
    const search = Object.fromEntries(url.searchParams.entries());
    assert.deepEqual(issuesQuery(search), { state: "", milestone: "000001" });
    assert.equal(resolveMilestoneFilter(search.milestone, SET).value, "000001");
  }
});

test("closed-row title link shares the same promise (one helper, both affordances)", () => {
  // Milestones.jsx closed rows reuse milestoneFilterHref for the title
  // link — closed milestones still land on the full (open + closed) set.
  assert.ok(milestoneFilterHref("o/r", "000002").includes("state=all"));
});

test("every in-app milestone link shares the helper (no hand-rolled half-promises)", () => {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");
  const src = (p) => readFileSync(path.join(root, p), "utf8");
  for (const page of ["src/pages/Milestones.jsx", "src/pages/Issues.jsx", "src/pages/Issue.jsx"]) {
    const s = src(page);
    assert.ok(s.includes("milestoneFilterHref"), `${page} routes milestone links through the shared helper`);
  }
  // No page hand-rolls ?milestone= hrefs anymore (the #564 half-promise).
  // (Matches href templates only — API-shape comments like
  // "GET …/issues?milestone=<id>" in Milestones.jsx:29 are fine.)
  assert.ok(!src("src/pages/Issue.jsx").includes("issues?milestone=${"), "thread sidebar uses the helper");
  assert.ok(!src("src/pages/Milestones.jsx").includes("issues?milestone=${"), "milestones page uses the helper");
  assert.ok(!src("src/pages/Issues.jsx").includes("issues?milestone=${"), "issues page uses the helper");
});
