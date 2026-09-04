import { test } from "node:test";
import assert from "node:assert/strict";

import { toggleLabel, labelColorMap, labelColor } from "../../src/lib/labels.js";

test("toggleLabel adds a missing label, sorted", () => {
  assert.deepEqual(toggleLabel(["ui", "bug"], "aaa"), ["aaa", "bug", "ui"]);
  assert.deepEqual(toggleLabel([], "bug"), ["bug"]);
  assert.deepEqual(toggleLabel(undefined, "bug"), ["bug"]);
});

test("toggleLabel removes case-insensitively, keeping stored spellings", () => {
  assert.deepEqual(toggleLabel(["Bug", "ui"], "bug"), ["ui"]);
  assert.deepEqual(toggleLabel(["bug"], "BUG"), []);
});

test("toggleLabel round-trips (add then remove is identity)", () => {
  const start = ["bug"];
  assert.deepEqual(toggleLabel(toggleLabel(start, "ui"), "ui"), start);
});

test("labelColorMap keys lowercase names; labelColor falls back to null", () => {
  const map = labelColorMap([
    { name: "Bug", color: "d73a4a" },
    { name: "ui", color: "0000ff" },
  ]);
  assert.equal(labelColor(map, "bug"), "d73a4a");
  assert.equal(labelColor(map, "BUG"), "d73a4a");
  assert.equal(labelColor(map, "ui"), "0000ff");
  // Deleted-after-application: bare string, never a broken swatch.
  assert.equal(labelColor(map, "gone"), null);
  assert.equal(labelColor(labelColorMap([]), "bug"), null);
  assert.equal(labelColor(labelColorMap(undefined), "bug"), null);
});
