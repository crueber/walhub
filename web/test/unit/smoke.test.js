// web/test/unit/smoke.test.js — fetch-based server smoke (D-WEB-4): the BUILT
// SPA must actually be served — shell, hashed assets with module MIME, SDK
// bundle. Skips when WALHUB_TEST_WEB_BASE_URL is unreachable, so `make test`
// stays green on a cold machine; CI always has a server up.

import { test } from "node:test";
import assert from "node:assert/strict";

const BASE = process.env.WALHUB_TEST_WEB_BASE_URL ?? "http://127.0.0.1:8080";

let up = false;
try {
  up = (await fetch(`${BASE}/healthz`, { signal: AbortSignal.timeout(1500) })).ok;
} catch { /* server absent → skip */ }

test("built SPA shell is served at / and /setup", { skip: !up && "server not running" }, async () => {
  for (const path of ["/", "/setup"]) {
    const res = await fetch(`${BASE}${path}`);
    assert.equal(res.status, 200, `${path} status`);
    const html = await res.text();
    assert.match(html, /id="root"/, `${path} must mount the Solid root`);
    assert.match(html, /\/_ui\/assets\/.+\.js/, `${path} must reference the hashed bundle`);
    assert.match(html, /class="dark"/, "dark mode ships as the default theme");
  }
});

test("hashed assets are served as JavaScript with immutable caching", { skip: !up && "server not running" }, async () => {
  const html = await (await fetch(`${BASE}/`)).text();
  const src = html.match(/src="(\/_ui\/assets\/[^"]+\.js)"/)?.[1];
  assert.ok(src, "shell references a hashed script");
  const res = await fetch(`${BASE}${src}`);
  assert.equal(res.status, 200);
  assert.match(res.headers.get("content-type") ?? "", /text\/javascript/, "module MIME is load-bearing");
  assert.match(res.headers.get("cache-control") ?? "", /immutable/, "hashed assets are immutable");
});

test("repos.js stays the dependency-free SDK bundle", { skip: !up && "server not running" }, async () => {
  const res = await fetch(`${BASE}/repos.js`);
  assert.equal(res.status, 200);
  assert.match(res.headers.get("content-type") ?? "", /text\/javascript/);
  const body = await res.text();
  assert.match(body, /class ReposError|ReposClient/, "bundle contains the SDK client");
  assert.match(body, /export/, "bundle is an ES module");
});
