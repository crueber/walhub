// web/test/unit/pushmirror.test.js — Forgejo #623: push-mirror lib rules
// (schedules incl. off-default, auth kinds, next-sync phrasing, outcome
// line, create validation, credential fields) plus the settings-nav
// registration of the Push mirror tab.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  PUSHMIRROR_PRESETS,
  DEFAULT_PUSHMIRROR_SCHEDULE,
  PUSHMIRROR_AUTH_KINDS,
  isPushMirrorSchedule,
  isPushMirrorAuthKind,
  pushMirrorScheduleLabel,
  pushMirrorAuthLabel,
  formatPushNextSync,
  formatPushLastResult,
  formatPushHostKey,
  validatePushMirrorCreate,
  credentialFieldsFor,
} from "../../src/lib/pushmirror.js";
import { SETTINGS_GROUP, resolveSettingsTab } from "../../src/lib/settingsNav.js";

test("schedules are off + the five server names, off default, no freeform cron", () => {
  assert.deepEqual(
    PUSHMIRROR_PRESETS.map((p) => p.id),
    ["", "hourly", "8h", "daily", "weekly", "monthly"],
  );
  assert.equal(DEFAULT_PUSHMIRROR_SCHEDULE, "");
  for (const p of PUSHMIRROR_PRESETS) assert.ok(isPushMirrorSchedule(p.id), `preset: ${p.id || "(off)"}`);
  for (const bad of ["never", "0 0 * * * *", "@hourly", null, undefined]) {
    assert.equal(isPushMirrorSchedule(bad), false, `rejected: ${String(bad)}`);
  }
  assert.equal(pushMirrorScheduleLabel(""), "On push only (no schedule)");
  assert.equal(pushMirrorScheduleLabel("daily"), "Every day");
  assert.equal(pushMirrorScheduleLabel("nope"), "nope");
});

test("auth kinds are the four server names", () => {
  assert.deepEqual(PUSHMIRROR_AUTH_KINDS.map((k) => k.id), ["none", "password", "token", "ssh"]);
  for (const k of PUSHMIRROR_AUTH_KINDS) assert.ok(isPushMirrorAuthKind(k.id), k.id);
  assert.equal(isPushMirrorAuthKind("keys"), false);
  assert.equal(pushMirrorAuthLabel("ssh"), "SSH deploy key");
  assert.equal(pushMirrorAuthLabel("nope"), "nope");
});

test("formatPushNextSync phrases the view without throwing", () => {
  const now = Date.parse("2026-09-16T12:00:00Z");
  assert.equal(formatPushNextSync(null, now), "");
  // Off renders as on-push only, never a fire time.
  assert.equal(formatPushNextSync({ schedule: "" }, now), "on push only");
  assert.equal(formatPushNextSync({ schedule: "", due: true }, now), "on push only");
  assert.equal(formatPushNextSync({ schedule: "daily", due: true, last_synced_at: "2026-09-16T00:00:00Z" }, now), "sync due");
  assert.equal(
    formatPushNextSync({ schedule: "daily", last_synced_at: "" }),
    "never synced",
  );
  assert.equal(formatPushNextSync({ schedule: "daily", next_sync_at: "2026-09-16T12:30:00Z" }, now), "next sync in 30m");
  assert.equal(formatPushNextSync({ schedule: "daily", next_sync_at: "2026-09-16T15:00:00Z" }, now), "next sync in 3h");
  assert.equal(formatPushNextSync({ schedule: "daily", next_sync_at: "2026-09-19T12:00:00Z" }, now), "next sync in 3d");
  assert.equal(formatPushNextSync({ schedule: "daily", next_sync_at: "2026-09-16T12:00:30Z" }, now), "next sync < 1m");
  assert.equal(formatPushNextSync({ schedule: "daily", next_sync_at: "2026-09-16T11:00:00Z" }, now), "sync due");
  assert.equal(formatPushNextSync({ schedule: "daily", next_sync_at: "not-a-time" }, now), "next sync unknown");
});

test("formatPushLastResult degrades gracefully", () => {
  assert.equal(formatPushLastResult(null), "no push mirror");
  assert.equal(formatPushLastResult({}), "never synced");
  assert.equal(formatPushLastResult({ last_synced_at: "x" }), "ok");
  assert.equal(formatPushLastResult({ last_result: "failed: boom" }), "failed: boom");
});

test("validatePushMirrorCreate names the first problem", () => {
  assert.equal(validatePushMirrorCreate({ upstreamUrl: "", authKind: "none", schedule: "" }).error, "upstream URL is required");
  assert.equal(validatePushMirrorCreate({ upstreamUrl: "u", authKind: "keys", schedule: "" }).error, "pick an auth method");
  assert.equal(validatePushMirrorCreate({ upstreamUrl: "u", authKind: "none", schedule: "never" }).error, "pick a schedule");
  assert.deepEqual(validatePushMirrorCreate({ upstreamUrl: "u", authKind: "ssh", schedule: "" }), {});
});

test("credentialFieldsFor shows only the kind's fields (write-only: never prefilled)", () => {
  assert.deepEqual(credentialFieldsFor("none"), []);
  assert.deepEqual(credentialFieldsFor("password"), ["username", "password"]);
  assert.deepEqual(credentialFieldsFor("token"), ["username", "token"]);
  assert.deepEqual(credentialFieldsFor("ssh"), ["ssh_private_key", "ssh_public_key", "ssh_known_hosts"]);
  assert.deepEqual(credentialFieldsFor("bogus"), []);
});

test("Push mirror tab is registered in the settings sidebar", () => {
  const ids = SETTINGS_GROUP.map((t) => t.id);
  assert.ok(ids.includes("pushmirror"), "push mirror is a settings sidebar entry");
  assert.equal(resolveSettingsTab("pushmirror"), "pushmirror");
});

test("formatPushHostKey phrases the SSH trust status without throwing", () => {
  assert.equal(formatPushHostKey(null), "");
  assert.equal(formatPushHostKey({ auth_kind: "token" }), "");
  // SSH with no trust yet: accept-new pending state.
  assert.equal(
    formatPushHostKey({ auth_kind: "ssh" }),
    "not yet observed — first sync trusts on use",
  );
  // Learned trust: fingerprint + first-trusted day.
  assert.equal(
    formatPushHostKey({
      auth_kind: "ssh",
      host_key_fingerprint: "SHA256:abc",
      host_key_accepted_at: "2026-09-16T12:00:00Z",
    }),
    "SHA256:abc · first trusted 2026-09-16",
  );
  // Operator-pinned trust (no learn stamp).
  assert.equal(
    formatPushHostKey({ auth_kind: "ssh", host_key_fingerprint: "SHA256:abc" }),
    "SHA256:abc · pinned by operator",
  );
});
