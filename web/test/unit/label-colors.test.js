// web/test/unit/label-colors.test.js — issue #325: label color picker.
//
// Headless cover for the palette/validation helpers (web/src/lib/label-colors.js)
// plus source-structure guards pinning the picker wiring: preset dropdown +
// custom-hex reveal + live preview reusing the list-row swatch rendering,
// LabelPicker idioms (outside-click/Esc close, onCleanup), no backend change.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

import {
  LABEL_COLOR_PRESETS,
  DEFAULT_LABEL_COLOR,
  isPresetLabelColor,
  isValidLabelColor,
} from "../../src/lib/label-colors.js";

const HEX6 = /^[0-9a-fA-F]{6}$/;

test("palette holds 8 label-friendly presets", () => {
  assert.equal(LABEL_COLOR_PRESETS.length, 8);
});

test("every preset is a valid lowercase 6-hex value, no duplicates", () => {
  for (const hex of LABEL_COLOR_PRESETS) {
    assert.match(hex, HEX6, `6-hex: ${hex}`);
    assert.equal(hex, hex.toLowerCase(), `lowercase: ${hex}`);
  }
  assert.equal(new Set(LABEL_COLOR_PRESETS).size, LABEL_COLOR_PRESETS.length, "no duplicate presets");
});

test("default color is the red preset (the form's long-standing default)", () => {
  assert.equal(DEFAULT_LABEL_COLOR, "d73a4a");
  assert.ok(LABEL_COLOR_PRESETS.includes(DEFAULT_LABEL_COLOR), "default is a preset");
});

test("isValidLabelColor accepts exactly 6 hex digits, nothing else", () => {
  for (const ok of ["d73a4a", "FFFFFF", "000000", "fbca04", "0E8a16"]) {
    assert.equal(isValidLabelColor(ok), true, `valid: ${ok}`);
  }
  for (const bad of ["", "#d73a4a", "d73a4", "d73a4aa", "gggggg", "d73a4a ", " d73a4a", "12345 ", null, undefined, 123456, "zzzzzz"]) {
    assert.equal(isValidLabelColor(bad), false, `invalid: ${String(bad)}`);
  }
});

test("isPresetLabelColor matches case-insensitively, rejects the rest", () => {
  assert.equal(isPresetLabelColor("d73a4a"), true);
  assert.equal(isPresetLabelColor("D73A4A"), true);
  assert.equal(isPresetLabelColor("ffffff"), false);
  assert.equal(isPresetLabelColor(""), false);
  assert.equal(isPresetLabelColor(null), false);
  assert.equal(isPresetLabelColor(undefined), false);
});

const picker = () => srcOf("../../src/components/LabelColorPicker.jsx");
const labels = () => srcOf("../../src/pages/Labels.jsx");

test("picker offers preset swatches plus a Custom-hex reveal", () => {
  const s = picker();
  assert.ok(s.includes("LABEL_COLOR_PRESETS"), "renders from the palette constant");
  assert.ok(s.includes('role="listbox"'), "popover is a listbox");
  assert.ok(s.includes('role="option"'), "presets are options");
  assert.ok(s.includes("Custom hex"), "custom-hex option present");
});

test("picker keeps pattern validation and previews with the list-row swatch rendering", () => {
  const s = picker();
  assert.ok(s.includes('pattern="[0-9a-fA-F]{6}"'), "pattern validation kept on the custom input");
  assert.ok(s.includes('aria-label="label color"'), "input keeps its accessible name");
  // Same rendering as the Labels.jsx list rows (:43-47 shape): h-3 w-3
  // rounded-full dot driven by an inline background-color.
  assert.ok(
    s.includes("inline-block h-3 w-3 rounded-full border border-zinc-300 dark:border-zinc-700"),
    "preview dot reuses the list-row swatch classes",
  );
  assert.ok(s.includes('"background-color"'), "preview uses the inline background-color approach");
  // Invalid values never drive a broken preview: style is applied only when valid.
  assert.ok(s.includes("valid() ?"), "preview style is conditional on validity");
  assert.ok(labels().includes("inline-block h-3 w-3 rounded-full"), "list-row swatch rendering unchanged");
});

test("picker follows the LabelPicker popover idioms and stays in the viewport", () => {
  const s = picker();
  assert.ok(s.includes('aria-haspopup="listbox"'), "trigger announces the popover");
  assert.ok(s.includes("aria-expanded"), "trigger exposes open state");
  assert.ok(s.includes('document.addEventListener("click"'), "outside-click closes the popover");
  assert.ok(s.includes("Escape"), "Esc closes the popover");
  assert.ok(s.includes("onCleanup"), "document listeners are removed on unmount");
  // Reuses the audited .label-drop panel (opaque + 390px viewport-bound in
  // web/src/ui.css per the #278 test) at w-64 = 256px: fits a 390px phone.
  assert.ok(s.includes("label-drop"), "popover reuses the audited .label-drop panel class");
  assert.ok(s.includes("w-64"), "popover keeps the narrow w-64 width");
  assert.ok(s.includes("flex-wrap") || s.includes("flex flex-wrap"), "trigger row wraps on narrow screens");
});

test("picker is presentational: no fetches, no backend change", () => {
  const s = picker();
  assert.ok(!s.includes("fetch("), "no network calls");
  assert.ok(!s.includes("labels.create"), "no direct mutations — parent owns submit");
  assert.ok(!s.includes("repoClient"), "no client dependency");
});

test("Labels create form delegates color to the picker with the same signal", () => {
  const s = labels();
  assert.ok(s.includes("LabelColorPicker"), "form renders the picker");
  assert.ok(s.includes("color={getColor()}"), "picker reads the form color signal");
  assert.ok(s.includes("onChange={setColor}"), "picker writes the form color signal");
  assert.equal((s.match(/aria-label="label color"/g) ?? []).length, 0, "bare color input gone from the form");
  assert.ok(!s.includes('class="input w-28"'), "raw hex input removed from the form (lives in the picker now)");
});
