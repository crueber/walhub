// web/test/unit/owner-repos-tab-422.test.js — Forgejo #422: the owner
// repositories listing moves off /:owner into its own tab at
// /:owner/repositories. /:owner is the profile view (identity only — the
// #437 sidebar tab list plus the #421 avatar asides live in the shared
// sidebar shell, no grid, no teaser); /:owner/repositories is the
// repositories tab (the toolbar/CTA/grid/import listing moved verbatim
// into the shared main column). Client-only: routes + Repos.jsx views, no
// API change, the listing rides the shared `repos:{owner}` key, <RepoRow>
// stays shared (no fork). The tab list (Forgejo #437, superseding the
// #435 top strip) is a vertical sidebar list, not the repo page's tab bar.
// No DOM: JSX pinned as source text, mirroring
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

test("tab list is the vertical sidebar list (Forgejo #437, not the repo tab bar)", () => {
  assert.ok(REPOS.includes("export function OwnerTabs"), "the list is a named component (tab list, not a bare link)");
  const tabs = block(REPOS, "export function OwnerTabs", "function ProfileForm");
  assert.ok(tabs.includes('aria-label="owner sections"'), "the list is a labelled landmark");
  assert.ok(tabs.includes("owner-tabs"), "own CSS hook kept");
  const tag = tagOf(REPOS, '<nav\n      class="owner-tabs');
  for (const token of ["flex", "w-full", "flex-col", "rounded-lg", "border", "border-zinc-200", "dark:border-zinc-700"]) {
    assert.ok(tag.includes(token), `list carries the sidebar-box token: ${token}`);
  }
  assert.ok(tabs.includes("Profile"), "Profile tab present");
  assert.ok(tabs.includes("Repositories"), "Repositories tab present");
  assert.ok(tabs.includes("href={`/${props.owner}`}"), "Profile tab links to /:owner");
  assert.ok(tabs.includes("href={`/${props.owner}/repositories`}"), "Repositories tab links to the tab route");
  assert.ok(tabs.includes("block w-full rounded-md px-3 py-1.5 text-sm"), "stacked full-width link treatment");
  assert.ok(tabs.includes('aria-current={active() === "profile" ? "page" : undefined}'), "profile tab marks aria-current");
  assert.ok(tabs.includes('aria-current={active() === "repos" ? "page" : undefined}'), "repositories tab marks aria-current");
  assert.ok(tabs.includes("tab-badge"), "repositories tab carries the count badge hook");
});

test("sidebar shell (tabs first) renders on both routes and both variants", () => {
  const use = REPOS.indexOf("<OwnerTabs owner={owner()} count={repoCount()} isOrg={isOrg()} />");
  assert.ok(use !== -1, "the sidebar renders the list with owner + shared-payload count + org gating (#430)");
  // Forgejo #437: one shared profile layout on every owner route — the
  // layout opens outside every Show, and the sidebar column (tabs first,
  // then the gated asides) opens after every view branch closes, so no
  // view or variant gate can hide tab navigation on any route.
  const page = REPOS.indexOf('<div class="repos-page">');
  const layout = REPOS.indexOf('<div class="profile-layout');
  assert.ok(page !== -1 && page < layout && layout < use, "page root, then the shared layout, then the tab list");
  const head = REPOS.slice(page, layout);
  const headOpens = (head.match(/<Show/g) || []).length;
  const headCloses = (head.match(/<\/Show>/g) || []).length;
  assert.ok(headOpens === headCloses, "every Show opened since the page root is closed before the layout (layout is outside)");
  assert.ok(!head.includes("view()"), "no view gate hides the layout on any route");
  assert.ok(!head.includes("isOrg()"), "no variant gate hides the layout on either variant");
  const col = REPOS.indexOf('<div class="profile-sidebar-col');
  assert.ok(col !== -1 && col < use, "the tab list renders inside the sidebar column");
  const tail = REPOS.slice(col);
  assert.ok(tail.includes('aria-label="Profile actions"'), "user aside kept below the tabs");
  assert.ok(tail.includes('aria-label="Organization actions"'), "org aside kept below the tabs");
});

test("profile view: identity only, no teaser, no grid/toolbar/import", () => {
  const teaser = block(REPOS, '<Show when={view() === "profile"}>', '<Show when={view() === "repos"}>');
  assert.ok(teaser.includes("<h1"), "identity header kept as the page h1");
  assert.ok(!teaser.includes("View all →"), "the leftover count teaser is deleted (#437)");
  assert.ok(!teaser.includes("href={`/${owner()}/repositories`}"), "no deep-link to the tab route on the profile view");
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

test("390px: vertical list stacks full-width, views wrap, no fixed widths", () => {
  const tag = tagOf(REPOS, '<nav\n      class="owner-tabs');
  assert.ok(tag.includes("w-full"), "list fills the sidebar column at every width");
  assert.ok(tag.includes("flex-col"), "tabs stack vertically (no internal scroll)");
  assert.ok(CSS.includes(".owner-tabs a { @apply block w-full; }"), "links stack full-width via the hook");
  assert.ok(!tag.includes("w-["), "no fixed widths on the list");
});
