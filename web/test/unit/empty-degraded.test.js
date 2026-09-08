// web/test/unit/empty-degraded.test.js — issue #209: empty/degraded repo
// states stay silent. The summary predicates (isEmptySummary /
// isDegradedSummary), the self-describing 404 fallback (isEmptyError), the
// shared-cache peek (peekCached/summaryOf), and the degraded wrapper
// (tolerateDegraded) are pure logic over the data-layer cache — headless
// through prefetchData, the same ensureEntry → start path components use.
// The tray must stay empty throughout: mapped outcomes resolve (never throw
// into start()'s reportError), and only expected-404 control flow is mapped.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  EMPTY_MARKER,
  EMPTY_REPO,
  prefetchData,
  trayErrors,
  isEmptySummary,
  isDegradedSummary,
  peekCached,
  summaryOf,
  isEmptyError,
  tolerateDegraded,
} from "../../src/lib/data.js";
import { ReposError } from "../../sdk/src/errors.js";

const tick = (ms = 10) => new Promise((r) => setTimeout(r, ms));

const empty404 = () => new ReposError(404, `not found: ${EMPTY_MARKER}unborn HEAD`, "/o/r/api/resolve/");
const damage404 = () => new ReposError(404, "not found: bad object aaaa", "/o/r/api/tree/aaaa");

test("EMPTY_MARKER is the server prefix (07_api §9.9)", () => {
  assert.equal(EMPTY_MARKER, "empty repository: ");
});

test("isEmptySummary: server health wins when present", () => {
  assert.equal(isEmptySummary({ health: "empty" }), true);
  assert.equal(isEmptySummary({ health: "healthy", head: null, branches: 0, tags: 0 }), false);
  assert.equal(isEmptySummary({ health: "degraded", head: null, branches: 0, tags: 0 }), false);
});

test("isEmptySummary: unborn shape guides on old servers without the field", () => {
  assert.equal(isEmptySummary({ head: null, branches: 0, tags: 0 }), true);
  assert.equal(isEmptySummary({ head: { name: "refs/heads/main", sha: "a" }, branches: 1, tags: 0 }), false);
  assert.equal(isEmptySummary({ head: null, branches: 0, tags: 1 }), false); // tags-only is a real repo
  assert.equal(isEmptySummary(null), false); // deleted repos keep the "not found" shell
  assert.equal(isEmptySummary(undefined), false);
});

test("isDegradedSummary keys on health only", () => {
  assert.equal(isDegradedSummary({ health: "degraded" }), true);
  assert.equal(isDegradedSummary({ health: "healthy" }), false);
  assert.equal(isDegradedSummary({ health: "empty" }), false);
  assert.equal(isDegradedSummary(null), false);
  assert.equal(isDegradedSummary({}), false);
});

test("isEmptyError matches only 404s carrying the marker", () => {
  assert.equal(isEmptyError(empty404()), true);
  assert.equal(isEmptyError(damage404()), false); // damage never carries the prefix
  assert.equal(isEmptyError(new ReposError(500, `${EMPTY_MARKER}boom`)), false); // status-gated
  assert.equal(isEmptyError(new Error("plain")), false);
  assert.equal(isEmptyError(null), false);
  assert.equal(isEmptyError("not found: empty repository: x"), false); // not an SDK 404
});

test("peekCached/summaryOf read the shared summary entry without fetching", async () => {
  assert.equal(peekCached("repo:peek/o"), undefined);
  const get = prefetchData("repo:peek/o", () => Promise.resolve({ health: "empty", head: null }));
  await tick();
  assert.deepEqual(peekCached("repo:peek/o"), { health: "empty", head: null });
  assert.deepEqual(summaryOf("peek/o"), { health: "empty", head: null });
  assert.equal(get(), summaryOf("peek/o"));
  assert.deepEqual(trayErrors(), []);
});

test("tolerateDegraded maps 404 to the fallback only when known-degraded", async () => {
  const seed = prefetchData("repo:deg/r", () => Promise.resolve({ health: "degraded", missing_total: 2 }));
  await tick();
  assert.equal(seed() !== undefined, true);
  assert.deepEqual(await tolerateDegraded(Promise.reject(damage404()), "deg/r", { degraded: true }), { degraded: true });

  const healthy = prefetchData("repo:ok/r", () => Promise.resolve({ health: "healthy", head: {} }));
  await tick();
  assert.equal(healthy() !== undefined, true);
  await assert.rejects(tolerateDegraded(Promise.reject(damage404()), "ok/r", { degraded: true }), (e) => e?.notFound === true);

  // Unknown summary (not yet loaded): every 404 rethrows — no global muting.
  await assert.rejects(tolerateDegraded(Promise.reject(damage404()), "ghost/r", { degraded: true }), (e) => e?.notFound === true);
  // Non-404 errors rethrow even when degraded.
  await assert.rejects(
    tolerateDegraded(Promise.reject(new ReposError(500, "boom")), "deg/r", { degraded: true }),
    (e) => e?.status === 500,
  );
});

test("suppressed empty fetch settles the sentinel silently: value set, tray empty", async () => {
  let calls = 0;
  const full = "sup/empty";
  const seed = prefetchData(`repo:${full}`, () => Promise.resolve({ health: "empty", head: null, branches: 0, tags: 0 }));
  await tick();
  assert.equal(seed() !== undefined, true);
  // The page-fetcher shape: known-empty short-circuits before touching the network.
  const get = prefetchData(`resolve:${full}/`, () => {
    if (isEmptySummary(summaryOf(full))) return Promise.resolve(EMPTY_REPO);
    calls++;
    return Promise.reject(new Error("must not fetch"));
  });
  await tick();
  assert.deepEqual(get(), EMPTY_REPO);
  assert.equal(calls, 0);
  assert.deepEqual(trayErrors(), []);
});

test("empty-prefix fallback settles the sentinel when the summary is not loaded", async () => {
  // No repo: entry seeded — the fetcher maps the self-describing 404.
  const get = prefetchData("resolve:noload/r/", () =>
    Promise.reject(empty404()).catch((err) => {
      if (isEmptyError(err)) return { empty: true };
      throw err;
    }),
  );
  await tick();
  assert.deepEqual(get(), { empty: true });
  assert.deepEqual(trayErrors(), []);
});

test("degraded sha-step mapping settles inline, tray empty", async () => {
  const full = "degmap/r";
  const seed = prefetchData(`repo:${full}`, () => Promise.resolve({ health: "degraded", missing_total: 1 }));
  await tick();
  assert.equal(seed() !== undefined, true);
  const get = prefetchData(`sha:aaaa:tree:`, () =>
    Promise.reject(damage404()).catch((err) => {
      if (err?.notFound && isDegradedSummary(summaryOf(full))) {
        return { degraded: true, sha: "aaaa", path: "" };
      }
      throw err;
    }),
  );
  await tick();
  assert.deepEqual(get(), { degraded: true, sha: "aaaa", path: "" });
  assert.deepEqual(trayErrors(), []);
});
