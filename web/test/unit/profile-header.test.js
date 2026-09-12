// web/test/unit/profile-header.test.js — Forgejo #395 (#390 follow-up):
// the /:owner user-profile header is one composed identity block —
// identity left (username h1, handle, location · timezone, bio, grouped
// action row), avatar right (h-24 circle with Regenerate/Remove grouped
// beneath it) — instead of #390's orphan justify-end avatar row floating
// above a header row that knew nothing about it (#403: the orphan CTA row
// above the header joined Edit profile in the grouped row under the bio;
// #413: the New-repository CTA left the header entirely for the
// Repositories toolbar, leaving Edit profile alone in the action row).
// Layout only: every gate (Edit = server can_edit,
// Regenerate/Remove = self-only, #376 invalidation, org path #359) is
// asserted unchanged. Orgs keep their own header untouched. No DOM:
// JSX pinned as source text, mirroring header-narrow.test.js /
// identity-nav.test.js. Light/dark share the treatment (layout classes
// + theme-independent ring classes on both sides of dark:).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const REPOS = srcOf("../../src/pages/Repos.jsx");

function block(src, start, end) {
  const s = src.indexOf(start);
  assert.ok(s !== -1, `expected block start ${start}`);
  const e = src.indexOf(end, s);
  assert.ok(e !== -1, `expected block end ${end}`);
  return src.slice(s, e);
}

test("composed grid: two columns on desktop, stacked on 390px with avatar on top", () => {
  const grid = block(REPOS, "profile-header", "</div>\n        <Show when={getEditing()");
  assert.ok(grid.includes("flex flex-col-reverse"), "base stacks (390px: no dead half-width band)");
  assert.ok(grid.includes("sm:flex-row"), "desktop restores the two-column row");
  assert.ok(grid.includes("sm:items-start sm:justify-between"), "desktop anchors identity left, avatar right");
  assert.ok(grid.includes("min-w-0 flex-1"), "identity column flexes (long names/bios cannot push the avatar out)");
  assert.ok(grid.includes("shrink-0"), "avatar column never shrinks");
  // flex-col-reverse + identity-first DOM: the h1 stays first for
  // heading/reading order while the avatar block paints on top when
  // stacked.
  assert.ok(grid.indexOf("<h1") < grid.indexOf("profile-avatar"), "identity precedes the avatar in DOM order");
});

test("identity column: username h1, handle, location · timezone, bio, Edit profile", () => {
  const grid = block(REPOS, "profile-header", "</div>\n        <Show when={getEditing()");
  assert.ok(grid.includes("<h1"), "username is the page h1 (was the shared h2)");
  assert.ok(grid.includes("{displayName()}"), "h1 renders the display name (owner slug when unset)");
  assert.ok(grid.includes("@{owner()}"), "handle renders under the name");
  assert.ok(grid.includes('join(" · ")'), "location · timezone separator kept");
  assert.ok(grid.includes("renderBody(profile().bio_markdown)"), "bio renders through the shared markdown pipeline");
  assert.ok(grid.includes("Edit profile"), "Edit profile lives with the identity content");
  assert.ok(grid.includes("getProfile()?.can_edit"), "Edit gate unchanged (server can_edit, client never decides)");
});

test("avatar actions sit with the avatar they act on", () => {
  const col = block(REPOS, "profile-avatar", "</div>\n          </Show>");
  assert.ok(col.includes("h-24 w-24"), "avatar at profile scale");
  assert.ok(col.includes("Regenerate avatar"), "Regenerate grouped under the avatar");
  assert.ok(col.includes("Remove avatar"), "Remove grouped under the avatar");
  assert.ok(col.indexOf("h-24 w-24") < col.indexOf("Regenerate avatar"), "actions render beneath the avatar");
  assert.ok(col.includes("sm:flex-col"), "actions stack under the avatar on desktop, wrap beside it at 390px");
});

