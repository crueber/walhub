// web/test/unit/issues-filter-order.test.js — Forgejo #415: the issues-list
// filter bar presents its fields in the order State, Assignee, Labels,
// Milestone, Refresh. Layout-only: same query params, same endpoints, same
// setFilter reset behavior; the grid column template is untouched.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const page = () => srcOf("../../src/pages/Issues.jsx");

test("filter bar field order is State, Assignee, Labels, Milestone, Refresh", () => {
  const s = page();
  const form = s.slice(s.indexOf('aria-label="issue filters"'));
  let at = -1;
  for (const t of ["\n          State", "\n          Assignee", "\n          Labels", "\n          Milestone", "Refresh"]) {
    const i = form.indexOf(t, at + 1);
    assert.ok(i > at, `${JSON.stringify(t.trim())} follows in filter-bar order`);
    at = i;
  }
});

test("grid column template intact at 2-col / 4-col / wide breakpoints", () => {
  const s = page();
  assert.ok(
    s.includes("grid grid-cols-2 gap-x-3 gap-y-2 p-3 sm:grid-cols-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_minmax(0,1fr)_minmax(0,1fr)_auto]"),
    "column template unchanged: 2-up phones, 4-col mid, 4+action wide",
  );
  assert.ok(
    s.includes("col-span-2 flex items-end sm:col-span-4 lg:col-span-1"),
    "Refresh cell still spans the row below mid widths, one column on wide",
  );
});

test("filter behavior unchanged: same params, same handlers, same endpoints", () => {
  const s = page();
  for (const k of ['setFilter("state"', 'setFilter("labels"', 'setFilter("assignee"', 'setFilter("milestone"']) {
    assert.ok(s.includes(k), `${k} handler still wired`);
  }
  assert.ok(s.includes("ctx.repoClient.issues.list(query())"), "same list endpoint");
  assert.ok(
    s.includes("labels: search.labels") && s.includes("assignee: search.assignee") && s.includes("milestone: search.milestone"),
    "same query params ride the request",
  );
});
