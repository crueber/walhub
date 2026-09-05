// web/test/unit/settings-nav.test.js — settings sidebar nav model (issue #123):
// two clear sections (standard settings + danger zone), stable ids, and the
// hash helpers that deep-link sidebar entries without ever blanking the page.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  SETTINGS_GROUP,
  DANGER_GROUP,
  SETTINGS_TABS,
  DEFAULT_SETTINGS_TAB,
  resolveSettingsTab,
  settingsTabIdFromHash,
} from "../../src/lib/settingsNav.js";

test("sidebar has a standard listing plus a separate danger section", () => {
  assert.ok(SETTINGS_GROUP.length >= 6, "standard listing holds every settings tab");
  assert.deepEqual(DANGER_GROUP.map((t) => t.id), ["danger"]);
  assert.deepEqual(
    SETTINGS_TABS.map((t) => t.id),
    [...SETTINGS_GROUP.map((t) => t.id), "danger"],
    "danger zone is last, in its own section",
  );
});

test("WAL moved from the repo tab bar into the standard listing", () => {
  const ids = SETTINGS_GROUP.map((t) => t.id);
  assert.ok(ids.includes("wal"), "WAL is a settings sidebar entry");
  assert.ok(!DANGER_GROUP.some((t) => t.id === "wal"), "WAL is not in the danger section");
});

test("tab ids and labels are unique and non-empty", () => {
  for (const list of [SETTINGS_TABS]) {
    assert.deepEqual(new Set(list.map((t) => t.id)).size, list.length, "ids unique");
    assert.deepEqual(new Set(list.map((t) => t.label)).size, list.length, "labels unique");
    for (const t of list) {
      assert.match(t.id, /^[a-z]+$/, `id shape: ${t.id}`);
      assert.ok(t.label.length > 0, `label present: ${t.id}`);
    }
  }
});

test("default tab is a standard entry, never danger", () => {
  assert.equal(resolveSettingsTab(DEFAULT_SETTINGS_TAB), DEFAULT_SETTINGS_TAB);
  assert.ok(SETTINGS_GROUP.some((t) => t.id === DEFAULT_SETTINGS_TAB));
});

test("resolveSettingsTab accepts every entry, rejects everything else", () => {
  for (const t of SETTINGS_TABS) assert.equal(resolveSettingsTab(t.id), t.id);
  for (const bad of ["", "WAL", " wal", "settings", "delete", undefined, null, 0, {}, []]) {
    assert.equal(resolveSettingsTab(bad), null, `rejected: ${String(bad)}`);
  }
});

test("hash helper reads #id entries and ignores the rest", () => {
  assert.equal(settingsTabIdFromHash("#wal"), "wal");
  assert.equal(settingsTabIdFromHash("#danger"), "danger");
  assert.equal(settingsTabIdFromHash("#scheduled"), "scheduled");
  for (const bad of ["", "#", "#nope", "#WAL", "wal", "#wal/extra", undefined, null, 0]) {
    assert.equal(settingsTabIdFromHash(bad), null, `ignored: ${String(bad)}`);
  }
});
