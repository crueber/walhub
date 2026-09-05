// web/test/unit/owners.test.js — owners (/) page helpers (issue #117):
// newest-first ordering proxy + cap slicing.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  MAX_OWNERS,
  MAX_REPOS_PER_OWNER,
  newestFirst,
  pageSlice,
} from "../../src/lib/owners.js";

test("caps are sane documented defaults", () => {
  assert.equal(MAX_OWNERS, 50);
  assert.equal(MAX_REPOS_PER_OWNER, 10);
});

test("newestFirst reverses server (ascending) order without mutating", () => {
  const server = ["acme", "demo", "jane"];
  const out = newestFirst(server);
  assert.deepEqual(out, ["jane", "demo", "acme"]);
  assert.deepEqual(server, ["acme", "demo", "jane"]); // input untouched
  assert.deepEqual(newestFirst([]), []);
});

test("newestFirst treats non-array input as empty", () => {
  assert.deepEqual(newestFirst(undefined), []);
  assert.deepEqual(newestFirst(null), []);
  assert.deepEqual(newestFirst("acme"), []);
});

test("pageSlice splits shown/extra at the cap", () => {
  const names = ["a", "b", "c"];
  assert.deepEqual(pageSlice(names, 10), { shown: ["a", "b", "c"], extra: 0 });
  assert.deepEqual(pageSlice(names, 2), { shown: ["a", "b"], extra: 1 });
  assert.deepEqual(pageSlice(names, 3), { shown: ["a", "b", "c"], extra: 0 });
});

test("pageSlice defaults cover the owners-page composition", () => {
  const owners = Array.from({ length: 60 }, (_, i) => `o${i}`);
  const { shown, extra } = pageSlice(newestFirst(owners), MAX_OWNERS);
  assert.equal(shown.length, 50);
  assert.equal(extra, 10);
  assert.equal(shown[0], "o59"); // newest-first survives the cap
  const repos = Array.from({ length: 12 }, (_, i) => `r${i}`);
  const rp = pageSlice(newestFirst(repos), MAX_REPOS_PER_OWNER);
  assert.equal(rp.shown.length, 10);
  assert.equal(rp.extra, 2);
});

test("pageSlice non-array input behaves as an empty list", () => {
  assert.deepEqual(pageSlice(undefined, 10), { shown: [], extra: 0 });
  assert.deepEqual(pageSlice(null, 10), { shown: [], extra: 0 });
  assert.deepEqual(pageSlice("a", 10), { shown: [], extra: 0 });
});

test("pageSlice non-positive or non-finite limits show none, count all as extra", () => {
  for (const limit of [0, -1, NaN, Infinity, "x"]) {
    const { shown, extra } = pageSlice(["a", "b"], limit);
    assert.deepEqual(shown, []);
    assert.equal(extra, 2);
  }
  assert.equal(pageSlice(["a", "b", "c"], 1.9).shown.length, 1); // floors
});
