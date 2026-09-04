// web/test/unit/releases-panel.test.js — keyAssets helper (issue #35):
// shown/extra split for the Latest sidebar card.
import { test } from "node:test";
import assert from "node:assert/strict";
import { keyAssets, LATEST_ASSET_LIMIT } from "../../src/lib/releases.js";

const assets = (n) => Array.from({ length: n }, (_, i) => ({ name: `a${i + 1}.zip` }));

test("default limit shows the first three, rest as extra", () => {
  assert.equal(LATEST_ASSET_LIMIT, 3);
  const { shown, extra } = keyAssets(assets(5));
  assert.deepEqual(shown.map((a) => a.name), ["a1.zip", "a2.zip", "a3.zip"]);
  assert.equal(extra, 2);
});

test("short lists show all with zero extra", () => {
  assert.deepEqual(keyAssets([]), { shown: [], extra: 0 });
  const two = keyAssets(assets(2));
  assert.equal(two.shown.length, 2);
  assert.equal(two.extra, 0);
  const exact = keyAssets(assets(3));
  assert.equal(exact.shown.length, 3);
  assert.equal(exact.extra, 0);
});

test("explicit limits split at the boundary", () => {
  assert.equal(keyAssets(assets(5), 1).extra, 4);
  assert.equal(keyAssets(assets(5), 5).extra, 0);
  assert.equal(keyAssets(assets(5), 99).extra, 0);
  assert.deepEqual(keyAssets(assets(2), 1).shown.map((a) => a.name), ["a1.zip"]);
});

test("non-array input behaves as an empty list", () => {
  assert.deepEqual(keyAssets(undefined), { shown: [], extra: 0 });
  assert.deepEqual(keyAssets(null), { shown: [], extra: 0 });
  assert.deepEqual(keyAssets("a1.zip"), { shown: [], extra: 0 });
});

test("non-positive or non-finite limits show none, count all as extra", () => {
  for (const limit of [0, -1, NaN, Infinity, "x"]) {
    const { shown, extra } = keyAssets(assets(4), limit);
    assert.deepEqual(shown, []);
    assert.equal(extra, 4);
  }
  // fractional limits floor
  assert.equal(keyAssets(assets(4), 2.9).shown.length, 2);
});
