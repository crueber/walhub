// web/test/unit/identity-nav.test.js — Forgejo #371: navbar identity surface.
//
// (1) The render matrix (mode × auth state × admin) over the pure
// lib/identity.js navModel — no DOM. (2) Source-text pins that App.jsx wires
// the model (shared "me" cache, discovery, Login href, IdentityMenu) and
// that IdentityMenu.jsx matches the app's popover patterns (outside-click,
// Esc + focus return, menu roles) with logout as a session-clearing anchor.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { navModel, menuItems, isSignedIn, canUseSetup, authMode } from "../../src/lib/identity.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const oidcLogin = {
  auth: { mode: "oidc", browser_login: true, login_url: "/_auth/login" },
};
const oidcDead = {
  auth: { mode: "oidc", browser_login: false, login_url: "" },
};
const noneDisc = { auth: { mode: "none", browser_login: false, login_url: "" } };
const tokenDisc = { auth: { mode: "token", browser_login: false, login_url: "" } };
const alice = { principal: "alice", write: true, anonymous: false, admin: false };
const root = { principal: "root", write: true, anonymous: false, admin: true };
const anon = { principal: "anonymous", write: false, anonymous: true, admin: false };

// --- matrix: signed-out ------------------------------------------------------

test("signed-out oidc + browser login → Login button with next, legacy nav", () => {
  const nav = navModel({ me: null, discovery: oidcLogin }, "/acme/widgets");
  assert.equal(nav.showLogin, true);
  assert.equal(nav.loginHref, "/_auth/login?next=%2Facme%2Fwidgets");
  assert.equal(nav.showIdentity, false);
  assert.equal(nav.showKeysInNav, true);
  assert.equal(nav.showSetupInNav, true);
  assert.deepEqual(nav.menuItems, []);
});

test("signed-out oidc without browser login → no Login button", () => {
  const nav = navModel({ me: null, discovery: oidcDead }, "/");
  assert.equal(nav.showLogin, false);
  assert.equal(nav.loginHref, "");
  assert.equal(nav.showIdentity, false);
});

test("signed-out in none mode → no Login, nav unchanged, no identity", () => {
  const nav = navModel({ me: null, discovery: noneDisc }, "/");
  assert.equal(nav.showLogin, false);
  assert.equal(nav.showIdentity, false);
  assert.equal(nav.signedIn, false);
  assert.equal(nav.showKeysInNav, true);
  assert.equal(nav.showInvitationsInNav, true);
  assert.equal(nav.showSetupInNav, true);
});

test("signed-out in token mode → no Login button", () => {
  const nav = navModel({ me: null, discovery: tokenDisc }, "/");
  assert.equal(nav.showLogin, false);
  assert.equal(nav.showIdentity, false);
});

test("anonymous principal is signed out even with a login flow", () => {
  const nav = navModel({ me: anon, discovery: oidcLogin }, "/");
  assert.equal(nav.signedIn, false);
  assert.equal(nav.showLogin, true);
  assert.equal(nav.showIdentity, false);
});

test("anonymous_read=false 401 (me null) keeps the Login path via discovery", () => {
  // /api/v1/me is AuthRead-gated: anon + flag off → 401 → null, while the
  // AuthOpen discovery still advertises the flow.
  const nav = navModel({ me: null, discovery: oidcLogin }, "/explore");
  assert.equal(nav.showLogin, true);
  assert.equal(nav.loginHref, "/_auth/login?next=%2Fexplore");
});

test("discovery failure → legacy nav, nothing identity (fail closed)", () => {
  const nav = navModel({ me: null, discovery: null }, "/");
  assert.equal(nav.mode, "");
  assert.equal(nav.showLogin, false);
  assert.equal(nav.showIdentity, false);
  assert.equal(nav.showKeysInNav, true);
  assert.equal(nav.showSetupInNav, true);
});

// --- matrix: signed-in -------------------------------------------------------

test("signed-in oidc non-admin → identity menu without setup", () => {
  const nav = navModel({ me: alice, discovery: oidcLogin }, "/");
  assert.equal(nav.signedIn, true);
  assert.equal(nav.showLogin, false);
  assert.equal(nav.showIdentity, true);
  assert.equal(nav.username, "alice");
  assert.deepEqual(
    nav.menuItems.map((i) => i.kind),
    ["profile", "keys", "invitations", "logout"],
  );
  assert.equal(nav.showKeysInNav, false);
  assert.equal(nav.showInvitationsInNav, false);
  assert.equal(nav.showSetupInNav, false);
});

test("signed-in with avatar_url → navModel carries the avatar URL", () => {
  const me = { ...alice, avatar_url: "/api/v1/users/alice/avatar?v=ts" };
  const nav = navModel({ me, discovery: oidcLogin }, "/");
  assert.equal(nav.avatarUrl, "/api/v1/users/alice/avatar?v=ts");
});

