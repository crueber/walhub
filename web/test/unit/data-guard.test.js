// web/test/unit/data-guard.test.js — #41: the invalidate/ordering guard.
// Ref-dependent thread GETs are SWR (private, max-age=0,
// stale-while-revalidate=60), so after a mutation the explicit reload()
// refetch and an in-flight (SSE/coalesced) refetch race — and the browser
// can serve the pre-mutation body as 200 from disk cache. With no ordering
// guard the stale commit could land last and the timeline stayed stale.
//
// These tests drive the real ensureEntry → start → invalidate path headless
// via prefetchData (useData's effect cannot run under the solid-js server
// build) with deferred fetches. The error tray must stay empty throughout:
// no test triggers a current-generation error (that path arms reportError's
// 10 s fade timer), only dropped stale ones.

import { test } from "node:test";
import assert from "node:assert/strict";

import { prefetchData, invalidate, initData, trayErrors } from "../../src/lib/data.js";
import { NOT_MODIFIED } from "../../sdk/src/errors.js";

function deferred() {
  let resolve, reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

const tick = (ms = 10) => new Promise((r) => setTimeout(r, ms));

test("invalidate of an unknown key is a silent no-op", () => {
  invalidate("guard:missing-never-created");
});

test("a stale loser resolving last never overwrites fresh state", async () => {
  const d1 = deferred(), d2 = deferred();
  const bodies = [() => d1.promise, () => d2.promise];
  let n = 0;
  const get = prefetchData("guard:stale-last", () => bodies[n++]());
  invalidate("guard:stale-last"); // gen 2 starts over the in-flight gen 1
  d2.resolve("fresh");
  await tick();
  assert.equal(get(), "fresh");
  d1.resolve("stale");
  await tick();
  assert.equal(get(), "fresh");
  assert.deepEqual(trayErrors(), []);
});

test("a stale error is dropped silently and the fresh generation still wins", async () => {
  const d1 = deferred(), d2 = deferred();
  const bodies = [() => d1.promise, () => d2.promise];
  let n = 0;
  const get = prefetchData("guard:stale-error", () => bodies[n++]());
  invalidate("guard:stale-error");
  d1.reject(new Error("stale boom"));
  await tick();
  assert.equal(get(), undefined);
  assert.deepEqual(trayErrors(), []);
  d2.resolve("good");
  await tick();
  assert.equal(get(), "good");
  assert.deepEqual(trayErrors(), []);
});

test("invalidate starts a new fetch even over an in-flight one", async () => {
  let calls = 0;
  const gate = deferred();
  const get = prefetchData("guard:inflight", () => { calls++; return gate.promise; });
  assert.equal(calls, 1);
  invalidate("guard:inflight"); // the old code skipped here (entry.promise set)
  assert.equal(calls, 2);
  gate.resolve("v");
  await tick();
  await tick();
  assert.equal(get(), "v");
});

test("304 refetch keeps the current value silently (no tray, no blank)", async () => {
  const bodies = [() => Promise.resolve({ v: 1 }), () => Promise.resolve(NOT_MODIFIED)];
  let n = 0;
  const get = prefetchData("guard:304", () => bodies[n++]());
  await tick();
  assert.deepEqual(get(), { v: 1 });
  invalidate("guard:304");
  await tick();
  assert.deepEqual(get(), { v: 1 });
  assert.deepEqual(trayErrors(), []);
});

test("invalidate runs the refetch under the SDK HTTP-cache bypass when available", async () => {
  let armed = 0;
  initData({ withNoStore: (fn) => { armed++; return fn(); } });
  try {
    const gate = deferred();
    prefetchData("guard:bypass", () => gate.promise);
    assert.equal(armed, 0); // background reads stay ordinary cached reads
    invalidate("guard:bypass");
    assert.equal(armed, 1);
    gate.resolve("v");
    await tick();
  } finally {
    initData(null);
  }
});
