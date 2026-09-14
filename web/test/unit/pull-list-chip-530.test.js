import { test } from "node:test";
import assert from "node:assert/strict";

import { pullListChip } from "../../src/lib/pull-state.js";

// Forgejo #530: the pulls list renders the merged chip from the PROut.merged
// flag (merge stamps StateClosed too, so row.state alone cannot tell merged
// from plain-closed). Text follows the list lowercase convention.
test("list chip: unmerged rows mirror state", () => {
  assert.deepEqual(pullListChip({ state: "open", merged: false }), { text: "open", cls: "chip chip-open" });
  assert.deepEqual(pullListChip({ state: "closed", merged: false }), { text: "closed", cls: "chip chip-closed" });
});

test("list chip: merged wins over closed state", () => {
  assert.deepEqual(pullListChip({ state: "closed", merged: true }), { text: "merged", cls: "chip chip-merged" });
});

test("list chip: merged wins even when the card still reads open", () => {
  assert.deepEqual(pullListChip({ state: "open", merged: true }), { text: "merged", cls: "chip chip-merged" });
});

test("list chip: absent flag reads unmerged (old payloads keep rendering)", () => {
  assert.deepEqual(pullListChip({ state: "open" }), { text: "open", cls: "chip chip-open" });
  assert.deepEqual(pullListChip({ state: "closed" }), { text: "closed", cls: "chip chip-closed" });
});

test("list chip: loading (no row yet) reads open", () => {
  assert.deepEqual(pullListChip(undefined), { text: "open", cls: "chip chip-open" });
});
