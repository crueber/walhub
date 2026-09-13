// web/test/unit/owner-profile-edit-exit-498.test.js — Forgejo #498:
// profile edit mode persisted across tab navigation and beyond the owner
// page: the module-scope signal (#455) had no exit path, and the form Show
// sat above the per-view Shows (#442 hoist) so it rendered on the
// repositories/organizations tabs too. Two changes: (1) the form re-scopes
// under view() === "profile" (profile view only); (2) a pathname effect +
// unmount cleanup in OwnerPage clears the module state unless the resulting
// path is the editing owner's profile view exactly (tab switches close it
// too — unsaved edits discarded by unmount). #455 openEditor
// (set-then-navigate to /{owner}) must keep working with no toggle-fight.
// No DOM: the exit predicate is a pure lib helper (imported — real behavior
// tests), the JSX wiring is pinned as source text (the 442/455 precedent).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { editStaysOpen } from "../../src/lib/owners.js";

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

// --- exit predicate: the decision table (criterion 1, 2, 5) ---

test("stays open exactly on the editing owner's profile view", () => {
  assert.equal(editStaysOpen("/alice", "alice"), true);
});

test("tab switches exit (criterion 1: tabs show normal content, not the form)", () => {
  assert.equal(editStaysOpen("/alice/repositories", "alice"), false);
  assert.equal(editStaysOpen("/alice/organizations", "alice"), false);
});

test("navigating away entirely exits (criterion 2: return finds it closed)", () => {
  assert.equal(editStaysOpen("/explore", "alice"), false);
  assert.equal(editStaysOpen("/", "alice"), false);
  assert.equal(editStaysOpen("/alice/settings", "alice"), false);
  assert.equal(editStaysOpen("/bob", "alice"), false, "another owner's profile is still away");
});

test("cross-owner navigation exits even onto a profile view", () => {
  // Keys on the EDITING slug, not the page's owner: /bob is a profile view
  // but not alice's, so editing alice must not leak onto bob's page.
  assert.equal(editStaysOpen("/bob", "alice"), false);
  assert.equal(editStaysOpen("/bob/repositories", "alice"), false);
});

test("closed stays closed on every route (no phantom opens)", () => {
  for (const path of ["/alice", "/alice/repositories", "/explore", "/"]) {
    assert.equal(editStaysOpen(path, null), false);
    assert.equal(editStaysOpen(path, ""), false);
    assert.equal(editStaysOpen(path, undefined), false);
  }
});

test("#455 flow intact at the predicate level: tab→profile landing stays open", () => {
  // openEditor's set-then-navigate lands on /{owner} with the editing slug
  // set — the RESULTING pathname keeps the form open (no toggle-fight).
  assert.equal(editStaysOpen("/alice", "alice"), true);
});

// --- JSX wiring: form re-scoped under the profile view ---

test("form renders once, first inside the profile branch (criterion 1)", () => {
  const uses = [...REPOS.matchAll(/<ProfileForm/g)];
  assert.equal(uses.length, 1, "exactly one ProfileForm use (never forked per view)");
  const profileGate = REPOS.indexOf('<Show when={view() === "profile"}>');
  const reposGate = REPOS.indexOf('<Show when={view() === "repos"}>');
  const form = REPOS.indexOf("<Show when={getEditing() && getProfile()?.can_edit}>");
  assert.ok(profileGate !== -1 && reposGate !== -1 && form !== -1, "profile gate, repos gate, and form gate all present");
  assert.ok(profileGate < form && form < reposGate, "the form Show nests inside the profile branch (above the identity block, before the repos branch)");
  const above = REPOS.slice(REPOS.indexOf('<div class="profile-main'), profileGate);
  assert.ok(!above.includes("<ProfileForm"), "no form copy above the per-view Shows (the #442 hoist is gone)");
});

