// web/test/unit/format.test.js — fmtSize helper (issues #27, #29): b/k/MB/GB
// boundaries, 0/undefined handling; fmtMode helper (issues #29, #211): git
// modes as ls-style rows (leading type char + rwx triplets); fmtSizeParts
// (issue #211 split size columns). The #223 type-column drop removed the kind
// label (mode lead char + icons already carry it).
import { test } from "node:test";
import assert from "node:assert/strict";
import { fmtSize, fmtSizeParts, fmtMode } from "../../src/lib/format.js";

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

test("fmtSize output is unchanged by the #211 split-column refactor", () => {
  assert.equal(fmtSize(92), "92b");
  assert.equal(fmtSize(48372), "47.2k");
  assert.equal(fmtSize(3 * 1024 * 1024), "3MB");
  assert.equal(fmtSize(2.5 * 1024 * 1024 * 1024), "2.5GB");
});

test("fmtSizeParts splits the issue #211 examples: 414 B, 17 KB, 18 MB", () => {
  assert.deepEqual(fmtSizeParts(414), { num: "414", unit: "B" });
  assert.deepEqual(fmtSizeParts(17 * 1024), { num: "17", unit: "KB" });
  assert.deepEqual(fmtSizeParts(18 * 1024 * 1024), { num: "18", unit: "MB" });
});

test("fmtSizeParts shares fmtSize's ladder and rounding", () => {
  assert.deepEqual(fmtSizeParts(0), { num: "0", unit: "B" });
  assert.deepEqual(fmtSizeParts(1023), { num: "1023", unit: "B" });
  assert.deepEqual(fmtSizeParts(1024), { num: "1", unit: "KB" });
  assert.deepEqual(fmtSizeParts(1536), { num: "1.5", unit: "KB" });
  assert.deepEqual(fmtSizeParts(48372), { num: "47.2", unit: "KB" });
  assert.deepEqual(fmtSizeParts(1048575), { num: "1024", unit: "KB" });
  assert.deepEqual(fmtSizeParts(1024 * 1024 * 1024), { num: "1", unit: "GB" });
  assert.deepEqual(fmtSizeParts(2.5 * 1024 * 1024 * 1024), { num: "2.5", unit: "GB" });
});

test("fmtSizeParts invalid input keeps the ? placeholder with an empty unit", () => {
  assert.deepEqual(fmtSizeParts(undefined), { num: "?", unit: "" });
  assert.deepEqual(fmtSizeParts(null), { num: "?", unit: "" });
  assert.deepEqual(fmtSizeParts(NaN), { num: "?", unit: "" });
  assert.deepEqual(fmtSizeParts(-1), { num: "?", unit: "" });
  assert.deepEqual(fmtSizeParts("not-a-number"), { num: "?", unit: "" });
});

test("fmtMode renders ls-style rows: leading type char + triplet (#211)", () => {
  assert.equal(fmtMode("100644"), ".rw-r--r--");
  assert.equal(fmtMode("100755"), ".rwxr-xr-x");
  assert.equal(fmtMode("120000"), "lrwxrwxrwx");
  assert.equal(fmtMode("160000"), "m---------");
  assert.equal(fmtMode("040000"), "drwxr-xr-x");
  assert.equal(fmtMode("40000"), "drwxr-xr-x");
});

test("fmtMode canonical modes decide alone; type only aids the fallback", () => {
  assert.equal(fmtMode("100644", "blob"), ".rw-r--r--");
  assert.equal(fmtMode("040000", "tree"), "drwxr-xr-x");
  assert.equal(fmtMode("160000", "commit"), "m---------");
  assert.equal(fmtMode("100600"), ".rw-------");
  assert.equal(fmtMode("644"), ".rw-r--r--");
  assert.equal(fmtMode(100755), ".rwxr-xr-x");
  assert.equal(fmtMode("755", "tree"), "drwxr-xr-x");
  assert.equal(fmtMode("0755", "tree"), "drwxr-xr-x");
  assert.equal(fmtMode("120000", "blob"), "lrwxrwxrwx");
});

test("fmtMode blanks stay blank (no bare type char)", () => {
  assert.equal(fmtMode(undefined), "");
  assert.equal(fmtMode(null), "");
  assert.equal(fmtMode(""), "");
  assert.equal(fmtMode("not-a-mode"), "");
  assert.equal(fmtMode("not-a-mode", "tree"), "");
});

