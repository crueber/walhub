// web/test/unit/release-upload-a11y-621.test.js — Forgejo #621: the
// release asset upload (web/src/pages/Release.jsx) used the same
// hidden-in-label file-input pattern #619 fixed on the profile sidebar —
// display:none drops the input from the Tab order with no focus
// passthrough. It now follows the #619 idiom verbatim: peer sr-only
// input (Tab-reachable) + #533 peer-focus-visible emerald ring on the
// visible .btn span. uploadLabel/onFile/disabled semantics byte-identical.
// Tailwind-only, no new CSS. JSX pinned as source text, mirroring
// avatar-upload-button-619.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const RELEASE = srcOf("../../src/pages/Release.jsx");
const PKG = JSON.parse(srcOf("../../package.json"));

function uploadBlock(src) {
  const start = src.indexOf("Forgejo #621");
  assert.ok(start > 0, "#621 block found");
  return src.slice(start, src.indexOf("</label>", start));
}

function inputTag(src) {
  const block = uploadBlock(src);
  const start = block.indexOf("<input");
  assert.ok(start > 0, "file input found");
  return block.slice(start, block.indexOf("/>", start));
}

test("upload input is peer sr-only, never display:none", () => {
  const input = inputTag(RELEASE);
  assert.ok(input.includes('class="peer sr-only"'), "Tab-reachable visually-hidden input");
  assert.ok(!input.includes("hidden"), "no display:none on the input");
  assert.ok(input.includes('type="file"'), "still a file input");
});

test("visible span carries the button idiom + focus ring", () => {
  const block = uploadBlock(RELEASE);
  assert.ok(block.includes("btn primary"), "canonical button classes kept");
  assert.ok(block.includes("peer-focus-visible:ring-2"), "keyboard focus visible");
  assert.ok(block.includes("peer-focus-visible:ring-emerald-500"), "emerald ring token");
  assert.ok(block.includes('role="button"'), "button role kept");
  assert.ok(block.includes("{getUpload() ? uploadLabel() : \"Upload asset\"}"), "label text wiring kept");
});

test("behavior byte-identical: onFile, disabled, busy visuals", () => {
  const block = uploadBlock(RELEASE);
  assert.ok(block.includes("onChange={onFile}"), "same change handler");
  assert.ok(block.includes("disabled={getBusy()}"), "same disabled wiring");
  assert.ok(block.includes("pointer-events-none opacity-50"), "same busy visuals");
});

test("no new CSS or dependencies (laws)", () => {
  const css = srcOf("../../src/ui.css");
  assert.ok(!css.includes("release-upload") && !css.includes("asset-upload"), "no upload-scoped CSS");
  for (const dep of ["solid-js", "@solidjs/router", "marked", "dompurify"]) {
    assert.ok(JSON.stringify(PKG.dependencies ?? {}).includes(dep), `runtime dep ${dep} declared`);
  }
});
