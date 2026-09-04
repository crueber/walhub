// web/test/unit/danger.test.js — typed-confirm gate for the Danger Zone (issue #39):
// the confirm button arms only on the EXACT `owner/name` match.

import { test } from "node:test";
import assert from "node:assert/strict";

import { dangerMatches } from "../../src/lib/danger.js";

test("dangerMatches arms only on the exact owner/name", () => {
  assert.equal(dangerMatches("crueber/walhub", "crueber/walhub"), true);
});

test("dangerMatches rejects near-misses", () => {
  const cases = [
    ["", "crueber/walhub"], // empty input
    ["crueber", "crueber/walhub"], // owner only
    ["walhub", "crueber/walhub"], // name only
    ["Crueber/walhub", "crueber/walhub"], // case differs
    ["crueber/walhub ", "crueber/walhub"], // trailing space
    [" crueber/walhub", "crueber/walhub"], // leading space
    ["crueber/walhub\n", "crueber/walhub"], // pasted newline
    ["crueber/walhu", "crueber/walhub"], // prefix
    ["other/walhub", "crueber/walhub"], // wrong owner
  ];
  for (const [typed, expected] of cases) {
    assert.equal(dangerMatches(typed, expected), false, JSON.stringify(typed));
  }
});

test("dangerMatches never arms on empty/non-string expectation", () => {
  assert.equal(dangerMatches("", ""), false);
  assert.equal(dangerMatches("x", ""), false);
  assert.equal(dangerMatches(null, "o/r"), false);
  assert.equal(dangerMatches("o/r", null), false);
  assert.equal(dangerMatches(undefined, undefined), false);
});
