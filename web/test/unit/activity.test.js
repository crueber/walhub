// web/test/unit/activity.test.js — last-active stamp for repo listings
// (issue #142): latestActivity extraction + ACTIVITY_TTL cache contract.
import { test } from "node:test";
import assert from "node:assert/strict";
import { ACTIVITY_TTL, latestActivity } from "../../src/lib/activity.js";

test("ACTIVITY_TTL matches the StarCount-style 30 s listing-row contract", () => {
  assert.equal(ACTIVITY_TTL, 30_000);
});

test("latestActivity prefers commit_date (when the commit landed)", () => {
  const page = {
    commits: [{ sha: "abc", commit_date: "2026-09-01T10:00:00Z", author_date: "2026-08-01T10:00:00Z" }],
  };
  assert.equal(latestActivity(page), "2026-09-01T10:00:00Z");
});

test("latestActivity falls back to author_date when commit_date is missing", () => {
  const page = { commits: [{ sha: "abc", author_date: "2026-08-01T10:00:00Z" }] };
  assert.equal(latestActivity(page), "2026-08-01T10:00:00Z");
});

test("latestActivity yields null for empty repos and missing pages", () => {
  assert.equal(latestActivity({ commits: [] }), null);
  assert.equal(latestActivity({}), null);
  assert.equal(latestActivity(null), null);
  assert.equal(latestActivity(undefined), null);
  assert.equal(latestActivity({ commits: [{}] }), null);
});
