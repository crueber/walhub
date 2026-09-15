// web/test/unit/settings-review-586.test.js — Forgejo #586: the
// General settings tab renders the self-approval toggle on the existing
// save path (settings PUT, admin-gated server-side with the 403
// surfacing in the note, settings:* + repo:{full} invalidations). The
// TOML round-trip itself is pinned in repo-review-586.test.js; this pins
// the tab wiring as source text, mirroring settings-features-522.test.js.

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

test("General tab renders the self-approval toggle from the shared helper", () => {
  const general = block(SETTINGS, "function GeneralTab(props)", "// --- tab 1: scheduled tasks");
  assert.ok(general.includes("SELF_APPROVAL_LABEL"), "the label comes from the shared model");
  assert.ok(general.includes("SELF_APPROVAL_HINT"), "the toggle names its consequence");
  // Forgejo #533: the toggle is the shared ToggleSwitch (the native
  // checkbox lives in the component, not inline here).
  assert.ok(general.includes("extractSelfApproval("), "prefill reads the [review] section");
  assert.ok(general.includes("withSelfApproval(current, allowed)"), "save composes the knob into the live doc");
  // Unset renders checked (the server default is allowed).
  assert.ok(general.includes("checked={getSelf() !== false}"), "unset renders checked (default-ON)");
  // The row IS the label (clicking anywhere toggles natively); the
  // text-field `input` class never belongs on a checkbox.
  assert.ok(general.includes("justify-between gap-4"), "the row follows the #533 aligned-row pattern");
});

test("self-approval save rides the existing General-tab path", () => {
  const general = block(SETTINGS, "function GeneralTab(props)", "// --- tab 1: scheduled tasks");
  assert.ok(general.includes("props.repo.settings.put("), "save goes through the existing settings PUT (admin-gated server-side)");
  // Non-admin saves surface the failure in the note (the description-save
  // read-mostly behavior) and reseed from server truth (the
  // visibility-save discipline — the form never shows an unsaved value).
  assert.ok(general.includes("setSelfNote(String(e.message ?? e))"), "failures surface in the note (403 = not admin)");
  assert.ok(general.includes("props.repo.settings.get()"), "failures reseed the toggle from server truth");
  // The undefined sentinel keeps user edits unclobbered (the
  // description-prefill pattern, with null reserved for
  // seeded-but-unset — Forgejo #605); the form flags unsaved changes.
  assert.ok(general.includes("getSelf() === undefined"), "the undefined sentinel keeps user edits unclobbered");
  assert.ok(general.includes("selfDirty()"), "the form flags unsaved changes");
  // Invalidations: the editors read the shared settings entries (the
  // summary projects no review knob, so no summary-ETag work).
  for (const key of ["`repo:${props.ctx.full}`", "`settings:${props.ctx.full}`", "`settings-effective:${props.ctx.full}`", "`settings-history:${props.ctx.full}`"]) {
    assert.ok(general.includes(`invalidate(${key})`), `save invalidates ${key}`);
  }
});
