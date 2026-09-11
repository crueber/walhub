// web/test/unit/issue-invalidation.test.js — issue #318: milestone
// reassignment showed stale membership in milestone-filtered lists
// until reload. The thread PATCH's SSE frame only invalidated caches on
// pages still mounted (and the issue page's accept filter gated even
// that), while client-side navigation carries the global useData LRU
// across pages. Cover both halves of the fix headless:
//
// 1. collabKeys: an `issue` frame fans out to the thread key, the
//    `issues:{full}:*` prefix (which covers BOTH the Issues.jsx query
//    windows AND the milestones page's `issues:{full}:milestone:{id}`
//    entries), and the `milestones:{full}` counts.
// 2. invalidateIssueLists: the mutation-site reconcile refetches exactly
//    the repo's list windows + counts — other repos and thread keys
//    untouched — driven through the real prefetchData → invalidate path
//    (useData's effect cannot run under the solid-js server build).

import { test } from "node:test";
import assert from "node:assert/strict";

import { collabKeys } from "../../src/lib/collab.js";
import { prefetchData, invalidateIssueLists } from "../../src/lib/data.js";

const tick = (ms = 10) => new Promise((r) => setTimeout(r, ms));

test("issue frame covers thread key, list prefix, milestone counts, summary (#318, #319)", () => {
  const full = "acme/repo";
  assert.deepEqual(collabKeys(full, { kind: "issue", num: 7 }), [
    `issue:${full}:7`,
    `issues:${full}:*`,
    `milestones:${full}`,
    `repo:${full}`, // #319: the shell's shared summary (badge numerators)
  ]);
});

test("issue frame prefix covers both stale surfaces (#318)", () => {
  const full = "acme/repo";
  const [prefix] = collabKeys(full, { kind: "issue", num: 7 }).filter((k) => k.endsWith("*"));
  const base = prefix.slice(0, -1);
  // Issues.jsx list window key shape (Issues.jsx:39).
  assert.ok(`issues:${full}:${JSON.stringify({ state: "", milestone: "m1" })}`.startsWith(base));
  // Milestones.jsx MilestoneIssues entry key shape (Milestones.jsx:32).
  assert.ok(`issues:${full}:milestone:m1`.startsWith(base));
});

test("invalidateIssueLists refetches the repo lists + counts + summary only (#318, #319)", async () => {
  const full = "inv/o";
  let windowCalls = 0, memberCalls = 0, countsCalls = 0, otherCalls = 0, threadCalls = 0, summaryCalls = 0;
  prefetchData(`issues:${full}:{"state":""}`, () => Promise.resolve({ n: ++windowCalls }));
  prefetchData(`issues:${full}:milestone:m1`, () => Promise.resolve({ n: ++memberCalls }));
  prefetchData(`milestones:${full}`, () => Promise.resolve({ n: ++countsCalls }));
  prefetchData(`repo:${full}`, () => Promise.resolve({ n: ++summaryCalls }));
  prefetchData(`issues:inv/other:{"state":""}`, () => Promise.resolve({ n: ++otherCalls }));
  prefetchData(`issue:${full}:7`, () => Promise.resolve({ n: ++threadCalls }));
  await tick();
  assert.deepEqual([windowCalls, memberCalls, countsCalls, summaryCalls, otherCalls, threadCalls], [1, 1, 1, 1, 1, 1]);

  invalidateIssueLists(full);
  await tick();
  await tick();
  assert.equal(windowCalls, 2); // Issues.jsx query window refetched
  assert.equal(memberCalls, 2); // MilestoneIssues entry refetched
  assert.equal(countsCalls, 2); // milestone counts refetched
  assert.equal(summaryCalls, 2); // #319: shared summary (badge) refetched
  assert.equal(otherCalls, 1); // another repo's window untouched
  assert.equal(threadCalls, 1); // thread keys are the caller's own job
});

test("invalidateIssueLists on uncached keys is a silent no-op (#318)", () => {
  invalidateIssueLists("inv/never-mounted");
});
