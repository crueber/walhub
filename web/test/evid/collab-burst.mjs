// web/test/evid/collab-burst.mjs — E9 measurement harness (Feature 08 §4
// storm coalescing + §6 cache-hit shape) on the REAL data-layer code path
// (useData + invalidateCollab + scheduleInvalidate from src/lib/data.js).
//
// SolidJS effects are SSR no-ops under plain node, so this harness needs
// the browser build:
//
//   node --conditions=browser web/test/evid/collab-burst.mjs
//
// (requires web/node_modules — `pnpm --dir web install` first).
// NOT part of `make test-web` (which stays install-free per 12 §5).

import assert from "node:assert/strict";
import { createRoot } from "solid-js";
import { useData, invalidateCollab } from "../../src/lib/data.js";

const tick = (ms = 60) => new Promise((r) => setTimeout(r, ms));
const full = "acme/burst";
const sha = "abc123";
const counts = { checks: 0, statuses: 0, index: 0 };

function seed() {
  useData(`checks:${full}:${sha}`, async () => {
    counts.checks++;
    return { sha };
  });
  useData(`statuses:${full}:${sha}`, async () => {
    counts.statuses++;
    return { sha };
  });
  useData(`checkindex:${full}:`, async () => {
    counts.index++;
    return { checks: [] };
  });
}

await new Promise((resolve) => {
  createRoot((dispose) => {
    seed();
    setTimeout(() => {
      dispose();
      resolve();
    }, 120);
  });
});
console.log("initial loads:", JSON.stringify(counts));
assert.deepEqual(counts, { checks: 1, statuses: 1, index: 1 });

// Warm revisit inside TTL: re-attach, expect zero refetches (cache-hit shape).
await new Promise((resolve) => {
  createRoot((dispose) => {
    seed();
    setTimeout(() => {
      dispose();
      resolve();
    }, 120);
  });
});
console.log("after warm revisit:", JSON.stringify(counts));
assert.deepEqual(counts, { checks: 1, statuses: 1, index: 1 });

// The burst: 30 check frames for one sha in a single tick (CI posting 30
// runs). Keys collect into a set and invalidate once per tick; the
// promise-cache single-flights per key. Naive per-frame invalidation
// would refetch 30× per key.
await new Promise((resolve) => {
  createRoot((dispose) => {
    seed();
    setTimeout(async () => {
      for (let i = 0; i < 30; i++) {
        invalidateCollab(full, { kind: "check", sha, context: `ci-${i}`, state: "pending" });
      }
      await tick(120);
      dispose();
      resolve();
    }, 120);
  });
});
console.log("after 30-frame burst:", JSON.stringify(counts));
assert.ok(counts.checks <= 3, `checks refetched ${counts.checks}x`);
assert.ok(counts.statuses <= 3, `statuses refetched ${counts.statuses}x`);
assert.ok(counts.index <= 3, `index refetched ${counts.index}x`);
console.log("E9 burst harness: OK (30 frames → ≤3 refetches per key)");
