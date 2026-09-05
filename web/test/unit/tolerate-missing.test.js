// web/test/unit/tolerate-missing.test.js — issue #150: expected-404
// auxiliary fetches stay silent. Per-row probes (star counts, last-active
// stamps) 404 in expected states — deleted between listing and fetch,
// invisible to the viewer, or a fork-provisioned prefix whose child
// manifest does not exist yet — so the fetcher maps 404 to a missing value
// via `tolerateMissing` instead of traying. These tests pin both halves:
// 404 resolves the missing value (tray untouched, driven through the real
// ensureEntry → start path), and every non-404 error still rethrows so the
// data layer trays real failures as before.
import { test } from "node:test";
import assert from "node:assert/strict";

import { prefetchData, tolerateMissing, trayErrors } from "../../src/lib/data.js";
import { ReposError } from "../../sdk/src/errors.js";

const tick = (ms = 10) => new Promise((r) => setTimeout(r, ms));

const social404 = () =>
  new ReposError(404, "not found: repo o/r-fork not found", "/o/r-fork/api/social");

test("tolerateMissing resolves the missing value on SDK 404", async () => {
  assert.equal(await tolerateMissing(Promise.reject(social404()), null), null);
  assert.deepEqual(
    await tolerateMissing(Promise.reject(social404()), { commits: [] }),
    { commits: [] },
  );
});

test("tolerateMissing passes through already-resolved values untouched", async () => {
  assert.deepEqual(await tolerateMissing(Promise.resolve({ stars: 3 }), null), { stars: 3 });
});

test("tolerateMissing rethrows every non-404 error untouched", async () => {
  const errs = [
    new ReposError(500, "boom"),
    new ReposError(401, "authentication required"),
    new ReposError(403, "forbidden"),
    new TypeError("network down"),
    new Error("plain failure"),
    "string failure",
  ];
  for (const err of errs) {
    await assert.rejects(tolerateMissing(Promise.reject(err), null), (e) => e === err);
  }
});

test("a 404 auxiliary fetch settles the row silently: value set, tray empty", async () => {
  const get = prefetchData("aux150:o/r-fork", () => tolerateMissing(Promise.reject(social404()), null));
  await tick();
  assert.equal(get(), null);
  assert.deepEqual(trayErrors(), []);
});

test("a 404 activity fetch settles to the empty state silently: tray empty", async () => {
  const get = prefetchData("aux150:o/r-fork:activity", () =>
    tolerateMissing(Promise.reject(social404()), { commits: [] }),
  );
  await tick();
  assert.deepEqual(get(), { commits: [] });
  assert.deepEqual(trayErrors(), []);
});
