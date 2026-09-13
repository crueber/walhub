// web/test/unit/profile-header.test.js — Forgejo #395 (#390 follow-up),
// reshaped by #403 (grouped action row, header divider), #413 (the
// New-repository CTA left the header for the Repositories toolbar), and
// #421 (GitHub-style two-column grid: main content left, avatar + grouped
// owner actions in a right sidebar under an <hr>, stacking below on
// narrow widths): the /:owner user-profile header is a composed identity
// block (username h1, handle, location · timezone, bio) atop the main
// column; the avatar and every owner action (Edit profile, Regenerate /
// Remove avatar) live grouped in the sidebar, none in the identity block.
// Layout only: every gate (Edit = server can_edit,
// Regenerate/Remove = self-only, #376 invalidation, org path #359) is
// asserted unchanged. Orgs keep their own header plus the same sidebar
// treatment. No DOM: JSX pinned as source text, mirroring
// header-narrow.test.js / identity-nav.test.js. Light/dark share the
// treatment (layout classes + theme-independent ring/divider classes on
// both sides of dark:).
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

test("page grid: main content left, sidebar right, stacked below on narrow", () => {
  const open = REPOS.indexOf('<div class="profile-layout');
  assert.ok(open !== -1, "two-column page grid exists");
  const tag = REPOS.slice(open, REPOS.indexOf(">", open) + 1);
  assert.ok(tag.includes("grid"), "the page body is a grid");
  assert.ok(tag.includes("grid-cols-1"), "base is one column (390px: sidebar below main)");
  assert.ok(tag.includes("sm:grid-cols-[minmax(0,1fr)_12rem]"), "desktop restores main-left / sidebar-right");
  assert.ok(tag.includes("minmax(0,1fr)"), "main column flexes (long names/bios cannot push the sidebar out)");
  const mainIdx = REPOS.indexOf('<div class="profile-main');
  const sideIdx = REPOS.indexOf("profile-sidebar");
  assert.ok(mainIdx !== -1 && sideIdx !== -1 && mainIdx < sideIdx, "DOM order stays main-first (h1 keeps heading order)");
  assert.ok(REPOS.indexOf("<h1") < sideIdx, "identity precedes the sidebar in DOM order");
});

test("identity column: username h1, handle, location · timezone, bio — no actions", () => {
  const grid = block(REPOS, '<div class="profile-header', "</div>\n            <Show when={getEditing()");
  assert.ok(grid.includes("<h1"), "username is the page h1 (was the shared h2)");
  assert.ok(grid.includes("{displayName()}"), "h1 renders the display name (owner slug when unset)");
  assert.ok(grid.includes("@{owner()}"), "handle renders under the name");
  assert.ok(grid.includes('join(" · ")'), "location · timezone separator kept");
  assert.ok(grid.includes("renderBody(profile().bio_markdown)"), "bio renders through the shared markdown pipeline");
  assert.ok(!grid.includes("Edit profile"), "Edit profile left the identity block for the sidebar (#421)");
  assert.ok(!grid.includes("Regenerate avatar"), "Regenerate lives in the sidebar, not the identity block");
  assert.ok(!grid.includes("Remove avatar"), "Remove lives in the sidebar, not the identity block");
});

test("sidebar: avatar on top, divider, grouped full-width actions", () => {
  const side = block(REPOS, 'aria-label="Profile actions"', "</aside>");
  assert.ok(side.includes("h-24 w-24"), "avatar at profile scale, on top");
  assert.ok(side.includes("Edit profile"), "Edit profile grouped in the sidebar");
  assert.ok(side.includes("Regenerate avatar"), "Regenerate grouped in the sidebar");
  assert.ok(side.includes("Remove avatar"), "Remove grouped in the sidebar");
  assert.ok(side.indexOf("h-24 w-24") < side.indexOf("<hr"), "divider renders beneath the avatar");
  assert.ok(side.indexOf("<hr") < side.indexOf("Edit profile"), "divider immediately above Edit profile");
  assert.ok(side.includes("getProfile()?.can_edit && !getEditing()"), "Edit gate unchanged (server can_edit, client never decides)");
});