test("avatar column renders for self even without an avatar (Regenerate opts back in)", () => {
  const gate = REPOS.slice(REPOS.indexOf("profile-avatar") - 200, REPOS.indexOf("profile-avatar"));
  assert.ok(gate.includes("userSrc() || isSelf()"), "column shows on avatar OR self (no-avatar self keeps Regenerate)");
  assert.ok(REPOS.includes("isSelf()"), "self gate kept (server re-checks; client never decides)");
  assert.ok(REPOS.includes('invalidate(`user:${owner()}`)'), "#376 avatar invalidation untouched");
  assert.ok(REPOS.includes('invalidate("me")'), "navbar avatar invalidation untouched");
});

test("action row carries Edit profile only; New repository moved to the toolbar (#413)", () => {
  const userShow = block(REPOS, "<Show when={!isOrg()}>", "<h3");
  const grid = userShow.indexOf("profile-header");
  assert.ok(grid !== -1, "profile header grid exists");
  assert.ok(!userShow.slice(0, grid).includes("New repository"), "no orphan CTA row exists above the header");
  assert.ok(!userShow.slice(0, grid).includes("justify-end"), "no orphan right-aligned row above the header");
  const row = block(REPOS, "mt-3 flex flex-wrap gap-2", "profile-avatar");
  assert.ok(!row.includes("New repository"), "New repository left the header action row for the toolbar");
  assert.ok(row.includes("Edit profile"), "Edit profile stays in the action row");
  assert.ok(row.includes("getProfile()?.can_edit"), "Edit gate unchanged (server can_edit, client never decides)");
  const toolbar = block(REPOS, "repos-toolbar", "</div>");
  assert.ok(toolbar.includes("New repository"), "New repository renders once, in the Repositories toolbar");
  assert.ok(toolbar.includes("btn primary"), "CTA keeps primary styling");
  assert.ok(toolbar.includes("<Show when={canWrite()}>"), "CTA keeps the canWrite gate");
});

test("light + dark share the treatment", () => {
  assert.ok(REPOS.includes("ring-zinc-300"), "light ring on the avatar");
  assert.ok(REPOS.includes("dark:ring-zinc-600"), "dark ring on the avatar");
});

test("org header untouched (#359) except the CTA move (#413): title-row avatar, badge, org-doc fields", () => {
  const org = block(REPOS, "<Show when={isOrg()}>", "repos-toolbar");
  assert.ok(org.includes("<OrgAvatar"), "org avatar still in the title row");
  assert.ok(org.includes("size={36}"), "org avatar keeps its own size");
  assert.ok(org.includes("org-badge"), "org badge kept");
  assert.ok(org.includes("{orgName()}"), "org display name kept");
  assert.ok(org.includes("<h2"), "org keeps the h2 title row (only the user header promotes to h1)");
  assert.ok(!org.includes("New repository"), "org title row no longer carries the CTA (it lives in the shared toolbar)");
  assert.ok(!org.includes('<div class="mb-1 flex items-center justify-between">'), "title row drops the two-child spread wrapper (single child)");
  assert.ok(org.includes('<h2 class="mb-1 flex items-center gap-2 text-xl font-semibold">'), "title row is now just the h2");
  assert.ok(org.includes("Manage organization"), "Manage affordance kept with its canManage gate");
  assert.ok(org.includes("renderBody(getOrg().bio_markdown)"), "org bio still reads the org doc, not the owner profile");
  assert.ok(!org.includes("profile-header"), "the composed user grid never renders for orgs");
  assert.ok(!org.includes("Regenerate avatar"), "avatar self-service stays non-org only");
});

test("no #345 collision surface: header carries no repo badges", () => {
  const userShow = block(REPOS, "<Show when={!isOrg()}>", "<h3");
  assert.ok(!userShow.includes("visibility-badge"), "visibility badges live on RepoRow only, never in the header");
  assert.ok(!userShow.includes("mirror-badge"), "mirror badges live on RepoRow only, never in the header");
});
