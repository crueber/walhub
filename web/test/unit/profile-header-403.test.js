// web/test/unit/profile-header-403.test.js — Forgejo #403: the owner
// profile header reads like GitHub's — grouped action row, no dead space.
// (#413 moved the New-repository CTA out of that grouped row into the
// Repositories toolbar; #421 moved the surviving grouped actions — Edit
// profile, Regenerate/Remove avatar — with the avatar into a right
// sidebar under an <hr>, leaving the identity block action-free. The pins
// below assert the surviving #403 shape: h1 anchor, tight handle, bottom
// divider, composition — plus the absence of the CTA from the header.)
// Layout only: Edit profile keeps its can_edit gate (now in the sidebar),
// every other Show gate stays byte-identical (canWrite, can_edit, isSelf,
// userSrc); no fetch, cache key, or server interaction changes. The org
// branch (#359) keeps its header minus the title-row CTA (likewise #413),
// plus the same sidebar treatment (#421). No DOM: JSX pinned as source
// text, mirroring profile-header.test.js.
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

test("no full-width row above the header; identity block carries no actions (#413, #421)", () => {
  const main = block(REPOS, '<div class="profile-main', "profile-sidebar");
  const above = main.slice(0, main.indexOf('<div class="profile-header'));
  assert.ok(!above.includes("New repository"), "no CTA above the header");
  assert.ok(!above.includes("justify-end"), "no orphan right-aligned row above the header");
  assert.ok(!above.includes("mb-3 flex"), "no orphan action row above the header");
  const identity = block(REPOS, '<div class="profile-header', "</div>\n            <Show when={getEditing()");
  assert.ok(!identity.includes("New repository"), "New repository left the header for the toolbar (#413)");
  assert.ok(!identity.includes("Edit profile"), "Edit profile left the identity block for the sidebar (#421)");
});

test("h1 anchors at text-2xl; handle sits tight beneath it", () => {
  const grid = block(REPOS, '<div class="profile-header', "</div>\n            <Show when={getEditing()");
  assert.ok(grid.includes('<h1 class="text-2xl font-semibold">'), "h1 at text-2xl");
  assert.ok(!grid.includes("text-xl"), "no undersized text-xl left in the header");
  assert.ok(grid.includes('<p class="muted mt-0.5 text-sm">@{owner()}</p>'), "handle tight under the name");
  assert.ok(grid.includes('<p class="muted mt-1 text-sm">'), "location · timezone rhythm kept");
  assert.ok(grid.includes('class="markdown-body mt-3"'), "bio gap unchanged");
});

test("header closes with a bottom divider before Repositories", () => {
  const open = REPOS.indexOf('<div class="profile-header');
  assert.ok(open !== -1, "profile header div exists");
  const tag = REPOS.slice(open, REPOS.indexOf(">", open) + 1);
  assert.ok(tag.includes("pb-6"), "header pads below its content");
  assert.ok(tag.includes("border-b"), "header closes with a bottom rule");
  assert.ok(tag.includes("border-zinc-200"), "light divider");
  assert.ok(tag.includes("dark:border-zinc-700"), "dark divider");
  const toolbar = block(REPOS, "repos-toolbar", "</div>");
  assert.ok(toolbar.includes("mt-6"), "Repositories toolbar keeps its spacing below the rule");
  assert.ok(toolbar.includes("<h3"), "heading lives in the toolbar row");
  assert.ok(toolbar.includes("text-base font-semibold"), "heading keeps its #403 type size");
});

test("composition: main left, sidebar right, stacked at 390px", () => {
  const open = REPOS.indexOf('<div class="profile-layout');
  assert.ok(open !== -1, "two-column page grid exists");
  const tag = REPOS.slice(open, REPOS.indexOf(">", open) + 1);
  assert.ok(tag.includes("grid-cols-1"), "base stacks (390px: sidebar below main)");
  assert.ok(tag.includes("sm:grid-cols-[minmax(0,1fr)_12rem]"), "desktop restores the two columns");
  assert.ok(tag.includes("minmax(0,1fr)"), "main column flexes");
  const sideTag = REPOS.slice(REPOS.indexOf('<aside class="profile-sidebar'), REPOS.indexOf(">", REPOS.indexOf('<aside class="profile-sidebar')) + 1);
  assert.ok(sideTag.includes("min-w-0"), "sidebar never forces overflow");
  assert.ok(REPOS.indexOf("<h1") < REPOS.indexOf("profile-sidebar"), "identity precedes the sidebar in DOM order");
  const actionsTag = REPOS.slice(REPOS.indexOf('<div class="profile-actions'), REPOS.indexOf(">", REPOS.indexOf('<div class="profile-actions')) + 1);
  assert.ok(actionsTag.includes("flex-col"), "the grouped action stack is vertical at 390px (no root overflow)");
  assert.ok(actionsTag.includes("w-full"), "actions fill the sidebar width");
});