test("avatar renders for the doc; actions stay self-gated with #376 invalidation", () => {
  assert.ok(REPOS.includes("<Show when={userSrc()}>"), "avatar renders from the doc pointer (no byte probing)");
  assert.ok(REPOS.includes("isSelf()"), "self gate kept (server re-checks; client never decides)");
  assert.ok(REPOS.includes('invalidate(`user:${owner()}`)'), "#376 avatar invalidation untouched");
  assert.ok(REPOS.includes('invalidate("me")'), "navbar avatar invalidation untouched");
});

test("action group carries Edit profile only from the header; New repository stays in the toolbar (#413)", () => {
  // Forgejo #437: all three views share the main column — scope the identity
  // pins to the profile branch (profile gate → repos gate).
  const profile = block(REPOS, '<Show when={view() === "profile"}>', '<Show when={view() === "repos"}>');
  assert.ok(!profile.includes("New repository"), "no CTA in the identity region (shared toolbar owns it)");
  assert.ok(!profile.includes("justify-end"), "no orphan right-aligned row in the identity region");
  const side = block(REPOS, 'aria-label="Profile actions"', "</aside>");
  assert.ok(!side.includes("New repository"), "New repository never joins the sidebar action group");
  assert.ok(side.includes("Edit profile"), "Edit profile grouped in the sidebar");
  assert.ok(side.includes("getProfile()?.can_edit"), "Edit gate unchanged (server can_edit, client never decides)");
  const toolbar = block(REPOS, "repos-toolbar", "</div>");
  assert.ok(toolbar.includes("New repository"), "New repository renders once, in the Repositories toolbar");
  assert.ok(toolbar.includes("btn primary"), "CTA keeps primary styling");
  assert.ok(toolbar.includes("<Show when={canWrite()}>"), "CTA keeps the canWrite gate");
});

test("light + dark share the treatment", () => {
  assert.ok(REPOS.includes("ring-zinc-300"), "light ring on the avatar");
  assert.ok(REPOS.includes("dark:ring-zinc-600"), "dark ring on the avatar");
  assert.ok(REPOS.includes("border-zinc-200"), "light divider (header rule + sidebar hr)");
  assert.ok(REPOS.includes("dark:border-zinc-700"), "dark divider (header rule + sidebar hr)");
});

test("org header keeps its fields (#359) with the same sidebar treatment (#421)", () => {
  // Forgejo #437: scope the org slice to the profile branch (profile gate →
  // repos gate) — the main column now carries all three view branches.
  const profile = block(REPOS, '<Show when={view() === "profile"}>', '<Show when={view() === "repos"}>');
  const org = profile.slice(profile.indexOf("<Show when={isOrg()}>"));
  assert.ok(!org.includes("<OrgAvatar"), "org avatar left the title row for the sidebar");
  assert.ok(org.includes("org-badge"), "org badge kept");
  assert.ok(org.includes("{orgName()}"), "org display name kept");
  assert.ok(org.includes("<h2"), "org keeps the h2 title row (only the user header promotes to h1)");
  assert.ok(!org.includes("New repository"), "org title row no longer carries the CTA (it lives in the shared toolbar)");
  assert.ok(org.includes("renderBody(getOrg().bio_markdown)"), "org bio still reads the org doc, not the owner profile");
  assert.ok(!org.includes("profile-header"), "the composed user grid never renders for orgs");
  assert.ok(!org.includes("Regenerate avatar"), "avatar self-service stays non-org only");
  const orgSide = block(REPOS, 'aria-label="Organization actions"', "</aside>");
  assert.ok(orgSide.includes("<OrgAvatar"), "org avatar renders in the sidebar");
  assert.ok(orgSide.includes("Manage organization"), "Manage affordance grouped in the sidebar");
  assert.ok(orgSide.includes("<Show when={canManage()}>"), "Manage gate byte-identical");
});

test("no #345 collision surface: header carries no repo badges", () => {
  const profile = block(REPOS, '<Show when={view() === "profile"}>', '<Show when={view() === "repos"}>');
  assert.ok(!profile.includes("visibility-badge"), "visibility badges live on RepoRow only, never in the header");
  assert.ok(!profile.includes("mirror-badge"), "mirror badges live on RepoRow only, never in the header");
});
