// web/test/unit/pull-diff-520.test.js — issue #520: the PR diff endpoint
// answers `text/plain`, but the SDK routed every non-SSE response through
// the JSON path — a real patch threw `invalid JSON`, and an empty patch
// (merged PR, base == head) resolved to null, so both consumers crashed on
// `res.patch` and the pages sat on "loading diff…" with a lying Files (0).
// The fix seam is the SDK call (raw: true); the pages normalize null-safe,
// count honestly, and render an error state + Retry instead of an eternal
// spinner. These tests pin the SDK routing, the pure normalization, the
// invalidate-retry seam the Retry buttons drive, and the page wiring.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, textResponse } from "../helpers/fetch.js";
import { parsePatchFiles, normalizePatchBody } from "../../src/lib/diff.js";
import { prefetchData, invalidate } from "../../src/lib/data.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const BASE = "http://api.test";

const PATCH = `diff --git a/x.txt b/x.txt
index 1111111..2222222 100644
--- a/x.txt
+++ b/x.txt
@@ -1,3 +1,3 @@
 a
-b
+c
 d
`;

function textOk(body) {
  return new Response(body, {
    status: 200,
    headers: { "content-type": "text/plain; charset=utf-8" },
  });
}

const tick = (ms = 10) => new Promise((r) => setTimeout(r, ms));

function clientFor(handler) {
  const { fetch } = fakeFetch(handler);
  return new ReposClient({ base: BASE, fetch, token: "t" });
}

test("pulls.diff resolves a text/plain patch as a string (not invalid-JSON)", async () => {
  const client = clientFor(() => textOk(PATCH));
  const res = await client.repo("o/r").pulls.diff(7);
  assert.equal(typeof res, "string");
  assert.equal(res, PATCH);
});

test("pulls.diff resolves an empty patch body to '' (never null)", async () => {
  const client = clientFor(() => textOk(""));
  const res = await client.repo("o/r").pulls.diff(7);
  assert.equal(res, "");
});

test("pulls.diff still throws ReposError on a server error", async () => {
  const client = clientFor(() => textResponse("boom", 500));
  await assert.rejects(client.repo("o/r").pulls.diff(7), (err) => {
    assert.equal(err.name, "ReposError");
    assert.equal(err.status, 500);
    return true;
  });
});

test("normalizePatchBody: strings pass through, nulls become ''", () => {
  assert.equal(normalizePatchBody(PATCH), PATCH); // the live path post-fix
  assert.equal(normalizePatchBody(""), "");
  assert.equal(normalizePatchBody(null), ""); // the #520 crash input
  assert.equal(normalizePatchBody(undefined), "");
  assert.equal(normalizePatchBody({ patch: PATCH }), PATCH); // dead defensive branch
  assert.equal(normalizePatchBody({ diff: PATCH }), PATCH);
  assert.equal(normalizePatchBody({ patch: null, diff: PATCH }), PATCH);
  assert.equal(normalizePatchBody({}), "");
});

test("empty patch normalizes to a truthful zero-file view (not a spinner)", () => {
  const view = parsePatchFiles(normalizePatchBody(""));
  assert.deepEqual(view.files, []);
  const nulled = parsePatchFiles(normalizePatchBody(null));
  assert.deepEqual(nulled.files, []);
});

test("invalidate() re-runs the fetch — the seam the Retry buttons drive", async () => {
  let n = 0;
  const get = prefetchData("diff520:refetch", async () => `v${++n}`);
  await tick();
  assert.equal(get(), "v1");
  invalidate("diff520:refetch");
  await tick();
  assert.equal(get(), "v2");
  assert.equal(n, 2);
});

test("PullFiles wires the #520 contract: normalize + honest count + error + Retry", () => {
  const src = srcOf("../../src/pages/PullFiles.jsx");
  assert.match(src, /normalizePatchBody/, "null-safe normalization");
  assert.doesNotMatch(src, /res\.patch \?\? res\.diff/, "bare res.patch deref is gone");
  assert.match(src, /getDiffError/, "page-local error signal");
  assert.match(src, /invalidate\(key\(\)\)/, "Retry drives invalidate()");
  assert.match(src, /Retry/, "Retry control renders");
  assert.match(src, /failed to load/, "honest heading on error");
  assert.match(src, /empty diff/, "loaded-empty stays truthful");
  assert.doesNotMatch(
    src,
    /\(getView\(\)\?\.files \?\? \[\]\)\.length/,
    "heading never counts an unloaded view as (0)",
  );
});

test("Pull inline diff wires the same #520 contract", () => {
  const src = srcOf("../../src/pages/Pull.jsx");
  assert.match(src, /normalizePatchBody/, "null-safe normalization");
  assert.match(src, /getDiffError/, "page-local error signal");
  assert.match(src, /invalidate\(diffKey\(\)\)/, "Retry drives invalidate()");
  assert.match(src, /Retry/, "Retry control renders");
  assert.match(src, /empty diff/, "loaded-empty renders instead of a spinner");
  assert.doesNotMatch(
    src,
    /\(getDiff\(\)\?\.files \?\? \[\]\)\.length/,
    "heading never counts an unloaded view as (0)",
  );
});

test("Commit diff is null-safe the same way", () => {
  const src = srcOf("../../src/pages/Commit.jsx");
  assert.match(src, /normalizePatchBody\(data\)/, "null data safe");
  assert.doesNotMatch(src, /data\.patch \?\?/, "bare data.patch deref is gone");
});

test("SDK diff call reads the body as text", () => {
  const src = srcOf("../../sdk/src/pulls.js");
  assert.match(src, /diff: \(num, opts\) =>\s*\n?\s*client\._call\(p\(`\/pulls\/\$\{num\}\/diff`\), \{ method: "GET", raw: true/, "raw:true routes past the JSON path");
});
