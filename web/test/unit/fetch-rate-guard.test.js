// web/test/unit/fetch-rate-guard.test.js — Forgejo #396: client fetch storm.
//
// With the settings page open, one tab issued ~5 GET/sec across ALL
// endpoints in bursts (~200 requests/endpoint/30 min) — far above the 5 s
// useData TTL cadence. The data layer idles quietly (no effect loop, no
// key churn — proven by the E9 harness), so the unbounded path is SSE
// invalidation: invalidate() ALWAYS starts a new generation (TTL bypass,
// #41 mutation rule), and the §4 coalescer only batched within one
// microtask — frames arriving in separate tasks each flushed a full
// refetch round per cached key (a 64-frame spread replay cost ~3
// GETs/frame; a sha key queued both explicitly and via its `*` prefix
// fetched TWICE per frame).
//
// The fix (data.js scheduleInvalidate flush): expand prefixes into a set
// first (one generation per entry per flush), then TTL-gate each key
// (fresh entries skip — sustained frame rates decay to TTL cadence;
// Infinity = immutable windows, never SSE-refetched). Mutation
// invalidate() stays eager (#41). These tests drive the real
// prefetchData → invalidateCollab path headless (useData's effect cannot
// run under the solid-js server build) with counting fetchers and assert
// fetches/key/window budgets. The 5.2 s sleep pins post-TTL liveness
// (timers fire late, never early — deterministic).

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

import {
  prefetchData, invalidate, invalidateCollab, initData,
} from "../../src/lib/data.js";

const tick = (ms = 10) => new Promise((r) => setTimeout(r, ms));

initData({ withNoStore: (fn) => fn() });

function counter() {
  let n = 0;
  const fn = async () => ({ n: ++n });
  return [fn, () => n];
}

// Seed a realistic multi-page cache under one repo full; returns getCount.
async function seedCache(full, sha) {
  const keys = [
    `repo:${full}`,
    `settings:${full}`,
    `access:${full}`,
    `perms:${full}`,
    `collaborators:${full}`,
    `checkindex:${full}:newest`,
    `checks:${full}:${sha}`,
    `statuses:${full}:${sha}`,
    `issue:${full}:7`,
    `issues:${full}:open`,
    `milestones:${full}`,
  ];
  const counts = new Map();
  for (const k of keys) {
    const [fn, get] = counter();
    counts.set(k, get);
    prefetchData(k, fn);
  }
  await tick(20);
  for (const k of keys) assert.equal(counts.get(k)(), 1, `seed fetch ${k}`);
  const deltas = () => Object.fromEntries([...counts].map(([k, get]) => [k, get() - 1]));
  return { keys, deltas };
}

test("sync frame burst coalesces to zero refetches on fresh keys (#396)", async () => {
  const full = "rate396/sync";
  const { deltas } = await seedCache(full, "a".repeat(40));
  for (let i = 0; i < 30; i++) {
    invalidateCollab(full, { kind: "check", sha: "a".repeat(40), context: `ci-${i}`, state: "pending" });
  }
  await tick(30);
  assert.deepEqual(deltas(), {
    [`repo:${full}`]: 0,
    [`settings:${full}`]: 0,
    [`access:${full}`]: 0,
    [`perms:${full}`]: 0,
    [`collaborators:${full}`]: 0,
    [`checkindex:${full}:newest`]: 0,
    [`checks:${full}:${"a".repeat(40)}`]: 0,
    [`statuses:${full}:${"a".repeat(40)}`]: 0,
    [`issue:${full}:7`]: 0,
    [`issues:${full}:open`]: 0,
    [`milestones:${full}`]: 0,
  });
});

test("spread frames respect TTL cadence — the #396 storm shape", async () => {
  const full = "rate396/spread";
  const sha = "b".repeat(40);
  const { deltas } = await seedCache(full, sha);
  // Live traffic: one frame per macrotask (microtask coalescing alone
  // cannot batch these — pre-fix this cost ~3 GETs/frame = 90 here).
  for (let i = 0; i < 30; i++) {
    invalidateCollab(full, { kind: "check", sha, context: `ci-${i}`, state: "pending" });
    await tick(0);
  }
  await tick(30);
  const d = deltas();
  for (const [k, v] of Object.entries(d)) assert.equal(v, 0, `spread refetch ${k}`);
});

