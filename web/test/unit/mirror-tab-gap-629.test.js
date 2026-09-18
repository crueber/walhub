// web/test/unit/mirror-tab-gap-629.test.js — Forgejo #629: the merged
// Mirror settings tab stacks the pull-mirror and push-mirror cards with
// touching borders, reading as one blob. The push card takes the file's
// stacked-section idiom (card mt-4 p-4, same as every other stacked
// section in Settings.jsx) so the two containers read as distinct.
// Class-only change: same cards, content, order, fetches. JSX pinned as
// source text, mirroring settings-mirror-merge-627.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const SETTINGS = srcOf("../../src/pages/Settings.jsx");

function tabRoot(src, tabFn) {
  const start = src.indexOf(`function ${tabFn}(`);
  assert.ok(start > 0, `${tabFn} found`);
  const section = src.indexOf("<section", start);
  return src.slice(section, src.indexOf(">", section) + 1);
}

test("push card carries the stacked-section gap; pull card does not", () => {
  assert.ok(tabRoot(SETTINGS, "PushMirrorTab").includes("card mt-4 p-4"), "push card separated below pull card");
  assert.ok(tabRoot(SETTINGS, "MirrorTab").includes("card p-4"), "pull card unchanged");
  assert.ok(!tabRoot(SETTINGS, "MirrorTab").includes("mt-4"), "no double gap above");
});

test("mirror branch still mounts both tabs, pull-then-push", () => {
  const branch = SETTINGS.slice(SETTINGS.indexOf('getTab() === "mirror"'));
  const end = branch.indexOf("</Show>");
  const show = branch.slice(0, end);
  assert.ok(show.includes("<MirrorTab"), "pull container mounted");
  assert.ok(show.includes("<PushMirrorTab"), "push container mounted");
  assert.ok(show.indexOf("<MirrorTab") < show.indexOf("<PushMirrorTab"), "pull-then-push order kept");
});

test("gap is the file idiom, no new CSS or dependencies", () => {
  const css = srcOf("../../src/ui.css");
  assert.ok(!css.includes("mirror-gap") && !css.includes("push-mirror +"), "no gap-scoped CSS");
  const matches = SETTINGS.match(/section class="card mt-4 p-4"/g) ?? [];
  assert.ok(matches.length >= 2, "gap uses the established stacked-section idiom");
});
