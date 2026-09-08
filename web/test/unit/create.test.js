import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { validateRepoName, isUiRouteCollision } from "../../sdk/src/create.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

function bearerClient(handler) {
  const { fetch, calls } = fakeFetch(handler);
  return { client: new ReposClient({ base: BASE, fetch, token: "t" }), calls };
}

test("repos.create POSTs the exact twin path with JSON payload", async () => {
  const { client, calls } = bearerClient(() => jsonResponse({ owner: "acme", name: "x", placeholder: true }));
  const res = await client.repos.create({ owner: "acme", name: "x" });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, `${BASE}/api/v1/repos`);
  assert.equal(calls[0].init.method, "POST");
  assert.deepEqual(JSON.parse(calls[0].init.body), { owner: "acme", name: "x" });
  assert.equal(res.placeholder, true);
});

test("repos.create rides the browser lane off-DOM", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({}));
  const c = new ReposClient({ base: BASE, fetch }); // no token, off-DOM → browser lane
  await c.repos.create({ owner: "acme", name: "x" });
  assert.equal(calls[0].url, `${BASE}/api-browser/v1/repos`);
});

test("repo.createPlaceholder PUTs the flag ride with object_format", async () => {
  const { client, calls } = bearerClient(() => jsonResponse({ placeholder: true }));
  await client.repo("o/r").createPlaceholder({ object_format: "sha256" });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, `${BASE}/o/r/api?placeholder=true&object_format=sha256`);
  assert.equal(calls[0].init.method, "PUT");
});

test("repo.create stays flag-less (frozen PUT semantic)", async () => {
  const { client, calls } = bearerClient(() => jsonResponse({}));
  await client.repo("o/r").create();
  assert.equal(calls[0].url, `${BASE}/o/r/api`);
});

test("validateRepoName: accepts good names, strips .git, rejects bad parts", () => {
  assert.deepEqual(validateRepoName("acme", "newthing"), { owner: "acme", name: "newthing" });
  assert.equal(validateRepoName("acme", "newthing.git").name, "newthing");
  assert.ok(validateRepoName("", "x").error);
  assert.ok(validateRepoName("acme", "").error);
  assert.ok(validateRepoName(".evil", "x").error);
  assert.ok(validateRepoName("acme", "..").error);
  assert.ok(validateRepoName("acme", ".hidden").error);
  assert.ok(validateRepoName("acme", "has space").error);
  assert.ok(validateRepoName("a".repeat(101), "x").error);
});

test("isUiRouteCollision: reserved names warn, real owners pass", () => {
  for (const n of ["import", "api", "keys", "setup", "explore", "how-it-works", "new", "notifications"]) {
    assert.equal(isUiRouteCollision(n), true, n);
    assert.equal(isUiRouteCollision(n.toUpperCase()), true, n);
  }
  assert.equal(isUiRouteCollision("acme"), false);
  assert.equal(isUiRouteCollision(""), false);
});

test("placeholder projection predicate: refs==0 && marker, never marker alone", () => {
  // The UI predicate (mirrors the server R1 B1 branch + the Tree guide):
  // affordances show only when the summary is empty AND carries the
  // placeholder projection. A stale marker on a real repo carries no
  // projection (server drops it), so this is false.
  const showPlaceholder = (summary) =>
    !!summary && summary.health === "empty" && summary.placeholder != null;
  assert.equal(showPlaceholder({ health: "empty", placeholder: { created_by: "a", created_at: "t" } }), true);
  assert.equal(showPlaceholder({ health: "empty" }), false); // PUT-created, no marker
  assert.equal(showPlaceholder({ health: "healthy", head: {} }), false);
  assert.equal(showPlaceholder({ health: "healthy" }), false); // stale marker renders as real
  assert.equal(showPlaceholder(null), false);
});
