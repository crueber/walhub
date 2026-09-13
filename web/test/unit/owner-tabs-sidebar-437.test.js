// web/test/unit/owner-tabs-sidebar-437.test.js — Forgejo #437: the owner
// tabs move off the #435 top strip into the profile-layout right sidebar
// as a vertical tab list above the avatar, and the leftover
// repository-count teaser ("N repositories · View all →") is deleted from
// the profile view. One shared two-column profile layout renders on ALL
// THREE owner routes: the main column swaps per view (identity / listing /
// membership) while the right sidebar column always carries the ungated
// OwnerTabs first, then the gated #421 avatar asides below — so tab
// navigation stays one click away on every owner route for every viewer.
// Layout / markup only: active-tab derivation, aria-current, the tab-badge
// count (shared `repos:{owner}` payload — no new fetch), the #370 isOrg
// gating, and every #421 aside gate stay byte-identical; routes unchanged;
// org variant keeps Profile | Repositories. No DOM: JSX pinned as source
// text, mirroring owner-repos-tab-422.test.js /
// owner-orgs-tab-430.test.js / profile-sidebar-421.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const REPOS = srcOf("../../src/pages/Repos.jsx");
const INDEX = srcOf("../../src/index.jsx");
const CSS = srcOf("../../src/ui.css");

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

test("teaser deleted: no repository-count teaser anywhere on the profile view", () => {
  assert.ok(!REPOS.includes("View all →"), "the teaser link is gone everywhere");
  const outer = REPOS.indexOf('<Show when={view() === "profile"}>');
  assert.ok(outer !== -1, "a profile-view gate still opens the identity region");
  const reposView = REPOS.indexOf('<Show when={view() === "repos"}>');
  assert.ok(reposView !== -1 && outer < reposView, "the profile branch opens before the repositories branch");
  const branch = REPOS.slice(outer, reposView);
  assert.ok(branch.includes('<div class="profile-header'), "identity header kept");
  assert.ok(branch.includes("<h1"), "username stays the page h1");
  assert.ok(!branch.includes("href={`/${owner()}/repositories`}"), "no deep-link to the tab route in the profile branch (teaser gone)");
  assert.ok(!branch.includes("repos-toolbar"), "no toolbar in the profile branch");
  assert.ok(!branch.includes("<RepoRow"), "no repo grid in the profile branch");
  assert.ok(!branch.includes("orgs-rail"), "no membership rail in the profile branch");
});

test("repositories tab keeps its own count paragraph", () => {
  const reposView = REPOS.slice(REPOS.indexOf('<Show when={view() === "repos"}>'));
  assert.ok(reposView.includes("repositor"), "the tab still names the repository count");
  assert.ok(reposView.includes("orderByActivity(doc().repos)"), "the count still rides the shared listing payload");
  assert.ok(reposView.includes("repos-toolbar"), "repositories view keeps its toolbar");
  assert.ok(reposView.includes("<RepoRow"), "repositories view keeps its grid");
});

test("no top strip: the only OwnerTabs use lives inside the sidebar column", () => {
  const uses = [...REPOS.matchAll(/<OwnerTabs owner=\{owner\(\)\} count=\{repoCount\(\)\} isOrg=\{isOrg\(\)\} \/>/g)];
  assert.equal(uses.length, 1, "exactly one tab-list use (no per-view copies)");
  const use = uses[0].index;
  const layout = REPOS.indexOf('<div class="profile-layout');
  assert.ok(layout !== -1 && layout < use, "the tab list renders inside the profile layout, never above it");
  const col = REPOS.indexOf('<div class="profile-sidebar-col');
  assert.ok(col !== -1 && col < use, "the tab list renders inside the sidebar column");
  const page = REPOS.indexOf('<div class="repos-page">');
  const between = REPOS.slice(page, layout);
  assert.ok(!between.includes("<OwnerTabs"), "no strip between the page root and the layout (strip-first gone)");
  assert.ok(!between.includes("<h1"), "no identity content precedes the layout either");
});

test("tabs render above the avatar on both variants (tabs-first sidebar order)", () => {
  const use = REPOS.indexOf("<OwnerTabs owner={owner()} count={repoCount()} isOrg={isOrg()} />");
  const col = REPOS.indexOf('<div class="profile-sidebar-col');
  for (const marker of ['aria-label="Profile actions"', 'aria-label="Organization actions"', "profile-avatar", "Edit profile", "Manage organization"]) {
    const at = REPOS.indexOf(marker, col);
    assert.ok(at !== -1 && use < at, `tabs render above ${marker}`);
  }
});

