import { test } from "node:test";
import assert from "node:assert/strict";

import { sortByNumDesc } from "../../src/lib/sort.js";

test("sortByNumDesc renders newest-first regardless of input order", () => {
  // The #48 screenshot order (activity-first: #2 touched last) must read
  // #3, #2, #1 after the render sort.
  const rows = [
    { num: 2, title: "reaction emoji check" },
    { num: 3, title: "clean live-update probe" },
    { num: 1, title: "test" },
  ];
  assert.deepEqual(
    sortByNumDesc(rows).map((r) => r.num),
    [3, 2, 1]
  );
});

test("sortByNumDesc mixes open and closed by number, not state", () => {
  const rows = [
    { num: 1, state: "open" },
    { num: 2, state: "closed" },
    { num: 3, state: "open" },
  ];
  assert.deepEqual(
    sortByNumDesc(rows).map((r) => r.num),
    [3, 2, 1]
  );
});

test("sortByNumDesc is non-mutating and null-safe", () => {
  const rows = [{ num: 1 }, { num: 2 }];
  const sorted = sortByNumDesc(rows);
  assert.deepEqual(rows.map((r) => r.num), [1, 2]);
  assert.deepEqual(sorted.map((r) => r.num), [2, 1]);
  assert.deepEqual(sortByNumDesc(undefined), []);
  assert.deepEqual(sortByNumDesc(null), []);
});
