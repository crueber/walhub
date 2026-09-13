// web/test/unit/owner-repos-tab-422.test.js — Forgejo #422: the owner
// repositories listing moves off /:owner into its own tab at
// /:owner/repositories. /:owner is the profile view (header, tab strip,
// repository-count teaser — no grid); /:owner/repositories is the
// repositories tab (tab strip + the toolbar/CTA/grid/import listing moved
// verbatim). Client-only: routes + Repos.jsx views, no API change, the
// listing rides the shared `repos:{owner}` key, <RepoRow> stays shared
// (no fork). Tab-strip (not a simple link) reusing the repo page's tab
// bar anatomy (Repo.jsx). No DOM: JSX pinned as source text, mirroring
// repos-toolbar-413.test.js / profile-sidebar-421.test.js.
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
const REPO = srcOf("../../src/pages/Repo.jsx");
const OWNERS = srcOf("../../src/pages/Owners.jsx");
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

test("route: /:owner/repositories registered before /:owner (static before dynamic)", () => {
  assert.ok(
    INDEX.includes('import Repos, { OwnerRepositories, OwnerOrganizations } from "./pages/Repos.jsx"'),
    "index imports the repositories-tab component next to Repos (organizations tab joins it per #430)"
  );
  assert.ok(INDEX.includes('<Route path="/:owner/repositories" component={OwnerRepositories} />'), "repositories tab route exists");
  assert.ok(INDEX.includes('<Route path="/:owner" component={Repos} />'), "/:owner profile route kept");
  const tab = INDEX.indexOf('path="/:owner/repositories"');
  const profile = INDEX.indexOf('path="/:owner" component={Repos}');
  const repo = INDEX.indexOf('path="/:owner/:name"');
  assert.ok(tab !== -1 && profile !== -1 && tab < profile, "tab route registers before the profile route");
  assert.ok(repo !== -1 && tab < repo, "tab route registers before /:owner/:name so it never resolves as a repo");
  const comment = INDEX.slice(Math.max(0, tab - 600), tab);
  assert.ok(comment.includes("Static before dynamic"), "the /orgs/new ordering rule is cited for the new route");
  assert.ok(comment.includes("repositories"), "the comment names the reservation");
});

test("tab strip reuses the repo page's tab bar anatomy", () => {
  assert.ok(REPOS.includes("export function OwnerTabs"), "the strip is a named component (tab strip, not a bare link)");
  const strip = block(REPOS, "export function OwnerTabs", "function ProfileForm");
  assert.ok(strip.includes('aria-label="owner sections"'), "the strip is a labelled landmark like the repo tabs");
  assert.ok(strip.includes("owner-tabs"), "own CSS hook (not the repo-tabs class)");
  const tag = tagOf(REPOS, '<nav\n      class="owner-tabs');
  for (const token of ["mb-4", "flex", "max-w-full", "gap-1", "overflow-x-auto", "whitespace-nowrap", "border-b"]) {
    assert.ok(tag.includes(token), `strip carries the repo-tab shape token: ${token}`);
  }
  const repoTag = tagOf(REPO, '<nav ref={tabsNav} class="repo-tabs');
  for (const token of ["flex", "max-w-full", "gap-1", "overflow-x-auto", "whitespace-nowrap", "border-b"]) {
    assert.ok(repoTag.includes(token), `repo-tab reference shape carries: ${token}`);
  }
  assert.ok(strip.includes("Profile"), "Profile tab present");
  assert.ok(strip.includes("Repositories"), "Repositories tab present");
  assert.ok(strip.includes("href={`/${props.owner}`}"), "Profile tab links to /:owner");
  assert.ok(strip.includes("href={`/${props.owner}/repositories`}"), "Repositories tab links to the tab route");
  assert.ok(strip.includes("rounded-t px-3 py-1.5 text-sm"), "tab link treatment matches Repo.jsx");
  assert.ok(strip.includes("!border-b-2 !border-emerald-500"), "active-tab underline matches Repo.jsx");
  assert.ok(strip.includes('aria-current={active() === "profile" ? "page" : undefined}'), "profile tab marks aria-current");
  assert.ok(strip.includes('aria-current={active() === "repos" ? "page" : undefined}'), "repositories tab marks aria-current");
  assert.ok(strip.includes("tab-badge"), "repositories tab carries the count badge hook");
});

test("strip renders on both routes and both variants (outside every isOrg Show)", () => {
  const use = REPOS.indexOf("<OwnerTabs owner={owner()} count={repoCount()} isOrg={isOrg()} />");
  assert.ok(use !== -1, "the page renders the strip with owner + shared-payload count + org gating (#430)");
  const orgOpen = REPOS.indexOf("<Show when={isOrg()}>");
  const between = REPOS.slice(orgOpen, use);
  const opens = (between.match(/<Show/g) || []).length;
  const closes = (between.match(/<\/Show>/g) || []).length;
  assert.ok(opens === closes, "every Show opened since the org block is closed before the strip (strip is outside)");
  assert.ok(!between.includes("view()"), "no view gate hides the strip on either route");
});

