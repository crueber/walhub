import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

/** Pulls/forks surface (docs/features/03 §8): exact endpoints + methods. */
const SURFACE = [
  { name: "pulls.list", run: (c) => c.repo("o/r").pulls.list({ state: "open", n: 10 }), method: "GET", path: "/o/r/api/pulls?state=open&n=10" },
  { name: "pulls.open", run: (c) => c.repo("o/r").pulls.open({ title: "t", base_ref: "refs/heads/main", head_ref: "refs/heads/topic" }), method: "POST", path: "/o/r/api/pulls" },
  { name: "pulls.get", run: (c) => c.repo("o/r").pulls.get(7), method: "GET", path: "/o/r/api/pulls/7" },
  { name: "pulls.get window", run: (c) => c.repo("o/r").pulls.get(7, {}), method: "GET", path: "/o/r/api/pulls/7" },
  { name: "pulls.update", run: (c) => c.repo("o/r").pulls.update(7, { title: "x" }), method: "PUT", path: "/o/r/api/pulls/7" },
  { name: "pulls.comment", run: (c) => c.repo("o/r").pulls.comment(7, "hi"), method: "POST", path: "/o/r/api/pulls/7/comments" },
  { name: "pulls.diff", run: (c) => c.repo("o/r").pulls.diff(7), method: "GET", path: "/o/r/api/pulls/7/diff" },
  { name: "pulls.commits", run: (c) => c.repo("o/r").pulls.commits(7, { n: 5 }), method: "GET", path: "/o/r/api/pulls/7/commits?n=5" },
  { name: "pulls.merge", run: (c) => c.repo("o/r").pulls.merge(7, { strategy: "merge" }), method: "POST", path: "/o/r/api/pulls/7/merge" },
  { name: "pulls.mergeTask", run: (c) => c.repo("o/r").pulls.mergeTask(7), method: "GET", path: "/o/r/api/pulls/7/merge/task" },
  { name: "pulls.updateBranch", run: (c) => c.repo("o/r").pulls.updateBranch(7, {}), method: "POST", path: "/o/r/api/pulls/7/update-branch" },
  { name: "pulls.deleteHead", run: (c) => c.repo("o/r").pulls.deleteHead(7), method: "DELETE", path: "/o/r/api/pulls/7/head" },
  { name: "forks.create", run: (c) => c.repo("o/r").forks.create({ name: "r-fork" }), method: "POST", path: "/api/v1/repos/o/r/forks" },
];

test("pulls surface: every member hits its exact endpoint and method", async () => {
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

test("pulls.open sends a JSON body", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ thread: {}, pr: {} }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("o/r").pulls.open({ title: "t", base_ref: "refs/heads/main", head_ref: "refs/heads/topic" });
  const sent = JSON.parse(calls[0].init.body);
  assert.equal(sent.title, "t");
  assert.equal(sent.base_ref, "refs/heads/main");
  assert.equal(sent.head_ref, "refs/heads/topic");
  assert.equal(calls[0].init.headers["Content-Type"], "application/json");
});

test("pulls.merge sends strategy and delete flag", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ task: {} }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("o/r").pulls.merge(3, { strategy: "squash", delete_head: true });
  const sent = JSON.parse(calls[0].init.body);
  assert.equal(sent.strategy, "squash");
  assert.equal(sent.delete_head, true);
});
