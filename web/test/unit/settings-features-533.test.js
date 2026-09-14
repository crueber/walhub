// web/test/unit/settings-features-533.test.js — Forgejo #533: the six
// #522 feature toggles render as aligned flex rows with shared
// toggle-switch controls. The text-field `input` utility on a checkbox
// stretched the control full-width (the "scattered" column); the fix is
// the shared ToggleSwitch (peer-pattern track + sliding knob, emerald on
// / zinc off, role=switch) with each row a single flex row (label left,
// switch right). Pins the component contract + the row layout + the
// untouched save path as source text (the settings-features-522 pattern:
// headless source pins, no DOM).

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const SETTINGS = srcOf("../../src/pages/Settings.jsx");
const SWITCH = srcOf("../../src/components/ToggleSwitch.jsx");

function block(src, start, end) {
  const s = src.indexOf(start);
  assert.ok(s !== -1, `expected block start ${start}`);
  const e = src.indexOf(end, s);
  assert.ok(e !== -1, `expected block end ${end}`);
  return src.slice(s, e);
}

const general = () => block(SETTINGS, "function GeneralTab(props)", "// --- tab 1: scheduled tasks");
// Scoped to the Features section (the description input above it
// legitimately carries the text-field utility — only the toggles must
// never wear it).
const features = () => block(SETTINGS, ">Features</h4>", "Save features</button>");

test("ToggleSwitch is a peer-pattern switch over a real checkbox", () => {
  assert.ok(SWITCH.includes('type="checkbox"'), "a real checkbox sits underneath the styling (keyboard-operable)");
  assert.ok(SWITCH.includes('role="switch"'), "switch semantics for assistive tech");
  assert.ok(SWITCH.includes("aria-checked"), "checked state is exposed");
  assert.ok(SWITCH.includes("peer sr-only") || SWITCH.includes("peer  sr-only") || (SWITCH.includes("peer") && SWITCH.includes("sr-only")), "the input is the visually-hidden peer");
  assert.ok(SWITCH.includes("peer-checked:"), "the knob/track react to the peer checked state");
  assert.ok(SWITCH.includes("peer-focus-visible:"), "keyboard focus shows a visible ring on the track");
});

test("ToggleSwitch track + knob scale, shape, and theme colors", () => {
  assert.ok(SWITCH.includes("w-9") && SWITCH.includes("h-5"), "w-9 h-5-scale track");
  assert.ok(SWITCH.includes("rounded-full"), "pill track + round knob");
  assert.ok(SWITCH.includes("peer-checked:translate-x-4"), "the knob slides on check");
  assert.ok(SWITCH.includes("bg-zinc-300") || SWITCH.includes("bg-zinc-200"), "zinc track when off");
  assert.ok(SWITCH.includes("peer-checked:bg-emerald-500") || SWITCH.includes("peer-checked:bg-emerald-600"), "emerald track when on");
  assert.ok(SWITCH.includes("dark:"), "both themes carry the control (dark is the default theme)");
});

test("ToggleSwitch carries no text-field styling and nests safely in a row label", () => {
  // Token-exact: the `input` text-field utility must never appear as a
  // class on the control (substring matching would trip on comment
  // prose like "toggles the input").
  for (const m of SWITCH.match(/class="[^"]*"/g) ?? []) {
    const tokens = m.slice(7, -1).split(/\s+/);
    assert.ok(!tokens.includes("input"), `no text-field utility on the control: ${m}`);
  }
  // Element check runs on comment-stripped source (the header documents
  // the no-nested-label rule with a literal `<label>` mention).
  const code = SWITCH.replace(/\/\/[^\n]*/g, "");
  assert.ok(!/<label[\s>]/.test(code), "root is not a label element — the row label wraps it (no nested labels)");
});

test("Features rows are aligned flex rows: label left, switch right", () => {
  const g = general();
  assert.ok(g.includes("flex") && g.includes("items-center") && g.includes("justify-between") && g.includes("gap-4"), "each row is a flex row with the switch anchored right");
  assert.ok(g.includes("<ToggleSwitch"), "rows use the shared switch");
  assert.ok(g.includes("FEATURE_LABELS[key]") && g.includes("FEATURE_HINTS[key]"), "title + muted hint stay on the left");
  assert.ok(g.includes("block text-xs"), "the hint sits beneath the title, not beside it");
  assert.ok(g.includes("grid gap-2"), "the group stays a vertical list");
});

test("no text-field class on any checkbox", () => {
  const f = features();
  // The direct cause of the stretching: `input` (the text-field
  // utility) applied to a checkbox.
  assert.ok(!f.includes("input mt-0.5") && !f.includes('class="input'), "the Features section puts no input-class control on a checkbox");
  for (const m of f.match(/<input[^>]*>/g) ?? []) {
    assert.fail(`no raw checkbox remains in the Features section: ${m}`);
  }
});

test("flagsDirty + save path are untouched", () => {
  const g = general();
  assert.ok(g.includes("flagsDirty()"), "the unsaved-changes note still keys off flagsDirty()");
  assert.ok(g.includes("withFeatures(current, flags)"), "save still composes the flags into the live doc");
  assert.ok(g.includes("props.repo.settings.put("), "save still rides the existing settings PUT");
  assert.ok(g.includes("setFlagsNote(String(e.message ?? e))"), "403-in-note behavior unchanged");
});
