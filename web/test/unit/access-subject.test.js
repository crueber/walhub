// web/test/unit/access-subject.test.js — issue #361: team subject picker.
//
// Headless cover for the picker/compose/validate logic
// (web/src/lib/access.js) plus source-structure guards pinning the
// Access-tab wiring: team dropdown fed by the owner org's team list,
// free-text fallback kept, client-side spelling validation with a
// friendly note, no backend change, no new deps.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

import {
  ACCESS_ORG_RE,
  ACCESS_SLUG_RE,
  looksLikeEmail,
  validateAccessSubject,
  composeTeamSubject,
  asTeamList,
  teamOptionLabel,
} from "../../src/lib/access.js";

test("rules mirror identity.ValidOrg / ValidSlug charsets", () => {
  assert.equal(ACCESS_ORG_RE.source, "^[a-z0-9-]{1,39}$");
  assert.equal(ACCESS_SLUG_RE.source, "^[a-z0-9-]{1,64}$");
  for (const ok of ["acme", "a", "x".repeat(39), "org-9"]) {
    assert.ok(ACCESS_ORG_RE.test(ok), `org ok: ${ok}`);
  }
  for (const bad of ["", "ACME", "has space", "under_score", "dot.name", "x".repeat(40), "org/slug"]) {
    assert.ok(!ACCESS_ORG_RE.test(bad), `org bad: ${bad}`);
  }
  assert.ok(ACCESS_SLUG_RE.test("x".repeat(64)));
  assert.ok(!ACCESS_SLUG_RE.test("x".repeat(65)));
});

test("looksLikeEmail catches typos, stays looser than the server", () => {
  for (const ok of ["jane@example.com", "SAM@Example.COM", "a+b@x.io"]) {
    assert.equal(looksLikeEmail(ok), true, `email ok: ${ok}`);
  }
  for (const bad of ["", "no-at-sign", "@nobody", "trailing@", "has space@x.io", "slash/a@x.io", "x".repeat(250) + "@x.io", null, undefined]) {
    assert.equal(looksLikeEmail(bad), false, `email bad: ${String(bad)}`);
  }
});

test("validateAccessSubject accepts user subjects, normalized", () => {
  assert.deepEqual(validateAccessSubject("user:jane@example.com"), { subject: "user:jane@example.com" });
  assert.deepEqual(validateAccessSubject("  USER:Sam@Example.COM  "), { subject: "user:sam@example.com" });
});

test("validateAccessSubject accepts team subjects, normalized like validateOrgName", () => {
  assert.deepEqual(validateAccessSubject("team:acme/frontend"), { subject: "team:acme/frontend" });
  assert.deepEqual(validateAccessSubject("Team:ACME/Frontend"), { subject: "team:acme/frontend" });
  assert.deepEqual(validateAccessSubject("  team:acme/platform-2  "), { subject: "team:acme/platform-2" });
});

test("validateAccessSubject rejects bad spellings with a friendly note", () => {
  for (const bad of ["", "   ", null, undefined]) {
    const v = validateAccessSubject(bad);
    assert.ok(v.error, `empty ${String(bad)} must error`);
    assert.match(v.error, /user:jane@example\.com/, "note shows a user example");
    assert.equal(v.subject, undefined);
  }
  const noPrefix = validateAccessSubject("jane@example.com");
  assert.match(noPrefix.error, /must start with "user:" or "team:"/, "missing prefix names both prefixes");
  const badUser = validateAccessSubject("user:not-an-email");
  assert.match(badUser.error, /user:jane@example\.com/, "bad user shows the user shape");
  for (const badTeam of ["team:acme", "team:acme/", "team:/frontend", "team:ACME!/x", "team:acme/a/b", "team:"]) {
    const v = validateAccessSubject(badTeam);
    assert.ok(v.error, `${badTeam} must error`);
    assert.match(v.error, /team:acme\/frontend/, "bad team shows the team shape");
  }
});

