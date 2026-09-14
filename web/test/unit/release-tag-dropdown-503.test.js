// web/test/unit/release-tag-dropdown-503.test.js — Forgejo #503: the New
// release page tag field is a native single-select dropdown of existing
// tags, not a free-form combobox input + caret button.
//
// Choice (the issue's decision point): native <select>, per the #416
// milestone single-select precedent (single-select consistency with the
// State field) — keyboard/arrows/Enter/Esc + screen-reader listbox
// semantics come free from the platform, and there is no custom popover to
// keep opaque (#405) or viewport-bound (#278). No type-to-filter remains;
// options ride the existing `tags:{full}` cache entry, no new fetch.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const page = () => srcOf("../../src/pages/ReleaseNew.jsx");
const lib = () => srcOf("../../src/lib/releases.js");

const count = (s, sub) => s.split(sub).length - 1;

// --- one control, a native select ------------------------------------------

test("tag field is exactly one native select (no input + caret pair)", () => {
  const s = page();
  assert.equal(count(s, "<select"), 1, "one control opens the tag list");
  assert.ok(s.includes('id="release-tag"'), "select keeps the release-tag id");
  assert.ok(s.includes('class="input font-mono"'), "app input styling, mono tag names");
  assert.ok(s.includes("value={getTag()}"), "select binds the tag signal");
  assert.ok(
    s.includes("onChange={(e) => setTag(e.currentTarget.value)}"),
    "picking an option sets the tag",
  );
});

test("combobox shape is gone: no free-text input, caret, popover, or filter", () => {
  const s = page();
  assert.ok(!s.includes('role="combobox"'), "no combobox input");
  assert.ok(!s.includes("release-tag-list"), "no popover list id");
  assert.ok(!s.includes("release-tag-opt-"), "no option row ids");
  assert.ok(!s.includes("Show recent tags"), "no caret trigger button");
  assert.ok(!s.includes("tag-drop"), "no custom popover panel");
  assert.ok(!s.includes("combobox"), "no combobox copy or comments");
  assert.ok(!s.includes("filterTagNames"), "no client-side type-to-filter");
  assert.ok(!s.includes("getTagOpen") && !s.includes("getTagActive"), "no popover open/active signals");
});

// --- options feed: the existing tags cache, nothing new --------------------

test("options come from the existing tags:{full} cache entry", () => {
  const s = page();
  assert.ok(
    s.includes("useData(`tags:${ctx.full}`, () => ctx.repoClient.tags({ n: 100 }))"),
    "same cache entry and fetch as before",
  );
  assert.equal(count(s, "repoClient.tags("), 1, "no new tags fetch introduced");
  assert.ok(s.includes("<For each={tagNames()}>"), "one option per cached tag");
  assert.ok(s.includes("<option value={name}>{name}</option>"), "option value is the tag name");
  assert.ok(
    s.includes('.replace(/^refs\\/tags\\//, "")'),
    "refs/tags/ prefix still stripped for display + submit",
  );
});

// --- required selection: nothing selected → no create ----------------------

test("placeholder is a disabled empty option; gates disable on nothing-selected", () => {
  const s = page();
  assert.ok(s.includes('<option value="" disabled>'), "placeholder carries the empty value");
  assert.ok(s.includes("Pick a tag…"), "placeholder prompts a pick");
  assert.ok(
    s.includes('<button type="submit" class="btn primary" disabled={getBusy() || !getTag()}>'),
    "create stays disabled until a tag is selected",
  );
  assert.ok(s.includes("disabled={getBusy() || !getTag()}"), "autodraft gated the same way");
  assert.ok(s.includes("Choose a tag for this release."), "empty-tag create keeps the inline error");
});

// --- zero-tags state: explicit empty option + how-tags-exist copy ----------

test("zero tags renders an explicit empty state and disables the select", () => {
  const s = page();
  assert.ok(
    s.includes('<option value="">{tagsLoading() ? "Loading tags…" : "No tags yet"}</option>'),
    "empty-state option (loading vs settled copy)",
  );
  assert.ok(
    s.includes("disabled={tagsLoading() || !hasTags()}"),
    "select disabled while loading and when the repo has no tags",
  );
  assert.ok(
    s.includes("No tags yet — create one from a commit page or push one with git."),
    "helper directs to how tags come to exist; no inline create hatch",
  );
});

// --- helper copy: picking, never typing ------------------------------------

test("helper copy describes picking from existing tags", () => {
  const s = page();
  assert.ok(
    s.includes("tags available — pick one from the list."),
    "count + pick-from-the-list copy",
  );
  assert.ok(!s.toLowerCase().includes("type the name"), 'no "type the name" phrasing');
  assert.ok(!s.includes("No matching tags"), "no filter no-match copy");
  assert.ok(s.includes('for="release-tag"'), "label still targets the control");
  assert.ok(
    s.includes("required, must already exist"),
    "required + must-exist note kept on the label",
  );
  assert.ok(
    s.includes('aria-describedby="release-tag-help"'),
    "select stays wired to its help text",
  );
});

// --- old shape deleted, not shimmed -----------------------------------------

test("filterTagNames helper is deleted with the combobox (no dead code)", () => {
  assert.ok(!lib().includes("filterTagNames"), "helper removed from web/src/lib/releases.js");
  assert.ok(!lib().includes("combobox"), "no combobox references left in the lib");
});
