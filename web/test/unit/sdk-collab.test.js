import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient, ReposError } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse, sseResponse, endlessSseResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";
const bearer = { base: BASE, token: "t" }; // bearer lane → paths unchanged

test("permissions/collaborators/assignables hit the 08 §3.6 endpoints", async () => {
  const rows = [
    { run: (c) => c.repo("o/r").permissions(), method: "GET", path: "/o/r/api/permissions" },
    { run: (c) => c.repo("o/r").collaborators.list(), method: "GET", path: "/o/r/api/collaborators" },
    { run: (c) => c.repo("o/r").assignables(), method: "GET", path: "/o/r/api/assignables" },
  ];
  for (const row of rows) {
    const { fetch, calls } = fakeFetch(() => jsonResponse({ ok: true }));
    const c = new ReposClient({ ...bearer, fetch });
    await row.run(c);
    assert.equal(calls.length, 1, row.path);
    assert.equal(calls[0].url, `${BASE}${row.path}`, row.path);
    assert.equal(calls[0].init.method, row.method, row.path);
  }
});

test("collab.stream merges event name + data into frames", async () => {
  const { fetch } = fakeFetch(() =>
    sseResponse([
      `event: issue_event\ndata: {"num":3,"seq":12,"at":"t"}\n\n`,
      `event: check\ndata: {"sha":"abc","context":"ci","state":"success","combined_state":"success","at":"t"}\n\n`,
    ]),
  );
  const c = new ReposClient({ ...bearer, fetch });
  const frames = [];
  // The stream ends (server never does — the EOF rejects so the page
  // reconnects instead of staring at a silent timeline).
  await assert.rejects(c.repo("o/r").collab.stream((f) => frames.push(f)), /stream ended/);
  assert.deepEqual(frames, [
    { kind: "issue_event", num: 3, seq: 12, at: "t" },
    { kind: "check", sha: "abc", context: "ci", state: "success", combined_state: "success", at: "t" },
  ]);
});

test("collab.stream surfaces HTTP errors as ReposError (plain-text body)", async () => {
  const { fetch } = fakeFetch(() => new Response("read access required", { status: 403 }));
  const c = new ReposClient({ ...bearer, fetch });
  await assert.rejects(c.repo("o/r").collab.stream(() => {}), (err) => {
    assert.ok(err instanceof ReposError);
    assert.equal(err.status, 403);
    assert.equal(err.message, "read access required");
    return true;
  });
});

test("collab.stream honors the caller signal (abort → 499, reader closed)", async () => {
  let cancelled = false;
  const { fetch } = fakeFetch(() => endlessSseResponse(`event: issue\ndata: {"num":1}\n\n`, { onCancel: () => { cancelled = true; } }));
  const c = new ReposClient({ ...bearer, fetch });
  const ctrl = new AbortController();
  const frames = [];
  const p = c.repo("o/r").collab.stream((f) => frames.push(f), { signal: ctrl.signal });
  await new Promise((r) => setTimeout(r, 50));
  assert.ok(frames.length > 0, "frames flow before abort");
  ctrl.abort();
  await assert.rejects(p, (err) => err instanceof ReposError && err.status === 499);
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(cancelled, true, "reader closed on abort");
});
