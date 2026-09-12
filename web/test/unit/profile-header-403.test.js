// web/test/unit/profile-header-403.test.js — Forgejo #403: the owner
// profile header reads like GitHub's — grouped action row, no dead space.
// (#413 moved the New-repository CTA out of that grouped row into the
// Repositories toolbar; the pins below assert the surviving #403 shape:
// h1 anchor, tight handle, bottom divider, composition — plus the absence
// of the CTA from the header.)
// Layout only: Edit profile keeps its can_edit gate under the bio, every
// other Show gate stays byte-identical (canWrite, can_edit, isSelf,
// userSrc); no fetch, cache key, or server interaction changes. The org
// branch (#359) keeps its header minus the title-row CTA (likewise #413).
// No DOM: JSX pinned as source text, mirroring profile-header.test.js.
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

test("no full-width row above the header; Edit profile alone under the bio (#413)", () => {
  const userShow = block(REPOS, "<Show when={!isOrg()}>", "<h3");
  const grid = userShow.indexOf("profile-header");
  assert.ok(grid !== -1, "profile header grid exists");
  assert.ok(!userShow.slice(0, grid).includes("New repository"), "no CTA above the header");
  assert.ok(!userShow.slice(0, grid).includes("justify-end"), "no orphan right-aligned row above the header");
  assert.ok(!userShow.slice(0, grid).includes("mb-3 flex"), "no orphan action row above the header");
  const row = block(REPOS, "mt-3 flex flex-wrap gap-2", "profile-avatar");
  assert.ok(!row.includes("New repository"), "New repository left the grouped row for the toolbar (#413)");
  assert.ok(row.includes("Edit profile"), "Edit profile stays in the row under the bio");
});

test("h1 anchors at text-2xl; handle sits tight beneath it", () => {
  const grid = block(REPOS, "profile-header", "</div>\n        <Show when={getEditing()");
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

test("composition preserved: identity left, avatar right, stacked at 390px", () => {
  const grid = block(REPOS, "profile-header", "</div>\n        <Show when={getEditing()");
  assert.ok(grid.includes("flex flex-col-reverse"), "base stacks (390px: avatar on top, identity below)");
  assert.ok(grid.includes("sm:flex-row"), "desktop restores the two-column row");
  assert.ok(grid.includes("sm:items-start sm:justify-between"), "identity left, avatar right");
  assert.ok(grid.includes("min-w-0 flex-1"), "identity column flexes");
  assert.ok(grid.includes("shrink-0"), "avatar column never shrinks");
  assert.ok(grid.indexOf("<h1") < grid.indexOf("profile-avatar"), "identity precedes the avatar in DOM order");
  assert.ok(grid.includes("flex-wrap"), "the grouped action row wraps at 390px (no root overflow)");
});

test("avatar column functionally untouched", () => {
  const col = block(REPOS, "profile-avatar", "</div>\n          </Show>");
  assert.ok(col.includes("h-24 w-24"), "avatar at profile scale");
  assert.ok(col.includes("Regenerate avatar"), "Regenerate grouped under the avatar");
  assert.ok(col.includes("Remove avatar"), "Remove grouped under the avatar");
  assert.ok(col.indexOf("h-24 w-24") < col.indexOf("Regenerate avatar"), "actions render beneath the avatar");
  const gate = REPOS.slice(REPOS.indexOf("profile-avatar") - 200, REPOS.indexOf("profile-avatar"));
  assert.ok(gate.includes("userSrc() || isSelf()"), "avatar column gate unchanged");
  assert.ok(REPOS.includes('invalidate(`user:${owner()}`)'), "#376 avatar invalidation untouched");
});

test("all Show gates byte-identical to pre-change", () => {
  for (const gate of [
    "<Show when={canWrite()}>",
    "<Show when={getProfile()?.can_edit && !getEditing()}>",
    "<Show when={isSelf()}>",
    "<Show when={userSrc()}>",
    "<Show when={userSrc() || isSelf()}>",
    "<Show when={profile().display_name}>",
    "<Show when={profile().location || profile().timezone}>",
    "<Show when={profile().bio_markdown}>",
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

test("org branch keeps its header minus the title-row CTA (#359, #413)", () => {
  const org = block(REPOS, "<Show when={isOrg()}>", "<h3");
  assert.ok(org.includes("<OrgAvatar"), "org avatar still in the title row");
  assert.ok(org.includes("size={36}"), "org avatar keeps its own size");
  assert.ok(!org.includes("New repository"), "org title row no longer carries the CTA (shared toolbar owns it)");
  assert.ok(org.includes("Manage organization"), "Manage affordance kept");
  assert.ok(!org.includes("profile-header"), "the composed user grid never renders for orgs");
  assert.ok(!org.includes("border-b"), "the user header divider never renders for orgs");
});
