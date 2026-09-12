// web/test/unit/profile-sidebar-421.test.js — Forgejo #421: the owner
// profile page (/:owner, web/src/pages/Repos.jsx) is a GitHub-style
// two-column grid — main content (identity, edit form, Repositories
// toolbar, listing) left, a narrow sidebar right carrying the avatar with
// the owner actions grouped vertically beneath it (Edit profile /
// Regenerate avatar / Remove avatar, each full-width, an <hr> in the
// header divider colors directly above the group). Below sm: the grid is
// one column so the sidebar stacks below the main column at 390px with no
// horizontal overflow (#273-#278); DOM order stays main-first so the h1
// keeps heading order. The org variant gets the same sidebar treatment
// (org avatar + Manage organization). Layout only: every gate (Edit =
// server can_edit, Regenerate/Remove = self-only, Manage = canManage,
// #376 invalidation, #420 hide-while-editing, #413 toolbar) stays
// byte-identical; no fetch, cache-key, or server changes. No DOM: JSX
// pinned as source text, mirroring profile-header.test.js /
// profile-header-403.test.js / repos-toolbar-413.test.js. Light/dark share
// the treatment (layout classes + theme-independent divider/ring classes
// on both sides of dark:).
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

function tagOf(src, start) {
  const open = src.indexOf(start);
  assert.ok(open !== -1, `expected element ${start}`);
  return src.slice(open, src.indexOf(">", open) + 1);
}

test("two-column page grid: main left, sidebar right, stacked below on narrow", () => {
  const tag = tagOf(REPOS, '<div class="profile-layout');
  assert.ok(tag.includes("grid"), "the page body is a grid");
  assert.ok(tag.includes("grid-cols-1"), "base is one column (390px: sidebar stacks below main)");
  assert.ok(tag.includes("sm:grid-cols-[minmax(0,1fr)_12rem]"), "desktop restores main-left / narrow-sidebar-right");
  assert.ok(tag.includes("gap-6"), "columns keep their rhythm");
  const main = tagOf(REPOS, '<div class="profile-main');
  assert.ok(main.includes("min-w-0"), "main column flexes (long names/bios cannot push the sidebar out)");
  const mainIdx = REPOS.indexOf('<div class="profile-main');
  const sideIdx = REPOS.indexOf("profile-sidebar");
  assert.ok(mainIdx !== -1 && sideIdx !== -1 && mainIdx < sideIdx, "DOM order stays main-first (h1 keeps heading order)");
  assert.ok(REPOS.indexOf("<h1") < sideIdx, "identity precedes the sidebar in DOM order");
});

test("identity block stays atop the main column above the toolbar", () => {
  const main = block(REPOS, '<div class="profile-main', "profile-sidebar");
  assert.ok(main.includes("<h1"), "display name is the page h1");
  assert.ok(main.includes("{displayName()}"), "h1 renders the display name (owner slug when unset)");
  assert.ok(main.includes("@{owner()}"), "handle renders under the name");
  assert.ok(main.includes('join(" · ")'), "location · timezone separator kept");
  assert.ok(main.includes("renderBody(profile().bio_markdown)"), "bio renders through the shared markdown pipeline");
  assert.ok(main.indexOf("profile-header") < main.indexOf("repos-toolbar"), "identity renders above the Repositories toolbar");
  assert.ok(main.indexOf("repos-toolbar") < main.indexOf("orderByActivity"), "toolbar renders above the listing");
  assert.ok(main.includes("<Show when={getEditing() && getProfile()?.can_edit}>"), "the edit form opens in place in the main column");
});

test("avatar + all owner actions live grouped in the sidebar, none in the identity block", () => {
  const identity = block(REPOS, '<div class="profile-header', "</div>\n            <Show when={getEditing()");
  assert.ok(!identity.includes("Edit profile"), "Edit profile left the identity block");
  assert.ok(!identity.includes("Regenerate avatar"), "Regenerate lives in the sidebar, not the identity block");
  assert.ok(!identity.includes("Remove avatar"), "Remove lives in the sidebar, not the identity block");
  assert.ok(!identity.includes("profile-avatar"), "the avatar left the identity block");
  const side = block(REPOS, 'aria-label="Profile actions"', "</aside>");
  assert.ok(side.includes("h-24 w-24"), "avatar at profile scale, on top of the sidebar");
  assert.ok(side.includes("Edit profile"), "Edit profile grouped in the sidebar");
  assert.ok(side.includes("Regenerate avatar"), "Regenerate grouped in the sidebar");
  assert.ok(side.includes("Remove avatar"), "Remove grouped in the sidebar");
  assert.ok(side.indexOf("h-24 w-24") < side.indexOf("Edit profile"), "actions render beneath the avatar");
  const actions = block(REPOS, "profile-actions", "</div>");
  assert.ok(actions.includes("flex w-full flex-col"), "the action group is one vertical full-width stack");
  for (const label of ["Edit profile", "Regenerate avatar", "Remove avatar"]) {
    const at = side.indexOf(label);
    assert.ok(at !== -1, `${label} renders in the sidebar`);
    const open = side.lastIndexOf("<button", at);
    const tag = side.slice(open, side.indexOf(">", open) + 1);
    assert.ok(tag.includes("w-full"), `${label} is full-width in the sidebar`);
  }
});