test("sidebar avatar + grouped actions functionally untouched", () => {
  const side = block(REPOS, 'aria-label="Profile actions"', "</aside>");
  assert.ok(side.includes("h-24 w-24"), "avatar at profile scale");
  assert.ok(side.includes("Edit profile"), "Edit profile grouped in the sidebar");
  assert.ok(side.includes("Regenerate avatar"), "Regenerate grouped in the sidebar");
  assert.ok(side.includes("Remove avatar"), "Remove grouped in the sidebar");
  assert.ok(side.indexOf("h-24 w-24") < side.indexOf("Regenerate avatar"), "actions render beneath the avatar");
  assert.ok(side.indexOf("<hr") < side.indexOf("Edit profile"), "divider immediately above Edit profile");
  assert.ok(REPOS.includes('invalidate(`user:${owner()}`)'), "#376 avatar invalidation untouched");
});

test("all Show gates byte-identical to pre-change", () => {
  for (const gate of [
    "<Show when={canWrite()}>",
    "<Show when={getProfile()?.can_edit && !getEditing()}>",
    "<Show when={isSelf()}>",
    "<Show when={userSrc()}>",
    "<Show when={profile().display_name}>",
    "<Show when={profile().location || profile().timezone}>",
    "<Show when={profile().bio_markdown && !getEditing()}>", // #420: rendered bio hides while the edit form is open
    "<Show when={getEditing() && getProfile()?.can_edit}>",
  ]) {
    assert.ok(REPOS.includes(gate), `gate kept byte-identical: ${gate}`);
  }
});

test("zero fetch/cache/server changes", () => {
  for (const key of ["`repos:${owner()}`", "`profile:${owner()}`", "`org:${owner()}`", "`user:${owner()}`"]) {
    assert.ok(REPOS.includes(key), `cache key untouched: ${key}`);
  }
  assert.ok(REPOS.includes('"me"'), "me fetch untouched");
  const ctas = [...REPOS.matchAll(/href={`\/new\?owner=\${encodeURIComponent\(owner\(\)\)}`}/g)];
  assert.equal(ctas.length, 1, "exactly one New-repository link: the shared Repositories toolbar");
  assert.ok(REPOS.includes("repos.owners.detailed(owner()"), "listing fetch untouched");
  assert.ok(REPOS.includes("repos.owners.updateProfile("), "profile save untouched");
});

test("org branch keeps its header minus the title-row CTA (#359, #413) plus the sidebar (#421)", () => {
  // Forgejo #437: scope the org slice to the profile branch (profile gate →
  // repos gate) — the main column now carries all three view branches
  // before the sidebar column.
  const profile = block(REPOS, '<Show when={view() === "profile"}>', '<Show when={view() === "repos"}>');
  const org = profile.slice(profile.indexOf("<Show when={isOrg()}>"));
  assert.ok(!org.includes("<OrgAvatar"), "org avatar left the title row for the sidebar");
  assert.ok(!org.includes("New repository"), "org title row no longer carries the CTA (shared toolbar owns it)");
  assert.ok(!org.includes("Manage organization"), "Manage lives in the sidebar, not the main column");
  assert.ok(org.includes("renderBody(getOrg().bio_markdown)"), "org bio still reads the org doc");
  assert.ok(!org.includes("profile-header"), "the composed user grid never renders for orgs");
  assert.ok(!org.includes("border-b"), "the user header divider never renders for orgs");
  const orgSide = block(REPOS, 'aria-label="Organization actions"', "</aside>");
  assert.ok(orgSide.includes("<OrgAvatar"), "org avatar renders in the sidebar");
  assert.ok(orgSide.includes("Manage organization"), "Manage affordance grouped in the sidebar");
  assert.ok(orgSide.includes("<Show when={canManage()}>"), "Manage gate byte-identical");
});