test("mixed-kind spread burst stays silent on fresh keys (#396)", async () => {
  const full = "rate396/mixed";
  const { deltas } = await seedCache(full, "c".repeat(40));
  const frames = [
    { kind: "issue", num: 7 },
    { kind: "issue_event", num: 7 },
    { kind: "pull", num: 3 },
    { kind: "access" },
    { kind: "release", tag: "v1" },
  ];
  for (let r = 0; r < 6; r++) {
    for (const f of frames) invalidateCollab(full, f);
    await tick(0);
  }
  await tick(30);
  const d = deltas();
  for (const [k, v] of Object.entries(d)) assert.equal(v, 0, `mixed refetch ${k}`);
});

test("post-TTL frames still refetch exactly once per key (liveness, #396)", async () => {
  const full = "rate396/liveness";
  const sha = "d".repeat(40);
  const { deltas } = await seedCache(full, sha);
  // checks:* TTL is 5 s — sleep past it (timers fire late, never early).
  await new Promise((r) => setTimeout(r, 5200));
  invalidateCollab(full, { kind: "check", sha });
  await tick(50);
  const d = deltas();
  assert.equal(d[`checkindex:${full}:newest`], 1, "stale checkindex refetches once");
  assert.equal(d[`checks:${full}:${sha}`], 1, "stale checks window refetches once (prefix+explicit deduped)");
  assert.equal(d[`statuses:${full}:${sha}`], 1, "stale statuses refetch once");
  assert.equal(d[`repo:${full}`], 0, "unmapped keys untouched");
});

test("immutable events windows never SSE-refetch (append path owns liveness, #396)", async () => {
  const full = "rate396/immutable";
  const key = `events:${full}:7::50`;
  const [fn, get] = counter();
  prefetchData(key, fn);
  await tick(20);
  assert.equal(get(), 1);
  for (let i = 0; i < 10; i++) {
    invalidateCollab(full, { kind: "issue_event", num: 7, seq: i });
    await tick(0);
  }
  await tick(30);
  assert.equal(get(), 1, "immutable window refetched on frames");
});

test("mutation invalidate() stays eager on fresh keys (#41 preserved, #396)", async () => {
  const full = "rate396/mutation";
  const key = `repo:${full}`;
  const [fn, get] = counter();
  prefetchData(key, fn);
  await tick(20);
  assert.equal(get(), 1);
  invalidate(key);
  invalidate(key);
  await tick(30);
  assert.equal(get(), 3, "eager mutation refetch bypasses the SSE fresh-skip");
});

test("unknown frame kinds and uncached repos are silent no-ops (#396)", async () => {
  const full = "rate396/unknown";
  const [fn, get] = counter();
  prefetchData(`repo:${full}`, fn);
  await tick(20);
  assert.equal(get(), 1);
  invalidateCollab(full, { kind: "definitely-not-a-kind" });
  invalidateCollab("rate396/never-mounted", { kind: "check", sha: "e".repeat(40) });
  invalidateCollab("rate396/never-mounted", { kind: "issue", num: 1 });
  await tick(30);
  assert.equal(get(), 1, "unknown/uncached invalidations fetched");
});

test("tasks poller cadence stays sensible (5 s busy / 15 s idle, #396)", () => {
  const src = fs.readFileSync(new URL("../../src/pages/Repo.jsx", import.meta.url), "utf8");
  assert.match(src, /export const BUSY_MS = 5000;/, "busy cadence 5 s, not 1.5 s");
  assert.match(src, /export const IDLE_MS = 15000;/, "idle cadence untouched");
  assert.match(src, /timer = setTimeout\(tick,/, "chained setTimeout (a slow poll never stacks)");
});
