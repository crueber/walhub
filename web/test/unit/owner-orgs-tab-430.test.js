// web/test/unit/owner-orgs-tab-430.test.js — Forgejo #430: the owner
// Organizations tab at /:owner/organizations. /:owner is the rail-free
// profile view (identity, tab strip, repository-count teaser); the #423
// membership list moves verbatim into the new view="orgs" branch (links to
// /:org, explicit "No organizations" empty state, loading state preserved).
// Client-only: route + Repos.jsx views, no API change, the list rides the
// shared `memberorgs:{owner}` key. The strip reads Profile | Repositories |
// Organizations with the same classes/active-underline/aria-current on all
// three routes; org profiles keep Profile | Repositories (#370: member
// principals are email spellings, not routable owner slugs). No DOM: JSX
// pinned as source text, mirroring owner-repos-tab-422.test.js.
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

function block(src, start, end) {
  const s = src.indexOf(start);
  assert.ok(s !== -1, `expected block start ${start}`);
  const e = src.indexOf(end, s);
  assert.ok(e !== -1, `expected block end ${end}`);
  return src.slice(s, e);
}

test("route: /:owner/organizations registered before /:owner (static before dynamic)", () => {
  assert.ok(
    INDEX.includes('import Repos, { OwnerRepositories, OwnerOrganizations } from "./pages/Repos.jsx"'),
    "index imports the organizations-tab component next to Repos"
  );
  assert.ok(INDEX.includes('<Route path="/:owner/organizations" component={OwnerOrganizations} />'), "organizations tab route exists");
  assert.ok(INDEX.includes('<Route path="/:owner" component={Repos} />'), "/:owner profile route kept");
  const tab = INDEX.indexOf('path="/:owner/organizations"');
  const profile = INDEX.indexOf('path="/:owner" component={Repos}');
  const repo = INDEX.indexOf('path="/:owner/:name"');
  assert.ok(tab !== -1 && profile !== -1 && tab < profile, "tab route registers before the profile route");
  assert.ok(repo !== -1 && tab < repo, "tab route registers before /:owner/:name so it never resolves as a repo");
  const comment = INDEX.slice(Math.max(0, tab - 600), tab);
  assert.ok(comment.includes("Static before dynamic"), "the /orgs/new ordering rule is cited for the new route");
  assert.ok(comment.includes("organizations"), "the comment names the reservation");
  assert.ok(comment.includes("430"), "the comment cites the issue");
});

test("active-tab derivation covers all three routes", () => {
  const strip = block(REPOS, "export function OwnerTabs", "function ProfileForm");
  assert.ok(strip.includes("`/${props.owner}/organizations`"), "derivation probes the organizations pathname");
  assert.ok(strip.includes("`/${props.owner}/repositories`"), "derivation probes the repositories pathname");
  assert.ok(strip.includes('? "orgs"'), "organizations pathname maps to the orgs tab");
  assert.ok(strip.includes('? "repos"'), "repositories pathname maps to the repos tab");
  assert.ok(strip.includes(': "profile"'), "anything else falls back to the profile tab");
  const orgsProbe = strip.indexOf("`/${props.owner}/organizations`");
  const reposProbe = strip.indexOf("`/${props.owner}/repositories`");
  assert.ok(orgsProbe !== -1 && reposProbe !== -1 && orgsProbe < reposProbe, "organizations probed before repositories");
  assert.ok(strip.includes("active()"), "tabs read the shared derivation (no per-tab pathname math)");
});

test("third tab keeps the strip's classes, underline, and aria-current", () => {
  const strip = block(REPOS, "export function OwnerTabs", "function ProfileForm");
  assert.ok(strip.includes("Organizations"), "Organizations tab present");
  assert.ok(strip.includes("href={`/${props.owner}/organizations`}"), "Organizations tab links to the tab route");
  assert.ok(strip.includes('aria-current={active() === "orgs" ? "page" : undefined}'), "organizations tab marks aria-current");
  assert.ok(
    strip.includes('classList={{ "!border-b-2 !border-emerald-500 !font-medium !text-zinc-900 dark:!text-zinc-100": active() === "orgs" }}'),
    "organizations tab carries the same active underline as the first two"
  );
  const orgsLink = strip.indexOf("href={`/${props.owner}/organizations`}");
  const clsDef = strip.indexOf('const cls =');
  assert.ok(clsDef !== -1 && clsDef < orgsLink, "the third tab renders after the shared cls definition (same link treatment)");
  const clsUses = [...strip.matchAll(/class=\{cls\}/g)];
  assert.equal(clsUses.length, 3, "all three tabs share the one cls link treatment");
});

