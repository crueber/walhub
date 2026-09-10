// web/test/unit/nav-api-right.test.js — navbar API placement (issue #238):
// the API link lives in the far-right ml-auto utility cluster (left of the
// tray + theme toggle), NOT in the primary site-nav; remaining nav order is
// explore → import → keys → setup; route/href unchanged (/api). No DOM: JSX
// is pinned as source text, mirroring landing.test.js / how-it-works.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const APP = srcOf("../../src/App.jsx");
const CSS = srcOf("../../src/ui.css");

function block(src, start, end) {
  const s = src.indexOf(start);
  assert.ok(s !== -1, `expected block start ${start}`);
  const e = src.indexOf(end, s);
  assert.ok(e !== -1, `expected block end ${end}`);
  return src.slice(s, e);
}

test("site-nav holds explore → import → keys → setup, no API", () => {
  const nav = block(APP, '<nav aria-label="Site" class="site-nav', "</nav>");
  for (const href of ["/explore", "/import", "/keys", "/setup"]) {
    assert.ok(nav.includes(`href="${href}"`), `site-nav must link ${href}`);
  }
  assert.ok(!nav.includes('href="/api"'), "API must not be in the site-nav");
  const order = ["/explore", "/import", "/keys", "/setup"].map((h) => nav.indexOf(`href="${h}"`));
  assert.deepEqual([...order].sort((a, b) => a - b), order, "nav order must be explore → import → keys → setup");
});

test("API renders in the ml-auto cluster, left of tray + toggle", () => {
  const right = block(APP, '<div class="ml-auto', "</div>\n        </div>");
  assert.ok(right.includes('href="/api"'), "ml-auto cluster must link /api");
  assert.ok(right.includes(">API<"), "ml-auto /api link keeps the API label");
  const api = right.indexOf('href="/api"');
  assert.ok(api < right.indexOf("<NotificationTray"), "API must sit left of the NotificationTray");
  assert.ok(api < right.indexOf("Toggle dark mode"), "API must sit left of the theme toggle");
});

test("API link keeps nav styling + route (single /api entry)", () => {
  const hits = APP.match(/href="\/api"/g) || [];
  assert.equal(hits.length, 1, "exactly one /api header entry (Apidocs.jsx route untouched)");
  assert.ok(APP.includes('href="/api" class="nav-link"'), "API link carries the nav-link class");
  assert.ok(
    CSS.includes(".nav-link") && /\.site-nav a,\s*\.nav-link\s*\{/.test(CSS),
    "ui.css gives .nav-link the identical .site-nav a treatment"
  );
});
