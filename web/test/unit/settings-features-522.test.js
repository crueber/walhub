// web/test/unit/settings-features-522.test.js — Forgejo #522: the
// General settings tab renders six feature toggles on the existing save
// path (settings PUT, admin-gated server-side with the 403 surfacing in
// the note, settings:* + repo:{full} invalidations so the tab bar and
// pills revalidate). The TOML round-trip itself is pinned in
// repo-features-522.test.js; this pins the tab wiring as source text,
// mirroring header-pills-447.test.js.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const SETTINGS = srcOf("../../src/pages/Settings.jsx");

function block(src, start, end) {
  const s = src.indexOf(start);
  assert.ok(s !== -1, `expected block start ${start}`);
  const e = src.indexOf(end, s);
  assert.ok(e !== -1, `expected block end ${end}`);
  return src.slice(s, e);
}

test("General tab renders six toggles from the shared flag model", () => {
  const general = block(SETTINGS, "function GeneralTab(props)", "// --- tab 1: scheduled tasks");
  assert.ok(general.includes("FEATURE_KEYS"), "the checkboxes iterate the shared six-flag model (one model, six booleans)");
  assert.ok(general.includes("FEATURE_LABELS[key]"), "labels come from the shared model");
  assert.ok(general.includes("FEATURE_HINTS[key]"), "each toggle names its consequence");
  // Forgejo #533: the toggles are the shared ToggleSwitch (the native
  // checkbox lives in the component, not inline here).
  assert.ok(general.includes("<ToggleSwitch"), "toggles render through the shared ToggleSwitch");
  // Unset renders checked (the server default is enabled).
  assert.ok(general.includes("checked={flags()[key] !== false}"), "unset flags render checked (all-on default)");
});

test("features save rides the existing General-tab path", () => {
  const general = block(SETTINGS, "function GeneralTab(props)", "// --- tab 1: scheduled tasks");
  // The [features] block is composed into the current doc (description
  // and every other section preserved — never a bare write).
  assert.ok(general.includes("withFeatures(current, flags)"), "save composes the flags into the live doc");
  assert.ok(general.includes("props.repo.settings.put("), "save goes through the existing settings PUT (admin-gated server-side)");
  // Non-admin saves surface the failure in the note (the description-save
  // read-mostly behavior) and reseed from server truth (the
  // visibility-save discipline — the form never shows an unsaved value).
  assert.ok(general.includes("setFlagsNote(String(e.message ?? e))"), "failures surface in the note (403 = not admin)");
  assert.ok(general.includes("props.repo.settings.get()"), "failures reseed the toggles from server truth");
  // Invalidations: the tab bar and pills read the shared summary
  // (repo:{full}); the editors read the shared settings entries.
  for (const key of ["`repo:${props.ctx.full}`", "`settings:${props.ctx.full}`", "`settings-effective:${props.ctx.full}`", "`settings-history:${props.ctx.full}`"]) {
    assert.ok(general.includes(`invalidate(${key})`), `save invalidates ${key}`);
  }
});

test("toggles prefill from the doc without clobbering edits", () => {
  const general = block(SETTINGS, "function GeneralTab(props)", "// --- tab 1: scheduled tasks");
  assert.ok(general.includes("extractFeatures("), "prefill reads the [features] section");
  assert.ok(general.includes("getFlags() === null"), "the null sentinel keeps user edits unclobbered (the description-prefill pattern)");
  assert.ok(general.includes("flagsDirty()"), "the form flags unsaved changes");
});
