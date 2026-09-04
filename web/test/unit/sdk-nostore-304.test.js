// web/test/unit/sdk-nostore-304.test.js — #41: mutation-triggered refetches
// must bypass the browser HTTP cache (SWR ref-dependent responses can serve
// a pre-mutation body as 200 from disk), and a 304 revalidation must resolve
// keep-current — never throw ReposError(304) into the error tray.

import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/core.js";
import { NOT_MODIFIED } from "../../sdk/src/errors.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://sdk.test";

function notModifiedResponse() {
  return new Response(null, { status: 304, headers: { "content-type": "application/json" } });
}

test("304 on a JSON GET resolves NOT_MODIFIED instead of throwing", async () => {
  const { fetch } = fakeFetch(() => notModifiedResponse());
  const c = new ReposClient({ base: BASE, fetch });
  assert.equal(await c.me(), NOT_MODIFIED);
});

test("304 on the raw path resolves NOT_MODIFIED instead of throwing", async () => {
  const { fetch } = fakeFetch(() => notModifiedResponse());
  const c = new ReposClient({ base: BASE, fetch });
  assert.equal(await c._call("/api/v1/setup.toml", { method: "GET", raw: true }), NOT_MODIFIED);
});

test("explicit per-call cache mode reaches the fetch init", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ ok: true }));
  const c = new ReposClient({ base: BASE, fetch });
  assert.deepEqual(await c.repo("a/b").issues.get(7, {}, { cache: "no-store" }), { ok: true });
  assert.equal(calls[0].init.cache, "no-store");
});

test("withNoStore arms cache bypass for sync SDK calls, then disarms", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({}));
  const c = new ReposClient({ base: BASE, fetch });
  await c.withNoStore(() => c.me());
  assert.equal(calls[0].init.cache, "no-store");
  assert.equal(calls[0].init.headers["Cache-Control"], "no-cache");
  assert.equal(calls[0].init.headers["Pragma"], "no-cache");
  await c.me();
  assert.equal(calls[1].init.cache, undefined);
  assert.equal(calls[1].init.headers["Cache-Control"], undefined);
});

test("withNoStore restores the arming when fn throws", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({}));
  const c = new ReposClient({ base: BASE, fetch });
  assert.throws(() => c.withNoStore(() => { throw new Error("boom"); }), /boom/);
  await c.me();
  assert.equal(calls[0].init.cache, undefined);
});
