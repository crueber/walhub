import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";
const HEAD = "a".repeat(40);
const CTX = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
const ANCHOR = {
  path: "src/main.go", side: "NEW", old_start: 0, old_lines: 0,
  new_start: 120, new_lines: 3, commit_sha: HEAD, context_sha: CTX,
};

/** Reviews surface (docs/features/04 §7): exact endpoints + methods. */
const SURFACE = [
  { name: "reviews.list", run: (c) => c.repo("o/r").pulls.reviews.list(7, { n: 10 }), method: "GET", path: "/o/r/api/pulls/7/reviews?n=10" },
  { name: "reviews.submit", run: (c) => c.repo("o/r").pulls.reviews.submit(7, { state: "APPROVED", commit_sha: HEAD }), method: "POST", path: "/o/r/api/pulls/7/reviews" },
  { name: "reviews.get", run: (c) => c.repo("o/r").pulls.reviews.get(7, 1), method: "GET", path: "/o/r/api/pulls/7/reviews/1" },
  { name: "reviews.dismiss", run: (c) => c.repo("o/r").pulls.reviews.dismiss(7, 1, { reason: "stale" }), method: "POST", path: "/o/r/api/pulls/7/reviews/1/dismiss" },
  { name: "threads.list", run: (c) => c.repo("o/r").pulls.threads.list(7, { resolved: false }), method: "GET", path: "/o/r/api/pulls/7/threads?resolved=false" },
  { name: "threads.create", run: (c) => c.repo("o/r").pulls.threads.create(7, { anchor: ANCHOR, body: "nit" }), method: "POST", path: "/o/r/api/pulls/7/threads" },
  { name: "threads.get", run: (c) => c.repo("o/r").pulls.threads.get(7, "00000001"), method: "GET", path: "/o/r/api/pulls/7/threads/00000001" },
  { name: "threads.comment", run: (c) => c.repo("o/r").pulls.threads.comment(7, "00000001", "ack"), method: "POST", path: "/o/r/api/pulls/7/threads/00000001/comments" },
  { name: "threads.resolve", run: (c) => c.repo("o/r").pulls.threads.resolve(7, "00000001"), method: "POST", path: "/o/r/api/pulls/7/threads/00000001/resolve" },
  { name: "threads.unresolve", run: (c) => c.repo("o/r").pulls.threads.unresolve(7, "00000001"), method: "POST", path: "/o/r/api/pulls/7/threads/00000001/unresolve" },
  { name: "requests.list", run: (c) => c.repo("o/r").pulls.requests.list(7), method: "GET", path: "/o/r/api/pulls/7/review-requests" },
  { name: "requests.add", run: (c) => c.repo("o/r").pulls.requests.add(7, ["bob"]), method: "POST", path: "/o/r/api/pulls/7/review-requests" },
  { name: "requests.remove", run: (c) => c.repo("o/r").pulls.requests.remove(7, ["bob"]), method: "DELETE", path: "/o/r/api/pulls/7/review-requests" },
  { name: "suggest", run: (c) => c.repo("o/r").pulls.suggest(7, "bo"), method: "GET", path: "/o/r/api/pulls/7/review-suggest?q=bo" },
];

test("reviews surface: every member hits its exact endpoint and method", async () => {
  for (const row of SURFACE) {
    const { fetch, calls } = fakeFetch((ctx) =>
      ctx.init.method === row.method ? jsonResponse({ ok: true }) : new Response("bad method", { status: 405 })
    );
    const client = new ReposClient({ base: BASE, fetch, token: "t" });
    await row.run(client);
    assert.equal(calls.length, 1, row.name);
    assert.equal(calls[0].url, `${BASE}${row.path}`, `${row.name} → ${row.method} ${row.path}`);
    assert.equal(calls[0].init.method, row.method, row.name);
  }
});

test("reviews.submit sends state, sha, and threads", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ review: {}, threads: [], summary: {} }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("o/r").pulls.reviews.submit(7, { state: "COMMENTED", commit_sha: HEAD, threads: [{ anchor: ANCHOR, body: "nit" }] });
  const sent = JSON.parse(calls[0].init.body);
  assert.equal(sent.state, "COMMENTED");
  assert.equal(sent.commit_sha, HEAD);
  assert.equal(sent.threads.length, 1);
  assert.equal(sent.threads[0].anchor.path, "src/main.go");
});
