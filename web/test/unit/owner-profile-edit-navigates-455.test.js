// web/test/unit/owner-profile-edit-navigates-455.test.js — Forgejo #455:
// clicking Edit profile on the repositories/organizations tabs opened the
// shared form inline but never left the tab — the tab bar kept highlighting
// the list tab while the profile form rendered above it. The sidebar button
// now routes to /{owner} from non-profile tabs (OwnerTabs derives Profile as
// active on that pathname) with the module-scope edit-open state keeping the
// form open across the remount; on the profile view the click stays
// setEditing(true) with no navigation. Gates byte-identical (#442 form,
// button, #420 bio). No DOM: JSX pinned as source text, mirroring
// owner-profile-edit-all-views-442.test.js / owner-links-445.test.js.
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

test("useNavigate seam: imported from solid-router and bound in OwnerPage", () => {
  assert.ok(
    REPOS.includes('import { useParams, useLocation, useNavigate, A } from "@solidjs/router";'),
    "useNavigate joins the existing solid-router import (no new dependency)",
  );
  const page = block(REPOS, "function OwnerPage(props) {", "export default function Repos()");
  assert.ok(page.includes("const navigate = useNavigate();"), "OwnerPage binds navigate");
});

test("openEditor: opens the form, navigates only off the profile view", () => {
  const opener = block(REPOS, "const openEditor = () => {", "};");
  assert.ok(opener.includes("setEditing(true)"), "the form opens on every tab");
  assert.ok(
    opener.includes('if (view() !== "profile") navigate(`/${owner()}`);'),
    "non-profile tabs route to /{owner} (Profile derives active there)",
  );
});

test("profile-view click unchanged: no navigation, form opens as today", () => {
  const calls = [...REPOS.matchAll(/navigate\(`\//g)];
  assert.equal(calls.length, 1, `exactly one navigate-to-path call in the page (found ${calls.length})`);
  assert.ok(
    !REPOS.includes('navigate(`/${owner()}/repositories`)') && !REPOS.includes('navigate(`/${owner()}/organizations`)'),
    "no navigation targets a list tab — the profile view never navigates",
  );
});

test("edit-open state shared at module scope so the form survives the remount", () => {
  const signal = REPOS.indexOf("const [getEditingOwner, setEditingOwner] = createSignal(null);");
  const page = REPOS.indexOf("function OwnerPage(props) {");
  assert.ok(signal !== -1 && page !== -1 && signal < page, "module-scope signal above OwnerPage (a local signal would reset on the tab→profile remount)");
  assert.ok(REPOS.includes("const getEditing = () => getEditingOwner() === owner();"), "open only for this page's slug");
  assert.ok(REPOS.includes("const setEditing = (open) => setEditingOwner(open ? owner() : null);"), "close clears; switching owners never leaks the form");
});

test("gates byte-identical: button, form, #420 bio", () => {
  assert.ok(
    REPOS.includes("<Show when={getProfile()?.can_edit && !getEditing()}>"),
    "button renders only for editors while closed",
  );
  assert.ok(
    REPOS.includes("<Show when={getEditing() && getProfile()?.can_edit}>"),
    "form renders only for editors while open",
  );
  const profile = block(REPOS, '<Show when={view() === "profile"}>', '<Show when={view() === "repos"}>');
  assert.ok(
    profile.includes("<Show when={profile().bio_markdown && !getEditing()}>"),
    "#420 bio hide-while-editing stays profile-view-only",
  );
});

test("Profile tab still derives active on /{owner} (no tab wiring change)", () => {
  const tabs = block(REPOS, "export function OwnerTabs", "function ProfileForm");
  assert.ok(tabs.includes("href={`/${props.owner}`}"), "Profile tab still links to /{owner}");
  assert.ok(tabs.includes(': "profile";'), "any non-repositories/organizations pathname falls back to profile");
});
