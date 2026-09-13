// web/test/unit/owner-links-445.test.js — Forgejo #445: owner-name links
// go to /:owner/repositories (the repos list), not the profile. Pins the
// link targets as source text (no DOM), mirroring
// owner-repos-tab-422.test.js / breadcrumb-head.test.js. No server changes,
// no new routes — pure link-target wiring.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const REPO = srcOf("../../src/pages/Repo.jsx");
const OWNERS = srcOf("../../src/pages/Owners.jsx");
const REPOS = srcOf("../../src/pages/Repos.jsx");
const TEAM = srcOf("../../src/pages/Team.jsx");

test("repo header breadcrumb: owner links to /:owner/repositories, repo name to /:owner/:repo", () => {
  assert.ok(
    REPO.includes("<A class=\"hover:underline\" href={`/${params.owner}/repositories`}>{params.owner}</A>"),
    "owner segment links to the repositories list, not the profile",
  );
  assert.ok(
    REPO.includes("<A class=\"hover:underline\" href={`/${full()}`}>{params.name}</A>"),
    "repo-name segment still links to /:owner/:repo",
  );
  assert.ok(
    !REPO.includes("<A class=\"hover:underline\" href={`/${params.owner}`}>{params.owner}</A>"),
    "no breadcrumb owner link still points at the bare profile path",
  );
});

test("explore: section heading and +N more link to /:owner/repositories", () => {
  const heading = 'href={`/${props.owner}/repositories`}';
  const hits = OWNERS.split(heading).length - 1;
  assert.equal(hits, 2, `heading + "+N more" both target the repos tab (found ${hits})`);
  assert.ok(!OWNERS.includes('href={`/${props.owner}`}'), "no explore owner link still points at the profile");
});

test("forked-from kept: links to the parent repo (owner/repo), not an owner page", () => {
  assert.ok(
    REPO.includes('forked from <A class="hover:underline" href={`/${s().fork_parent}`}>{s().fork_parent}</A>'),
    "forked-from still links to the parent repo page (GitHub behavior — fork_parent is owner/repo)",
  );
});

test("org-name links kept: Organizations tab + Team back link target the org profile", () => {
  assert.ok(REPOS.includes("href={`/${org}`}"), "member-orgs links still target the org profile");
  assert.ok(TEAM.includes("href={`/${org()}`}"), "Team back link still targets the org profile");
});

test("Profile tab untouched: still links to /:owner", () => {
  assert.ok(REPOS.includes("href={`/${props.owner}`}"), "Profile tab still links to the profile");
});

test("landing on /:owner/repositories shows the Repositories tab as active", () => {
  const tabs = REPOS.slice(
    REPOS.indexOf("export function OwnerTabs"),
    REPOS.indexOf("function ProfileForm"),
  );
  assert.ok(
    tabs.includes("loc.pathname === `/${props.owner}/repositories`"),
    "active-tab derivation keys off the /:owner/repositories pathname",
  );
  assert.ok(
    tabs.includes('aria-current={active() === "repos" ? "page" : undefined}'),
    "Repositories tab marks aria-current=\"page\" when active",
  );
});
