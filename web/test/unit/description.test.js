// web/test/unit/description.test.js — issue #235 repo description helpers:
// the top-level `description` key round-trips through settings TOML text,
// section-nested lookalikes are ignored, and the client-side shape check
// mirrors the server limit (512 chars, single line).

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  MAX_DESCRIPTION_LENGTH,
  validateDescription,
  escapeTomlBasic,
  unescapeTomlBasic,
  extractDescription,
  withDescription,
} from "../../src/lib/repoDescription.js";

test("limit mirrors the server (512)", () => {
  assert.equal(MAX_DESCRIPTION_LENGTH, 512);
});

test("validateDescription accepts empty and plain text", () => {
  assert.equal(validateDescription(""), null);
  assert.equal(validateDescription("A short repo line"), null);
  assert.equal(validateDescription("x".repeat(512)), null);
});

test("validateDescription rejects non-text, multi-line, over-long", () => {
  assert.ok(validateDescription(undefined));
  assert.ok(validateDescription(null));
  assert.ok(validateDescription(42));
  assert.ok(validateDescription("one\ntwo"));
  assert.ok(validateDescription("a\rb"));
  assert.ok(validateDescription("x".repeat(513)));
});

test("basic-string escaping round-trips quotes, backslashes, controls", () => {
  for (const d of ['say "hi"', "back\\slash", "tab\there", "snowman ☃"]) {
    assert.equal(unescapeTomlBasic(escapeTomlBasic(d)), d, `round-trip: ${d}`);
  }
});

test("extractDescription reads basic and literal strings, else empty", () => {
  assert.equal(extractDescription(""), "");
  assert.equal(extractDescription("[bundles]\nmin_commits = 5\n"), "");
  assert.equal(extractDescription('description = "hello"\n[bundles]\n'), "hello");
  assert.equal(extractDescription("description = 'literal'\n"), "literal");
  assert.equal(extractDescription('description = "say \\"hi\\""\n'), 'say "hi"');
});

test("extractDescription ignores section-nested lookalikes", () => {
  const doc = '[bundles]\ndescription = "not top-level"\n';
  assert.equal(extractDescription(doc), "");
});

test("extractDescription reads a top-level key after a section", () => {
  // TOML-wise this belongs to the section (server rejects it); the helper
  // only scans before the first header, so it stays invisible here too.
  const doc = '[bundles]\nmin_commits = 5\ndescription = "late"\n';
  assert.equal(extractDescription(doc), "");
});

test("withDescription inserts into an empty doc", () => {
  assert.equal(withDescription("", "hi"), 'description = "hi"\n');
});

test("withDescription replaces the top-level line in place", () => {
  const doc = 'description = "old"\n[bundles]\nmin_commits = 5\n';
  const out = withDescription(doc, "new");
  assert.equal(extractDescription(out), "new");
  assert.ok(out.includes("[bundles]\nmin_commits = 5\n"), "section body untouched");
  assert.equal(out.split("\n").filter((l) => l.startsWith("description")).length, 1);
});

test("withDescription prepends when absent, leaves sections alone", () => {
  const doc = '[bundles]\nmin_commits = 5\ndescription = "nested"\n';
  const out = withDescription(doc, "top");
  assert.ok(out.startsWith('description = "top"\n'));
  assert.ok(out.includes('description = "nested"'), "section body untouched");
  assert.equal(extractDescription(out), "top");
});

test("withDescription escapes hostile values", () => {
  const out = withDescription("", 'a"b\\c');
  assert.equal(extractDescription(out), 'a"b\\c');
});
