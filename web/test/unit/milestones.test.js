import { test } from "node:test";
import assert from "node:assert/strict";

import { milestoneTitle, milestoneDisplay, milestonePatch, splitMilestones, milestoneFilterHref, milestoneTotal } from "../../src/lib/milestones.js";

const SET = [
  { id: "000001", title: "v1.1" },
  { id: "000002", title: "v2.0" },
];

test("milestoneTitle resolves ids to titles", () => {
  assert.equal(milestoneTitle(SET, "000001"), "v1.1");
  assert.equal(milestoneTitle(SET, "000002"), "v2.0");
});

test("milestoneTitle passes null through (no milestone)", () => {
  assert.equal(milestoneTitle(SET, null), null);
  assert.equal(milestoneTitle(SET, undefined), null);
});

test("milestoneTitle renders unknown ids bare (deleted milestone self-heal)", () => {
  assert.equal(milestoneTitle(SET, "0000ff"), "0000ff");
  assert.equal(milestoneTitle([], "000001"), "000001");
  assert.equal(milestoneTitle(undefined, "000001"), "000001");
});

test("milestoneDisplay waits for the side-cache (issue #259, no id flash)", () => {
  // Unloaded set (undefined): pending — the caller renders a
  // placeholder, never the bare id.
  assert.deepEqual(milestoneDisplay(undefined, "000001"), { pending: true, text: null });
  // Loaded set: titles, with the bare-id self-heal for deleted ids.
  assert.deepEqual(milestoneDisplay(SET, "000001"), { pending: false, text: "v1.1" });
  assert.deepEqual(milestoneDisplay(SET, "0000ff"), { pending: false, text: "0000ff", unknown: true });
  assert.deepEqual(milestoneDisplay([], "000001"), { pending: false, text: "000001", unknown: true });
  // Null stays null (no milestone).
  assert.equal(milestoneDisplay(SET, null), null);
  assert.equal(milestoneDisplay(undefined, null), null);
});
test("milestonePatch builds the set body", () => {
  assert.deepEqual(milestonePatch(null, "000001"), { milestone: "000001" });
  assert.deepEqual(milestonePatch("000002", "000001"), { milestone: "000001" });
});

test("milestonePatch clears with explicit null (never absent)", () => {
  const body = milestonePatch("000001", null);
  assert.deepEqual(body, { milestone: null });
  assert.ok("milestone" in body, "key must be present: absent means no change server-side");
});

test("milestonePatch returns null for no-ops (skip the round trip)", () => {
  assert.equal(milestonePatch(null, null), null);
  assert.equal(milestonePatch(undefined, null), null);
  assert.equal(milestonePatch("000001", "000001"), null);
});

test("splitMilestones separates open cards from collapsed closed rows (issue #314)", () => {
  const set = [
    { id: "000001", title: "v1.1", state: "open" },
    { id: "000002", title: "v1.0", state: "closed" },
    { id: "000003", title: "v1.2", state: "open" },
  ];
  const { open, closed } = splitMilestones(set);
  assert.deepEqual(open.map((m) => m.id), ["000001", "000003"]);
  assert.deepEqual(closed.map((m) => m.id), ["000002"]);
});

test("splitMilestones is null-safe and keeps unknown states visible", () => {
  assert.deepEqual(splitMilestones(undefined), { open: [], closed: [] });
  assert.deepEqual(splitMilestones(null), { open: [], closed: [] });
  assert.deepEqual(splitMilestones([]), { open: [], closed: [] });
  // Unknown state fails visible in the open section, never silently collapsed.
  const { open, closed } = splitMilestones([{ id: "000001", state: "archived" }]);
  assert.equal(open.length, 1);
  assert.equal(closed.length, 0);
});

test("milestoneFilterHref builds the shared issue-filter URL (issue #314)", () => {
  assert.equal(milestoneFilterHref("o/r", "000001"), "/o/r/issues?milestone=000001");
  // Same helper feeds the View button and the closed-row title link.
  assert.equal(milestoneFilterHref("o/r", "ab cd"), "/o/r/issues?milestone=ab%20cd");
});

test("milestoneTotal sums open + closed for the View label (issue #314)", () => {
  assert.equal(milestoneTotal({ open_issues: 2, closed_issues: 3 }), 5);
  assert.equal(milestoneTotal({ open_issues: 0, closed_issues: 0 }), 0);
  assert.equal(milestoneTotal({}), 0);
  assert.equal(milestoneTotal(null), 0);
});
