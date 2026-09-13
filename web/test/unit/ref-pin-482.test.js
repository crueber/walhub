// web/test/unit/ref-pin-482.test.js — Forgejo #482 headless cover: the
// pinned default-branch derivation lives in lib/ref-pill.js (pure,
// dependency-free) so `node --test` exercises it without SolidJS.
import { test } from "node:test";
import assert from "node:assert/strict";
import { pinnedDefault, dedupeRefs } from "../../src/lib/ref-pill.js";

const MAIN = { name: "refs/heads/main", sha: "1ca4c5b4" + "0".repeat(32) };

test("pinnedDefault pins the summary head on branches", () => {
  assert.deepEqual(pinnedDefault(MAIN, "branches"), MAIN);
});

test("pinnedDefault pins a non-main default too", () => {
  const head = { name: "refs/heads/trunk", sha: "abc123" };
  assert.deepEqual(pinnedDefault(head, "branches"), head);
});

test("pinnedDefault never pins tags", () => {
  assert.equal(pinnedDefault(MAIN, "tags"), null);
});

test("pinnedDefault pins nothing without a heads/ name", () => {
  assert.equal(pinnedDefault(null, "branches"), null);
  assert.equal(pinnedDefault(undefined, "branches"), null);
  assert.equal(pinnedDefault({ name: null, sha: "abc" }, "branches"), null);
  assert.equal(pinnedDefault({ name: "abc123", sha: "abc123" }, "branches"), null);
  assert.equal(pinnedDefault({ name: "refs/tags/v1", sha: "abc" }, "branches"), null);
});

test("pinnedDefault pins nothing without a sha", () => {
  assert.equal(pinnedDefault({ name: "refs/heads/main" }, "branches"), null);
  assert.equal(pinnedDefault({ name: "refs/heads/main", sha: "" }, "branches"), null);
});

test("dedupeRefs drops the streamed duplicate of the pin", () => {
  const streamed = [
    { name: "refs/heads/feat/a", sha: "1" },
    { name: "refs/heads/main", sha: "2" },
    { name: "refs/heads/fix/b", sha: "3" },
  ];
  const out = dedupeRefs(MAIN, streamed);
  assert.deepEqual(out.map((r) => r.name), ["refs/heads/feat/a", "refs/heads/fix/b"]);
});

test("dedupeRefs keeps every row when the page misses the default", () => {
  const streamed = [{ name: "refs/heads/fix/a", sha: "1" }];
  assert.deepEqual(dedupeRefs(MAIN, streamed), streamed);
});

test("dedupeRefs with no pin returns the list untouched", () => {
  const streamed = [{ name: "refs/heads/main", sha: "1" }];
  assert.deepEqual(dedupeRefs(null, streamed), streamed);
  assert.deepEqual(dedupeRefs(null, []), []);
  assert.deepEqual(dedupeRefs(MAIN, []), []);
});
