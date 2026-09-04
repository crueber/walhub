import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";
const SHA = "a".repeat(40);

/** Checks/CI-token surface (docs/features/05 §4): exact endpoints + methods. */
const SURFACE = [
  { name: "checks.combined", run: (c) => c.repo("o/r").checks.combined(SHA), method: "GET", path: `/o/r/api/checks/${SHA}` },
  { name: "checks.statuses", run: (c) => c.repo("o/r").checks.statuses(SHA), method: "GET", path: `/o/r/api/checks/statuses/${SHA}` },
  { name: "checks.list", run: (c) => c.repo("o/r").checks.list({ n: 10 }), method: "GET", path: "/o/r/api/checks?n=10" },
  { name: "checks.list cursor", run: (c) => c.repo("o/r").checks.list({ after: SHA }), method: "GET", path: `/o/r/api/checks?after=${SHA}` },
  { name: "checks.report", run: (c) => c.repo("o/r").checks.report(SHA, { context: "ci/build", state: "success" }), method: "POST", path: `/o/r/api/checks/statuses/${SHA}` },
  { name: "ciTokens.create", run: (c) => c.repo("o/r").ciTokens.create({ name: "woodpecker" }), method: "POST", path: "/o/r/api/checks/tokens" },
  { name: "ciTokens.list", run: (c) => c.repo("o/r").ciTokens.list(), method: "GET", path: "/o/r/api/checks/tokens" },
  { name: "ciTokens.revoke", run: (c) => c.repo("o/r").ciTokens.revoke("abcd1234"), method: "DELETE", path: "/o/r/api/checks/tokens/abcd1234" },
];

test("checks surface: every member hits its exact endpoint and method", async () => {
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

test("checks.report sends the status payload", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ sha: SHA }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("o/r").checks.report(SHA, {
    context: "ci/build",
    state: "failure",
    target_url: "https://ci.example/7",
    description: "run 7",
  });
  const sent = JSON.parse(calls[0].init.body);
  assert.equal(sent.context, "ci/build");
  assert.equal(sent.state, "failure");
  assert.equal(sent.target_url, "https://ci.example/7");
  assert.equal(sent.description, "run 7");
  assert.equal(calls[0].init.headers["Content-Type"], "application/json");
});

test("ciTokens.create sends name and scopes", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ id: "abcd1234" }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("o/r").ciTokens.create({ name: "woodpecker", scopes: ["checks:write"] });
  const sent = JSON.parse(calls[0].init.body);
  assert.equal(sent.name, "woodpecker");
  assert.deepEqual(sent.scopes, ["checks:write"]);
});
