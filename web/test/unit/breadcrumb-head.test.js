// web/test/unit/breadcrumb-head.test.js — issue #29 follow-up regression:
// the Tree/Blob Breadcrumb head link ("main" below the tabs) duplicated the
// header's `main @ sha` pill and the Code tab, so it was dropped — at repo
// root no breadcrumb renders at all; on subpaths only the path segments
// render (each linked except the last).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

for (const file of ["../../src/pages/Tree.jsx", "../../src/pages/Blob.jsx"]) {
  test(`${file}: Breadcrumb has no head link`, () => {
    const src = srcOf(file);
    assert.ok(!src.includes("props.head"), `${file} must not reference props.head`);
    assert.ok(!src.includes('"root"'), `${file} must not render a root fallback label`);
    assert.match(
      src,
      /<Show when=\{parts\(\)\.length > 0\}>[\s\S]*<nav class="crumbs/,
      `${file} renders no breadcrumb at repo root`,
    );
    assert.match(
      src,
      /<Show when=\{i\(\) > 0\}>\{" \/ "\}<\/Show>/,
      `${file} separates path-only segments without a leading separator`,
    );
  });
}

test("Tree/Blob call sites pass no head prop", () => {
  for (const file of ["../../src/pages/Tree.jsx", "../../src/pages/Blob.jsx"]) {
    const src = srcOf(file);
    assert.ok(!src.includes("head="), `${file} call site must not pass head`);
  }
});
