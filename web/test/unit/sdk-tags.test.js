import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";
const SHA = "0123456789abcdef0123456789abcdef01234567";

/** Tags surface (Forgejo #253/#263): create a lightweight or annotated tag. */
const SURFACE = [
  { name: "tagsApi.create", run: (c) => c.repo("o/r").tagsApi.create({ name: "v1", sha: SHA }), method: "POST", path: "/o/r/api/tags" },
];

test("tags surface: create hits its exact endpoint and method", async () => {
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

test("tagsApi.create sends a JSON body", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ name: "v1", sha: SHA, ref: "refs/tags/v1" }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  const tag = await client.repo("o/r").tagsApi.create({ name: "v1", sha: SHA });
  const sent = JSON.parse(calls[0].init.body);
  assert.equal(sent.name, "v1");
  assert.equal(sent.sha, SHA);
  assert.equal(calls[0].init.headers["Content-Type"], "application/json");
  assert.equal(tag.ref, "refs/tags/v1");
});

test("tagsApi.create flows an annotation message to the annotated path", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ name: "v2", sha: "a".repeat(40), ref: "refs/tags/v2" }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("o/r").tagsApi.create({ name: "v2", sha: SHA, message: "release two" });
  const sent = JSON.parse(calls[0].init.body);
  assert.equal(sent.message, "release two");
});
