// web/test/unit/checks-tab-505.test.js — issue #505: the Checks tab hides
// on check-less repos and reappears on the first report. The visibility
// rule is the pure showChecksTab helper in web/src/lib/tabs.js (the
// settingsNav convention — no Solid, no DOM); the shell wiring (tab
// filter, hidden-state stream) is pinned as source text, mirroring
// fork-pill-split-464.test.js (#527 retired the header reporting link). The deep-link
// route renders regardless — activeTab keeps mapping checks/check
// segments even when the tab is hidden.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import { activeTab, showChecksTab } from "../../src/lib/tabs.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const REPO = srcOf("../../src/pages/Repo.jsx");

test("showChecksTab: the flag decides, everything unknown shows", () => {
  assert.equal(showChecksTab({ has_checks: true }), true);
  assert.equal(showChecksTab({ has_checks: false }), false);
  // Loading (undefined), deleted (null), and pre-#505 servers (no
  // field) keep the tab — hiding on unknown would flicker the strip
  // on every load and strand old servers with no Checks entry.
  for (const s of [undefined, null, {}, { open_issues: 0 }]) {
    assert.equal(showChecksTab(s), true, String(s));
  }
});

test("deep links stay sane while hidden: checks/check segments still map", () => {
  // The route renders its empty state regardless of the tab (no
  // 404/bounce); the matcher is untouched by the visibility rule.
  for (const path of ["/o/r/checks", "/o/r/checks/abc123def456", "/o/r/check"]) {
    assert.equal(activeTab(path), "checks", path);
  }
  // And the helper never feeds the matcher — a hidden tab still
  // highlights when the URL is a checks page.
  assert.equal(showChecksTab({ has_checks: false }), false);
  assert.equal(activeTab("/o/r/checks"), "checks");
});

test("Repo.jsx: Checks tab renders through the visibility helper", () => {
  assert.ok(REPO.includes("showChecksTab"), "shell imports the visibility helper");
  assert.ok(REPO.includes("<For each={TABS}>"), "nav still iterates the full TABS list (the #274 pin)");
  assert.ok(
    REPO.includes('t.id !== "checks" || showChecksTab(getSummary(), { denied: summaryDenied() })'),
    "the Checks entry — and only it — gates on the shared summary flag plus the #513 denial flag",
  );
  assert.ok(REPO.includes('id: "checks"'), "the TABS model keeps the checks entry (filter is runtime-only)");
});

test("Repo.jsx: hidden-state shell stream reappears the tab without reload", () => {
  assert.ok(REPO.includes("useCollabStream"), "shell holds a collab subscription");
  assert.ok(REPO.includes('["check"]'), "the shell subscription watches check frames only");
  assert.ok(REPO.includes("checksHidden"), "the stream mounts only while the tab is hidden");
  assert.ok(REPO.includes("createMemo"), "a boolean memo guards the effect against per-refresh reconnects");
});

test("Repo.jsx: header meta line drops the reporting-API link (#527)", () => {
  // #527 retires the #505 meta-line pointer: the header renders
  // branches · tags only while the tab is hidden. Discoverability
  // for check-less repos lives in Settings → CI tokens; the Checks
  // toolbar link itself is untouched.
  const meta = REPO.slice(REPO.indexOf("repo-meta"));
  assert.ok(!meta.includes('href="/api#checks-ci"'), "header keeps no reporting-API link");
  assert.ok(!meta.includes("reporting API"), "no reporting-API label in the header meta line");
  assert.ok(!meta.includes("!showChecksTab(s())"), "no hidden-state conditional link in the meta line");
  const checks = srcOf("../../src/pages/Checks.jsx");
  assert.ok(checks.includes('href="/api#checks-ci"'), "the Checks toolbar reporting link is untouched");
});