test("signed-in without avatar_url → username fallback (empty avatarUrl)", () => {
  const nav = navModel({ me: alice, discovery: oidcLogin }, "/");
  assert.equal(nav.avatarUrl, "");
});

test("signed-out → no avatar URL even when discovery is healthy", () => {
  const nav = navModel({ me: null, discovery: oidcLogin }, "/");
  assert.equal(nav.avatarUrl, "");
  const anonNav = navModel({ me: anon, discovery: oidcLogin }, "/");
  assert.equal(anonNav.avatarUrl, "");
});

test("signed-in oidc admin → setup joins the menu", () => {
  const nav = navModel({ me: root, discovery: oidcLogin }, "/");
  assert.deepEqual(
    nav.menuItems.map((i) => i.kind),
    ["profile", "keys", "invitations", "setup", "logout"],
  );
  assert.equal(nav.menuItems.find((i) => i.kind === "setup").href, "/setup");
});

test("menu hrefs: profile is the owner page, logout clears + returns home", () => {
  const items = menuItems(alice, "oidc");
  assert.equal(items.find((i) => i.kind === "profile").href, "/alice");
  assert.equal(items.find((i) => i.kind === "keys").href, "/keys");
  assert.equal(items.find((i) => i.kind === "invitations").href, "/invitations");
  assert.equal(items.find((i) => i.kind === "logout").href, "/_auth/logout?next=/");
});

test("signed-in token mode gets the same menu treatment", () => {
  const nav = navModel({ me: alice, discovery: tokenDisc }, "/");
  assert.equal(nav.showIdentity, true);
  assert.equal(nav.showLogin, false);
  assert.equal(nav.showKeysInNav, false);
});

test("none mode never signs in (the none principal is anon-all)", () => {
  assert.equal(isSignedIn({ principal: "anon", write: true, anonymous: false, admin: true }, "none"), false);
  const nav = navModel(
    { me: { principal: "anon", write: true, anonymous: false, admin: true }, discovery: noneDisc },
    "/",
  );
  assert.equal(nav.showIdentity, false);
  assert.equal(nav.showSetupInNav, true);
});

test("helpers: authMode/canUseSetup edges", () => {
  assert.equal(authMode(null), "");
  assert.equal(authMode({ auth: { mode: "bogus" } }), "");
  assert.equal(authMode(oidcLogin), "oidc");
  assert.equal(canUseSetup(alice, "oidc"), false);
  assert.equal(canUseSetup(root, "oidc"), true);
  assert.equal(canUseSetup(null, "none"), true);
});

test("login next defaults to / and encodes query strings", () => {
  assert.equal(navModel({ me: null, discovery: oidcLogin }).loginHref, "/_auth/login?next=%2F");
  assert.equal(
    navModel({ me: null, discovery: oidcLogin }, "/invitations?id=1&token=a%20b").loginHref,
    "/_auth/login?next=%2Finvitations%3Fid%3D1%26token%3Da%2520b",
  );
});

// --- App.jsx wiring pins -----------------------------------------------------

const APP = srcOf("../../src/App.jsx");

test("App lifts me() + discovery into the shell on shared cache keys", () => {
  assert.ok(APP.includes('useData("me"'), "shell must fetch me on the shared key");
  assert.ok(APP.includes("repos.me()"), "shell must call the SDK me()");
  assert.ok(APP.includes('useData("discovery"'), "shell must fetch discovery");
  assert.ok(APP.includes("repos.discovery()"), "shell must call the SDK discovery()");
  assert.ok(APP.includes(".catch(() => null)"), "auth payloads must degrade to null, never the tray");
});

test("App renders Login through the OIDC pathway with next", () => {
  assert.ok(APP.includes("nav().showLogin"), "Login gated on the model");
  assert.ok(APP.includes("nav().loginHref"), "Login href carries next");
  assert.ok(APP.includes(">Login<"), "Login button label kept");
});

test("App renders the identity control far-right in the cluster", () => {
  assert.ok(APP.includes("<IdentityMenu"), "identity menu mounted");
  assert.ok(APP.includes("nav().showIdentity"), "menu gated on signed-in");
  assert.ok(APP.includes("avatarUrl={nav().avatarUrl}"), "stable avatar URL passed through (#376)");
  const right = APP.slice(APP.indexOf('<div class="ml-auto'));
  assert.ok(right.indexOf("<NotificationTray") < right.indexOf("<IdentityMenu"), "identity sits right of the tray (#390 far-right)");
  assert.ok(right.indexOf("Toggle dark mode") < right.indexOf("<IdentityMenu"), "identity sits right of the theme toggle (#390 far-right)");
});

test("App moves keys/invitations/setup out of the primary nav when signed in", () => {
  assert.ok(APP.includes("nav().showKeysInNav"), "keys nav-gated");
  assert.ok(APP.includes("nav().showInvitationsInNav"), "invitations nav-gated");
  assert.ok(APP.includes("nav().showSetupInNav"), "setup nav-gated");
});

// --- IdentityMenu.jsx popover pins -------------------------------------------

