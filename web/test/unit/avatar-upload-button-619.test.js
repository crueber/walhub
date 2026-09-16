// web/test/unit/avatar-upload-button-619.test.js — Forgejo #619: the user
// profile sidebar upload (web/src/pages/Repos.jsx, isSelf-gated action
// stack) renders a styled "Upload profile image" button via the
// hidden-input pattern — no raw "Choose File" control — with the
// constraint text as muted helper copy below. Behavior byte-for-byte:
// same accept list, same onChange (upload + reset), same isSelf gate,
// same placement above Regenerate. Tailwind-only, canonical .btn
// (guideline §2 by reference). JSX pinned as source text, mirroring
// user-avatar-upload-601.test.js / profile-sidebar-421.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const REPOS = srcOf("../../src/pages/Repos.jsx");
const PKG = JSON.parse(srcOf("../../package.json"));

function block(src, start, end) {
  const s = src.indexOf(start);
  assert.ok(s !== -1, `expected block start ${start}`);
  const e = src.indexOf(end, s);
  assert.ok(e !== -1, `expected block end ${end}`);
  return src.slice(s, e);
}

test("#619: upload renders as a hidden input + full-width .btn span in a label wrapper", () => {
  const selfBlock = REPOS.slice(REPOS.indexOf("<Show when={isSelf()}>"));
  const labelAt = selfBlock.indexOf('<label class="w-full">');
  assert.ok(labelAt !== -1, "upload label wrapper is full-width");
  const label = selfBlock.slice(labelAt, selfBlock.indexOf("</label>", labelAt));
  assert.ok(label.includes('type="file"'), "label carries the file input");
  assert.ok(label.includes('class="hidden"'), "file input is hidden (no visible Choose File)");
  assert.ok(!label.includes("w-full text-xs"), "raw-input text-xs treatment is gone");
  assert.ok(label.includes("Upload profile image"), "styled button text present");
  const spanAt = label.indexOf("Upload profile image");
  const spanOpen = label.lastIndexOf("<span", spanAt);
  const spanTag = label.slice(spanOpen, label.indexOf(">", spanOpen) + 1);
  assert.ok(spanTag.includes("btn"), "button uses the canonical .btn idiom");
  assert.ok(spanTag.includes("w-full"), "button is full-width like its siblings");
  assert.ok(spanTag.includes("justify-center"), "button centers like its siblings");
  assert.ok(spanTag.includes("cursor-pointer"), "label affordance reads clickable");
  assert.ok(spanTag.includes('role="button"'), "span keeps the button role");
});

test("#619: helper copy is centered muted text below the button", () => {
  const label = block(REPOS, '<label class="w-full">', "</label>");
  assert.ok(label.includes("PNG/JPEG/GIF, ≤ 2 MiB — cropped square"), "constraint text kept");
  const btnAt = label.indexOf("Upload profile image");
  const helpAt = label.indexOf("PNG/JPEG/GIF, ≤ 2 MiB — cropped square");
  assert.ok(btnAt !== -1 && helpAt !== -1 && btnAt < helpAt, "helper renders below the button");
  const helpOpen = label.lastIndexOf("<span", helpAt);
  const helpTag = label.slice(helpOpen, label.indexOf(">", helpOpen) + 1);
  assert.ok(helpTag.includes("muted"), "helper uses the muted idiom");
  assert.ok(helpTag.includes("text-center"), "helper is centered");
  assert.ok(helpTag.includes("text-xs"), "helper keeps the small size");
  assert.ok(helpTag.includes("block"), "helper spans the row below the button");
});

test("#619: behavior preserved — accept, onChange reset, isSelf gate, placement above Regenerate", () => {
  const label = block(REPOS, '<label class="w-full">', "</label>");
  assert.ok(
    label.includes('accept="image/png,image/jpeg,image/gif"'),
    "accept list byte-identical (PNG/JPEG/GIF only)"
  );
  assert.ok(label.includes("uploadAvatar(e.currentTarget.files?.[0])"), "onChange still uploads the picked file");
  assert.ok(label.includes('e.currentTarget.value = ""'), "onChange still resets the input value");
  const selfBlock = REPOS.slice(REPOS.indexOf("<Show when={isSelf()}>"));
  assert.ok(selfBlock.includes('<label class="w-full">'), "upload stays inside the isSelf() gate");
  assert.ok(
    selfBlock.indexOf("Upload profile image") < selfBlock.indexOf("Regenerate avatar"),
    "upload stays above Regenerate avatar"
  );
  assert.ok(!selfBlock.includes("Choose File"), "no raw Choose File text remains");
});

test("#619: siblings share the .btn treatment; sidebar stack + 390px intact", () => {
  const selfBlock = REPOS.slice(REPOS.indexOf("<Show when={isSelf()}>"));
  for (const text of ["Upload profile image", "Regenerate avatar"]) {
    const at = selfBlock.indexOf(text);
    assert.ok(at !== -1, `${text} renders in the self block`);
    const open = selfBlock.lastIndexOf("<span", at) !== -1 && text === "Upload profile image"
      ? selfBlock.lastIndexOf("<span", at)
      : selfBlock.lastIndexOf("<button", at);
    const tag = selfBlock.slice(open, selfBlock.indexOf(">", open) + 1);
    assert.ok(tag.includes("btn"), `${text} composes .btn`);
    assert.ok(tag.includes("w-full"), `${text} is full-width (390px stacking sidebar)`);
    assert.ok(tag.includes("justify-center"), `${text} centers`);
  }
  const actionsAt = REPOS.indexOf("profile-actions");
  assert.ok(actionsAt !== -1, "action stack intact");
  const actionsTag = REPOS.slice(actionsAt, REPOS.indexOf(">", actionsAt) + 1);
  assert.ok(actionsTag.includes("flex-col"), "actions still stack vertically at 390px");
  assert.ok(actionsTag.includes("w-full"), "label wrapper constrains to the column (no overflow)");
});

test("#619: Tailwind-only, no new CSS, no new deps", () => {
  const label = block(REPOS, '<label class="w-full">', "</label>");
  assert.ok(!label.includes("<style"), "no one-off CSS for the upload button");
  const css = srcOf("../../src/ui.css");
  assert.ok(!css.includes("avatar-upload"), "no new upload rule in the shared sheet");
  assert.ok(!css.includes("profile-upload"), "no new profile-upload rule in the shared sheet");
  for (const dep of Object.keys({ ...PKG.dependencies, ...PKG.devDependencies })) {
    assert.ok(!/file-upload|dropzone|upload/i.test(dep), `no upload dependency added (${dep})`);
  }
});

test("#619: sibling raw inputs out of scope — Org/Release/import untouched", () => {
  const org = srcOf("../../src/pages/Org.jsx");
  assert.ok(org.includes('type="file"'), "Org.jsx avatar input untouched");
  const rel = srcOf("../../src/pages/Release.jsx");
  assert.ok(rel.includes('type="file"'), "Release.jsx input untouched");
});

test("#619: law 12 — FIXED amendment lives in docs/go/12_web_ui.md", () => {
  const doc = srcOf("../../../docs/go/12_web_ui.md");
  assert.ok(doc.includes("Forgejo #619"), "doc names the issue");
  assert.ok(doc.includes("Upload profile image"), "doc names the button");
});
