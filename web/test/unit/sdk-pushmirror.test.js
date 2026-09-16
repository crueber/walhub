import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

/** Push-mirror surface (docs/features/13_push_mirror.md): exact endpoints + methods. */
const SURFACE = [
  { name: "pushmirror.get", run: (c) => c.repo("a/b").pushmirror.get(), method: "GET", path: "/a/b/api/pushmirror" },
  { name: "pushmirror.put", run: (c) => c.repo("a/b").pushmirror.put({ schedule: "daily" }), method: "PUT", path: "/a/b/api/pushmirror" },
  { name: "pushmirror.remove", run: (c) => c.repo("a/b").pushmirror.remove(), method: "DELETE", path: "/a/b/api/pushmirror" },
  { name: "pushmirror.syncNow", run: (c) => c.repo("a/b").pushmirror.syncNow({ force: true }), method: "POST", path: "/a/b/api/pushmirror/sync" },
  { name: "pushmirror.syncStatus", run: (c) => c.repo("a/b").pushmirror.syncStatus("x"), method: "GET", path: "/a/b/api/pushmirror/sync?id=x" },
  { name: "pushmirror.keygen", run: (c) => c.repo("a/b").pushmirror.keygen(), method: "POST", path: "/a/b/api/pushmirror/keygen" },
];

test("pushmirror surface: every member hits its exact endpoint and method", async () => {
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

test("pushmirror.put sends secrets write-only, syncNow sends force only, keygen sends known_hosts", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ ok: true }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("a/b").pushmirror.put({ upstream_url: "https://x/y.git", auth_kind: "token", token: "tok" });
  const put = JSON.parse(calls[0].init.body);
  assert.equal(put.token, "tok");
  assert.equal(put.auth_kind, "token");
  await client.repo("a/b").pushmirror.syncNow({ force: true });
  const sync = JSON.parse(calls[1].init.body);
  assert.equal(sync.force, true);
  assert.ok(!("token" in sync), "sync body carries no credentials");
  await client.repo("a/b").pushmirror.keygen({ known_hosts: "kh" });
  assert.equal(JSON.parse(calls[2].init.body).known_hosts, "kh");
});

test("pushmirror.syncStatus without id lists active+recent", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ active: [], recent: [] }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("a/b").pushmirror.syncStatus();
  assert.equal(calls[0].url, `${BASE}/a/b/api/pushmirror/sync`);
});
