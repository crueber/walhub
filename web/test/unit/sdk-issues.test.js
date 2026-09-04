import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

/** Issues/labels/milestones surface (docs/features/02 §7): exact endpoints + methods. */
const SURFACE = [
  { name: "issues.list", run: (c) => c.repo("o/r").issues.list({ state: "open", n: 10 }), method: "GET", path: "/o/r/api/issues?state=open&n=10" },
  { name: "issues.create", run: (c) => c.repo("o/r").issues.create({ title: "t" }), method: "POST", path: "/o/r/api/issues" },
  { name: "issues.get", run: (c) => c.repo("o/r").issues.get(7), method: "GET", path: "/o/r/api/issues/7" },
  { name: "issues.get window", run: (c) => c.repo("o/r").issues.get(7, { after_seq: 3, n: 20 }), method: "GET", path: "/o/r/api/issues/7?after_seq=3&n=20" },
  { name: "issues.patch", run: (c) => c.repo("o/r").issues.patch(7, { state: "closed" }), method: "PATCH", path: "/o/r/api/issues/7" },
  { name: "issues.comment", run: (c) => c.repo("o/r").issues.comment(7, "hi"), method: "POST", path: "/o/r/api/issues/7/comments" },
  { name: "issues.events", run: (c) => c.repo("o/r").issues.events(7, { n: 5 }), method: "GET", path: "/o/r/api/issues/7/events?n=5" },
  { name: "issues.reactions.add", run: (c) => c.repo("o/r").issues.reactions.add(7, { target_event_seq: 0, content: "+1" }), method: "POST", path: "/o/r/api/issues/7/reactions" },
  { name: "issues.reactions.remove", run: (c) => c.repo("o/r").issues.reactions.remove(7, 0, "+1"), method: "DELETE", path: "/o/r/api/issues/7/reactions/0/%2B1" },
  { name: "labels.list", run: (c) => c.repo("o/r").labels.list(), method: "GET", path: "/o/r/api/labels" },
  { name: "labels.create", run: (c) => c.repo("o/r").labels.create({ name: "bug", color: "d73a4a" }), method: "POST", path: "/o/r/api/labels" },
  { name: "labels.update", run: (c) => c.repo("o/r").labels.update("bug", { color: "000000" }), method: "PATCH", path: "/o/r/api/labels/bug" },
  { name: "labels.delete", run: (c) => c.repo("o/r").labels.delete("bug"), method: "DELETE", path: "/o/r/api/labels/bug" },
  { name: "milestones.list", run: (c) => c.repo("o/r").milestones.list({ state: "open" }), method: "GET", path: "/o/r/api/milestones?state=open" },
  { name: "milestones.get", run: (c) => c.repo("o/r").milestones.get("000001"), method: "GET", path: "/o/r/api/milestones/000001" },
  { name: "milestones.create", run: (c) => c.repo("o/r").milestones.create({ title: "v1" }), method: "POST", path: "/o/r/api/milestones" },
  { name: "milestones.update", run: (c) => c.repo("o/r").milestones.update("000001", { state: "closed" }), method: "PATCH", path: "/o/r/api/milestones/000001" },
  { name: "milestones.delete", run: (c) => c.repo("o/r").milestones.delete("000001"), method: "DELETE", path: "/o/r/api/milestones/000001" },
];

test("issues surface: every member hits its exact endpoint and method", async () => {
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

test("issues.create sends a JSON body", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ thread: {}, events: [] }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("o/r").issues.create({ title: "t", body: "b" });
  const sent = JSON.parse(calls[0].init.body);
  assert.equal(sent.title, "t");
  assert.equal(sent.body, "b");
  assert.equal(calls[0].init.headers["Content-Type"], "application/json");
});