test("sidebar shell renders on every view and both variants (column outside every gate)", () => {
  const layout = REPOS.indexOf('<div class="profile-layout');
  const page = REPOS.indexOf('<div class="repos-page">');
  const head = REPOS.slice(page, layout);
  const headOpens = (head.match(/<Show/g) || []).length;
  const headCloses = (head.match(/<\/Show>/g) || []).length;
  assert.ok(headOpens === headCloses, "every Show opened since the page root is closed before the layout");
  assert.ok(!head.includes("view()"), "no view gate hides the layout on any route");
  assert.ok(!head.includes("isOrg()"), "no variant gate hides the layout on either variant");
  // The sidebar column opens after all three view branches close: every
  // Show opened inside the layout's main column is closed before it, so the
  // column (and the ungated tab list first inside it) is outside every gate.
  const col = REPOS.indexOf('<div class="profile-sidebar-col');
  const middle = REPOS.slice(layout, col);
  const opens = (middle.match(/<Show/g) || []).length;
  const closes = (middle.match(/<\/Show>/g) || []).length;
  assert.ok(opens === closes, "every Show opened in the main column is closed before the sidebar column");
  for (const gate of ['<Show when={view() === "profile"}>', '<Show when={view() === "repos"}>', '<Show when={view() === "orgs"}>']) {
    assert.ok(middle.includes(gate), `the ${gate} branch lives in the main column before the sidebar`);
  }
  const tail = REPOS.slice(col);
  assert.ok(tail.includes("<OwnerTabs owner={owner()} count={repoCount()} isOrg={isOrg()} />"), "tab list first in the column");
  assert.ok(tail.includes('aria-label="Profile actions"'), "user aside kept below the tabs");
  assert.ok(tail.includes('aria-label="Organization actions"'), "org aside kept below the tabs");
});

test("vertical true-tab look in the sidebar zinc language", () => {
  const tag = tagOf(REPOS, '<nav\n      class="owner-tabs');
  for (const token of ["flex", "w-full", "flex-col", "gap-1", "rounded-lg", "border", "border-zinc-200", "dark:border-zinc-700"]) {
    assert.ok(tag.includes(token), `tab list carries the sidebar-box token: ${token}`);
  }
  for (const token of ["overflow-x-auto", "whitespace-nowrap", "border-b", "mb-4", "max-w-full"]) {
    assert.ok(!tag.includes(token), `strip token gone from the vertical list: ${token}`);
  }
  assert.ok(tag.includes('aria-label="owner sections"'), "labelled landmark kept");
  const tabs = block(REPOS, "export function OwnerTabs", "function ProfileForm");
  assert.ok(tabs.includes('"block w-full rounded-md px-3 py-1.5 text-sm'), "links stack full-width with the sidebar text treatment");
  assert.ok(!tabs.includes("rounded-t"), "strip link treatment gone");
  for (const tab of ["profile", "repos", "orgs"]) {
    assert.ok(
      tabs.includes(`"!bg-zinc-100 !font-medium !text-zinc-900 dark:!bg-zinc-800 dark:!text-zinc-100": active() === "${tab}"`),
      `${tab} tab fills its row with the container selected background (connected true-tab look, zinc in both themes)`
    );
  }
  assert.ok(!tabs.includes("!border-emerald-500"), "the strip's emerald underline is gone from the tab list");
});

test("active-tab derivation + aria-current + badge + org gating survive the move", () => {
  const tabs = block(REPOS, "export function OwnerTabs", "function ProfileForm");
  assert.ok(tabs.includes("`/${props.owner}/organizations`"), "derivation probes the organizations pathname");
  assert.ok(tabs.includes("`/${props.owner}/repositories`"), "derivation probes the repositories pathname");
  assert.ok(tabs.includes('? "orgs"'), "organizations pathname maps to the orgs tab");
  assert.ok(tabs.includes('? "repos"'), "repositories pathname maps to the repos tab");
  assert.ok(tabs.includes(': "profile"'), "anything else falls back to the profile tab");
  for (const tab of ["profile", "repos", "orgs"]) {
    assert.ok(tabs.includes(`aria-current={active() === "${tab}" ? "page" : undefined}`), `${tab} tab marks aria-current`);
  }
  assert.ok(tabs.includes("tab-badge"), "the badge hook survives");
  const badge = tabs.indexOf("tab-badge");
  const reposLink = tabs.indexOf("href={`/${props.owner}/repositories`}");
  const orgsLink = tabs.indexOf("href={`/${props.owner}/organizations`}");
  assert.ok(badge > reposLink && badge < orgsLink, "the badge still rides the Repositories tab only");
  assert.ok(tabs.includes("props.count"), "badge still derives from the shared listing payload (no new fetch)");
  const gate = tabs.lastIndexOf("<Show when={!props.isOrg}>", orgsLink);
  assert.ok(gate !== -1 && gate < orgsLink, "the Organizations tab stays user-profiles-only (#370)");
  const clsUses = [...tabs.matchAll(/class=\{cls\}/g)];
  assert.equal(clsUses.length, 3, "all three tabs share the one cls link treatment");
});

