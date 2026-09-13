// web/test/unit/owner-name-row-497.test.js — Forgejo #497: the Owner/Name
// row on /new and /import is one shared component (OwnerNameRow.jsx) with
// matched control heights, top-aligned labels, an asymmetric Owner/Name
// split, and a shift-free error slot. OrgNew.jsx has no Owner field and is
// unaffected. No DOM: layout pinned as source text (the fork-page-438 /
// create-forms-479 / repo-name-486 test idiom).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const ROW = srcOf("../../src/components/OwnerNameRow.jsx");
const NEW = srcOf("../../src/pages/New.jsx");
const IMPORT = srcOf("../../src/pages/Import.jsx");
const ORGNEW = srcOf("../../src/pages/OrgNew.jsx");

test("both pages render the shared row (identical by construction)", () => {
  for (const [name, src, prefix] of [["New.jsx", NEW, "new"], ["Import.jsx", IMPORT, "import"]]) {
    assert.ok(
      src.includes('import OwnerNameRow from "../components/OwnerNameRow.jsx";'),
      `${name}: imports the shared row`,
    );
    assert.ok(src.includes("<OwnerNameRow"), `${name}: renders the shared row`);
    assert.ok(src.includes(`prefix="${prefix}"`), `${name}: namespaces ids with prefix="${prefix}"`);
    assert.ok(src.includes("nameCharsError={nameCharsError}"), `${name}: passes the shared charset rule through`);
  }
  // The old copy-pasted row markup is gone from both pages (no drift
  // possible): no in-page owner select, no in-page name error slot, no
  // in-page two-column row (the header comment's "never a bare
  // grid-cols-2" prose is not markup — match class attributes only).
  for (const [name, src] of [["New.jsx", NEW], ["Import.jsx", IMPORT]]) {
    assert.ok(!src.includes("-name-error\" class="), `${name}: no in-page error slot remains`);
    assert.ok(!/<div class="[^"]*grid-cols-2[^"]*">/.test(src), `${name}: no in-page two-column row remains`);
  }
});

test("matched heights: one shared h-9 on both controls, native select kept", () => {
  // Both selects (loading fallback + populated) and the input carry the
  // same explicit height utility — the .input padding/line-height box can
  // no longer render select-vs-input a few px apart.
  const selectHeights = ROW.match(/<select[^>]*class="input h-9 truncate font-mono"/g) ?? [];
  assert.equal(selectHeights.length, 2, "both selects (fallback + loaded) share h-9");
  assert.ok(ROW.includes('<input') && ROW.includes('class="input h-9 font-mono"'), "name input shares h-9");
  // Deliberate non-choice: no appearance-none in any control class — the
  // native dropdown arrow and keyboard behavior stay, so no custom
  // chevron is needed (the word survives only in the header comment
  // recording the decision).
  assert.ok(!/class="[^"]*appearance-none/.test(ROW), "native select chrome kept (no appearance-none)");
});

test("labels share one baseline; the grid top-aligns", () => {
  const labels = ROW.match(/<span class="text-sm font-medium">(Owner|Name)<\/span>/g) ?? [];
  assert.deepEqual(labels, ['<span class="text-sm font-medium">Owner</span>', '<span class="text-sm font-medium">Name</span>'], "both labels, same size/weight, Owner first");
  assert.ok(ROW.includes("items-start"), "row grid top-aligns both columns");
});

test("error slot sits below the grid: reserved, live, wired", () => {
  const gridIdx = ROW.indexOf('<div class="grid grid-cols-1 items-start');
  const slotIdx = ROW.indexOf("<p id={errorId()}");
  assert.ok(gridIdx !== -1 && slotIdx !== -1 && slotIdx > gridIdx, "error paragraph renders after (outside) the two-column grid");
  assert.ok(ROW.includes("min-h-[2rem]"), "reserved two-line height kept (no shift on appear/disappear)");
  assert.ok(ROW.includes('aria-live="polite"'), "slot stays aria-live polite");
  assert.ok(ROW.includes("aria-describedby={errorId()}"), "input describes the slot");
  assert.ok(ROW.includes("aria-invalid={!!props.nameCharsError()}"), "input carries live aria-invalid");
  // Always rendered: no <Show> gate may wrap the slot (gated blocks reflow).
  const slotRegion = ROW.slice(slotIdx, slotIdx + 400);
  assert.ok(!slotRegion.includes("<Show"), "slot is unconditional");
});

test("asymmetric split: narrower Owner, truncate, mobile stack intact", () => {
  assert.ok(
    ROW.includes("sm:grid-cols-[minmax(0,1fr)_minmax(0,2fr)]"),
    "Owner takes 1/3, Name 2/3 at sm: and up",
  );
  assert.ok(ROW.includes("grid-cols-1"), "single-column stack below sm:");
  assert.ok(!ROW.includes("grid-cols-2 gap-3") && !/sm:grid-cols-2["\s]/.test(ROW), "no 50/50 split remains");
  const minW0 = ROW.match(/<label class="grid min-w-0 gap-1"/g) ?? [];
  assert.equal(minW0.length, 2, "both cells min-w-0 so minmax(0, …) can shrink");
  assert.ok(ROW.includes("truncate"), "long owner names ellipsis instead of stretching the column");
});

test("owner select keyboard/a11y behavior unchanged", () => {
  const labelled = ROW.match(/aria-label="Owner"/g) ?? [];
  assert.equal(labelled.length, 2, "both selects (fallback + loaded) keep aria-label=Owner");
  assert.ok(ROW.includes("when={props.getOwners() !== null}"), "null (loading) still renders the disabled fallback");
  assert.ok(ROW.includes('<option>{props.getOwner() || "…"}</option>'), "fallback shows the current owner");
  assert.ok(ROW.includes("<For each={props.getOwners() ?? []}>"), "options still render from the admitted set");
  assert.ok(ROW.includes('aria-label="Name"'), "name input keeps aria-label=Name");
});

test("OrgNew unaffected: no Owner field, no shared row", () => {
  assert.ok(!ORGNEW.includes("OwnerNameRow"), "OrgNew does not use the shared row");
  assert.ok(!ORGNEW.includes("owner-name-row"), "no row hook class leaks in");
  assert.ok(!ORGNEW.includes("grid-cols-2"), "still no two-column row at all");
  assert.ok(!ORGNEW.includes("getOwners"), "no owner-list wiring");
});
