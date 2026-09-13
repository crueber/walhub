import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { forkDefaultName, forkBranchShort } from "../../src/lib/fork.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

test("forkDefaultName appends -fork (the server default)", () => {
  assert.equal(forkDefaultName("widget"), "widget-fork");
  assert.equal(forkDefaultName("  widget  "), "widget-fork");
});

test("forkBranchShort strips refs/heads/", () => {
  assert.equal(forkBranchShort("refs/heads/main"), "main");
  assert.equal(forkBranchShort("refs/heads/feat/x"), "feat/x");
  assert.equal(forkBranchShort(""), "");
});

test("forks.create POSTs the twin path with the full fork opts", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ task: {}, repo: "me/widget-fork" }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  const repo = client.repo("acme/widget");
  const res = await repo.forks.create({
    target_owner: "me",
    name: "widget-fork",
    visibility: "private",
    branch: "refs/heads/dev",
    description: "my fork",
  });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, `${BASE}/api/v1/repos/acme/widget/forks`);
  assert.equal(calls[0].init.method, "POST");
  assert.deepEqual(JSON.parse(calls[0].init.body), {
    target_owner: "me",
    name: "widget-fork",
    visibility: "private",
    branch: "refs/heads/dev",
    description: "my fork",
  });
  assert.equal(res.repo, "me/widget-fork");
});

test("forks.list GETs the twin path with pagination", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ forks: [], more: false }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("acme/widget").forks.list({ n: 50 });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, `${BASE}/api/v1/repos/acme/widget/forks?n=50`);
  assert.equal(calls[0].init.method, "GET");
});
