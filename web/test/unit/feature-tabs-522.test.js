// web/test/unit/feature-tabs-522.test.js — Forgejo #522: the
// Issues/Pulls/Releases tabs hide when their feature flag is explicitly
// off, stay fail-open on unknown (loading, deleted, pre-#522 servers),
// and their paths re-highlight onto Code while the routes still map
// (deep links stay sane — the showChecksTab pattern). The shell wiring
// (tab filter, activeTab threading) is pinned as source text, mirroring
// checks-tab-505.test.js.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import {
  activeTab,
  showFeatureTab,
  isFeatureTabDisabled,
  FEATURE_TABS,
} from "../../src/lib/tabs.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const REPO = srcOf("../../src/pages/Repo.jsx");

test("FEATURE_TABS names exactly the flag-gated tabs", () => {
  assert.deepEqual([...FEATURE_TABS].sort(), ["issues", "pulls", "releases"]);
});

test("showFeatureTab: explicit false hides, everything unknown shows", () => {
  assert.equal(showFeatureTab({ features: { issues: false } }, "issues"), false);
  assert.equal(showFeatureTab({ features: { pulls: false } }, "pulls"), false);
  assert.equal(showFeatureTab({ features: { releases: false } }, "releases"), false);
  // Sibling flags don't leak across tabs.
  assert.equal(showFeatureTab({ features: { issues: false } }, "pulls"), true);
  // Loading (undefined), deleted (null), pre-#522 servers (no field),
  // and non-feature tabs keep showing — fail-open, the showChecksTab
  // discipline.
  for (const s of [undefined, null, {}, { open_issues: 1 }, { features: null }]) {
    for (const tab of ["issues", "pulls", "releases"]) {
      assert.equal(showFeatureTab(s, tab), true, `${String(s)} / ${tab}`);
    }
  }
  for (const tab of ["code", "commits", "checks", "releases-x", "settings"]) {
    assert.equal(showFeatureTab({ features: { issues: false } }, tab), true, tab);
  }
});

test("isFeatureTabDisabled mirrors the gate per tab", () => {
  assert.equal(isFeatureTabDisabled({ features: { releases: false } }, "releases"), true);
  assert.equal(isFeatureTabDisabled({ features: { releases: false } }, "code"), false);
  assert.equal(isFeatureTabDisabled(undefined, "issues"), false);
});

test("activeTab without a summary keeps the legacy mapping", () => {
  // The second param is optional: every existing caller and the
  // repo-tabs matrix keep passing one argument.
  for (const [path, want] of [
    ["/o/r/issues", "issues"],
    ["/o/r/issues/new", "issues"],
    ["/o/r/pulls", "pulls"],
    ["/o/r/releases/v1.0", "releases"],
    ["/o/r/checks", "checks"],
  ]) {
    assert.equal(activeTab(path), want, path);
  }
});

test("activeTab with a summary re-highlights disabled sections onto Code", () => {
  const off = (k) => ({ features: { [k]: false } });
  assert.equal(activeTab("/o/r/issues", off("issues")), "code");
  assert.equal(activeTab("/o/r/issues/12", off("issues")), "code");
  assert.equal(activeTab("/o/r/pulls", off("pulls")), "code");
  assert.equal(activeTab("/o/r/pull/7/files", off("pulls")), "code");
  assert.equal(activeTab("/o/r/releases", off("releases")), "code");
  // Enabled flags and unknown summaries keep the section highlight…
  assert.equal(activeTab("/o/r/issues", { features: { issues: true } }), "issues");
  assert.equal(activeTab("/o/r/issues", undefined), "issues");
  assert.equal(activeTab("/o/r/issues", {}), "issues");
  // …and non-feature sections never re-highlight (checks keeps its own
  // helper; labels/milestones still map to issues when enabled).
  assert.equal(activeTab("/o/r/checks", off("issues")), "checks");
  assert.equal(activeTab("/o/r/labels", off("pulls")), "issues");
  assert.equal(activeTab("/o/r/pulls", off("issues")), "pulls");
});

test("Repo.jsx: Issues/Pulls/Releases tabs render through the visibility helper", () => {
  assert.ok(REPO.includes("showFeatureTab"), "shell imports the visibility helper");
  assert.ok(
    REPO.includes('showFeatureTab(getSummary(), t.id)'),
    "the feature-gated entries filter on the shared summary flags",
  );
  for (const id of ['"issues"', '"pulls"', '"releases"']) {
    assert.ok(REPO.includes(`id: ${id}`), `the TABS model keeps the ${id} entry (filter is runtime-only)`);
  }
});

test("Repo.jsx: activeTab highlights thread the shared summary", () => {
  assert.ok(
    REPO.includes("activeTab(location.pathname, getSummary())"),
    "tab highlight re-maps disabled sections onto Code from the shared summary",
  );
});
