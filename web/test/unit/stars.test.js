// web/test/unit/stars.test.js — star-count display for repo listings
// (issue #137): fmtStars formatting + SOCIAL_TTL cache contract.
import { test } from "node:test";
import assert from "node:assert/strict";
import { SOCIAL_TTL, fmtStars } from "../../src/lib/stars.js";

test("SOCIAL_TTL matches the 08 §6 social:{o}/{r} cache-key row", () => {
  assert.equal(SOCIAL_TTL, 30_000);
});

test("fmtStars renders counts walhub-style", () => {
  assert.equal(fmtStars(3), "(3 ⭐)");
  assert.equal(fmtStars(0), "(0 ⭐)");
  assert.equal(fmtStars(1024), "(1024 ⭐)");
});

test("fmtStars floors fractional counts", () => {
  assert.equal(fmtStars(2.9), "(2 ⭐)");
});

test("fmtStars hides not-yet-loaded and bad data so the placeholder shows", () => {
  for (const bad of [null, undefined, NaN, -1, "x"]) {
    assert.equal(fmtStars(bad), "");
  }
});
