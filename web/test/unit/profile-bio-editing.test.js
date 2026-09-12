// web/test/unit/profile-bio-editing.test.js — Forgejo #420:
// the owner-profile rendered bio hides while the edit form is open —
// the form's own inline preview is the only rendered surface during
// editing. Pure client Show-condition change: no API, cache, or ETag
// impact. No DOM: JSX pinned as source text, mirroring
// profile-header.test.js / header-narrow.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const REPOS = srcOf("../../src/pages/Repos.jsx");
const ORG = srcOf("../../src/pages/Org.jsx");

// The <Show ...> opener immediately preceding a marker.
function showOpener(src, marker) {
  const i = src.indexOf(marker);
  assert.ok(i !== -1, `expected marker ${marker}`);
  const open = src.lastIndexOf("<Show", i);
  assert.ok(open !== -1, `expected <Show> before ${marker}`);
  return src.slice(open, i);
}

test("user header bio hides while the edit form is open", () => {
  const opener = showOpener(REPOS, "renderBody(profile().bio_markdown)");
  assert.ok(opener.includes("profile().bio_markdown"), "still gated on a set bio");
  assert.ok(opener.includes("!getEditing()"), "hidden whenever the edit form is open");
});

test("user header bio still reads the same doc through the same pipeline", () => {
  const opener = showOpener(REPOS, "renderBody(profile().bio_markdown)");
  assert.ok(opener.includes("profile().bio_markdown"), "no new data source, no cache-key change");
  assert.ok(REPOS.includes("innerHTML={renderBody(profile().bio_markdown)}"), "render pipeline unchanged");
});

test("edit form gating unchanged: editors only, onDone restores", () => {
  assert.ok(
    REPOS.includes("<Show when={getEditing() && getProfile()?.can_edit}>"),
    "form renders only for editors with the form open"
  );
  const done = REPOS.slice(REPOS.indexOf("onDone={(saved)"));
  assert.ok(done.includes("setEditing(false)"), "onDone closes the form (restores the bio)");
  assert.ok(done.includes("invalidate(`profile:${owner()}`)"), "save invalidates the profile doc");
});

test("form inline preview is the only rendered surface while editing", () => {
  const preview = showOpener(REPOS, "innerHTML={renderBody(getBio())}");
  assert.ok(preview.includes("getBio()"), "form keeps its live preview (textarea-driven, not the server doc)");
});

test("org landing header untouched: org bio never gated on editing", () => {
  const opener = showOpener(REPOS, "renderBody(getOrg().bio_markdown)");
  assert.ok(opener.includes("getOrg()?.bio_markdown"), "org bio still reads the org doc");
  assert.ok(!opener.includes("getEditing"), "orgs edit via /:org/settings — no inline form, no gate");
});

test("org settings surfaces untouched: read-only branch + ProfileTab form as today", () => {
  assert.ok(ORG.includes("read-only — org owner required"), "read-only branch kept");
  assert.ok(!ORG.includes("getEditing"), "no editing signal introduced in Org.jsx");
  assert.ok(ORG.includes("innerHTML={renderBody(o().bio_markdown)}"), "read-only rendered bio kept");
  assert.ok(ORG.includes("innerHTML={renderBody(getBio())}"), "settings-form inline preview kept");
});
