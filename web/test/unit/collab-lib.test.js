import { test } from "node:test";
import assert from "node:assert/strict";

import { TTL, collabKeys, backoffMs } from "../../src/lib/collab.js";

test("TTL table covers every 08 §6 key with the specified window", () => {
  assert.deepEqual(
    {
      repo: TTL.repo,
      perms: TTL.perms,
      issues: TTL.issues,
      pulls: TTL.pulls,
      issue: TTL.issue,
      pull: TTL.pull,
      events: TTL.events,
      labels: TTL.labels,
      milestones: TTL.milestones,
      diff: TTL.diff,
      prcommits: TTL.prcommits,
      reviews: TTL.reviews,
      threads: TTL.threads,
      checks: TTL.checks,
      checkindex: TTL.checkindex,
      releases: TTL.releases,
      release: TTL.release,
      social: TTL.social,
      assignables: TTL.assignables,
      notifications: TTL.notifications,
    },
    {
      repo: 5_000,
      perms: 30_000,
      issues: 5_000,
      pulls: 5_000,
      issue: 5_000,
      pull: 5_000,
      events: Infinity,
      labels: 30_000,
      milestones: 30_000,
      diff: Infinity,
      prcommits: Infinity,
      reviews: 5_000,
      threads: 5_000,
      checks: 5_000,
      checkindex: 5_000,
      releases: 30_000,
      release: 60_000,
      social: 30_000,
      assignables: 300_000,
      notifications: 5_000,
    },
  );
});

test("collabKeys maps every 08 §4 kind to its cache keys", () => {
  const full = "acme/repo";
  assert.deepEqual(collabKeys(full, { kind: "issue", num: 3 }), [`issue:${full}:3`, `issues:${full}:*`]);
  assert.deepEqual(collabKeys(full, { kind: "issue_event", num: 3 }), [`issue:${full}:3`, `events:${full}:3:*`]);
  assert.deepEqual(collabKeys(full, { kind: "pull", num: 9 }), [`pull:${full}:9`, `pulls:${full}:*`, `pulldiff:${full}:9`]);
  assert.deepEqual(collabKeys(full, { kind: "review", num: 9 }), [`pull:${full}:9`, `reviews:${full}:9`]);
  assert.deepEqual(collabKeys(full, { kind: "thread", num: 9, thread_id: "t1" }), [
    `threads:${full}:9`,
    `thread:9:t1`,
  ]);
  assert.deepEqual(collabKeys(full, { kind: "thread", num: 9 }), [`threads:${full}:9`]);
  assert.deepEqual(collabKeys(full, { kind: "check", sha: "abc" }), [
    `checkindex:${full}:*`,
    `checks:${full}:*`,
    `checks:${full}:abc`,
    `statuses:${full}:abc`,
  ]);
  assert.deepEqual(collabKeys(full, { kind: "release", tag: "v1" }), [
    `releases:${full}:*`,
    `latest:${full}`,
    `release:${full}:v1`,
  ]);
  assert.deepEqual(collabKeys(full, { kind: "access" }), [
    `perms:${full}`,
    `access:${full}`,
    `collaborators:${full}`,
    `assignables:${full}`,
  ]);
});

test("collabKeys drops unknown kinds (forward compatible)", () => {
  assert.deepEqual(collabKeys("o/r", { kind: "bogus-future-kind", num: 1 }), []);
  assert.deepEqual(collabKeys("o/r", null), []);
  assert.deepEqual(collabKeys("o/r", {}), []);
});

test("backoffMs: 1 s start, doubling, 30 s cap (08 §4)", () => {
  assert.equal(backoffMs(0), 1_000);
  assert.equal(backoffMs(1), 2_000);
  assert.equal(backoffMs(2), 4_000);
  assert.equal(backoffMs(5), 30_000);
  assert.equal(backoffMs(10), 30_000);
  assert.equal(backoffMs(100), 30_000);
});