test("Organizations tab is user-profiles only (#370)", () => {
  const strip = block(REPOS, "export function OwnerTabs", "function ProfileForm");
  const orgsLink = strip.indexOf("href={`/${props.owner}/organizations`}");
  const gate = strip.lastIndexOf("<Show when={!props.isOrg}>", orgsLink);
  assert.ok(gate !== -1 && gate < orgsLink, "the Organizations tab renders behind an isOrg gate");
  const profileLink = strip.indexOf("href={`/${props.owner}`}");
  const reposLink = strip.indexOf("href={`/${props.owner}/repositories`}");
  assert.ok(!strip.slice(0, profileLink).includes("!props.isOrg"), "Profile tab unconditional");
  assert.ok(!strip.slice(0, reposLink).includes("<Show when={!props.isOrg}>"), "Repositories tab unconditional");
  assert.ok(REPOS.includes("props: owner, count (number|null), isOrg (bool)"), "the gating prop is documented on OwnerTabs");
  const use = REPOS.indexOf("<OwnerTabs owner={owner()} count={repoCount()} isOrg={isOrg()} />");
  assert.ok(use !== -1, "the page feeds the org variant into the strip");
});

test("orgs view: the membership list moved verbatim behind view()===\"orgs\"", () => {
  const start = REPOS.indexOf('<Show when={view() === "orgs"}>');
  assert.ok(start !== -1, "the membership list lives behind the orgs-view gate");
  const listing = REPOS.slice(start);
  assert.ok(listing.includes('<section class="orgs-rail mt-6" aria-label="Organizations">'), "rail hook + landmark kept");
  assert.ok(listing.includes("<h3 class=\"text-base font-semibold\">Organizations</h3>"), "section heading kept");
  assert.ok(listing.includes("normalizeMemberOrgs(orgs())"), "list shape still coerced through the shared helper");
  assert.ok(listing.includes("href={`/${org}`}"), "each org links to /:org");
  assert.ok(listing.includes("No organizations"), "explicit GitHub-parity empty state kept");
  assert.ok(listing.includes("loading…"), "loading state preserved");
  assert.ok(REPOS.includes("memberorgs:${owner()}"), "the #423 cache key rides along (no new fetch)");
  assert.ok(REPOS.includes("repos.users.orgs(owner())"), "the membership fetch is untouched");
  assert.ok(!listing.includes("repos-toolbar"), "no repos toolbar on the organizations tab");
  assert.ok(!listing.includes("<RepoRow"), "no repo grid on the organizations tab");
  assert.ok(!listing.includes("View all →"), "no profile teaser on the organizations tab");
});

test("profile view: rail-free, teaser intact", () => {
  const teaser = block(REPOS, '<Show when={view() === "profile"}>', '<Show when={view() === "repos"}>');
  assert.ok(!teaser.includes("orgs-rail"), "no membership rail on the profile view (#430 planner's call: rail-free)");
  assert.ok(!teaser.includes("No organizations"), "no membership empty state on the profile view");
  assert.ok(teaser.includes("View all →"), "teaser links onward (no dead profile)");
  assert.ok(teaser.includes("href={`/${owner()}/repositories`}"), "teaser deep-links the repositories tab");
  assert.ok(!teaser.includes("repos-toolbar"), "no toolbar on the profile view");
  assert.ok(!teaser.includes("<RepoRow"), "no repo grid on the profile view");
});

test("views: named export serves the organizations tab", () => {
  assert.ok(REPOS.includes("return <OwnerPage view=\"orgs\" />"), "named export serves the organizations tab");
  assert.ok(REPOS.includes("export function OwnerOrganizations()"), "the tab route has its named component");
  assert.ok(REPOS.includes("return <OwnerPage view=\"profile\" />"), "default export still serves /:owner as the profile");
  assert.ok(REPOS.includes("return <OwnerPage view=\"repos\" />"), "repositories export untouched");
  assert.ok(REPOS.includes("organizations"), "the reservation note names the shadowed org slug");
});

test("repositories count badge behavior unchanged", () => {
  const strip = block(REPOS, "export function OwnerTabs", "function ProfileForm");
  assert.ok(strip.includes("tab-badge"), "the badge hook survives");
  const badge = strip.indexOf("tab-badge");
  const reposLink = strip.indexOf("href={`/${props.owner}/repositories`}");
  const orgsLink = strip.indexOf("href={`/${props.owner}/organizations`}");
  assert.ok(badge > reposLink && badge < orgsLink, "the badge still rides the Repositories tab only");
  assert.ok(strip.includes("props.count"), "badge still derives from the shared listing payload (no new fetch)");
});
