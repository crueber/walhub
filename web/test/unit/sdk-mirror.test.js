import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

/** Mirrors surface (docs/features/11_mirror.md §5): exact endpoints + methods. */
const SURFACE = [
  { name: "mirrors.create", run: (c) => c.mirrors.create({ source_url: "u", owner: "a", name: "b" }), method: "POST", path: "/api/v1/repos/mirrors" },
  { name: "mirror.get", run: (c) => c.repo("a/b").mirror.get(), method: "GET", path: "/a/b/api/mirror" },
  { name: "mirror.put", run: (c) => c.repo("a/b").mirror.put({ schedule: "daily" }), method: "PUT", path: "/a/b/api/mirror" },
  { name: "mirror.remove", run: (c) => c.repo("a/b").mirror.remove(), method: "DELETE", path: "/a/b/api/mirror" },
  { name: "mirror.syncNow", run: (c) => c.repo("a/b").mirror.syncNow({ force: true }), method: "POST", path: "/a/b/api/mirror/sync" },
  { name: "mirror.syncStatus", run: (c) => c.repo("a/b").mirror.syncStatus("x"), method: "GET", path: "/a/b/api/mirror/sync?id=x" },
];

test("mirrors surface: every member hits its exact endpoint and method", async () => {
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

test("mirror.put sends the schedule, syncNow sends token+force", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ ok: true }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("a/b").mirror.put({ schedule: "hourly" });
  assert.equal(JSON.parse(calls[0].init.body).schedule, "hourly");
  await client.repo("a/b").mirror.syncNow({ token: "tok", force: true });
  const sent = JSON.parse(calls[1].init.body);
  assert.equal(sent.token, "tok");
  assert.equal(sent.force, true);
});

test("mirror.syncStatus without id lists active+recent", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ active: [], recent: [] }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("a/b").mirror.syncStatus();
  assert.equal(calls[0].url, `${BASE}/a/b/api/mirror/sync`);
});
