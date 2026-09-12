// web/test/unit/visibility.test.js — Forgejo #345 (badge rules) + #374
// (visibility modes split: public / authenticated / private, with
// owner-appropriate select options).

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  visibilityBadge,
  VISIBILITY_OPTIONS,
  isVisibility,
  visibilityOptions,
} from "../../src/lib/visibility.js";

test("public, authenticated, and private badges with accessible titles", () => {
  assert.deepEqual(visibilityBadge({ visibility: "public" }), {
    show: true,
    label: "public",
    title: "public — anyone may read",
  });
  assert.deepEqual(visibilityBadge({ visibility: "authenticated" }), {
    show: true,
    label: "authenticated",
    title: "private — signed-in users may read",
  });
  assert.deepEqual(visibilityBadge({ visibility: "private" }), {
    show: true,
    label: "private",
    title: "private — owner or organization members only",
  });
});

test("unknown, empty, and missing visibility render nothing", () => {
  for (const doc of [{}, { visibility: "" }, { visibility: "hidden" }, null, undefined]) {
    assert.deepEqual(visibilityBadge(doc), { show: false, label: "", title: "" }, `hidden: ${JSON.stringify(doc)}`);
  }
});

test("spelling is case-normalized, options are the three server spellings", () => {
  assert.equal(visibilityBadge({ visibility: "Public" }).label, "public");
  assert.equal(visibilityBadge({ visibility: "AUTHENTICATED" }).label, "authenticated");
  assert.equal(visibilityBadge({ visibility: "PRIVATE" }).label, "private");
  assert.deepEqual(VISIBILITY_OPTIONS, ["public", "authenticated", "private"]);
  assert.equal(isVisibility("public"), true);
  assert.equal(isVisibility("authenticated"), true);
  assert.equal(isVisibility("private"), true);
  for (const bad of ["", "hidden", "internal", "members only", null, undefined]) {
    assert.equal(isVisibility(bad), false, `rejected: ${String(bad)}`);
  }
});

test("visibilityOptions differ only in the private label by owner kind", () => {
  assert.deepEqual(visibilityOptions(false), [
    { value: "public", label: "public — anyone may read" },
    { value: "authenticated", label: "private — logged in only" },
    { value: "private", label: "private — owner only" },
  ]);
  assert.deepEqual(visibilityOptions(true), [
    { value: "public", label: "public — anyone may read" },
    { value: "authenticated", label: "private — logged in only" },
    { value: "private", label: "private — org members only" },
  ]);
  // Every offered value validates (the server accepts the same set).
  for (const opts of [visibilityOptions(false), visibilityOptions(true)]) {
    for (const o of opts) {
      assert.equal(isVisibility(o.value), true, `offered: ${o.value}`);
    }
  }
});