test("composeTeamSubject builds the server spelling from org + slug", () => {
  assert.equal(composeTeamSubject("acme", "frontend"), "team:acme/frontend");
  assert.equal(composeTeamSubject(" ACME ", " Frontend "), "team:acme/frontend");
  // Composed values always validate — the dropdown can never stage a
  // spelling the client itself would reject.
  assert.deepEqual(validateAccessSubject(composeTeamSubject("acme", "frontend")), {
    subject: "team:acme/frontend",
  });
});

test("asTeamList takes the bare-array teams.list shape, degrades to []", () => {
  const rows = [{ slug: "frontend", name: "Frontend" }, { slug: "platform" }];
  assert.deepEqual(asTeamList(rows), rows);
  assert.deepEqual(asTeamList([]), []);
  for (const empty of [null, undefined, {}, { teams: null }, "nope"]) {
    assert.deepEqual(asTeamList(empty), [], `degrades: ${String(empty)}`);
  }
  assert.deepEqual(asTeamList({ teams: rows }), rows, "envelope tolerated");
  assert.deepEqual(asTeamList([{ slug: "ok" }, null, {}, { slug: 7 }, { slug: "" }]), [{ slug: "ok" }], "slugless rows dropped");
});

test("teamOptionLabel prefers slug — name, hides redundant names", () => {
  assert.equal(teamOptionLabel({ slug: "frontend", name: "Frontend" }), "frontend");
  assert.equal(teamOptionLabel({ slug: "frontend", name: "Web UI" }), "frontend — Web UI");
  assert.equal(teamOptionLabel({ slug: "frontend" }), "frontend");
  assert.equal(teamOptionLabel({ slug: "frontend", name: "" }), "frontend");
});

const access = () => srcOf("../../src/pages/Access.jsx");

test("Access tab fetches the owner org's team list for the picker", () => {
  const s = access();
  assert.ok(s.includes('from "../../sdk/src/index.js"'), "reuses the default SDK client (Org.jsx precedent), no new dep");
  assert.ok(s.includes("repos.orgs.teams.list"), "picker is fed by client.orgs.teams.list");
  assert.ok(s.includes("org-teams:"), "team roster cached under its own data key");
  assert.ok(s.includes("asTeamList"), "payload normalized through the headless helper");
});

test("Access tab offers a discoverable team dropdown beside the free-text input", () => {
  const s = access();
  assert.ok(s.includes("pick a team"), "dropdown has a discoverable placeholder");
  assert.ok(s.includes("teamOptionLabel"), "options render slug — name via the helper");
  assert.ok(s.includes("pickTeam"), "choosing a team composes the subject");
  assert.ok(s.includes("composeTeamSubject"), "composition goes through the helper");
  // Free-text fallback kept: the original input, placeholder, and label.
  assert.ok(s.includes('placeholder="user:jane@example.com"'), "free-text placeholder kept");
  assert.ok(s.includes("subject (user:email or team:org/slug)"), "subject label kept");
  // The dropdown only renders when the owner org actually has teams —
  // user-owned repos and denied/empty lists keep the text-only form.
  assert.ok(s.includes("(getTeamRows() ?? []).length > 0"), "dropdown gated on a non-empty roster");
  assert.ok(s.includes("then(asTeamList, () => [])"), "list failure degrades to free text");
});

test("Access tab validates spelling client-side with a friendly note", () => {
  const s = access();
  assert.ok(s.includes("validateAccessSubject"), "add path validates through the helper");
  assert.ok(s.includes("setNote(checked.error)"), "invalid spellings surface the friendly note");
  assert.ok(!s.includes("if (!sub) return;"), "empty subjects no longer silently no-op");
});

test("picker adds no popover, no CSS, no request beyond the one roster GET", () => {
  const s = access();
  assert.ok(s.includes("<select"), "team picker is a native select — no popover idiom needed");
  assert.ok(!s.includes("absolute"), "no absolutely-positioned panel, so the #278 viewport bound is unaffected");
  assert.ok(!s.includes("label-drop"), "no new panel class");
  assert.equal((s.match(/repos\.orgs\.teams\.list/g) ?? []).length, 1, "exactly one roster call site (single GET, never a hot path)");
  assert.ok(!s.includes("teams.create"), "no team mutations — parent access PUT owns the save");
});
