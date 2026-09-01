import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient, ReposError } from "../../sdk/src/index.js";
import { parseFrame, readSse } from "../../sdk/src/sse.js";
import { fakeFetch, jsonResponse, sseResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

test("public surface: named exports + default export (§1.1)", async () => {
  const mod = await import("../../sdk/src/index.js");
  assert.equal(typeof mod.ReposClient, "function");
  assert.equal(typeof mod.ReposError, "function");
  // default export is a ready-to-use client instance (§5b usage)
  assert.equal(typeof mod.default.configure, "function");
  assert.equal(typeof mod.default.repo, "function");
  assert.ok(new mod.ReposClient({ base: BASE }) instanceof ReposClient);
  assert.ok(mod.ReposError.prototype instanceof Error);
});

/** Endpoint table (12_web_ui.md §1.1 / MASTER_RUST_SPEC §10.1) — wire-identical argv. */
const SURFACE = [
  { name: "me", run: (c) => c.me(), method: "GET", path: "/api/v1/me" },
  { name: "owners.list", run: (c) => c.owners.list(), method: "GET", path: "/api/v1/owners" },
  { name: "owners.repos", run: (c) => c.owners.repos("demo"), method: "GET", path: "/api/v1/owners/demo/repos" },
  { name: "repo.get", run: (c) => c.repo("o/r").get(), method: "GET", path: "/o/r/api" },
  { name: "repo.create", run: (c) => c.repo("o/r").create(), method: "PUT", path: "/o/r/api" },
  { name: "repo.delete", run: (c) => c.repo("o/r").delete(), method: "DELETE", path: "/o/r/api" },
  { name: "repo.refs", run: (c) => c.repo("o/r").refs(), method: "GET", path: "/o/r/api/refs" },
  { name: "repo.branches", run: (c) => c.repo("o/r").branches({ q: "x" }), method: "GET", path: "/o/r/api/refs/branches?q=x" },
  { name: "repo.tags", run: (c) => c.repo("o/r").tags({ prefix: "v" }), method: "GET", path: "/o/r/api/refs/tags?prefix=v" },
  { name: "repo.resolve", run: (c) => c.repo("o/r").resolve("main/src/x.js"), method: "GET", path: "/o/r/api/resolve/main/src/x.js" },
  { name: "repo.tree", run: (c) => c.repo("o/r").tree("main", "src"), method: "GET", path: "/o/r/api/tree/main/src" },
  { name: "repo.tree root", run: (c) => c.repo("o/r").tree("main"), method: "GET", path: "/o/r/api/tree/main" },
  { name: "repo.blob", run: (c) => c.repo("o/r").blob("main", "README.md"), method: "GET", path: "/o/r/api/blob/main/README.md" },
  { name: "repo.raw", run: (c) => c.repo("o/r").raw("main", "README.md"), method: "GET", path: "/o/r/api/blob/main/README.md?raw" },
  { name: "repo.commits", run: (c) => c.repo("o/r").commits({ ref: "main", path: "src", skip: 5, n: 20 }), method: "GET", path: "/o/r/api/commits?ref=main&path=src&skip=5&n=20" },
  { name: "repo.commit", run: (c) => c.repo("o/r").commit("abc123"), method: "GET", path: "/o/r/api/commit/abc123" },
  { name: "repo.overview", run: (c) => c.repo("o/r").overview(), method: "GET", path: "/o/r/api/overview" },
  { name: "repo.tasks", run: (c) => c.repo("o/r").tasks(), method: "GET", path: "/o/r/api/tasks" },
  { name: "repo.task", run: (c) => c.repo("o/r").task("t9"), method: "GET", path: "/o/r/api/tasks/t9" },
  { name: "repo.ops.list", run: (c) => c.repo("o/r").ops.list(), method: "GET", path: "/o/r/api/ops" },
  { name: "repo.policy.get", run: (c) => c.repo("o/r").policy.get(), method: "GET", path: "/o/r/api/policy" },
  { name: "repo.policy.put", run: (c) => c.repo("o/r").policy.put({ protect: [] }), method: "PUT", path: "/o/r/api/policy" },
  { name: "repo.policy.delete", run: (c) => c.repo("o/r").policy.delete(), method: "DELETE", path: "/o/r/api/policy" },
  { name: "repo.policy.validate", run: (c) => c.repo("o/r").policy.validate({}), method: "POST", path: "/o/r/api/policy/validate" },
  { name: "repo.policy.dryRun", run: (c) => c.repo("o/r").policy.dryRun(5), method: "POST", path: "/o/r/api/policy/dry-run?last=5" },
  { name: "repo.settings.get", run: (c) => c.repo("o/r").settings.get(), method: "GET", path: "/o/r/api/settings" },
  { name: "repo.settings.put", run: (c) => c.repo("o/r").settings.put("[bundles]\n", "msg"), method: "PUT", path: "/o/r/api/settings?message=msg" },
  { name: "repo.settings.delete", run: (c) => c.repo("o/r").settings.delete(), method: "DELETE", path: "/o/r/api/settings" },
  { name: "repo.settings.effective", run: (c) => c.repo("o/r").settings.effective(), method: "GET", path: "/o/r/api/settings/effective" },
  { name: "repo.settings.history", run: (c) => c.repo("o/r").settings.history(), method: "GET", path: "/o/r/api/settings/history" },
  { name: "repo.settings.describe", run: (c) => c.repo("o/r").settings.describe(), method: "GET", path: "/o/r/api/settings/describe" },
  { name: "repo.settings.validate", run: (c) => c.repo("o/r").settings.validate("[x]\n"), method: "POST", path: "/o/r/api/settings/validate" },
];

test("surface table: every member hits its exact endpoint and method", async () => {
  for (const row of SURFACE) {
    const { fetch, calls } = fakeFetch((ctx) =>
      ctx.init.method === row.method ? jsonResponse({ ok: true }) : new Response("bad method", { status: 405 }),
    );
    const c = new ReposClient({ base: BASE, fetch, token: "t" }); // bearer lane → paths unchanged
    await row.run(c);
    assert.equal(calls.length, 1, row.name);
    assert.equal(calls[0].url, `${BASE}${row.path}`, `${row.name} → ${row.method} ${row.path}`);
    assert.equal(calls[0].init.method, row.method, row.name);
  }
});

test("repo.urls deep links (§1.1)", () => {
  const { fetch } = fakeFetch(() => jsonResponse({}));
  const c = new ReposClient({ base: BASE, fetch });
  const u = c.repo("demo/hello").urls;
  assert.equal(u.html, `${BASE}/demo/hello`);
  assert.equal(u.clone, `${BASE}/demo/hello.git`);
  assert.equal(u.api, `${BASE}/demo/hello/api`);
  assert.equal(u.raw("main", "a/b.txt"), `${BASE}/demo/hello/raw/main/a/b.txt`);
  assert.equal(u.tree("v1"), `${BASE}/demo/hello/tree/v1`);
  assert.equal(u.blob("main", "x.md"), `${BASE}/demo/hello/blob/main/x.md`);
  assert.equal(u.commit("deadbeef"), `${BASE}/demo/hello/commit/deadbeef`);
});

test("repo() tolerates a .git suffix", () => {
  const { fetch } = fakeFetch(() => jsonResponse({}));
  const c = new ReposClient({ base: BASE, fetch });
  assert.equal(c.repo("demo/hello.git").prefix, "demo/hello/api");
});

test("ops.run POSTs JSON params and resolves with the SSE result; cancel is returned", async () => {
  const { fetch, calls } = fakeFetch((ctx) => {
    assert.equal(ctx.init.method, "POST");
    assert.deepEqual(JSON.parse(ctx.init.body), { strategy: "rolling" });
    return sseResponse([
      `event: progress\ndata: {"label":"run","done":1,"total":3,"unit":"step"}\n\n`,
      `event: result\ndata: {"task":"t1"}\n\n`,
    ]);
  });
  const c = new ReposClient({ base: BASE, fetch });
  const seen = [];
  const { result, cancel } = await c.repo("o/r").ops.run("fsck", { strategy: "rolling" }, (p) => seen.push(p));
  assert.deepEqual(result, { task: "t1" });
  assert.equal(seen.length, 1);
  assert.ok(calls[0].url.endsWith("/o/r/api-browser/ops/fsck"), calls[0].url); // off-DOM = browser lane
});

test("task attach streams SSE events and returns {result, cancel}", async () => {
  const { fetch } = fakeFetch(() =>
    sseResponse([`event: task\ndata: {"id":"t1","state":"running"}\n\nevent: result\ndata: {"state":"done"}\n\n`]),
  );
  const c = new ReposClient({ base: BASE, fetch });
  const seen = [];
  const { result, cancel } = await c.repo("o/r").task("t1", (p) => seen.push(p));
  assert.deepEqual(result, { state: "done" });
  assert.deepEqual(seen[0], { event: "task", id: "t1", state: "running" });
  assert.equal(typeof cancel, "function");
});

test("task without onEvent returns the JSON snapshot", async () => {
  const { fetch } = fakeFetch(() => jsonResponse({ id: "t1", state: "done" }));
  const c = new ReposClient({ base: BASE, fetch });
  assert.deepEqual(await c.repo("o/r").task("t1"), { id: "t1", state: "done" });
});

test("ReposError getters and fields (§1.1)", () => {
  const e = new ReposError(404, "nope", `${BASE}/x`);
  assert.equal(e.status, 404);
  assert.equal(e.message, "nope");
  assert.equal(e.url, `${BASE}/x`);
  assert.equal(e.notFound, true);
  assert.equal(e.unauthorized, false);
  const e401 = new ReposError(401, "unauth");
  assert.equal(e401.notFound, false);
  assert.equal(e401.unauthorized, true);
  assert.equal(new ReposError(502, "x").notFound, false);
});

test("every SSE frame surfaces both handlers; result resolves after frames", async () => {
  const global = [];
  const perCall = [];
  const { fetch } = fakeFetch(() => sseResponse([`event: notice\ndata: {"text":"a"}\n\n`, `event: result\ndata: 42\n\n`]));
  const c = new ReposClient({ base: BASE, fetch, onProgress: (p) => global.push(p) });
  const v = await c.repo("o/r").get({ onProgress: (p) => perCall.push(p) });
  assert.equal(v, 42);
  assert.equal(global.length, 1);
  assert.equal(perCall.length, 1);
});

test("readSse closes the reader even when onFrame throws", async () => {
  let cancelled = false;
  const encoder = new TextEncoder();
  const body = new ReadableStream({
    pull(controller) {
      controller.enqueue(encoder.encode(`event: result\ndata: 1\n\n`));
    },
    cancel() {
      cancelled = true;
    },
  });
  const res = new Response(body, { status: 200, headers: { "content-type": "text/event-stream" } });
  await assert.rejects(readSse(res, () => {
    throw new Error("boom");
  }), /boom/);
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(cancelled, true);
});