const MENU = srcOf("../../src/components/IdentityMenu.jsx");

test("IdentityMenu: outside-click dismisses without fighting the toggle", () => {
  assert.ok(MENU.includes('document.addEventListener("click", onDocClick)'), "document click listener");
  assert.ok(MENU.includes("!root.contains(e.target)"), "clicks inside the root ignored");
  assert.ok(MENU.includes("close(false)"), "outside click closes without stealing focus");
  assert.ok(MENU.includes('document.removeEventListener("click", onDocClick)'), "listener removed in onCleanup");
  assert.ok(MENU.includes("onCleanup"), "cleanup wired");
});

test("IdentityMenu: keyboard support (menu roles, arrows, Escape refocus)", () => {
  assert.ok(MENU.includes('aria-haspopup="menu"'), "toggle advertises the menu");
  assert.ok(MENU.includes("aria-expanded"), "toggle exposes expanded state");
  assert.ok(MENU.includes('role="menu"') && MENU.includes('role="menuitem"'), "menu roles kept");
  assert.ok(MENU.includes('e.key === "Escape"'), "Escape handler kept");
  assert.ok(MENU.includes("close(true)"), "Escape refocuses the toggle");
  assert.ok(MENU.includes("ArrowDown") && MENU.includes("ArrowUp"), "arrow-key navigation kept");
});

test("IdentityMenu: avatar-or-username + logout as session-clearing anchor", () => {
  assert.ok(MENU.includes("props.username"), "username rendered when no avatar exists");
  assert.ok(MENU.includes("props.avatarUrl"), "optional avatar URL prop (#376: me().avatar_url)");
  assert.ok(MENU.includes("<A") && MENU.includes('href={item.href}'), "router links for in-app entries");
  assert.ok(MENU.includes("<a") && MENU.includes('href={item.href}'), "plain anchor for logout (cookie clear + redirect)");
});

test("IdentityMenu: long usernames cannot widen the 390px header", () => {
  assert.ok(MENU.includes("max-w-"), "trigger width capped");
  // The no-avatar fallback is a single-initial chip, so a long username
  // can never stretch the trigger row.
  assert.ok(MENU.includes("slice(0, 1)"), "fallback renders one initial, not the full name");
});

// --- Forgejo #390: bare-circle trigger + profile avatar ---------------------

test("IdentityMenu #390: trigger is a bare circle, not a button box", () => {
  const btn = MENU.slice(MENU.indexOf("<button"), MENU.indexOf("onClick={toggle}"));
  assert.ok(!btn.includes('"btn ') && !btn.includes('"btn"'), "no btn container class on the trigger");
  assert.ok(btn.includes("rounded-full"), "trigger itself is round");
});

test("IdentityMenu #390: avatar is larger than 20px with a light ring + hover emphasis", () => {
  assert.ok(!MENU.includes("h-5 w-5"), "the 20px inline image is gone");
  assert.ok(MENU.includes("h-8 w-8"), "avatar circle at h-8 w-8");
  assert.ok(MENU.includes("ring-1 ring-zinc-300"), "light outer ring (light theme)");
  assert.ok(MENU.includes("dark:ring-zinc-600"), "ring treatment in dark theme");
  assert.ok(MENU.includes("group-hover:ring-2"), "hover ring emphasis");
});

test("IdentityMenu #390: dropdown affordance survives the box removal", () => {
  assert.ok(MENU.includes("▾"), "caret beside the circle");
  assert.ok(MENU.includes('aria-haspopup="menu"'), "trigger advertises the menu");
  assert.ok(MENU.includes("aria-expanded"), "trigger exposes expanded state");
});

test("IdentityMenu #390: initials fallback keeps the same circle shape", () => {
  const fb = MENU.slice(MENU.indexOf("fallback={"), MENU.indexOf("</Show>", MENU.indexOf("fallback={")));
  assert.ok(fb.includes("rounded-full"), "fallback is a circle");
  assert.ok(fb.includes("h-8 w-8"), "fallback matches the avatar size");
  assert.ok(fb.includes("ring-1"), "fallback shares the ring treatment");
});

test("Repos #390: profile page shows a large avatar above the New-repository row", () => {
  const REPOS = srcOf("../../src/pages/Repos.jsx");
  assert.ok(REPOS.includes("h-24 w-24"), "avatar at 96px (>= 72px)");
  assert.ok(REPOS.includes("justify-end"), "avatar floated right");
  const large = REPOS.indexOf("h-24 w-24");
  const headerRow = REPOS.indexOf("justify-between");
  const newBtn = REPOS.indexOf("New repository");
  assert.ok(large < headerRow && headerRow < newBtn, "large avatar renders above the title + New-repository row");
  assert.ok(REPOS.includes("ring-zinc-300"), "ring treatment consistent with the navbar");
  assert.ok(!REPOS.includes("width={36}"), "the small inline user avatar is gone (org avatar keeps its own size)");
});
