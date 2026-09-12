// web/test/unit/transfer.test.js — repo transfer (Forgejo #358): the SDK
// POST shape + lane, and the transferBody helper.
import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";
import { transferBody } from "../../src/lib/transfer.js";

const BASE = "http://api.test";

function bearerClient(handler) {
  const { fetch, calls } = fakeFetch(handler);
  return { client: new ReposClient({ base: BASE, fetch, token: "t" }), calls };
}

test("repo.transfer POSTs /{o}/{r}/api/transfer with the destination body", async () => {
  const { client, calls } = bearerClient(() => jsonResponse({ owner: "beta", repo: "r" }));
  const res = await client.repo("acme/r").transfer({ owner: "beta" });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, `${BASE}/acme/r/api/transfer`);
  assert.equal(calls[0].init.method, "POST");
  assert.deepEqual(JSON.parse(calls[0].init.body), { owner: "beta" });
  assert.deepEqual(res, { owner: "beta", repo: "r" });
});

test("repo.transfer carries an explicit rename and rides the browser lane off-DOM", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ owner: "beta", repo: "r2" }));
  const c = new ReposClient({ base: BASE, fetch }); // no token, off-DOM → browser lane
  await c.repo("acme/r").transfer({ owner: "beta", repo: "r2" });
  assert.equal(calls[0].url, `${BASE}/acme/r/api-browser/transfer`);
  assert.deepEqual(JSON.parse(calls[0].init.body), { owner: "beta", repo: "r2" });
});

test("transferBody trims and omits an empty repo (server defaults)", () => {
  assert.deepEqual(transferBody({ owner: "  beta ", repo: "" }), { owner: "beta" });
  assert.deepEqual(transferBody({ owner: "beta", repo: " r2 " }), { owner: "beta", repo: "r2" });
  assert.deepEqual(transferBody({}), { owner: "" });
  assert.deepEqual(transferBody(null), { owner: "" });
  assert.deepEqual(transferBody({ owner: 7, repo: ["x"] }), { owner: "" });
});
