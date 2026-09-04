import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { sha256Hex } from "../../sdk/src/releases.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

/** Releases/social surface (docs/features/07 §7): exact endpoints + methods. */
const SURFACE = [
  { name: "releases.list", run: (c) => c.repo("o/r").releases.list({ n: 10 }), method: "GET", path: "/o/r/api/releases?n=10" },
  { name: "releases.latest", run: (c) => c.repo("o/r").releases.latest(), method: "GET", path: "/o/r/api/releases/latest" },
  { name: "releases.latest prereleases", run: (c) => c.repo("o/r").releases.latest({ include_prereleases: 1 }), method: "GET", path: "/o/r/api/releases/latest?include_prereleases=1" },
  { name: "releases.get", run: (c) => c.repo("o/r").releases.get("v1.0.0"), method: "GET", path: "/o/r/api/releases/v1.0.0" },
  { name: "releases.get encoded", run: (c) => c.repo("o/r").releases.get("a/b"), method: "GET", path: "/o/r/api/releases/a%2Fb" },
  { name: "releases.put", run: (c) => c.repo("o/r").releases.put("v1", { name: "R" }), method: "PUT", path: "/o/r/api/releases/v1" },
  { name: "releases.remove", run: (c) => c.repo("o/r").releases.remove("v1"), method: "DELETE", path: "/o/r/api/releases/v1" },
  { name: "releases.autodraft", run: (c) => c.repo("o/r").releases.autodraft({ tag: "v2" }), method: "GET", path: "/o/r/api/releases/autodraft?tag=v2" },
  { name: "releases.deleteAsset", run: (c) => c.repo("o/r").releases.deleteAsset("v1", "tool"), method: "DELETE", path: "/o/r/api/releases/v1/assets/tool" },
  { name: "star.set", run: (c) => c.repo("o/r").star.set(), method: "PUT", path: "/o/r/api/star" },
  { name: "star.remove", run: (c) => c.repo("o/r").star.remove(), method: "DELETE", path: "/o/r/api/star" },
  { name: "social.get", run: (c) => c.repo("o/r").social.get(), method: "GET", path: "/o/r/api/social" },
  { name: "social.myStarred", run: (c) => c.social.myStarred({ n: 5 }), method: "GET", path: "/api/v1/me/starred?n=5" },
  { name: "social.userStarred", run: (c) => c.social.userStarred("jane"), method: "GET", path: "/api/v1/users/jane/starred" },
];

test("releases/social surface: every member hits its exact endpoint and method", async () => {
  for (const row of SURFACE) {
    const { fetch, calls } = fakeFetch((ctx) =>
      ctx.init.method === row.method ? jsonResponse({ ok: true }) : new Response("bad method", { status: 405 })
    );
    const client = new ReposClient({ base: BASE, fetch, token: "t" }); // bearer lane → paths unchanged
    await row.run(client);
    assert.equal(calls.length, 1, row.name);
    assert.equal(calls[0].url, `${BASE}${row.path}`, `${row.name} → ${row.method} ${row.path}`);
    assert.equal(calls[0].init.method, row.method, row.name);
  }
});

test("releases.put sends a JSON body", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ tag: "v1" }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("o/r").releases.put("v1", { name: "R", draft: true });
  const sent = JSON.parse(calls[0].init.body);
  assert.equal(sent.name, "R");
  assert.equal(sent.draft, true);
  assert.equal(calls[0].init.headers["Content-Type"], "application/json");
});

test("releases.uploadAsset hashes and posts bytes with the sha header", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ name: "tool" }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  const data = new TextEncoder().encode("payload");
  await client.repo("o/r").releases.uploadAsset("v1", "tool", data);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, `${BASE}/o/r/api/releases/v1/assets/tool`);
  assert.equal(calls[0].init.method, "POST");
  assert.equal(calls[0].init.headers["X-Walgit-Asset-Sha256"], await sha256Hex(data));
  assert.ok(calls[0].init.body instanceof Uint8Array);
});

test("releases.uploadAsset honors a precomputed sha", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ name: "tool" }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("o/r").releases.uploadAsset("v1", "tool", "x", { sha256: "0".repeat(64) });
  assert.equal(calls[0].init.headers["X-Walgit-Asset-Sha256"], "0".repeat(64));
});

test("releaseAssetUrl builds the static byte URL", () => {
  const client = new ReposClient({ base: BASE, token: "t" });
  assert.equal(
    client.repo("o/r").releaseAssetUrl("a/b", "tool"),
    `${BASE}/o/r/releases/a%2Fb/assets/tool`
  );
});

test("sha256Hex matches the well-known empty digest", async () => {
  assert.equal(await sha256Hex(new Uint8Array(0)), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
});
