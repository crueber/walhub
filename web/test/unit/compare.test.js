// web/test/unit/compare.test.js — branch-comparison helpers (issue #34):
// compareHistories / tipSubject / fmtBounded / toShortRef over
// commits()-shaped pages.
import { test } from "node:test";
import assert from "node:assert/strict";
import { PREVIEW_WINDOW, compareHistories, tipSubject, fmtBounded, toShortRef } from "../../src/lib/compare.js";

const c = (sha, subject = sha) => ({ sha, subject });
const page = (shas, more = false) => ({ commits: shas.map((s) => c(s)), more });

test("head-only commits split at the merge-base approximation", () => {
  const r = compareHistories(page(["h3", "h2", "b1", "b0"]), page(["b1", "b0"]));
  assert.deepEqual(r.unique.map((x) => x.sha), ["h3", "h2"]);
  assert.equal(r.ahead, 2);
  assert.equal(r.behind, 0);
  assert.equal(r.truncatedHead, false);
  assert.equal(r.truncatedBase, false);
});

test("a base that moved ahead counts behind", () => {
  const r = compareHistories(page(["h2", "b1", "b0"]), page(["b2", "b1", "b0"]));
  assert.deepEqual(r.unique.map((x) => x.sha), ["h2"]);
  assert.equal(r.ahead, 1);
  assert.equal(r.behind, 1);
});

test("base at the head tip means ahead 0, behind 0", () => {
  const r = compareHistories(page(["b1", "b0"]), page(["b1", "b0"]));
  assert.deepEqual(r.unique, []);
  assert.equal(r.ahead, 0);
  assert.equal(r.behind, 0);
});

test("fully merged head reports no-op (empty unique)", () => {
  const r = compareHistories(page(["b2", "b1"]), page(["b2", "b1", "b0"]));
  assert.deepEqual(r.unique, []);
  assert.equal(r.behind, 0);
});

test("exhausted windows with no meeting point are exact, not truncated", () => {
  const r = compareHistories(page(["h1"]), page(["b0"]));
  assert.equal(r.ahead, 1);
  assert.equal(r.behind, 1);
  assert.equal(r.truncatedHead, false);
  assert.equal(r.truncatedBase, false);
});

test("a meeting point past a `more` window truncates with lower bounds", () => {
  const r = compareHistories(page(["h1"], true), page(["b0"], true));
  assert.equal(r.truncatedHead, true);
  assert.equal(r.truncatedBase, true);
});

test("missing input degrades to empty, never throws", () => {
  const r = compareHistories(undefined, undefined);
  assert.deepEqual(r, { unique: [], ahead: 0, behind: 0, truncatedHead: false, truncatedBase: false });
});

test("tipSubject takes the first line, trims, tolerates gaps", () => {
  assert.equal(tipSubject(c("x", "Add thing\n\nbody here")), "Add thing");
  assert.equal(tipSubject(c("x", "  padded  ")), "padded");
  assert.equal(tipSubject({ sha: "x", message: "via message\nsecond" }), "via message");
  assert.equal(tipSubject({ sha: "x" }), "");
  assert.equal(tipSubject(undefined), "");
});

test("fmtBounded marks truncated counts with +", () => {
  assert.equal(fmtBounded(3, false), "3");
  assert.equal(fmtBounded(3, true), "3+");
  assert.equal(fmtBounded(0, false), "0");
});

test("toShortRef strips refs/heads for the commits resolver", () => {
  assert.equal(toShortRef("refs/heads/topic"), "topic");
  assert.equal(toShortRef("main"), "main");
  assert.equal(toShortRef("refs/tags/v1"), "refs/tags/v1");
  assert.equal(toShortRef("0b87efd6ebbf"), "0b87efd6ebbf");
  assert.equal(toShortRef(""), "");
});

test("PREVIEW_WINDOW matches the n=100 the page fetches", () => {
  assert.equal(PREVIEW_WINDOW, 100);
});
