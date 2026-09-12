// web/test/unit/repos-toolbar-413.test.js — Forgejo #413: the
// New-repository CTA moves out of both header spots (the user action row
// under the bio, the org title row) into a Repositories toolbar — heading
// row, CTA right-anchored, wrapping at 390px per #273-#278. Layout only:
// the canWrite gate, the /new?owner= href with encodeURIComponent, Edit
// profile (server can_edit), Manage organization (canManage), the header
// divider (#403), and every fetch/cache key stay unchanged. No DOM: JSX
// pinned as source text, mirroring profile-header.test.js.
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

test("user header action row no longer carries the CTA (#421: identity carries no actions at all)", () => {
  const identity = block(REPOS, '<div class="profile-header', "</div>");
  assert.ok(!identity.includes("New repository"), "CTA left the user header for the toolbar");
  assert.ok(!identity.includes("/new?owner="), "no create link hides in the header");
  assert.ok(!identity.includes("Edit profile"), "Edit profile left the identity block for the sidebar (#421)");
  const side = block(REPOS, 'aria-label="Profile actions"', "</aside>");
  assert.ok(side.includes("Edit profile"), "Edit profile grouped in the sidebar");
  assert.ok(!side.includes("New repository"), "New repository never joins the sidebar action group");
});

test("org title row no longer carries the CTA", () => {
  const org = block(REPOS, "<Show when={isOrg()}>", "<h3");
  assert.ok(!org.includes("New repository"), "CTA left the org title row");
  assert.ok(!org.includes("/new?owner="), "no create link hides in the org header");
});

test("CTA renders once, in the Repositories toolbar, right-anchored", () => {
  const toolbar = block(REPOS, "repos-toolbar", "</div>");
  assert.ok(toolbar.includes("<h3"), "heading row owns the section title");
  assert.ok(toolbar.includes("Repositories</h3>"), "heading text unchanged");
  assert.ok(toolbar.includes("text-base font-semibold"), "heading keeps its type size");
  assert.ok(toolbar.includes("New repository"), "CTA sits with the list it populates");
  assert.ok(toolbar.includes("btn primary px-3 py-1"), "CTA keeps primary styling");
  assert.ok(toolbar.indexOf("Repositories</h3>") < toolbar.indexOf("New repository"), "heading first, CTA after (right-anchored)");
  const tags = REPOS.slice(REPOS.indexOf('<div class="repos-toolbar'), REPOS.indexOf(">", REPOS.indexOf('<div class="repos-toolbar')) + 1);
  assert.ok(tags.includes("flex"), "toolbar is a flex row");
  assert.ok(tags.includes("items-center"), "heading and CTA share a baseline");
  assert.ok(tags.includes("justify-between"), "CTA anchors right (the org title-row shape)");
  assert.ok(tags.includes("flex-wrap"), "390px wraps instead of overflowing");
  assert.ok(tags.includes("gap-2"), "wrapped rows keep their rhythm");
});

test("gate and href preserved: canWrite + pre-filled owner", () => {
  const toolbar = block(REPOS, "repos-toolbar", "</div>");
  assert.ok(toolbar.includes("<Show when={canWrite()}>"), "CTA still gated on canWrite (never promises what POST /api/v1/repos refuses)");
  assert.ok(
    toolbar.includes("href={`/new?owner=${encodeURIComponent(owner())}`}"),
    "pre-filled owner href preserved with encodeURIComponent"
  );
  // Forgejo #422: the Repos.jsx file-header comment names the CTA too —
  // count the rendered element (the text node closing into </A>), not prose.
  const ctas = [...REPOS.matchAll(/New repository\s*</g)];
  assert.equal(ctas.length, 1, "exactly one New-repository CTA on the page");
});

test("Edit profile / Manage organization gating and behavior unchanged", () => {
  assert.ok(REPOS.includes("<Show when={getProfile()?.can_edit && !getEditing()}>"), "Edit profile gate byte-identical");
  assert.ok(REPOS.includes("Edit profile"), "Edit profile affordance kept");
  assert.ok(REPOS.includes("<Show when={canManage()}>"), "Manage organization gate byte-identical");
  assert.ok(REPOS.includes("Manage organization"), "Manage affordance kept");
  assert.ok(REPOS.includes("isSelf()"), "avatar self-only gate kept");
});

test("header divider stays; import link untouched", () => {
  const open = REPOS.indexOf('<div class="profile-header');
  const tag = REPOS.slice(open, REPOS.indexOf(">", open) + 1);
  assert.ok(tag.includes("pb-6") && tag.includes("border-b"), "user header still closes with the #403 divider");
  assert.ok(REPOS.includes("import into {owner()}"), "import link kept where it was (placement optional per issue)");
  assert.ok(REPOS.includes("href={`/import?owner=${encodeURIComponent(owner())}`}"), "import href unchanged");
});

test("no data-fetch, cache-key, or gating-logic changes", () => {
  for (const key of ["`repos:${owner()}`", "`profile:${owner()}`", "`org:${owner()}`", "`user:${owner()}`", '"me"']) {
    assert.ok(REPOS.includes(key), `fetch surface untouched: ${key}`);
  }
  for (const gate of [
    "<Show when={canWrite()}>",
    "<Show when={getProfile()?.can_edit && !getEditing()}>",
    "<Show when={isSelf()}>",
    "<Show when={userSrc()}>",
    "<Show when={getEditing() && getProfile()?.can_edit}>",
  ]) {
    assert.ok(REPOS.includes(gate), `gate kept byte-identical: ${gate}`);
  }
  assert.ok(REPOS.includes("repos.owners.detailed(owner()"), "listing fetch untouched");
  assert.ok(REPOS.includes("repos.owners.updateProfile("), "profile save untouched");
  assert.ok(REPOS.includes("invalidate(`profile:${owner()}`)"), "#234 save→invalidate untouched");
});