test("routes unchanged: all three owner routes registered static-before-dynamic", () => {
  assert.ok(INDEX.includes('<Route path="/:owner/repositories" component={OwnerRepositories} />'), "repositories tab route kept");
  assert.ok(INDEX.includes('<Route path="/:owner/organizations" component={OwnerOrganizations} />'), "organizations tab route kept");
  assert.ok(INDEX.includes('<Route path="/:owner" component={Repos} />'), "/:owner profile route kept");
  const tab = INDEX.indexOf('path="/:owner/repositories"');
  const orgs = INDEX.indexOf('path="/:owner/organizations"');
  const profile = INDEX.indexOf('path="/:owner" component={Repos}');
  const repo = INDEX.indexOf('path="/:owner/:name"');
  assert.ok(tab < profile && orgs < profile, "tab routes register before the profile route");
  assert.ok(tab < repo && orgs < repo, "tab routes register before /:owner/:name");
});

test("no data-fetch, cache-key, or gating-logic changes", () => {
  for (const key of ["`repos:${owner()}`", "`profile:${owner()}`", "`org:${owner()}`", "`memberorgs:${owner()}`", "`user:${owner()}`", '"me"']) {
    assert.ok(REPOS.includes(key), `fetch surface untouched: ${key}`);
  }
  for (const gate of [
    "<Show when={canWrite()}>",
    "<Show when={!isOrg()}>",
    "<Show when={isOrg()}>",
    "<Show when={getProfile()?.can_edit && !getEditing()}>",
    "<Show when={isSelf()}>",
    "<Show when={userSrc()}>",
    "<Show when={getEditing() && getProfile()?.can_edit}>",
    "<Show when={canManage()}>",
    "<Show when={profile().bio_markdown && !getEditing()}>",
    "<Show when={!isOrg() && (userSrc() || isSelf() || getProfile()?.can_edit)}>",
    "<Show when={isOrg() && (getOrg()?.avatar_content_type || canManage())}>",
  ]) {
    assert.ok(REPOS.includes(gate), `gate kept byte-identical: ${gate}`);
  }
  assert.ok(REPOS.includes("repos.owners.detailed(owner()"), "listing fetch untouched");
  assert.ok(REPOS.includes("repos.users.orgs(owner())"), "membership fetch untouched");
  assert.ok(REPOS.includes("repos.owners.updateProfile("), "profile save untouched");
  assert.ok(REPOS.includes("const repoCount = () =>"), "tab count still derives from the shared payload");
  assert.ok(REPOS.includes("return <OwnerPage view=\"profile\" />"), "default export still serves the profile");
  assert.ok(REPOS.includes("return <OwnerPage view=\"repos\" />"), "repositories export untouched");
  assert.ok(REPOS.includes("return <OwnerPage view=\"orgs\" />"), "organizations export untouched");
});

test("390px: vertical list needs no scroll rules; grid still stacks; no fixed widths", () => {
  const tag = tagOf(REPOS, '<nav\n      class="owner-tabs');
  assert.ok(!tag.includes("w-["), "no fixed widths on the tab list");
  assert.ok(!tag.includes("overflow-x-auto"), "no internal scrolling on the vertical list");
  assert.ok(CSS.includes(".owner-tabs a { @apply block w-full; }"), "links stack full-width via the hook");
  assert.ok(!CSS.includes(".owner-tabs { scrollbar-width: none; }"), "strip scrollbar rules retired with the strip");
  const col = tagOf(REPOS, '<div class="profile-sidebar-col');
  assert.ok(col.includes("min-w-0"), "sidebar column never forces overflow");
  assert.ok(col.includes("flex-col"), "column stacks vertically at every width");
  assert.ok(!col.includes("w-["), "no fixed widths on the sidebar column");
  const layout = tagOf(REPOS, '<div class="profile-layout');
  assert.ok(layout.includes("grid-cols-1"), "base is one column (390px: sidebar stacks below main)");
  assert.ok(layout.includes("sm:grid-cols-[minmax(0,1fr)_12rem]"), "desktop restores main-left / narrow-sidebar-right");
  const reposView = REPOS.slice(REPOS.indexOf('<Show when={view() === "repos"}>'), REPOS.indexOf('<Show when={view() === "orgs"}>'));
  assert.ok(!reposView.includes("w-["), "no fixed widths in the repositories view");
  assert.ok(reposView.includes("flex-wrap"), "toolbar wraps at 390px");
  const orgsView = REPOS.slice(REPOS.indexOf('<Show when={view() === "orgs"}>'));
  assert.ok(!orgsView.includes("w-["), "no fixed widths in the organizations view");
  assert.ok(orgsView.includes("flex-wrap"), "membership list wraps at 390px");
});
