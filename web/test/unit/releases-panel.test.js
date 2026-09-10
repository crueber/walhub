// web/test/unit/releases-panel.test.js — keyAssets helper (issue #35):
// shown/extra split for the Latest row card — plus filterTagNames
// (issue #254): the client-side tag filter for the new-release combobox —
// plus filterReleases/excerptBody (issue #270): the list filter chips and
// row excerpts.
import { test } from "node:test";
import assert from "node:assert/strict";
import { keyAssets, LATEST_ASSET_LIMIT, filterTagNames, filterReleases, excerptBody } from "../../src/lib/releases.js";

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

test("filterReleases: all returns every release as a fresh array", () => {
  const rels = [{ tag: "v1" }, { tag: "v2", draft: true }];
  const out = filterReleases(rels, "all");
  assert.deepEqual(out, rels);
  assert.notEqual(out, rels);
  assert.deepEqual(filterReleases(rels, "bogus"), rels);
});

test("filterReleases: drafts/prereleases narrow by flag", () => {
  const rels = [
    { tag: "v3" },
    { tag: "v2", draft: true },
    { tag: "v1", prerelease: true },
  ];
  assert.deepEqual(filterReleases(rels, "drafts").map((r) => r.tag), ["v2"]);
  assert.deepEqual(filterReleases(rels, "prereleases").map((r) => r.tag), ["v1"]);
});

test("filterReleases: non-array input behaves as an empty list", () => {
  assert.deepEqual(filterReleases(undefined, "all"), []);
  assert.deepEqual(filterReleases(null, "drafts"), []);
  assert.deepEqual(filterReleases("v1", "all"), []);
});

test("excerptBody: first non-blank line, markers stripped", () => {
  assert.equal(excerptBody("\n\n## Highlights\n- a\n- b"), "Highlights");
  assert.equal(excerptBody("> quoted line\nsecond"), "quoted line");
  assert.equal(excerptBody("- item one\n- item two"), "item one");
  assert.equal(excerptBody("1. first step\n2. second"), "first step");
  assert.equal(excerptBody("  spaced   out   "), "spaced out");
});

test("excerptBody: truncates long lines with an ellipsis", () => {
  const long = "x".repeat(200);
  const out = excerptBody(long, 140);
  assert.equal(out.length, 140);
  assert.ok(out.endsWith("…"));
  assert.equal(excerptBody("short", 140), "short");
});

test("excerptBody: non-string input renders as empty", () => {
  assert.equal(excerptBody(undefined), "");
  assert.equal(excerptBody(null), "");
  assert.equal(excerptBody(42), "");
  assert.equal(excerptBody(""), "");
});
