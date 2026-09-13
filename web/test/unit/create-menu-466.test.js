// web/test/unit/create-menu-466.test.js — Forgejo #466: one navbar create
// (+) button left of the avatar with a New repository / Import repository /
// New organization dropdown; the redundant CTAs (navbar import link,
// Repositories toolbar CTA, Owners header buttons) are gone.
//
// (1) The navModel gate + createItems hrefs (pure lib/identity.js — no DOM).
// (2) App.jsx wiring (CreateMenu left of IdentityMenu on the showCreate
// gate; import out of the site-nav; /import route untouched). (3) The
// CreateMenu.jsx popover contract (cloned from IdentityMenu: outside-click +
// onCleanup, Esc + focus return, arrows, Tab-out, menu roles, no
// toggle-fight, viewport-bound panel, compact plus trigger). (4) The three
// removals. No DOM: model + JSX pinned as source text, mirroring
// identity-nav.test.js / repos-toolbar-413.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { navModel, createItems } from "../../src/lib/identity.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const APP = srcOf("../../src/App.jsx");
const MENU = srcOf("../../src/components/CreateMenu.jsx");
const ICONS = srcOf("../../src/lib/icons.jsx");
const REPOS = srcOf("../../src/pages/Repos.jsx");
const OWNERS = srcOf("../../src/pages/Owners.jsx");
const ROUTES = srcOf("../../src/index.jsx");

const oidcLogin = {
  auth: { mode: "oidc", browser_login: true, login_url: "/_auth/login" },
};
const noneDisc = { auth: { mode: "none", browser_login: false, login_url: "" } };
const alice = { principal: "alice", write: true, anonymous: false, admin: false };
const anon = { principal: "anonymous", write: false, anonymous: true, admin: false };

// --- model: showCreate shares the identity gate --------------------------------

test("signed-in → create button on, three items in order", () => {
  const nav = navModel({ me: alice, discovery: oidcLogin }, "/");
  assert.equal(nav.showCreate, true);
  assert.equal(nav.showCreate, nav.showIdentity, "create shares the identity gate exactly");
  assert.deepEqual(
    nav.createItems.map((i) => i.kind),
    ["new-repo", "import", "new-org"],
  );
});

test("createItems hrefs: /new, /import, /orgs/new", () => {
  const items = createItems();
  assert.equal(items.find((i) => i.kind === "new-repo").href, "/new");
  assert.equal(items.find((i) => i.kind === "new-repo").label, "New repository");
  assert.equal(items.find((i) => i.kind === "import").href, "/import");
  assert.equal(items.find((i) => i.kind === "import").label, "Import repository");
  assert.equal(items.find((i) => i.kind === "new-org").href, "/orgs/new");
  assert.equal(items.find((i) => i.kind === "new-org").label, "New organization");
});

test("signed-out → no create button, empty items (navbar unchanged)", () => {
  for (const me of [null, anon]) {
    const nav = navModel({ me, discovery: oidcLogin }, "/");
    assert.equal(nav.showCreate, false, `me=${JSON.stringify(me)} hides creation`);
    assert.deepEqual(nav.createItems, []);
  }
});

test("none mode + discovery failure → no create button (fail closed)", () => {
  assert.equal(navModel({ me: alice, discovery: noneDisc }, "/").showCreate, false);
  assert.equal(navModel({ me: null, discovery: noneDisc }, "/").showCreate, false);
  assert.equal(navModel({ me: null, discovery: null }, "/").showCreate, false);
});

// --- App.jsx wiring --------------------------------------------------------------

test("App renders the create button immediately left of the identity menu", () => {
  assert.ok(APP.includes("<CreateMenu"), "create menu mounted");
  assert.ok(APP.includes("nav().showCreate"), "create gated on the model (not inline branching)");
  assert.ok(APP.includes("items={nav().createItems}"), "dropdown entries ride the model hrefs");
  const right = APP.slice(APP.indexOf('<div class="ml-auto'));
  assert.ok(right.includes("nav().showCreate"), "button lives in the shrink-0 right cluster");
  assert.ok(right.indexOf("<CreateMenu") < right.indexOf("<IdentityMenu"), "create sits left of the identity menu");
  assert.ok(right.indexOf("Toggle dark mode") < right.indexOf("<CreateMenu"), "identity stays the far-right element (#390)");
});

test("import link out of the site-nav; /import route untouched", () => {
  const nav = APP.slice(APP.indexOf('<nav aria-label="Site"'), APP.indexOf("</nav>"));
  assert.ok(!nav.includes('href="/import"'), "no import entry in the primary nav");
  assert.ok(ROUTES.includes('<Route path="/import"'), "/import route still registered");
  assert.ok(ROUTES.includes("component={Import}"), "/import still serves the Import page");
});

// --- CreateMenu.jsx popover contract (cloned from IdentityMenu) -------------------

