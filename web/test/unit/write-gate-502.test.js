import { test } from "node:test";
import assert from "node:assert/strict";

import {
  isAnonymousViewer,
  sanitizeNextClient,
  anonWriteTarget,
  write401Target,
  interstitialLoginHref,
} from "../../src/lib/writeGate.js";

const OIDC = { auth: { mode: "oidc", browser_login: true, login_url: "/_auth/login" } };
const TOKEN = { auth: { mode: "token", browser_login: false, login_url: "" } };
const NONE = { auth: { mode: "none" } };
const ANON = { principal: "anonymous", anonymous: true, write: false, admin: false };
const SIGNED = { principal: "jane", anonymous: false, write: true, admin: false };

test("isAnonymousViewer: mode × identity", async () => {
  assert.equal(isAnonymousViewer(null, OIDC), true, "signed out under oidc gates");
  assert.equal(isAnonymousViewer(ANON, OIDC), true, "explicit anonymous gates");
  assert.equal(isAnonymousViewer(SIGNED, OIDC), false, "signed in writes");
  assert.equal(isAnonymousViewer(null, TOKEN), true, "signed out under token gates");
  assert.equal(isAnonymousViewer(SIGNED, TOKEN), false);
  assert.equal(isAnonymousViewer(null, NONE), false, "none mode never gates");
  assert.equal(isAnonymousViewer(ANON, NONE), false, "none principal writes");
  assert.equal(isAnonymousViewer(null, null), false, "unloaded discovery never gates");
  assert.equal(isAnonymousViewer(null, {}), false);
});

test("sanitizeNextClient mirrors the server rule", async () => {
  assert.equal(sanitizeNextClient("/o/r/issues"), "/o/r/issues");
  assert.equal(sanitizeNextClient("/o/r/fork?x=1"), "/o/r/fork?x=1");
  assert.equal(sanitizeNextClient("/"), "/");
  assert.equal(sanitizeNextClient("//evil.com/x"), "/", "no protocol-relative");
  assert.equal(sanitizeNextClient("https://evil.com/"), "/", "no scheme");
  assert.equal(sanitizeNextClient("o/r"), "/", "no relative");
  assert.equal(sanitizeNextClient(""), "/", "no empty");
  assert.equal(sanitizeNextClient(null), "/");
  assert.equal(sanitizeNextClient(undefined), "/");
});

test("anonWriteTarget: href shape for anonymous, null for writers", async () => {
  assert.equal(
    anonWriteTarget({ me: null, discovery: OIDC }, "/o/r/issues/3", "Comment on #3"),
    "/login-required?next=%2Fo%2Fr%2Fissues%2F3&action=Comment%20on%20%233",
  );
  assert.equal(
    anonWriteTarget({ me: ANON, discovery: OIDC }, "//evil.com", "Star"),
    "/login-required?next=%2F&action=Star",
    "hostile next sanitizes to /",
  );
  assert.equal(anonWriteTarget({ me: ANON, discovery: OIDC }, "/o/r"), "/login-required?next=%2Fo%2Fr");
  assert.equal(anonWriteTarget({ me: SIGNED, discovery: OIDC }, "/o/r", "Star"), null);
  assert.equal(anonWriteTarget({ me: null, discovery: NONE }, "/o/r", "Star"), null);
});

test("write401Target: only anonymous 401s route", async () => {
  const anon = { me: null, discovery: OIDC };
  assert.equal(
    write401Target({ status: 401 }, anon, "/o/r", "Star"),
    "/login-required?next=%2Fo%2Fr&action=Star",
  );
  assert.equal(write401Target({ status: 401, unauthorized: true }, anon, "/o/r"), "/login-required?next=%2Fo%2Fr");
  assert.equal(write401Target({ status: 403 }, anon, "/o/r", "Star"), null, "403 keeps the tray path");
  assert.equal(write401Target({ status: 500 }, anon, "/o/r", "Star"), null);
  assert.equal(write401Target({ status: 401 }, { me: SIGNED, discovery: OIDC }, "/o/r", "Star"), null, "signed-in 401s keep the popup path");
  assert.equal(write401Target(null, anon, "/o/r"), null);
});

test("interstitialLoginHref: discovery login_url + sanitized next", async () => {
  assert.equal(interstitialLoginHref(OIDC, "/o/r/fork"), "/_auth/login?next=%2Fo%2Fr%2Ffork");
  assert.equal(interstitialLoginHref(OIDC, "//evil.com/x"), "/_auth/login?next=%2F");
  assert.equal(interstitialLoginHref(TOKEN, "/o/r"), "", "no browser flow → disabled");
  assert.equal(interstitialLoginHref(null, "/o/r"), "");
});
