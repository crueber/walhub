import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/core.js";
import { fakeFetch, jsonResponse, sseResponse, textResponse, opaqueRedirectResponse, endlessSseResponse } from "../helpers/fetch.js";

const BASE = "http://sdk.test";

function client(handler, opts = {}) {
  const { fetch, calls } = fakeFetch(handler);
  return { client: new ReposClient({ base: BASE, fetch, ...opts }), calls };
}

test("plain JSON response resolves directly", async () => {
  const { client: c } = client(() => jsonResponse({ entries: [1, 2] }));
  assert.deepEqual(await c.repo("a/b").tree("main", ""), { entries: [1, 2] });
});

test("doc skeleton case: SSE envelope → result payload", async () => {
  const { client: c } = client(() =>
    sseResponse([`event: progress\ndata: {"n":1}\n\nevent: result\ndata: {"entries":[]}\n\n`]),
  );
  assert.deepEqual(await c.repo("a/b").tree("main", ""), { entries: [] });
});

test("GET sends Accept: application/json, text/event-stream", async () => {
  const { client: c, calls } = client(() => jsonResponse({}));
  await c.me();
  assert.equal(calls[0].init.headers.Accept, "application/json, text/event-stream");
});

test("notice/progress/task surface through per-call onProgress", async () => {
  const seen = [];
  const { client: c } = client(() =>
    sseResponse([
      `: walgit\n\n`,
      `event: notice\ndata: {"text":"packing"}\n\n`,
      `event: progress\ndata: {"label":"pack","done":1,"total":2,"unit":"obj"}\n\n`,
      `event: task\ndata: {"id":"t1","kind":"fsck","state":"running"}\n\n`,
      `event: result\ndata: {"ok":true}\n\n`,
    ]),
  );
  const out = await c.repo("a/b").overview({ onProgress: (p) => seen.push(p) });
  assert.deepEqual(out, { ok: true });
  assert.deepEqual(seen[0], { event: "notice", text: "packing" });
  assert.deepEqual(seen[1], { event: "progress", label: "pack", done: 1, total: 2, unit: "obj" });
  assert.deepEqual(seen[2], { event: "task", id: "t1", kind: "fsck", state: "running" });
});

test("notice/progress/task ALSO surface through the client-global handler", async () => {
  const seen = [];
  const { client: c } = client(() => sseResponse([`event: progress\ndata: {"label":"x","done":1}\n\nevent: result\ndata: 7\n\n`]));
  c.onProgress = (p) => seen.push(p);
  assert.equal(await c.repo("a/b").refs(), 7);
  assert.deepEqual(seen, [{ event: "progress", label: "x", done: 1 }]);
});

test("error frame → ReposError(status, message)", async () => {
  const { client: c } = client(() => sseResponse([`event: error\ndata: {"status":503,"message":"packs not ready"}\n\n`]));
  await assert.rejects(c.repo("a/b").overview(), (err) => {
    assert.ok(err instanceof Error);
    assert.equal(err.status, 503);
    assert.equal(err.message, "packs not ready");
    return true;
  });
});

test("stream ending without a result → ReposError(502, 'stream ended without a result')", async () => {
  const { client: c } = client(() => sseResponse([`event: progress\ndata: {"label":"x","done":1}\n\n`]));
  await assert.rejects(c.repo("a/b").overview(), (err) => {
    assert.equal(err.status, 502);
    assert.equal(err.message, "stream ended without a result");
    return true;
  });
});

test("keepalive comments are ignored", async () => {
  const { client: c } = client(() =>
    sseResponse([`: walgit\n\n`, `: keepalive\n\n`, `event: result\ndata: {"v":1}\n\n`, `: keepalive\n\n`]),
  );
  assert.deepEqual(await c.me(), { v: 1 });
});

test("frames split across chunk boundaries parse correctly", async () => {
  const { client: c } = client(() =>
    sseResponse([`event: result\nda`, `ta: {"split":tr`, `ue}\n\n`]),
  );
  assert.deepEqual(await c.me(), { split: true });
});

test("empty frames and CR line endings are tolerated", async () => {
  const { client: c } = client(() => sseResponse(["\r\n\r\nevent: result\r\ndata: {\"cr\":1}\r\n\r\n"]));
  assert.deepEqual(await c.me(), { cr: 1 });
});

test("abort mid-stream → ReposError(499, 'aborted'); reader closed", async () => {
  let cancelled = false;
  const { client: c } = client(() =>
    endlessSseResponse(`event: progress\ndata: {"label":"x","done":0}\n\n`, { onCancel: () => (cancelled = true) }),
  );
  const ac = new AbortController();
  const p = c.repo("a/b").overview({ signal: ac.signal });
  ac.abort();
  await assert.rejects(p, (err) => err.status === 499 && err.message === "aborted");
  // give the microtask queue a beat for the finally to run
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(cancelled, true, "underlying reader must be cancelled (SDK closes the reader)");
});

test("non-2xx plain-text body → ReposError with the verbatim message", async () => {
  const { client: c } = client(() => textResponse("unknown revision", 404));
  await assert.rejects(c.repo("a/b").tree("nope", ""), (err) => {
    assert.equal(err.status, 404);
    assert.equal(err.message, "unknown revision");
    assert.equal(err.notFound, true);
    return true;
  });
});

test("non-2xx empty body falls back to 'HTTP <status>'", async () => {
  const { client: c } = client(() => new Response("", { status: 500 }));
  await assert.rejects(c.me(), (err) => err.status === 500 && err.message === "HTTP 500");
});

test("opaque redirect (browser lane) is treated like 401: auth then retry once", async () => {
  let n = 0;
  let authed = 0;
  const { client: c, calls } = client(() => {
    n++;
    if (n === 1) return opaqueRedirectResponse();
    return jsonResponse({ principal: "jane" });
  });
  c.authenticate = async () => {
    authed++;
  };
  assert.deepEqual(await c.me(), { principal: "jane" });
  assert.equal(authed, 1);
  assert.equal(calls.length, 2, "retried exactly once");
});
