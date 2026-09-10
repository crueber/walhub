import { test } from "node:test";
import assert from "node:assert/strict";

import { milestoneTitle, milestoneDisplay, milestonePatch } from "../../src/lib/milestones.js";

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
