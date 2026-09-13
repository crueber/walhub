// web/test/unit/owner-tabs-top-435.test.js — Forgejo #435: the owner tab
// strip belongs at the top of the page, and non-profile tabs must not
// render the profile. Two defects on the owner surface (OwnerPage in
// web/src/pages/Repos.jsx): (1) the strip rendered mid-page inside
// .profile-main below the identity header — it now leads the page flow
// (below the navbar, above the identity content) on all three owner
// routes; (2) the identity header (and sidebar) rendered on every view —
// they now render ONLY on view()==='profile', so the repositories and
// organizations tabs render the strip + their own content only. Layout /
// markup only: tab-bar anatomy, active underline, aria-current, every
// gate, fetch, and cache key byte-identical; routes unchanged; org
// variant keeps Profile | Repositories. No DOM: JSX pinned as source
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

test("strip is the first element of the owner page flow on every route", () => {
  const use = REPOS.indexOf("<OwnerTabs owner={owner()} count={repoCount()} isOrg={isOrg()} />");
  assert.ok(use !== -1, "the page renders the strip with owner + shared-payload count + org gating");
  const layout = REPOS.indexOf('<div class="profile-layout');
  assert.ok(layout !== -1 && use < layout, "strip renders before the identity layout");
  const header = REPOS.indexOf('<div class="profile-header');
  assert.ok(header !== -1 && use < header, "strip renders above the identity header (not a subsection nav)");
  const h1 = REPOS.indexOf("<h1");
  assert.ok(h1 !== -1 && use < h1, "strip renders above the page h1");
  // Outside every Show: no view or variant gate can hide it on any route.
  const page = REPOS.indexOf('<div class="repos-page">');
  const between = REPOS.slice(page, use);
  const opens = (between.match(/<Show/g) || []).length;
  const closes = (between.match(/<\/Show>/g) || []).length;
  assert.ok(opens === closes, "every Show opened since the page root is closed before the strip");
  assert.ok(!between.includes("view()"), "no view gate hides the strip");
  assert.ok(!between.includes("isOrg()"), "no variant gate hides the strip");
});

test("identity header and both sidebars live inside the profile-only branch", () => {
  const outer = REPOS.indexOf('<Show when={view() === "profile"}>');
  assert.ok(outer !== -1, "a profile-view gate opens the identity region");
  const reposView = REPOS.indexOf('<Show when={view() === "repos"}>');
  assert.ok(reposView !== -1 && outer < reposView, "the profile branch opens before the repositories branch");
  for (const marker of [
    '<div class="profile-header',
    '<div class="profile-layout',
    'aria-label="Profile actions"',
    'aria-label="Organization actions"',
  ]) {
    const at = REPOS.indexOf(marker);
    assert.ok(at !== -1 && outer < at && at < reposView, `${marker} renders inside the profile-only branch`);
  }
});

test("repositories and organizations views render no profile chrome", () => {
  const reposView = REPOS.slice(REPOS.indexOf('<Show when={view() === "repos"}>'));
  for (const marker of ["profile-header", "profile-sidebar", "Profile actions", "Organization actions", "<h1", "View all →"]) {
    assert.ok(!reposView.includes(marker), `repositories view renders no ${marker}`);
  }
  assert.ok(reposView.includes("repos-toolbar"), "repositories view keeps its toolbar");
  assert.ok(reposView.includes("<RepoRow"), "repositories view keeps its grid");
  const orgsView = REPOS.slice(REPOS.indexOf('<Show when={view() === "orgs"}>'));
  for (const marker of ["profile-header", "profile-sidebar", "Profile actions", "Organization actions", "<h1", "repos-toolbar", "<RepoRow"]) {
    assert.ok(!orgsView.includes(marker), `organizations view renders no ${marker}`);
  }
  assert.ok(orgsView.includes("orgs-rail"), "organizations view keeps its membership list");
});

test("profile view renders as today: header + sidebar + teaser", () => {
  const outer = REPOS.indexOf('<Show when={view() === "profile"}>');
  const reposView = REPOS.indexOf('<Show when={view() === "repos"}>');
  const branch = REPOS.slice(outer, reposView);
  assert.ok(branch.includes('<div class="profile-header'), "identity header kept");
  assert.ok(branch.includes("<h1"), "username stays the page h1");
  assert.ok(branch.includes("{displayName()}"), "h1 renders the display name");
  assert.ok(branch.includes('aria-label="Profile actions"'), "user sidebar kept");
  assert.ok(branch.includes('aria-label="Organization actions"'), "org sidebar kept");
  assert.ok(branch.includes("View all →"), "repository-count teaser kept");
  assert.ok(branch.includes("href={`/${owner()}/repositories`}"), "teaser deep-links the tab route");
  assert.ok(!branch.includes("repos-toolbar"), "no toolbar in the profile branch");
  assert.ok(!branch.includes("<RepoRow"), "no repo grid in the profile branch");
  assert.ok(!branch.includes("orgs-rail"), "no membership rail in the profile branch");
});

