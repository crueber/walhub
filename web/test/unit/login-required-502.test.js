import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const INDEX = srcOf("../../src/index.jsx");
const PAGE = srcOf("../../src/pages/LoginRequired.jsx");
const GATE = srcOf("../../src/lib/writeGate.js");

test("login-required route registers before /:owner (static-before-dynamic)", async () => {
  const routeAt = INDEX.indexOf('path="/login-required"');
  const ownerAt = INDEX.indexOf('path="/:owner"');
  assert.ok(routeAt !== -1, "/login-required route exists");
  assert.ok(ownerAt !== -1, "/:owner route exists");
  assert.ok(routeAt < ownerAt, "static route precedes the dynamic owner route");
  assert.ok(INDEX.includes("login-required"), "reservation note names the route");
  assert.ok(INDEX.includes("LoginRequired"), "page component wired");
});

test("interstitial explains, logs in via the OIDC entry, and returns", async () => {
  assert.ok(PAGE.includes("sanitizeNextClient"), "next is sanitized (no open redirect)");
  assert.ok(PAGE.includes("interstitialLoginHref"), "Log-in rides the discovery login_url");
  assert.ok(PAGE.includes("/_auth/login") || GATE.includes("/_auth/login"), "OIDC entry documented");
  assert.ok(PAGE.includes("action") && PAGE.includes("this action"), "action names the attempt, with a default");
  assert.ok(PAGE.includes("history.back()"), "back uses history with a fallback");
  assert.ok(PAGE.includes(">Log in<") || PAGE.includes("Log in"), "primary Log-in affordance");
  assert.ok(PAGE.includes("Back") && PAGE.includes("Cancel"), "back + cancel affordances");
  assert.ok(PAGE.includes("max-w-xl") && PAGE.includes("px-4"), "narrow-viewport arithmetic: centered capped card with side padding");
  assert.ok(PAGE.includes("dark:"), "dark theme variants present");
});

test("every inventoried affordance follows the one writeGate pattern", async () => {
  const pages = {
    "Repo.jsx (star/watch pre-flight + fork label)": srcOf("../../src/pages/Repo.jsx"),
    "Issues.jsx (New issue)": srcOf("../../src/pages/Issues.jsx"),
    "Pulls.jsx (New pull)": srcOf("../../src/pages/Pulls.jsx"),
    "Issue.jsx (composer + reactions)": srcOf("../../src/pages/Issue.jsx"),
    "Pull.jsx (composer)": srcOf("../../src/pages/Pull.jsx"),
    "IssueNew.jsx (create)": srcOf("../../src/pages/IssueNew.jsx"),
    "PullNew.jsx (open)": srcOf("../../src/pages/PullNew.jsx"),
    "Fork.jsx (fork)": srcOf("../../src/pages/Fork.jsx"),
    "New.jsx (create repo)": srcOf("../../src/pages/New.jsx"),
    "OrgNew.jsx (create org)": srcOf("../../src/pages/OrgNew.jsx"),
    "Import.jsx (import/mirror)": srcOf("../../src/pages/Import.jsx"),
    "ReleaseNew.jsx (create release)": srcOf("../../src/pages/ReleaseNew.jsx"),
  };
  for (const [name, src] of Object.entries(pages)) {
    assert.ok(
      src.includes("anonWriteTarget") || src.includes("writeGateHref"),
      `${name} pre-flights through anonWriteTarget`,
    );
  }
  // List pages (Issues/Pulls) only navigate — the write fires on the
  // composer page they link to, which carries the opt-out below. Every
  // page that fires a write opts out of popup auth or catches write 401s.
  const writers = { ...pages };
  delete writers["Issues.jsx (New issue)"];
  delete writers["Pulls.jsx (New pull)"];
  for (const [name, src] of Object.entries(writers)) {
    assert.ok(
      src.includes("noPopupAuth") || src.includes("write401Target"),
      `${name} opts out of popup auth or catches write 401s`,
    );
  }
  // Shared discovery cache key everywhere (zero new requests — law 6);
  // me rides the shared key too, except the three standalone form pages
  // (New/OrgNew/Import) whose pre-existing local me() fetch predates the
  // shell cache — the gate reuses it instead of adding a fetch.
  for (const [name, src] of Object.entries(pages)) {
    assert.ok(src.includes('useData("discovery"'), `${name} reads discovery from the shared cache key`);
    assert.ok(
      src.includes('useData("me"') || src.includes("repos") && src.includes(".me()"),
      `${name} has a me source`,
    );
  }
});
