// web/test/unit/breadcrumb-head.test.js — issue #29 follow-up regression:
// the Tree/Blob Breadcrumb first crumb is a "root" link to the repo root
// (href=/{full}); it never shows the branch name (no head prop) so it can't
// duplicate the header's `main @ sha` pill. At repo root only the root link
// renders; on subpaths root + intermediates are linked, final is strong.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

for (const file of ["../../src/pages/Tree.jsx", "../../src/pages/Blob.jsx"]) {
  test(`${file}: Breadcrumb first crumb is a root link`, () => {
    const src = srcOf(file);
    assert.ok(!src.includes("props.head"), `${file} must not reference props.head`);
    assert.ok(!src.includes("head="), `${file} call site must not pass head`);
    assert.match(
      src,
      /href=\{`\/\$\{props\.full\}`\}>root<\/A>/,
      `${file} first crumb links to the repo root labeled root`,
    );
    assert.match(
      src,
      /<nav class="crumbs/,
      `${file} renders the breadcrumb nav even at repo root`,
    );
    assert.ok(
      !src.includes("<Show when={parts().length > 0}>"),
      `${file} must not hide the breadcrumb at repo root`,
    );
  });
}

test("Blob keeps rev for subpath hrefs", () => {
  const src = srcOf("../../src/pages/Blob.jsx");
  assert.ok(src.includes("shortRef(props.rev)"), "Blob subpath hrefs still use rev");
  assert.ok(src.includes("rev={"), "Blob call site still passes rev");
});
