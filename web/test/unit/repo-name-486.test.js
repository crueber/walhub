// web/test/unit/repo-name-486.test.js — Forgejo #486: the Name fields on
// /new and /import carry live charset validation (error only while the
// entered name is actually invalid) in a reserved-height slot, replacing
// the always-on helper spans. No DOM: the shared rule is unit-tested
// directly, page wiring is pinned as source text (the fork-page-438 /
// create-forms-479 test idiom).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { validateRepoChars, REPO_NAME_RULE } from "../../src/lib/repo-name.js";
import { validateRepoName } from "../../sdk/src/create.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const NEW = srcOf("../../src/pages/New.jsx");
const IMPORT = srcOf("../../src/pages/Import.jsx");

test("valid names show no message (the acceptance happy path)", () => {
  for (const good of ["foo.bar-2_baz", "newthing", "monorepo", "a", "UPPER.Mixed-1_2", "x".repeat(100)]) {
    assert.equal(validateRepoChars(good), "", `${good} must validate clean`);
  }
});

test("invalid names name the charset rule, live as the user types", () => {
  for (const bad of ["foo bar", "a/b", "..", ".hidden", "hi git", "bang!", "has space", "a@b.c", "x".repeat(101), ".git"]) {
    assert.equal(validateRepoChars(bad), REPO_NAME_RULE, `${bad} must report the rule`);
  }
  assert.match(REPO_NAME_RULE, /letters, digits/);
});

test("empty shows NO message (the disabled submit covers required)", () => {
  for (const empty of ["", "   ", null, undefined]) {
    assert.equal(validateRepoChars(empty), "", `empty ${String(empty)} must stay silent`);
  }
});

test("rule mirrors the server-enforced name half (sdk validateRepoName parity)", () => {
  // Agreement: chars-clean ⟺ sdk name-segment clean (owner fixed valid).
  const names = ["foo.bar-2_baz", "a", "x".repeat(100), "foo bar", "a/b", "..", ".hidden", "hi git", "x".repeat(101), "under_score-ok.9"];
  for (const name of names) {
    const sdkNameError = (() => {
      const v = validateRepoName("bob", name);
      if (!v.error) return "";
      return /name/.test(v.error) ? v.error : "";
    })();
    assert.equal(
      validateRepoChars(name) === "",
      sdkNameError === "",
      `${name}: chars rule must agree with the sdk name segment`,
    );
  }
  // Deliberate divergences: empty and bare ".git" are required-errors
  // server-side, but the live region stays silent on empty (required rides
  // the disabled button) and ".git" reads as invalid chars (leading dot).
  assert.ok(validateRepoName("bob", "").error, "sdk still requires a name at submit");
  assert.equal(validateRepoName("bob", "a.git").error, undefined, "sdk strips the .git suffix");
  assert.equal(validateRepoChars("a.git"), "", ".git suffix strips to a valid name");
});

test("always-on helper spans are gone from both pages", () => {
  for (const [name, src] of [["New.jsx", NEW], ["Import.jsx", IMPORT]]) {
    assert.ok(!src.includes("new-name-help") && !src.includes("import-name-help"), `${name}: no name helper id remains`);
    assert.ok(!src.includes("URL path after the owner"), `${name}: helper copy gone`);
  }
});

test("both inputs wire aria-invalid + aria-describedby to a live message", () => {
  const pairs = [
    ["New.jsx", NEW, "new-name-error"],
    ["Import.jsx", IMPORT, "import-name-error"],
  ];
  for (const [name, src, id] of pairs) {
    assert.ok(src.includes(`id="${id}"`), `${name}: message element #${id} exists`);
    assert.ok(src.includes(`aria-describedby="${id}"`), `${name}: input references #${id}`);
    assert.ok(src.includes("aria-invalid={!!nameCharsError()}"), `${name}: input carries live aria-invalid`);
    assert.ok(src.includes('aria-live="polite"'), `${name}: message container is aria-live polite`);
    assert.ok(src.includes("validateRepoChars"), `${name}: consumes the shared rule`);
  }
});

test("no grid shift: the message is a reserved-height slot in the name cell", () => {
  for (const [name, src, id] of [["New.jsx", NEW, "new-name-error"], ["Import.jsx", IMPORT, "import-name-error"]]) {
    // Always rendered (no <Show> gate around the slot) with one line of
    // reserved height, inside the same label cell — the Owner/Name row
    // keeps its height whether the message shows or not.
    assert.ok(src.includes(`<p id="${id}" class="min-h-[1rem]`), `${name}: reserved-height slot`);
    const labelIdx = src.indexOf(`for="${id === "new-name-error" ? "new-name" : "import-name"}"`);
    const slotIdx = src.indexOf(`<p id="${id}"`);
    assert.ok(labelIdx !== -1 && slotIdx > labelIdx, `${name}: slot lives inside the name label cell`);
  }
  // The old conditionally-mounted live blocks are gone (they reflowed the
  // grid while typing).
  assert.ok(!NEW.includes("<Show when={fieldError() && getName()}>"), "New.jsx: shifting live block removed");
  // The collapsing Owner/Name row itself is untouched (stacked + sm:2-col).
  for (const [name, src] of [["New.jsx", NEW], ["Import.jsx", IMPORT]]) {
    assert.ok(
      src.includes('<div class="grid grid-cols-1 gap-3 sm:grid-cols-2">'),
      `${name}: collapsing Owner/Name row unchanged`,
    );
  }
});

test("submit-time behavior: client gate blocks invalid names, server 400s unchanged", () => {
  // New.jsx already gated submit via fieldError (kept); Import.jsx gains
  // the gate on the button AND in start() for both modes.
  assert.ok(NEW.includes("validateRepoName(getOwner(), getName())"), "New.jsx submit gate unchanged");
  assert.ok(NEW.includes("disabled={getBusy() || !!fieldError()"), "New.jsx button still gates on field errors");
  assert.ok(IMPORT.includes("disabled={getBusy() || anonymous() || !getUrl() || !getOwner() || !getName() || !!nameCharsError()}"), "Import.jsx button gates on the chars rule");
  assert.ok(IMPORT.includes("const nameChars = validateRepoChars(getName());"), "Import.jsx start() gates before submit");
  assert.ok(IMPORT.includes("setPhase(\"error\")"), "Import.jsx gate lands in the existing error block");
});
