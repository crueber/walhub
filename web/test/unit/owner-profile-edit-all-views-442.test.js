// web/test/unit/owner-profile-edit-all-views-442.test.js — Forgejo #442:
// clicking Edit profile on the repositories/organizations tabs did nothing:
// the button toggles the shared-scope setEditing signal, but the
// ProfileForm lived inside the view() === "profile" gate, so on the other
// tabs the button vanished and no form appeared. Prescribed option 1: the
// form Show is hoisted out of the view gate into the main column above the
// per-view Shows — one copy, same gate, same onDone, no state plumbing (the
// signal already lived at the shared OwnerPage scope). The #420 bio
// hide-while-editing gate stays profile-view-only; the sidebar button gate
// is untouched.
//
// SUPERSEDED in placement by Forgejo #498 (this file's first two tests
// rewritten #498-scoped): the hoist leaked the form onto the
// repositories/organizations tabs, so the form re-scopes under
// view() === "profile" — profile view only. What #442 keeps: the sidebar
// button on every tab opens the editor THROUGH the #455 navigate-to-/{owner}
// flow (openEditor), the module-scope signal (it must survive the
// tab→profile remount), the byte-identical gate/onDone, and the
// profile-view-only #420 bio gate. The exit path (effect + cleanup) and the
// profile-only placement pins live in owner-profile-edit-exit-498.test.js.
// No DOM: JSX pinned as source text, mirroring
// profile-sidebar-421.test.js / profile-bio-editing.test.js.
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

test("form renders once, in the main column inside the profile branch only", () => {
  // Forgejo #498 (supersedes the #442 hoist this test used to pin): the
  // above-the-gates placement leaked the form onto the tabs, so the single
  // copy re-scopes under view() === "profile" — first in the profile
  // branch, above the identity block. Reachability from every tab is kept
  // through openEditor's navigate-to-/{owner} (#455), not through rendering
  // on every tab.
  const uses = [...REPOS.matchAll(/<ProfileForm/g)];
  assert.equal(uses.length, 1, "exactly one ProfileForm use (scoped, never forked per view)");
  const main = REPOS.indexOf('<div class="profile-main');
  const profileGate = REPOS.indexOf('<Show when={view() === "profile"}>');
  const reposGate = REPOS.indexOf('<Show when={view() === "repos"}>');
  const form = REPOS.indexOf("<Show when={getEditing() && getProfile()?.can_edit}>");
  assert.ok(main !== -1 && form !== -1 && main < form, "the form Show renders inside the main column");
  assert.ok(profileGate < form && form < reposGate, "the form nests inside the profile branch (before the repos branch)");
  const col = REPOS.indexOf('<div class="profile-sidebar-col');
  assert.ok(col !== -1 && form < col, "the form renders in the main column, not the sidebar");
});

test("form nests in the profile branch only — no copy on the tabs", () => {
  // Forgejo #498: the tabs show their normal content while editing (the
  // exit effect clears the state too — pinned in -exit-498).
  const profile = block(REPOS, '<Show when={view() === "profile"}>', '<Show when={view() === "repos"}>');
  assert.ok(profile.includes("<ProfileForm"), "the single form copy lives inside the profile branch");
  const repos = block(REPOS, '<Show when={view() === "repos"}>', '<Show when={view() === "orgs"}>');
  assert.ok(!repos.includes("<ProfileForm"), "no form copy inside the repositories branch");
  const orgs = REPOS.slice(REPOS.indexOf('<Show when={view() === "orgs"}>'));
  const orgsMain = orgs.slice(0, orgs.indexOf('<div class="profile-sidebar-col'));
  assert.ok(!orgsMain.includes("<ProfileForm"), "no form copy inside the organizations branch");
});

test("gate and save path byte-identical: editors only, onDone closes + invalidates", () => {
  assert.ok(
    REPOS.includes("<Show when={getEditing() && getProfile()?.can_edit}>"),
    "form renders only for editors with the form open"
  );
  const done = REPOS.slice(REPOS.indexOf("onDone={(saved)"));
  assert.ok(done.includes("setEditing(false)"), "onDone closes the form on save and on cancel");
  assert.ok(done.includes("invalidate(`profile:${owner()}`)"), "save invalidates the profile doc");
  assert.ok(REPOS.includes("repos.owners.updateProfile("), "profile save still rides the SDK");
});

test("editing state at module scope: survives tab navigation, no plumbing", () => {
  // Forgejo #455: the three owner routes are sibling Route components, so
  // tab navigation remounts OwnerPage — a local signal would reset. The
  // truth lives at module scope (per-owner slug), derived through local
  // accessors; still no plumbing between components.
  const signal = REPOS.indexOf("const [getEditingOwner, setEditingOwner] = createSignal(null);");
  const page = REPOS.indexOf("function OwnerPage(props) {");
  assert.ok(signal !== -1 && page !== -1 && signal < page, "the edit-open signal lives at module scope above OwnerPage");
  assert.ok(REPOS.includes("const getEditing = () => getEditingOwner() === owner();"), "per-owner read accessor in OwnerPage");
  assert.ok(REPOS.includes("const setEditing = (open) => setEditingOwner(open ? owner() : null);"), "per-owner write accessor in OwnerPage");
  assert.ok(REPOS.includes("onClick={openEditor}"), "the sidebar button opens the form through openEditor (#455)");
  const buttons = [...REPOS.matchAll(/onClick=\{openEditor\}/g)];
  assert.equal(buttons.length, 1, "exactly one edit opener (the shared sidebar button)");
});

test("button still disappears while editing on all views and reappears after", () => {
  assert.ok(
    REPOS.includes("<Show when={getProfile()?.can_edit && !getEditing()}>"),
    "the button gate is untouched (hidden while editing, restored on cancel/save)"
  );
  const side = block(REPOS, 'aria-label="Profile actions"', "</aside>");
  assert.ok(side.includes("Edit profile"), "Edit profile still grouped in the sidebar");
});

test("#420 intact: the rendered bio hides while editing, profile-view-only", () => {
  const profile = block(REPOS, '<Show when={view() === "profile"}>', '<Show when={view() === "repos"}>');
  assert.ok(
    profile.includes("<Show when={profile().bio_markdown && !getEditing()}>"),
    "the bio hide-while-editing gate stays in the profile branch"
  );
  const repos = block(REPOS, '<Show when={view() === "repos"}>', '<Show when={view() === "orgs"}>');
  assert.ok(!repos.includes("!getEditing()"), "no editing gate leaks into the repositories branch");
  assert.ok(REPOS.includes("innerHTML={renderBody(getBio())}"), "the form keeps its own inline preview while editing");
});

test("no data-fetch, cache-key, or other gating-logic changes", () => {
  for (const key of ["`repos:${owner()}`", "`profile:${owner()}`", "`org:${owner()}`", "`memberorgs:${owner()}`", "`user:${owner()}`", '"me"']) {
    assert.ok(REPOS.includes(key), `fetch surface untouched: ${key}`);
  }
  for (const gate of [
    // Forgejo #466: the canWrite gate left with the toolbar CTA (the navbar
    // create button owns creation now) — every remaining gate byte-identical.
    "<Show when={!isOrg()}>",
    "<Show when={isOrg()}>",
    "<Show when={getProfile()?.can_edit && !getEditing()}>",
    "<Show when={isSelf()}>",
    "<Show when={userSrc()}>",
    "<Show when={canManage()}>",
  ]) {
    assert.ok(REPOS.includes(gate), `gate kept byte-identical: ${gate}`);
  }
});
