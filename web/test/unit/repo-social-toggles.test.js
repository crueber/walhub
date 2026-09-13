// web/test/unit/repo-social-toggles.test.js — Forgejo #447: the repo header
// star/watch toggles render icon + count LEFT of a text label ("★ 3 Star",
// "👁 2 Watch") — the canonical idiom the Fork link and Clone trigger share.
// (Issue #285 pinned icon+count-only with no words; #447 supersedes that
// direction — the label is what unifies the four controls into one strip.)
// The accessible name still carries the verb: title + aria-label keep it and
// aria-pressed keeps the state for assistive tech. No DOM: JSX is pinned as
// source text, mirroring clone-outside-close.test.js / nav-api-right.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const repo = () => srcOf("../../src/pages/Repo.jsx");

function block(src, startMarker, endMarker) {
  const s = src.indexOf(startMarker);
  assert.ok(s !== -1, `expected block start ${startMarker}`);
  const e = src.indexOf(endMarker, s);
  assert.ok(e !== -1, `expected block end ${endMarker}`);
  return src.slice(s, e);
}

test("StarToggle renders count left of the Star label", () => {
  const star = block(repo(), "function StarToggle(props)", "function TasksOverlay");
  assert.ok(star.includes("★ {s().stars ?? 0} Star"), "star toggle renders ★ + live count + Star label, count first");
});

test("WatchToggle renders count left of the Watch label", () => {
  const watch = block(repo(), "function WatchToggle(props)", "function RefPicker");
  assert.ok(watch.includes("👁 {w().watchers ?? 0} Watch"), "watch toggle renders 👁 + live count + Watch label, count first");
});

test("toggles keep accessible names + pressed state + active styling", () => {
  const src = repo();
  const star = block(src, "function StarToggle(props)", "function TasksOverlay");
  const watch = block(src, "function WatchToggle(props)", "function RefPicker");
  for (const [name, b] of [["StarToggle", star], ["WatchToggle", watch]]) {
    assert.ok(b.includes("aria-pressed"), `${name} keeps aria-pressed`);
    assert.ok(b.includes("aria-label="), `${name} carries the verb in aria-label`);
    assert.ok(b.includes("title="), `${name} keeps the tooltip title`);
    assert.ok(b.includes("primary"), `${name} keeps the active/filled primary treatment`);
    assert.ok(b.includes("onClick={flip}"), `${name} stays one-click togglable`);
  }
  assert.ok(star.includes('"Unstar this repo"') && star.includes('"Star this repo"'), "star aria-label/title carry the verb");
  assert.ok(watch.includes('"Unwatch this repo"') && watch.includes('"Watch this repo"'), "watch aria-label/title carry the verb");
});
