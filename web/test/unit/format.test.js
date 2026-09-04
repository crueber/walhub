// web/test/unit/format.test.js — fmtSize helper (issue #27): B/k/MB/GB
// boundaries, 0/undefined handling.
import { test } from "node:test";
import assert from "node:assert/strict";
import { fmtSize } from "../../src/lib/format.js";

test("bytes under 1 KiB render as whole bytes", () => {
  assert.equal(fmtSize(0), "0 B");
  assert.equal(fmtSize(1), "1 B");
  assert.equal(fmtSize(483), "483 B");
  assert.equal(fmtSize(1023), "1023 B");
});

test("k boundary at 1024 with one decimal trimmed", () => {
  assert.equal(fmtSize(1024), "1k");
  assert.equal(fmtSize(1536), "1.5k");
  assert.equal(fmtSize(48372), "47.2k");
  assert.equal(fmtSize(1048575), "1024k");
});

test("MB and GB boundaries", () => {
  assert.equal(fmtSize(1024 * 1024), "1MB");
  assert.equal(fmtSize(3 * 1024 * 1024), "3MB");
  assert.equal(fmtSize(1024 * 1024 * 1024), "1GB");
  assert.equal(fmtSize(2.5 * 1024 * 1024 * 1024), "2.5GB");
});

test("missing or invalid sizes render as ?", () => {
  assert.equal(fmtSize(undefined), "?");
  assert.equal(fmtSize(null), "?");
  assert.equal(fmtSize(NaN), "?");
  assert.equal(fmtSize("not-a-number"), "?");
});