test("CreateMenu: outside-click dismisses without fighting the trigger", () => {
  assert.ok(MENU.includes('document.addEventListener("click", onDocClick)'), "document click listener");
  assert.ok(MENU.includes("!root.contains(e.target)"), "clicks inside the root ignored");
  assert.ok(MENU.includes("close(false)"), "outside click closes without stealing focus");
  assert.ok(MENU.includes('document.removeEventListener("click", onDocClick)'), "listener removed in onCleanup");
  assert.ok(MENU.includes("onCleanup"), "cleanup wired");
  // No toggle-fight: the trigger lives inside the root div.
  assert.ok(MENU.indexOf("ref={root}") < MENU.indexOf("onClick={toggle}"), "trigger inside the root");
});

test("CreateMenu: keyboard support (menu roles, arrows, Escape refocus, Tab-out)", () => {
  assert.ok(MENU.includes('aria-haspopup="menu"'), "trigger advertises the menu");
  assert.ok(MENU.includes("aria-expanded"), "trigger exposes expanded state");
  assert.ok(MENU.includes('role="menu"') && MENU.includes('role="menuitem"'), "menu roles kept");
  assert.ok(MENU.includes('e.key === "Escape"'), "Escape handler kept");
  assert.ok(MENU.includes("close(true)"), "Escape refocuses the trigger");
  assert.ok(MENU.includes("ArrowDown") && MENU.includes("ArrowUp"), "arrow-key navigation kept");
  assert.ok(MENU.includes('e.key === "Tab"'), "Tab-out dismisses");
});

test("CreateMenu: compact plus trigger with accessible name", () => {
  const btn = MENU.slice(MENU.indexOf("<button"), MENU.indexOf("</button>"));
  assert.ok(btn.includes('"btn px-2 py-1"'), "trigger keeps the canonical cluster metrics (compact ~32px)");
  assert.ok(btn.includes('aria-label="Create new"'), "trigger has an accessible name");
  assert.ok(btn.includes('title="Create new"'), "trigger has a tooltip");
  assert.ok(btn.includes('<Icon name="plus"'), "glyph comes through the shared Icon mechanism");
  assert.ok(!MENU.includes("<svg"), "no hand-rolled svg at the call site");
});

test("CreateMenu: three router links that navigate and close", () => {
  assert.ok(MENU.includes("<A"), "entries are router links");
  assert.ok(MENU.includes("href={item.href}"), "hrefs ride the model (no hardcoded targets)");
  assert.ok(MENU.includes("onClick={() => setOpen(false)}"), "selecting an item closes the menu");
  assert.ok(!MENU.includes("<a"), "no plain anchors — every entry is in-app");
  assert.ok(MENU.includes("props.items"), "entries come from props (App passes nav().createItems)");
});

test("CreateMenu: panel bounded on phone widths", () => {
  assert.ok(MENU.includes("max-w-[calc(100vw-1rem)]"), "right-anchored panel carries the mobile-popover bound");
  assert.ok(MENU.includes("absolute right-0"), "panel stays right-anchored under the trigger");
});

test("plus icon: registered through the shared mechanism, currentColor only", () => {
  assert.ok(ICONS.includes("plus:"), "plus registered in the icon map");
  assert.ok(ICONS.includes('viewBox: "0 0 16 16"'), "plus on the 16-unit grid");
  const entry = ICONS.slice(ICONS.indexOf("plus:"), ICONS.indexOf("plus:") + 300);
  assert.ok(entry.includes("currentColor"), "plus paints via currentColor");
  assert.ok(!entry.match(/#[0-9a-fA-F]{3,8}\b/), "no hex literal in the plus body");
});

// --- removals ---------------------------------------------------------------------

test("Repos.jsx toolbar: heading remains, CTA gone", () => {
  assert.ok(REPOS.includes("repos-toolbar"), "toolbar row kept");
  assert.ok(REPOS.includes("Repositories</h3>"), "heading text unchanged");
  assert.ok(!REPOS.includes("New repository"), "no CTA anywhere on the page");
  assert.ok(!REPOS.includes("/new?owner="), "no create link left behind");
  assert.ok(REPOS.includes("import into {owner()}"), "import footer untouched");
});

test("Owners.jsx header: heading alone, all three CTAs gone", () => {
  assert.ok(OWNERS.includes('<h2 class="mb-4 text-xl font-semibold">Owners</h2>'), "heading stands alone");
  assert.ok(!OWNERS.includes("New repository"), "New repository CTA gone");
  assert.ok(!OWNERS.includes("New organization"), "New organization CTA gone");
  assert.ok(!OWNERS.includes("Import repository"), "Import repository CTA gone");
  assert.ok(!OWNERS.includes('href="/new"'), "no /new link left behind");
  assert.ok(!OWNERS.includes('href="/orgs/new"'), "no /orgs/new link left behind");
  assert.ok(OWNERS.includes("What is walhub?"), "intro card untouched");
});

// --- law 6: no new requests ----------------------------------------------------------

test("no new network requests: menu + model ride existing payloads", () => {
  assert.ok(!MENU.includes("fetch("), "menu fetches nothing");
  assert.ok(!MENU.includes("repos."), "menu touches no SDK surface");
  assert.ok(!MENU.includes("useData"), "menu reads no cache key of its own");
  const model = srcOf("../../src/lib/identity.js");
  assert.ok(!model.includes("fetch("), "model fetches nothing (me + discovery ride the shell keys)");
});