test("profile view: teaser with count + link, no grid/toolbar/import", () => {
  const teaser = block(REPOS, '<Show when={view() === "profile"}>', '<Show when={view() === "repos"}>');
  assert.ok(teaser.includes("orderByActivity"), "teaser count rides the shared listing payload");
  assert.ok(teaser.includes("repositor"), "teaser names the repository count");
  assert.ok(teaser.includes("View all →"), "teaser links onward (no dead profile)");
  assert.ok(teaser.includes("href={`/${owner()}/repositories`}"), "teaser deep-links the tab route");
  assert.ok(!teaser.includes("repos-toolbar"), "no toolbar on the profile view");
  assert.ok(!teaser.includes("<RepoRow"), "no repo grid on the profile view");
  assert.ok(!teaser.includes("import into {owner()}"), "no import footer on the profile view");
  assert.ok(!teaser.includes("New repository"), "no create CTA on the profile view");
});

test("repositories view: toolbar + CTA + grid + import moved verbatim", () => {
  const start = REPOS.indexOf('<Show when={view() === "repos"}>');
  assert.ok(start !== -1, "the listing lives behind the repos-view gate");
  const listing = REPOS.slice(start);
  const toolbar = block(listing, "repos-toolbar", "</div>");
  assert.ok(toolbar.includes("Repositories</h3>"), "heading text unchanged");
  assert.ok(toolbar.includes("New repository"), "CTA moved with the list it populates");
  assert.ok(toolbar.includes("<Show when={canWrite()}>"), "CTA gate byte-identical");
  assert.ok(toolbar.includes("href={`/new?owner=${encodeURIComponent(owner())}`}"), "CTA href byte-identical");
  assert.ok(toolbar.includes("flex-wrap"), "390px toolbar wrap kept");
  assert.ok(listing.includes("<RepoRow owner={owner()}"), "grid rows render through the shared component");
  assert.ok(listing.includes("orderByActivity(doc().repos)"), "grid ordering untouched");
  assert.ok(listing.includes("import into {owner()}"), "import link moved with the listing");
  assert.ok(listing.includes("href={`/import?owner=${encodeURIComponent(owner())}`}"), "import href unchanged");
  assert.ok(listing.includes("organization settings"), "org settings footer link moved with the listing");
});

test("views: default export is the profile, named export is the tab", () => {
  assert.ok(REPOS.includes("function OwnerPage(props)"), "one shared page component (never forked)");
  assert.ok(REPOS.includes("const view = () => props.view ?? \"profile\""), "view defaults to the profile");
  assert.ok(REPOS.includes("return <OwnerPage view=\"profile\" />"), "default export serves /:owner as the profile");
  assert.ok(REPOS.includes("export function OwnerRepositories()"), "the tab route has its named component");
  assert.ok(REPOS.includes("return <OwnerPage view=\"repos\" />"), "named export serves the repositories tab");
});

test("RepoRow stays shared: no fork, /explore untouched", () => {
  const defs = [...REPOS.matchAll(/function RepoRow\(/g)];
  assert.equal(defs.length, 1, "exactly one RepoRow definition");
  assert.ok(REPOS.includes("export function RepoRow"), "RepoRow stays exported from Repos.jsx");
  assert.ok(OWNERS.includes("RepoRow"), "/explore still consumes the shared row");
  assert.ok(OWNERS.includes("from \"./Repos.jsx\"") || OWNERS.includes("from './Repos.jsx'"), "/explore still imports the row from Repos.jsx");
  assert.ok(!OWNERS.includes("function RepoRow"), "/explore does not fork the row");
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
    "<Show when={canManage()}>",
  ]) {
    assert.ok(REPOS.includes(gate), `gate kept byte-identical: ${gate}`);
  }
  assert.ok(REPOS.includes("repos.owners.detailed(owner()"), "listing fetch untouched");
  assert.ok(REPOS.includes("repos.owners.updateProfile("), "profile save untouched");
  assert.ok(REPOS.includes("const repoCount = () =>"), "tab/teaser count derives from the shared payload (no new fetch)");
  assert.ok(!REPOS.includes("repos.owners.list("), "no new endpoint rides along for the count");
});

test("390px: strip scrolls internally, teaser wraps, no fixed widths", () => {
  const tag = tagOf(REPOS, '<nav\n      class="owner-tabs');
  assert.ok(tag.includes("max-w-full"), "strip never grows past the viewport");
  assert.ok(tag.includes("overflow-x-auto"), "strip scrolls internally like the repo tabs");
  assert.ok(tag.includes("whitespace-nowrap"), "tabs never wrap mid-label");
  assert.ok(CSS.includes(".owner-tabs { scrollbar-width: none; }"), "strip hides its scrollbar like the repo tabs");
  assert.ok(CSS.includes(".owner-tabs a { @apply shrink-0; }"), "strip links never shrink to unreadability");
  assert.ok(!tag.includes("w-["), "no fixed widths on the strip");
});
