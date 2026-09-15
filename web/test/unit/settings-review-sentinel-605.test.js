// web/test/unit/settings-review-sentinel-605.test.js — Forgejo #605:
// Settings → General "Code review" stuck on "loading…" forever for
// repos without [review] allow_self_approval (the common case).
// extractSelfApproval() returns null for unset (contract: null = unset
// → render checked, server default allowed), but the prefill used
// getSelf() === null as the unseeded sentinel — seeding null never left
// the sentinel — and the <Show> gate (getSelf() !== null) + save
// disabled keyed on the same sentinel. Option A: unseeded is
// undefined; null keeps meaning seeded-but-unset → checked.
// extractSelfApproval()'s null contract is untouched (pinned in
// repo-review-586.test.js).

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

const general = () => block(SETTINGS, "function GeneralTab(props)", "// --- tab 1: scheduled tasks");

test("signals start undefined (unseeded), never null", () => {
  const g = general();
  assert.ok(g.includes("const [getSelf, setSelf] = createSignal(undefined)"), "value starts unseeded");
  assert.ok(g.includes("const [getSelfBase, setSelfBase] = createSignal(undefined)"), "baseline starts unseeded");
});

test("prefill seeds exactly once on undefined, so unset null still leaves the sentinel", () => {
  const g = general();
  assert.ok(g.includes("getSelf() === undefined"), "prefill checks the unseeded sentinel");
  assert.ok(!g.includes("getSelf() === null"), "no null-sentinel prefill remains to re-trap unset docs");
});

test("<Show> gate and save disabled key on undefined, so seeded null renders instead of loading", () => {
  const g = general();
  assert.ok(g.includes("<Show when={getSelf() !== undefined}"), "unset renders the toggle, not the loading fallback");
  assert.ok(g.includes("disabled={getSelf() === undefined}"), "save enables once the doc is seeded (even when unset)");
});

test("null keeps meaning unset → checked (default-ON contract untouched)", () => {
  const g = general();
  assert.ok(g.includes("checked={getSelf() !== false}"), "unset (null) renders checked; only explicit false denies");
  assert.ok(g.includes("const allowed = getSelf() !== false"), "save maps unset to allowed");
});

test("explicit true/false seeds still render checked/unchecked through the same expression", () => {
  // checked={getSelf() !== false}: true → checked, false → unchecked.
  // Headless pin of the render expression (Settings.jsx keeps the DOM;
  // repoReview.js keeps the pure TOML mapping, pinned in repo-review-586).
  const g = general();
  const m = g.match(/checked=\{getSelf\(\) !== false\}/);
  assert.ok(m, "single render expression covers true/false/null without branching");
});
