import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient, selectLane, laneUrl, resolveBase, DEFAULT_BASE } from "../../sdk/src/core.js";
import { deps as authDeps, openAuthPopup, authenticateUrl } from "../../sdk/src/auth.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

function client(handler, opts = {}) {
  const { fetch, calls } = fakeFetch(handler);
  return { client: new ReposClient({ base: BASE, fetch, ...opts }), calls };
}

/** Run fn with a page-origin shim; restore afterward. */
function withLocation(origin, fn) {
  const had = "location" in globalThis;
  const prev = globalThis.location;
  globalThis.location = { origin };
  try {
    return fn();
  } finally {
    if (had) globalThis.location = prev;
    else delete globalThis.location;
  }
}

test("lane selection table (§1.2, order fixed)", () => {
  // 1. explicit token → bearer, regardless of origin
  assert.equal(selectLane({ token: "t" }, "http://elsewhere.test"), "bearer");
  assert.equal(selectLane({ token: "t" }, BASE), "bearer");
  // 2. page origin == API base → same-origin
  withLocation(BASE, () => assert.equal(selectLane({}, BASE), "same-origin"));
  withLocation(BASE, () => assert.equal(selectLane({ token: "" }, BASE), "same-origin"));
  // 3. else → browser
  withLocation("http://elsewhere.test", () => assert.equal(selectLane({}, BASE), "browser"));
  // off-DOM (no location) → browser
  assert.equal(selectLane({}, BASE), "browser");
});

test("lane request properties: bearer omits credentials, same-origin keeps session, browser includes + manual", async () => {
  const { client: bearer, calls: b1 } = client(() => jsonResponse({}));
  bearer.configure({ token: "tok" });
  await bearer.me();
  assert.equal(b1[0].init.headers.Authorization, "Bearer tok");
  assert.equal(b1[0].init.credentials, "omit");

  const { client: same, calls: b2 } = client(() => jsonResponse({}));
  withLocation(BASE, () => same.me());
  assert.equal(b2[0].init.credentials, "same-origin");
  assert.equal(b2[0].init.headers.Authorization, undefined);

  const { client: browser, calls: b3 } = client(() => jsonResponse({}));
  withLocation("http://elsewhere.test", () => browser.repo("o/r").refs());
  assert.equal(b3[0].init.credentials, "include");
  assert.equal(b3[0].init.redirect, "manual");
  assert.equal(b3[0].url, "http://api.test/o/r/api-browser/refs");
});

test("browser lane rewrites repo paths to /api-browser, keeps /api/v1 paths", () => {
  assert.equal(laneUrl(BASE, "browser", "/o/r/api/refs"), `${BASE}/o/r/api-browser/refs`);
  assert.equal(laneUrl(BASE, "browser", "/o/r/api"), `${BASE}/o/r/api-browser`);
  assert.equal(laneUrl(BASE, "browser", "/api/v1/me"), `${BASE}/api/v1/me`);
  assert.equal(laneUrl(BASE, "bearer", "/o/r/api/refs"), `${BASE}/o/r/api/refs`);
  assert.equal(laneUrl(BASE, "same-origin", "/o/r/api/refs"), `${BASE}/o/r/api/refs`);
});

test("401 in the browser lane → popup auth → retry exactly once", async () => {
  let n = 0;
  const { client: c, calls } = client(() => {
    n++;
    if (n === 1) return new Response("denied", { status: 401 });
    return jsonResponse({ principal: "jane" });
  });
  let authCalls = 0;
  c.authenticate = () => {
    authCalls++;
    return Promise.resolve();
  };
  assert.deepEqual(await c.repo("o/r").tree("main", ""), { principal: "jane" });
  assert.equal(n, 2);
  assert.equal(calls.length, 2, "retried exactly once, not twice");
});

test("401 → ReposError when the retry is also 401 (single retry, no loop)", async () => {
  const { client: c } = client(() => new Response("no", { status: 401 }));
  c.authenticate = () => Promise.resolve();
  await assert.rejects(c.me(), (err) => err.status === 401);
});