test("active-tab derivation + underline + aria-current correct per route", () => {
  const strip = block(REPOS, "export function OwnerTabs", "function ProfileForm");
  assert.ok(strip.includes("`/${props.owner}/organizations`"), "derivation probes the organizations pathname");
  assert.ok(strip.includes("`/${props.owner}/repositories`"), "derivation probes the repositories pathname");
  assert.ok(strip.includes('? "orgs"'), "organizations pathname maps to the orgs tab");
  assert.ok(strip.includes('? "repos"'), "repositories pathname maps to the repos tab");
  assert.ok(strip.includes(': "profile"'), "anything else falls back to the profile tab");
  for (const tab of ["profile", "repos", "orgs"]) {
    assert.ok(strip.includes(`aria-current={active() === "${tab}" ? "page" : undefined}`), `${tab} tab marks aria-current`);
    assert.ok(
      strip.includes(`"!border-b-2 !border-emerald-500 !font-medium !text-zinc-900 dark:!text-zinc-100": active() === "${tab}"`),
      `${tab} tab carries the active underline`
    );
  }
  const clsUses = [...strip.matchAll(/class=\{cls\}/g)];
  assert.equal(clsUses.length, 3, "all three tabs share the one cls link treatment");
});

test("org variant keeps working: Profile | Repositories, gates/feeds unchanged", () => {
  const strip = block(REPOS, "export function OwnerTabs", "function ProfileForm");
  const orgsLink = strip.indexOf("href={`/${props.owner}/organizations`}");
  const gate = strip.lastIndexOf("<Show when={!props.isOrg}>", orgsLink);
  assert.ok(gate !== -1 && gate < orgsLink, "the Organizations tab stays user-profiles-only (#370)");
  const use = REPOS.indexOf("<OwnerTabs owner={owner()} count={repoCount()} isOrg={isOrg()} />");
  assert.ok(use !== -1, "the page feeds the org variant into the strip");
  const outer = REPOS.indexOf('<Show when={view() === "profile"}>');
  const reposView = REPOS.indexOf('<Show when={view() === "repos"}>');
  const branch = REPOS.slice(outer, reposView);
  assert.ok(branch.includes("<Show when={isOrg()}>"), "org identity renders in the profile branch");
  assert.ok(branch.includes("renderBody(getOrg().bio_markdown)"), "org bio still reads the org doc");
  assert.ok(branch.includes("Manage organization"), "Manage affordance stays grouped in the org sidebar");
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
  ]) {
    assert.ok(REPOS.includes(gate), `gate kept byte-identical: ${gate}`);
  }
  assert.ok(REPOS.includes("repos.owners.detailed(owner()"), "listing fetch untouched");
  assert.ok(REPOS.includes("repos.users.orgs(owner())"), "membership fetch untouched");
  assert.ok(REPOS.includes("repos.owners.updateProfile("), "profile save untouched");
  assert.ok(REPOS.includes("const repoCount = () =>"), "tab/teaser count still derives from the shared payload");
  assert.ok(REPOS.includes("return <OwnerPage view=\"profile\" />"), "default export still serves the profile");
  assert.ok(REPOS.includes("return <OwnerPage view=\"repos\" />"), "repositories export untouched");
  assert.ok(REPOS.includes("return <OwnerPage view=\"orgs\" />"), "organizations export untouched");
});

test("390px: strip scrolls internally, views wrap, no fixed widths", () => {
  const tag = tagOf(REPOS, '<nav\n      class="owner-tabs');
  assert.ok(tag.includes("max-w-full"), "strip never grows past the viewport");
  assert.ok(tag.includes("overflow-x-auto"), "strip scrolls internally like the repo tabs");
  assert.ok(tag.includes("whitespace-nowrap"), "tabs never wrap mid-label");
  assert.ok(CSS.includes(".owner-tabs { scrollbar-width: none; }"), "strip hides its scrollbar like the repo tabs");
  assert.ok(CSS.includes(".owner-tabs a { @apply shrink-0; }"), "strip links never shrink to unreadability");
  assert.ok(!tag.includes("w-["), "no fixed widths on the strip");
  const layout = tagOf(REPOS, '<div class="profile-layout');
  assert.ok(layout.includes("grid-cols-1"), "profile branch stacks at 390px (sidebar below main)");
  const reposView = REPOS.slice(REPOS.indexOf('<Show when={view() === "repos"}>'), REPOS.indexOf('<Show when={view() === "orgs"}>'));
  assert.ok(!reposView.includes("w-["), "no fixed widths in the repositories view");
  assert.ok(reposView.includes("flex-wrap"), "toolbar wraps at 390px");
  const orgsView = REPOS.slice(REPOS.indexOf('<Show when={view() === "orgs"}>'));
  assert.ok(!orgsView.includes("w-["), "no fixed widths in the organizations view");
  assert.ok(orgsView.includes("flex-wrap"), "membership list wraps at 390px");
});