test("tabs carry no form: repos/orgs branches render their normal content", () => {
  const repos = block(REPOS, '<Show when={view() === "repos"}>', '<Show when={view() === "orgs"}>');
  assert.ok(!repos.includes("<ProfileForm"), "no form in the repositories branch");
  const orgs = REPOS.slice(REPOS.indexOf('<Show when={view() === "orgs"}>'));
  const orgsMain = orgs.slice(0, orgs.indexOf('<div class="profile-sidebar-col'));
  assert.ok(!orgsMain.includes("<ProfileForm"), "no form in the organizations branch");
  assert.ok(repos.includes("repos-toolbar"), "repositories tab keeps its toolbar content");
  assert.ok(orgsMain.includes("orgs-rail"), "organizations tab keeps its membership content");
});

// --- JSX wiring: exit effect + cleanup (criteria 2, 5) ---

test("OwnerPage binds useLocation and exits via the shared predicate", () => {
  const page = block(REPOS, "function OwnerPage(props) {", "export default function Repos()");
  assert.ok(page.includes("const loc = useLocation();"), "OwnerPage reads the pathname (the import was already there, unused)");
  assert.ok(
    page.includes("if (!editStaysOpen(loc.pathname, getEditingOwner())) setEditingOwner(null);"),
    "exit keyed on the resulting pathname vs the editing slug (not the page owner)",
  );
  assert.ok(page.includes("createEffect(() => {"), "pathname effect re-runs on every navigation");
  assert.ok(page.includes("onCleanup(() => {"), "unmount cleanup covers the leaving navigation the effect cannot re-run on");
});

test("no unconditional clear: cleanup keeps the #455 landing open", () => {
  const page = block(REPOS, "function OwnerPage(props) {", "export default function Repos()");
  const cleanups = [...page.matchAll(/onCleanup\(\(\) => \{[^}]*\}\);/gs)];
  assert.equal(cleanups.length, 1, "exactly one OwnerPage cleanup (the edit exit — popover cleanups live in Repo.jsx)");
  assert.ok(
    cleanups[0][0].includes("editStaysOpen"),
    "the cleanup applies the same resulting-pathname condition — the #455 remount (destination IS /{editing}) keeps the form, a true leave clears it",
  );
});

test("imports carry the exit seam with no new dependency", () => {
  assert.ok(
    REPOS.includes('import { createSignal, createEffect, onCleanup, For, Show } from "solid-js";'),
    "onCleanup joins the solid-js import (the Repo.jsx popover precedent)",
  );
  assert.ok(
    REPOS.includes('import { editStaysOpen, orderByActivity } from "../lib/owners.js";'),
    "predicate shared from the headless-testable owners lib (no new module, no new dep)",
  );
});

// --- no toggle-fight with #455 openEditor (criterion 5) ---

test("openEditor still opens then navigates (#455 preserved, criterion 3)", () => {
  const opener = block(REPOS, "const openEditor = () => {", "};");
  const setAt = opener.indexOf("setEditing(true)");
  const navAt = opener.indexOf('if (view() !== "profile") navigate(`/${owner()}`);');
  assert.ok(setAt !== -1 && navAt !== -1 && setAt < navAt, "open-then-navigate ordering: the state is set before the router moves, so the batched effect sees only the resulting pathname");
  assert.ok(!opener.includes("await"), "fully synchronous — no await between set and navigate for an effect to observe the intermediate path");
  const calls = [...REPOS.matchAll(/navigate\(`\//g)];
  assert.equal(calls.length, 1, `exactly one navigate-to-path call in the page (found ${calls.length}) — the profile view never navigates`);
});

// --- save/cancel + gates unchanged (criterion 4) ---

test("gate and save path byte-identical: editors only, onDone closes + invalidates", () => {
  assert.ok(
    REPOS.includes("<Show when={getEditing() && getProfile()?.can_edit}>"),
    "form renders only for editors with the form open (now nested, same condition)",
  );
  const done = REPOS.slice(REPOS.indexOf("onDone={(saved)"));
  assert.ok(done.includes("setEditing(false)"), "onDone closes the form on save and on cancel");
  assert.ok(done.includes("invalidate(`profile:${owner()}`)"), "save invalidates the profile doc");
  assert.ok(
    REPOS.includes("<Show when={profile().bio_markdown && !getEditing()}>"),
    "#420 bio hide-while-editing intact",
  );
});
