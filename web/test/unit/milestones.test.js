import { test } from "node:test";
import assert from "node:assert/strict";

import { milestoneTitle, milestonePatch } from "../../src/lib/milestones.js";

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
