import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

/** Attachments surface (docs/features/02 §12): upload endpoint + headers. */

test("attachments.upload posts raw bytes with name query and sha header", async () => {
  const record = { name: "s.png", size: 8, sha256: "x", content_type: "image/png", url: "/o/r/attachments/x/s.png" };
  const { fetch, calls } = fakeFetch(() => jsonResponse(record));
  const client = new ReposClient({ base: BASE, fetch, token: "t" }); // bearer lane → paths unchanged
  const file = new Blob([new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])], { type: "image/png" });
  file.name = "shot.png";
  const got = await client.repo("o/r").attachments.upload(file);
  assert.deepEqual(got, record);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, `${BASE}/o/r/api/attachments?name=shot.png`);
  assert.equal(calls[0].init.method, "POST");
  assert.equal(calls[0].init.headers["Content-Type"], "application/octet-stream");
  const sha = calls[0].init.headers["X-Walgit-Attachment-Sha256"];
  assert.match(sha, /^[0-9a-f]{64}$/, "sha256 header is 64 hex");
  assert.ok(calls[0].init.body instanceof Uint8Array);
});

test("attachments.upload honors explicit name/sha/contentType", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ ok: true }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("o/r").attachments.upload(new Uint8Array([1, 2, 3]), {
    name: "a](b).png",
    sha256: "0".repeat(64),
    contentType: "image/png",
  });
  assert.equal(calls[0].url, `${BASE}/o/r/api/attachments?name=${encodeURIComponent("a](b).png")}`);
  assert.equal(calls[0].init.headers["X-Walgit-Attachment-Sha256"], "0".repeat(64));
  assert.equal(calls[0].init.headers["Content-Type"], "image/png");
});

test("attachments.upload defaults the name when the blob is anonymous", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ ok: true }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("o/r").attachments.upload(new Uint8Array([1]));
  assert.equal(calls[0].url, `${BASE}/o/r/api/attachments?name=image`);
});

test("attachments.upload omits the sha header without subtle crypto (S4)", async () => {
  const { fetch, calls } = fakeFetch(() => jsonResponse({ ok: true }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  const realCrypto = globalThis.crypto;
  try {
    Object.defineProperty(globalThis, "crypto", { value: undefined, configurable: true });
    await client.repo("o/r").attachments.upload(new Uint8Array([9, 9]));
  } finally {
    Object.defineProperty(globalThis, "crypto", { value: realCrypto, configurable: true });
  }
  assert.ok(!("X-Walgit-Attachment-Sha256" in calls[0].init.headers), "no sha header without subtle");
});

test("attachments.upload rejects unusable data", async () => {
  const { fetch } = fakeFetch(() => jsonResponse({ ok: true }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await assert.rejects(() => client.repo("o/r").attachments.upload(42), /bytes, a string, or a Blob/);
});
