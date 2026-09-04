// web/test/unit/format.test.js — fmtSize helper (issues #27, #29): b/k/MB/GB
// boundaries, 0/undefined handling; fmtMode helper (issue #29): git modes as
// rwx triplets.
import { test } from "node:test";
import assert from "node:assert/strict";
import { fmtSize, fmtMode } from "../../src/lib/format.js";

test("bytes under 1 KiB render with no space, lowercase b", () => {
  assert.equal(fmtSize(0), "0b");
  assert.equal(fmtSize(1), "1b");
  assert.equal(fmtSize(92), "92b");
  assert.equal(fmtSize(483), "483b");
  assert.equal(fmtSize(1023), "1023b");
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

test("fmtMode maps known git modes to rwx triplets", () => {
  assert.equal(fmtMode("100644"), "rw-r--r--");
  assert.equal(fmtMode("100755"), "rwxr-xr-x");
  assert.equal(fmtMode("120000"), "rwxrwxrwx");
  assert.equal(fmtMode("160000"), "m---------");
  assert.equal(fmtMode("040000"), "rwxr-xr-x");
  assert.equal(fmtMode("40000"), "rwxr-xr-x");
});

test("fmtMode falls back to the low permission bits; blanks stay blank", () => {
  assert.equal(fmtMode("100600"), "rw-------");
  assert.equal(fmtMode("644"), "rw-r--r--");
  assert.equal(fmtMode(100755), "rwxr-xr-x");
  assert.equal(fmtMode(undefined), "");
  assert.equal(fmtMode(null), "");
  assert.equal(fmtMode(""), "");
  assert.equal(fmtMode("not-a-mode"), "");
});