test("second 401 while a popup is open reuses the SAME in-flight promise (single-flight)", async () => {
  let opens = 0;
  let release;
  const gate = new Promise((r) => (release = r));
  const { client: c } = client(() => new Response("no", { status: 401 }));
  c.authenticate = () => {
    opens++;
    return gate;
  };
  const p1 = c.me().catch((e) => ({ err: e }));
  const p2 = c.repo("a/b").refs().catch((e) => ({ err: e }));
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(opens, 1, "only ONE popup auth promise for two concurrent 401s");
  release();
  await Promise.all([p1, p2]);
  assert.equal(opens, 1);
});

test("a REJECTED popup auth clears the slot so a later call retries", async () => {
  let opens = 0;
  const { client: c } = client(() => new Response("no", { status: 401 }));
  c.authenticate = () => {
    opens++;
    return opens === 1 ? Promise.reject(new Error("popup blocked")) : Promise.resolve();
  };
  await assert.rejects(c.me(), (err) => err.status === 401);
  await assert.rejects(c.me(), () => true);
  assert.equal(opens, 2, "the failed auth slot was cleared; second call opened a new popup");
});

// ── auth.js popup flow (with injected DOM fakes via auth.deps) ──────────────

function injectAuthDeps({ events, timeouts = [], origin = "http://x.test" }) {
  const win = { close() {} };
  authDeps.open = (url, name, features) => {
    events.opened = { url, name, features };
    return win;
  };
  authDeps.addEventListener = (t, fn) => (events[t] = fn);
  authDeps.removeEventListener = (t) => delete events[t];
  authDeps.setTimeout = (fn, ms) => {
    timeouts.push(fn);
    return timeouts.length;
  };
  authDeps.clearTimeout = () => {};
  authDeps.origin = () => origin;
}

test("popup timeout rejects with ReposError(504)", async () => {
  const events = {};
  const timeouts = [];
  injectAuthDeps({ events, timeouts });
  const p = openAuthPopup("http://x.test");
  timeouts[0](); // fire the 120 s timeout immediately
  await assert.rejects(p, (err) => err.status === 504 && err.message === "popup auth timed out");
});

test("postMessage: foreign origin / wrong type ignored; our origin + repos:authenticated resolves", async () => {
  const events = {};
  injectAuthDeps({ events });
  const p = openAuthPopup("http://x.test");
  events.message({ origin: "http://evil.test", data: { type: "repos:authenticated" } });
  events.message({ origin: "http://x.test", data: { type: "other" } });
  events.message({ origin: "http://x.test", data: { type: "repos:authenticated" } });
  await p; // resolved by the third message
  assert.equal(events.message, undefined, "listener removed after settle");
});

test("sign-in popup opens <base>/api-browser/v1/authenticate with the expected window features", async () => {
  assert.equal(authenticateUrl("http://x.test"), "http://x.test/api-browser/v1/authenticate");
  const events = {};
  injectAuthDeps({ events });
  const p = openAuthPopup("http://x.test");
  events.message({ origin: "http://x.test", data: { type: "repos:authenticated" } });
  await p;
  assert.equal(events.opened.url, "http://x.test/api-browser/v1/authenticate");
  assert.equal(events.opened.name, "repos-auth");
  assert.match(events.opened.features, /width=520/);
});

// ── base URL resolution (§1.3) ──────────────────────────────────────────────

test("base URL resolution: option wins; off-DOM default is http://127.0.0.1:8080", () => {
  assert.equal(resolveBase({ base: "http://cfg.test/" }), "http://cfg.test");
  assert.equal(resolveBase({}), DEFAULT_BASE);
  assert.equal(new ReposClient().base, DEFAULT_BASE);
  // script[data-base] beats import.meta/page origin (browser-only path)
  const had = "document" in globalThis;
  const prev = globalThis.document;
  globalThis.document = {
    querySelector: (sel) => (sel === "script[data-base]" ? { getAttribute: () => "http://attr.test/" } : null),
  };
  try {
    assert.equal(resolveBase({}), "http://attr.test");
  } finally {
    if (had) globalThis.document = prev;
    else delete globalThis.document;
  }
});

test("configure() re-evaluates base and token (lane re-derived)", async () => {
  const { client: c, calls } = client(() => jsonResponse({}));
  assert.equal(c.lane, "browser");
  c.configure({ base: "http://api.test", token: "t" });
  assert.equal(c.lane, "bearer");
  await c.me();
  assert.equal(calls[0].url, "http://api.test/api/v1/me");
  assert.equal(calls[0].init.headers.Authorization, "Bearer t");
});
