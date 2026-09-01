import { test } from "node:test";
import assert from "node:assert/strict";

import { fakeFetch, sseResponse, jsonResponse, endlessSseResponse } from "../helpers/fetch.js";
import { ReposClient } from "../../sdk/src/core.js";
import { parseFrame } from "../../sdk/src/sse.js";

const BASE = "http://api.test";

function client(handler) {
  const { fetch, calls } = fakeFetch(handler);
  return { client: new ReposClient({ base: BASE, fetch }), calls };
}

test("refStream: ref frames invoke onRef as they arrive; done terminates", async () => {
  const refs = [];
  const { client: c, calls } = client(() =>
    sseResponse([
      `event: ref\ndata: {"name":"refs/heads/main","sha":"aa11"}\n\n`,
      `event: ref\ndata: {"name":"refs/heads/dev","sha":"bb22"}\n\n`,
      `event: done\ndata: {"more":false}\n\n`,
    ]),
  );
  const cancel = await c.repo("o/r").refStream("branches", { q: "ma" }, (ref) => refs.push(ref));
  assert.deepEqual(refs, [
    { name: "refs/heads/main", sha: "aa11" },
    { name: "refs/heads/dev", sha: "bb22" },
  ]);
  assert.equal(typeof cancel, "function", "refStream returns a cancellation function (§1.6)");
  // GET with query, lane auth only (§1.5)
  assert.equal(calls[0].init.method, "GET");
  assert.ok(calls[0].url.endsWith("/o/r/api-browser/refs/branches?q=ma"));
});

test("refStream incremental delivery: onRef fires before the stream ends", async () => {
  const refs = [];
  let push;
  let close;
  const encoder = new TextEncoder();
  const body = new ReadableStream({
    start(controller) {
      controller.enqueue(encoder.encode(`event: ref\ndata: {"name":"refs/heads/one","sha":"1"}\n\n`));
      push = (s) => controller.enqueue(encoder.encode(s));
      close = () => controller.close();
    },
  });
  const { client: c } = client(() => new Response(body, { status: 200, headers: { "content-type": "text/event-stream" } }));
  const done = c.repo("o/r").refStream("branches", {}, (ref) => refs.push(ref));
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(refs.length, 1, "painted incrementally, before stream end");
  push(`event: ref\ndata: {"name":"refs/heads/two","sha":"2"}\n\nevent: done\ndata: {"more":true}\n\n`);
  close();
  await done.then((c) => c());
  assert.deepEqual(refs, [
    { name: "refs/heads/one", sha: "1" },
    { name: "refs/heads/two", sha: "2" },
  ]);
});

test("refStream: aborting the signal closes the stream reader (controller swap pattern)", async () => {
  let cancelled = false;
  const { client: c } = client(() =>
    endlessSseResponse(`event: ref\ndata: {"name":"refs/heads/x","sha":"3"}\n\n`, { onCancel: () => (cancelled = true) }),
  );
  const ac = new AbortController();
  const refs = [];
  const p = c.repo("o/r").refStream("tags", {}, (r) => refs.push(r), { signal: ac.signal });
  const rejection = p.catch((err) => err);
  await new Promise((r) => setTimeout(r, 10));
  ac.abort(); // the UI aborts the in-flight stream on every keystroke
  const err = await rejection;
  assert.equal(err?.status, 499, "aborted stream rejects with ReposError(499)");
  assert.equal(cancelled, true, "reader cancelled by the SDK (§1.6)");
});

test("refStream passes q/prefix/after/n through the query string", async () => {
  const { client: c, calls } = client(() => sseResponse([`event: done\ndata: {"more":false}\n\n`]));
  await c.repo("o/r").refStream("tags", { prefix: "refs/tags/v", after: "v1", n: 50 }, () => {});
  assert.ok(calls[0].url.includes("prefix=refs%2Ftags%2Fv"));
  assert.ok(calls[0].url.includes("after=v1"));
  assert.ok(calls[0].url.includes("n=50"));
});

test("branches()/tags() are JSON-only accept (§1.1)", async () => {
  const { client: c, calls } = client(() => jsonResponse({ refs: [], more: false }));
  await c.repo("o/r").branches({ q: "ma", n: 50 });
  assert.equal(calls[0].init.headers.Accept, "application/json");
  assert.ok(calls[0].url.endsWith("/o/r/api-browser/refs/branches?q=ma&n=50"), calls[0].url); // off-DOM = browser lane
});

test("refStream Accept is text/event-stream (ref-list dialect trigger)", async () => {
  const { client: c, calls } = client(() => sseResponse([`event: done\ndata: {"more":false}\n\n`]));
  await c.repo("o/r").refStream("branches", {}, () => {});
  assert.equal(calls[0].init.headers.Accept, "text/event-stream");
});

test("parseFrame: ref dialect frames", () => {
  assert.deepEqual(parseFrame(`event: ref\ndata: {"name":"refs/heads/main","sha":"abc"}`), {
    event: "ref",
    data: { name: "refs/heads/main", sha: "abc" },
  });
  assert.deepEqual(parseFrame(`event: done\ndata: {"more":true}`), { event: "done", data: { more: true } });
  assert.deepEqual(parseFrame(`: walgit`), { event: "message", data: "" });
});