test("an <hr>-style divider sits between the avatar and the action group, immediately above Edit profile", () => {
  const side = block(REPOS, 'aria-label="Profile actions"', "</aside>");
  const hr = side.indexOf("<hr");
  assert.ok(hr !== -1, "the sidebar carries a divider");
  const hrTag = side.slice(hr, side.indexOf(">", hr) + 1);
  assert.ok(hrTag.includes("border-zinc-200"), "light divider matches the header rule");
  assert.ok(hrTag.includes("dark:border-zinc-700"), "dark divider matches the header rule");
  assert.ok(side.indexOf("h-24 w-24") < hr, "divider renders below the avatar");
  assert.ok(hr < side.indexOf("Edit profile"), "divider renders immediately above Edit profile");
  // No orphan rule: each divider's own Show also requires the avatar
  // above it, so an avatarless-but-actionable sidebar (self opted out
  // via #376 Remove, editor without an avatar) opens on actions, not a
  // stray line.
  const hrShow = side.slice(side.lastIndexOf("<Show when=", hr), hr);
  assert.ok(hrShow.includes("userSrc()"), "user divider requires the avatar above it");
  assert.ok(hrShow.includes("getProfile()?.can_edit"), "user divider still requires an actionable viewer");
  const orgSide = block(REPOS, 'aria-label="Organization actions"', "</aside>");
  const orgHr = orgSide.indexOf("<hr");
  assert.ok(orgHr !== -1, "the org sidebar carries the same divider");
  assert.ok(orgSide.indexOf("<OrgAvatar") < orgHr, "org divider renders below the avatar");
  assert.ok(orgHr < orgSide.indexOf("Manage organization"), "org divider renders immediately above Manage organization");
  const orgHrShow = orgSide.slice(orgSide.lastIndexOf("<Show when=", orgHr), orgHr);
  assert.ok(orgHrShow.includes("avatar_content_type"), "org divider requires the org avatar above it");
  assert.ok(orgHrShow.includes("canManage()"), "org divider still requires a manager");
});

test("action gates byte-identical: Edit = server can_edit, avatar actions = self-only", () => {
  for (const gate of [
    "<Show when={getProfile()?.can_edit && !getEditing()}>",
    "<Show when={isSelf()}>",
    "<Show when={userSrc()}>",
    "<Show when={getEditing() && getProfile()?.can_edit}>",
    "<Show when={profile().bio_markdown && !getEditing()}>",
  ]) {
    assert.ok(REPOS.includes(gate), `gate kept byte-identical: ${gate}`);
  }
  assert.ok(REPOS.includes('invalidate(`user:${owner()}`)'), "#376 avatar invalidation untouched");
  assert.ok(REPOS.includes('invalidate("me")'), "navbar avatar invalidation untouched");
});

test("390px safety: no fixed widths beside the avatar, no orphan rows", () => {
  const layout = block(REPOS, '<div class="profile-layout', 'aria-label="Profile actions"');
  assert.ok(!layout.includes("flex justify-end"), "no right-aligned orphan row anywhere above the sidebar");
  const sideTag = tagOf(REPOS, '<aside class="profile-sidebar');
  assert.ok(sideTag.includes("flex-col"), "sidebar stacks vertically at every width");
  assert.ok(!sideTag.includes("flex-col-reverse"), "no visual/DOM order split");
  assert.ok(sideTag.includes("min-w-0"), "sidebar never forces overflow");
  const actionsTag = tagOf(REPOS, '<div class="profile-actions');
  assert.ok(actionsTag.includes("flex-col"), "actions stack vertically at 390px");
  assert.ok(!actionsTag.includes("flex-row"), "actions never sit side-by-side");
});

test("org variant: org header in main, avatar + Manage grouped in the sidebar", () => {
  const main = block(REPOS, '<div class="profile-main', "profile-sidebar");
  const org = main.slice(main.indexOf("<Show when={isOrg()}>"), main.indexOf("repos-toolbar"));
  assert.ok(org.includes("<h2"), "org keeps the h2 title row (only the user header promotes to h1)");
  assert.ok(org.includes("{orgName()}"), "org display name kept");
  assert.ok(org.includes("org-badge"), "org badge kept");
  assert.ok(org.includes("@{owner()}"), "org handle kept");
  assert.ok(org.includes("renderBody(getOrg().bio_markdown)"), "org bio still reads the org doc, not the owner profile");
  assert.ok(!org.includes("<OrgAvatar"), "the org avatar left the title row for the sidebar");
  assert.ok(!org.includes("profile-header"), "the user identity grid never renders for orgs");
  assert.ok(!org.includes("Regenerate avatar"), "avatar self-service stays non-org only");
  const orgSide = block(REPOS, 'aria-label="Organization actions"', "</aside>");
  assert.ok(orgSide.includes("<OrgAvatar"), "org avatar renders in the sidebar");
  assert.ok(orgSide.includes("size={96}"), "org avatar at sidebar scale");
  assert.ok(orgSide.includes("Manage organization"), "Manage affordance grouped in the sidebar");
  assert.ok(orgSide.includes("href={`/${owner()}/settings`}"), "Manage href unchanged");
  assert.ok(orgSide.includes("<Show when={canManage()}>"), "Manage gate byte-identical (server decides, client never does)");
  const gate = REPOS.slice(REPOS.indexOf('aria-label="Organization actions"') - 260, REPOS.indexOf('aria-label="Organization actions"'));
  assert.ok(gate.includes("getOrg()?.avatar_content_type"), "sidebar shows for an avatar-naming org");
  assert.ok(gate.includes("canManage()"), "sidebar shows for a manager without an avatar");
});
