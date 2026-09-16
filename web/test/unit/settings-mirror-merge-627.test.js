// web/test/unit/settings-mirror-merge-627.test.js — Forgejo #627: the
// Settings sidebar carries ONE Mirror entry rendering TWO containers
// (pull MirrorTab, then push PushMirrorTab). Source-text pins mirroring
// settings-features-522.test.js: the registry holds a single mirror entry,
// "pushmirror" survives only as a resolve alias, and the mirror <Show>
// branch mounts both tabs in sidebar order with no pushmirror branch left.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const SETTINGS = srcOf("../../src/pages/Settings.jsx");
const NAV = srcOf("../../src/lib/settingsNav.js");
const DOC = srcOf("../../../docs/go/12_web_ui.md");

import {
  SETTINGS_GROUP,
  resolveSettingsTab,
  settingsTabIdFromHash,
} from "../../src/lib/settingsNav.js";

test("registry holds exactly one Mirror entry, no pushmirror entry", () => {
  const ids = SETTINGS_GROUP.map((t) => t.id);
  assert.equal(ids.filter((id) => id === "mirror").length, 1, "one Mirror row");
  assert.ok(!ids.includes("pushmirror"), "pushmirror is not a sidebar entry");
  assert.ok(!SETTINGS_GROUP.some((t) => t.label === "Push mirror"), "no Push mirror label");
});

test("pushmirror is an explicit mirror alias, unknown still falls back", () => {
  assert.equal(resolveSettingsTab("pushmirror"), "mirror");
  assert.equal(settingsTabIdFromHash("#pushmirror"), "mirror");
  assert.equal(settingsTabIdFromHash("#mirror"), "mirror");
  assert.equal(resolveSettingsTab("pushmirrors"), null, "near-miss still rejected");
  assert.ok(NAV.includes('id === "pushmirror"'), "alias is explicit in settingsNav.js, not a fallback accident");
});

test("mirror tab renders both containers, pull then push", () => {
  const start = SETTINGS.indexOf('getTab() === "mirror"');
  assert.ok(start !== -1, "mirror branch exists");
  const lineEnd = SETTINGS.indexOf("\n", start);
  const line = SETTINGS.slice(start, lineEnd);
  assert.ok(line.includes("<MirrorTab"), "mirror branch mounts MirrorTab");
  assert.ok(line.includes("<PushMirrorTab"), "mirror branch mounts PushMirrorTab");
  assert.ok(
    line.indexOf("<MirrorTab") < line.indexOf("<PushMirrorTab"),
    "pull MirrorTab renders before push PushMirrorTab (sidebar order)",
  );
  assert.ok(line.includes("<MirrorTab ctx={ctx} repo={repo}"), "MirrorTab props unchanged");
  assert.ok(line.includes("<PushMirrorTab ctx={ctx} repo={repo}"), "PushMirrorTab props unchanged");
});

test("no pushmirror tab branch remains", () => {
  assert.ok(!SETTINGS.includes('getTab() === "pushmirror"'), "pushmirror <Show> branch deleted");
  assert.ok(!SETTINGS.includes("#pushmirror"), "no #pushmirror deep link authored in Settings.jsx");
});

test("law 12: docs amendment for Forgejo #627", () => {
  assert.ok(DOC.includes("Forgejo #627"), "docs/go/12_web_ui.md records the #627 decision");
});
