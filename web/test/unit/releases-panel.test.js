// web/test/unit/releases-panel.test.js — keyAssets helper (issue #35):
// shown/extra split for the Latest sidebar card — plus filterTagNames
// (issue #254): the client-side tag filter for the new-release combobox.
import { test } from "node:test";
import assert from "node:assert/strict";
import { keyAssets, LATEST_ASSET_LIMIT, filterTagNames } from "../../src/lib/releases.js";

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

test("filterTagNames: blank query returns every name in stream order", () => {
  const names = ["v2.0.0", "v1.1.0", "v1.0.0"];
  assert.deepEqual(filterTagNames(names, ""), names);
  assert.deepEqual(filterTagNames(names, "   "), names);
  assert.deepEqual(filterTagNames(names, undefined), names);
  // a fresh array, not the input itself
  assert.notEqual(filterTagNames(names, ""), names);
});

test("filterTagNames: substring match, case-insensitive, order preserved", () => {
  const names = ["v2.0.0", "v1.1.0", "v1.0.0", "nightly"];
  assert.deepEqual(filterTagNames(names, "v1"), ["v1.1.0", "v1.0.0"]);
  assert.deepEqual(filterTagNames(names, "V2"), ["v2.0.0"]);
  assert.deepEqual(filterTagNames(names, "night"), ["nightly"]);
  assert.deepEqual(filterTagNames(names, "zzz"), []);
});

test("filterTagNames: non-array input behaves as an empty list", () => {
  assert.deepEqual(filterTagNames(undefined, "v1"), []);
  assert.deepEqual(filterTagNames(null, ""), []);
  assert.deepEqual(filterTagNames("v1.0.0", ""), []);
});
