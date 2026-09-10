// web/test/unit/mirror.test.js — Forgejo #240: mirror lib rules
// (presets, next-sync phrasing, outcome line, create validation) plus
// the settings-nav registration of the Mirror tab.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  MIRROR_PRESETS,
  DEFAULT_MIRROR_PRESET,
  isMirrorPreset,
  mirrorPresetLabel,
  formatNextSync,
  formatLastResult,
  validateMirrorCreate,
  mirrorRowBadge,
} from "../../src/lib/mirror.js";
import { SETTINGS_GROUP, resolveSettingsTab } from "../../src/lib/settingsNav.js";

test("presets are the five server names, daily default, no freeform cron", () => {
  assert.deepEqual(MIRROR_PRESETS.map((p) => p.id), ["hourly", "8h", "daily", "weekly", "monthly"]);
  assert.equal(DEFAULT_MIRROR_PRESET, "daily");
  for (const p of MIRROR_PRESETS) assert.ok(isMirrorPreset(p.id), p.id);
  for (const bad of ["never", "0 0 * * * *", "@hourly", "", null, undefined]) {
    assert.equal(isMirrorPreset(bad), false, `rejected: ${String(bad)}`);
  }
  assert.equal(mirrorPresetLabel("daily"), "Every day");
  assert.equal(mirrorPresetLabel("nope"), "nope");
});

test("formatNextSync phrases the view without throwing", () => {
  const now = Date.parse("2026-09-09T12:00:00Z");
  assert.equal(formatNextSync(null, now), "");
  assert.equal(formatNextSync({ due: true, last_synced_at: "2026-09-09T00:00:00Z" }, now), "sync due");
  assert.equal(
    formatNextSync({ last_synced_at: "", next_sync_at: "" }, now),
    "never synced — first sync pending",
  );
  assert.equal(formatNextSync({ next_sync_at: "2026-09-09T12:30:00Z" }, now), "next sync in 30m");
  assert.equal(formatNextSync({ next_sync_at: "2026-09-09T15:00:00Z" }, now), "next sync in 3h");
  assert.equal(formatNextSync({ next_sync_at: "2026-09-12T12:00:00Z" }, now), "next sync in 3d");
  assert.equal(formatNextSync({ next_sync_at: "2026-09-09T12:00:30Z" }, now), "next sync < 1m");
  assert.equal(formatNextSync({ next_sync_at: "2026-09-09T11:00:00Z" }, now), "sync due");
  assert.equal(formatNextSync({ next_sync_at: "not-a-time" }, now), "next sync unknown");
});

test("formatLastResult degrades gracefully", () => {
  assert.equal(formatLastResult(null), "not a mirror");
  assert.equal(formatLastResult({}), "never synced");
  assert.equal(formatLastResult({ last_synced_at: "x" }), "ok");
  assert.equal(formatLastResult({ last_result: "failed: boom" }), "failed: boom");
});

test("validateMirrorCreate names the first problem", () => {
  assert.equal(validateMirrorCreate({ sourceUrl: "", owner: "a", name: "b", schedule: "daily" }).error, "source URL is required");
  assert.equal(validateMirrorCreate({ sourceUrl: "u", owner: "", name: "b", schedule: "daily" }).error, "owner and name are required");
  assert.equal(validateMirrorCreate({ sourceUrl: "u", owner: "a", name: "b", schedule: "never" }).error, "pick a schedule preset");
  assert.deepEqual(validateMirrorCreate({ sourceUrl: "u", owner: "a", name: "b", schedule: "daily" }), {});
});

test("Mirror tab is registered in the settings sidebar", () => {
  const ids = SETTINGS_GROUP.map((t) => t.id);
  assert.ok(ids.includes("mirror"), "mirror is a settings sidebar entry");
  assert.equal(resolveSettingsTab("mirror"), "mirror");
});

test("mirrorRowBadge turns the listing flag into badge presence/label (Forgejo #281)", () => {
  // Non-mirrors (and missing rows) render no badge.
  assert.deepEqual(mirrorRowBadge({ mirror: false }), { show: false, label: "", title: "" });
  assert.deepEqual(mirrorRowBadge({}), { show: false, label: "", title: "" });
  assert.deepEqual(mirrorRowBadge(null), { show: false, label: "", title: "" });
  assert.deepEqual(mirrorRowBadge(undefined), { show: false, label: "", title: "" });
  // Mirror with a known upstream names it (the accessible label).
  const named = mirrorRowBadge({ mirror: true, upstream: "https://example.com/up.git" });
  assert.equal(named.show, true);
  assert.equal(named.label, "mirror");
  assert.equal(named.title, "mirror of https://example.com/up.git · pull-only");
  // Mirror with an unparseable/absent upstream still badges (fail closed), unnamed.
  for (const row of [{ mirror: true }, { mirror: true, upstream: "" }, { mirror: true, upstream: "   " }]) {
    const b = mirrorRowBadge(row);
    assert.equal(b.show, true, `show: ${JSON.stringify(row)}`);
    assert.equal(b.label, "mirror");
    assert.equal(b.title, "mirror · pull-only");
  }
});
