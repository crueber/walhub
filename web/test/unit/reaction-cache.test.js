// web/test/unit/reaction-cache.test.js — issues #36/#42: the optimistic
// reaction path (reactions.adjustSummary + data.patchCached).
//
// The thread page paints a reaction click synchronously and reconciles it
// with the guarded invalidate() refetch. These tests drive the real cache
// (ensureEntry → prefetchData → patchCached → invalidate) headless: the
// optimistic value must commit synchronously, must survive a stale
// pre-mutation body resolving after it, and must be replaced by the
// post-mutation refetch. No test triggers a current-generation error, so
// the tray stays empty (reportError's 10 s fade timer never arms).

import { test } from "node:test";
import assert from "node:assert/strict";

import { prefetchData, patchCached, invalidate, trayErrors } from "../../src/lib/data.js";
import { adjustSummary, summaryEntries } from "../../src/lib/reactions.js";

function deferred() {
  let resolve, reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

const tick = (ms = 10) => new Promise((r) => setTimeout(r, ms));

const view = (summary) => ({ thread: { reaction_summary: summary }, events: [] });
const bump = (seq, content, delta) => (v) => ({
  ...v,
  thread: { ...v.thread, reaction_summary: adjustSummary(v.thread?.reaction_summary, seq, content, delta) },
});

test("patchCached of an unknown key is a silent false (no throw, no entry)", () => {
  assert.equal(patchCached("rx:missing-never-created", (v) => v), false);
});

test("optimistic add paints synchronously, guarded refetch reconciles it", async () => {
  const bodies = [() => Promise.resolve(view({})), () => Promise.resolve(view({ "000003": { "+1": 1 } }))];
  let n = 0;
  const get = prefetchData("rx:add", () => bodies[n++]());
  await tick();
  assert.deepEqual(summaryEntries(get().thread.reaction_summary, 3), []);

  assert.equal(patchCached("rx:add", bump(3, "+1", +1)), true);
  assert.deepEqual(summaryEntries(get().thread.reaction_summary, 3), [["+1", 1]]);

  invalidate("rx:add"); // post-mutation body agrees: the guess stands
  await tick();
  await tick();
  assert.deepEqual(summaryEntries(get().thread.reaction_summary, 3), [["+1", 1]]);
  assert.deepEqual(trayErrors(), []);
});

test("a stale pre-mutation body resolving after the guess is dropped", async () => {
  const gate = deferred();
  const bodies = [() => Promise.resolve(view({})), () => gate.promise, () => Promise.resolve(view({ "000003": { heart: 1 } }))];
  let n = 0;
  const get = prefetchData("rx:stale", () => bodies[n++]());
  await tick();
  assert.deepEqual(summaryEntries(get().thread.reaction_summary, 3), []);

  invalidate("rx:stale"); // gen 2: a pre-mutation read now in flight…
  assert.equal(patchCached("rx:stale", bump(3, "heart", +1)), true); // …then the click paints
  assert.deepEqual(summaryEntries(get().thread.reaction_summary, 3), [["heart", 1]]);
  gate.resolve(view({})); // the stale pre-mutation body lands last…
  await tick();
  await tick();
  assert.deepEqual(summaryEntries(get().thread.reaction_summary, 3), [["heart", 1]]); // …and is dropped

  invalidate("rx:stale"); // gen 4: the post-mutation refetch reconciles
  await tick();
  await tick();
  assert.deepEqual(summaryEntries(get().thread.reaction_summary, 3), [["heart", 1]]);
  assert.deepEqual(trayErrors(), []);
});

test("failed mutation rolls back via reconcile (invalidate restores server truth)", async () => {
  let server = view({});
  const get = prefetchData("rx:rollback", () => Promise.resolve(server));
  await tick();
  assert.equal(patchCached("rx:rollback", bump(0, "eyes", +1)), true);
  assert.deepEqual(summaryEntries(get().thread.reaction_summary, 0), [["eyes", 1]]);
  // POST failed: server truth never moved — the guarded refetch rolls back.
  invalidate("rx:rollback");
  await tick();
  await tick();
  assert.deepEqual(summaryEntries(get().thread.reaction_summary, 0), []);
  assert.deepEqual(trayErrors(), []);
});

test("toggle remove path: -1 guess prunes the chip, refetch confirms", async () => {
  const get = prefetchData("rx:toggle", () => Promise.resolve(view({ "000000": { "+1": 1 } })));
  await tick();
  assert.deepEqual(summaryEntries(get().thread.reaction_summary, 0), [["+1", 1]]);
  assert.equal(patchCached("rx:toggle", bump(0, "+1", -1)), true);
  assert.deepEqual(summaryEntries(get().thread.reaction_summary, 0), []);
  assert.deepEqual(get().thread.reaction_summary, {});
  assert.deepEqual(trayErrors(), []);
});
