// web/test/unit/visibility.test.js — Forgejo #345: visibility badge rules
// (summary + listing-row payload, no extra fetch).

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  visibilityBadge,
  VISIBILITY_OPTIONS,
  isVisibility,
} from "../../src/lib/visibility.js";

test("public and private badge with accessible titles", () => {
  assert.deepEqual(visibilityBadge({ visibility: "public" }), {
    show: true,
    label: "public",
    title: "public — anyone may read",
  });
  assert.deepEqual(visibilityBadge({ visibility: "private" }), {
    show: true,
    label: "private",
    title: "private — members only",
  });
});

test("unknown, empty, and missing visibility render nothing", () => {
  for (const doc of [{}, { visibility: "" }, { visibility: "hidden" }, null, undefined]) {
    assert.deepEqual(visibilityBadge(doc), { show: false, label: "", title: "" }, `hidden: ${JSON.stringify(doc)}`);
  }
});

test("spelling is case-normalized, options are the two server spellings", () => {
  assert.equal(visibilityBadge({ visibility: "Public" }).label, "public");
  assert.equal(visibilityBadge({ visibility: "PRIVATE" }).label, "private");
  assert.deepEqual(VISIBILITY_OPTIONS, ["public", "private"]);
  assert.equal(isVisibility("public"), true);
  assert.equal(isVisibility("private"), true);
  for (const bad of ["", "hidden", "internal", null, undefined]) {
    assert.equal(isVisibility(bad), false, `rejected: ${String(bad)}`);
  }
});
