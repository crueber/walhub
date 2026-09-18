// web/test/unit/settings-heading-631.test.js — Forgejo #631: the Settings
// page rendered a doc-style h2 ("Settings", text-lg + mb-3) that pushed
// the sidebar nav and the content column a row below the tab bar. The
// heading is now sr-only — screen readers keep the document outline,
// sighted layout starts directly with the nav + content row. No other
// layout change. JSX pinned as source text.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const SETTINGS = srcOf("../../src/pages/Settings.jsx");

function pageHead(src) {
  const start = src.indexOf('<div class="settings-page">');
  assert.ok(start > 0, "settings page root found");
  return src.slice(start, src.indexOf('<div class="flex flex-col gap-4', start));
}

test("page heading is sr-only, not a visual band", () => {
  const head = pageHead(SETTINGS);
  assert.ok(head.includes('<h2 class="sr-only">Settings</h2>'), "heading present for screen readers");
  assert.ok(!head.includes("text-lg") && !head.includes("mb-3"), "no visual heading metrics");
});

test("nav + content row follows the heading directly", () => {
  const row = '<div class="flex flex-col gap-4 lg:flex-row lg:gap-6">';
  const at = SETTINGS.indexOf(row);
  assert.ok(at > 0, "content row found");
  const after = SETTINGS.slice(at + row.length, at + row.length + 200);
  assert.ok(after.includes('<nav class="min-w-0 shrink-0 lg:w-56"'), "sidebar nav first in the row");
});

test("no new CSS or dependencies", () => {
  const css = srcOf("../../src/ui.css");
  assert.ok(!css.includes("settings-page"), "no page-scoped CSS added");
});
