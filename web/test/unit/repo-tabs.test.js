// web/test/unit/repo-tabs.test.js — the repo tab matcher (issue #25, Bug 1):
// the active tab derives from the FIRST segment after /:owner/:name, so blob
// and tree paths whose filenames contain a tab word (checks.go, wal.go,
// settings.json, …) still highlight Code.

import { test } from "node:test";
import assert from "node:assert/strict";

import { activeTab, tabBadge } from "../../src/lib/tabs.js";

const CODE = [
  "/o/r",
  "/o/r/",
  "/o/r/tree/main",
  "/o/r/tree/main/cmd/walhub",
  "/o/r/blob/main/README.md",
  // Adversarial filenames: every tab word as a *filename* must stay on Code.
  "/o/r/blob/main/cmd/walhub/checks.go",
  "/o/r/blob/main/wal.go",
  "/o/r/blob/main/settings.json",
  "/o/r/blob/main/issues.md",
  "/o/r/blob/main/pulls.txt",
  "/o/r/blob/main/releases.md",
  "/o/r/blob/main/commits.log",
  "/o/r/blob/main/branches.txt",
  "/o/r/blob/main/tags.yaml",
  "/o/r/blob/main/commit-msg.txt",
  "/o/r/blob/main/pull_request.md",
  "/o/r/tree/main/checks",
  "/o/r/tree/main/releases/v1",
  "/o/r/tree/feat%2Fchecks/cmd",
  "/o/r/blob/checks/cmd/walhub/checks.go", // ref literally named "checks"
];

const SECTIONS = [
  ["/o/r/commits", "commits"],
  ["/o/r/commits?path=cmd%2Fwalhub", "commits"],
  ["/o/r/commit/abc123def456", "commits"],
  ["/o/r/issues", "issues"],
  ["/o/r/issues/new", "issues"],
  ["/o/r/issues/12", "issues"],
  ["/o/r/labels", "issues"],
  ["/o/r/milestones", "issues"],
  ["/o/r/pulls", "pulls"],
  ["/o/r/pull/7", "pulls"],
  ["/o/r/pull/7/files", "pulls"],
  // WAL moved into the settings sidebar (issue #123): the kept /wal route
  // highlights Settings instead of a tab that no longer exists.
  ["/o/r/wal", "settings"],
  ["/o/r/settings", "settings"],
  ["/o/r/settings/policy", "settings"],
  ["/o/r/checks", "checks"],
  ["/o/r/releases", "releases"],
  ["/o/r/releases/v1.0", "releases"],
];

test("blob/tree paths — incl. adversarial tab-word filenames — highlight Code", () => {
  for (const path of CODE) assert.equal(activeTab(path), "code", path);
});

test("section paths highlight their own tab", () => {
  for (const [path, want] of SECTIONS) assert.equal(activeTab(path), want, path);
});

test("short, unknown, and empty paths fall back to Code", () => {
  // branches/tags have no tab and no route: a highlight-nothing id would
  // leave the tab bar blank, so they fall back to Code like any unknown.
  for (const path of ["/", "/o", "/o/r/foobar", "/o/r/branches", "/o/r/tags", "", undefined, null]) {
    assert.equal(activeTab(path), "code", String(path));
  }
});

// Issue #319: open-count badges next to the Issues and Pulls tabs. The
// tab render shows the badge only when the numerator is > 0 (GitHub
// semantics: no zero badges) — tabBadge maps everything else to 0.

test("tabBadge returns the open count for issues/pulls tabs", () => {
  const summary = { open_issues: 3, open_pulls: 1 };
  assert.equal(tabBadge(summary, "issues"), 3);
  assert.equal(tabBadge(summary, "pulls"), 1);
});

test("tabBadge hides at 0: other tabs, missing summary, pre-#319 servers", () => {
  const summary = { open_issues: 0, open_pulls: 0 };
  for (const tab of ["code", "commits", "issues", "pulls", "checks", "releases", "settings"]) {
    assert.equal(tabBadge(summary, tab), 0, tab);
  }
  // Loading (undefined), deleted (null), and pre-#319 servers (no fields).
  for (const s of [undefined, null, {}]) {
    assert.equal(tabBadge(s, "issues"), 0, String(s));
    assert.equal(tabBadge(s, "pulls"), 0, String(s));
  }
  // Unknown tab ids never badge, even with counts present.
  assert.equal(tabBadge({ open_issues: 5, open_pulls: 5 }, "wal"), 0);
});
