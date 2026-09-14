import { test } from "node:test";
import assert from "node:assert/strict";

import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse, textResponse } from "../helpers/fetch.js";

const BASE = "http://api.test";

/**
 * Forgejo #502: `_call` accepts `noPopupAuth: true` — the anonymous
 * write-interstitial flow opts write calls out of the 401→popup retry so a
 * server 401 surfaces as a ReposError (the page routes it to
 * /login-required) instead of opening the sign-in popup and re-running the
 * write. Default path (no flag) keeps the popup retry byte-identical.
 */

function client401Then(okBody, { authenticate } = {}) {
  let n = 0;
  const { fetch, calls } = fakeFetch(() => {
    n += 1;
    if (n === 1) return textResponse("authentication required", 401);
    return jsonResponse(okBody);
  });
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  let popupCalls = 0;
  client.authenticate = async () => {
    popupCalls += 1;
    if (authenticate) await authenticate();
  };
  return { client, calls, popupCalls: () => popupCalls };
}

test("noPopupAuth: 401 surfaces without popup and without retry", async () => {
  const { client, calls, popupCalls } = client401Then({ stars: 1 });
  await assert.rejects(() => client.repo("o/r").star.set({ noPopupAuth: true }), (err) => {
    assert.equal(err.status, 401);
    assert.equal(err.unauthorized, true);
    return true;
  });
  assert.equal(calls.length, 1, "no retry fetch");
  assert.equal(popupCalls(), 0, "popup auth must not fire");
});

test("default path still popup-retries the 401", async () => {
  const { client, calls, popupCalls } = client401Then({ stars: 2 });
  const out = await client.repo("o/r").star.set();
  assert.equal(out.stars, 2, "retried response wins");
  assert.equal(calls.length, 2, "one retry fetch");
  assert.equal(popupCalls(), 1, "popup auth fires once");
});

test("noPopupAuth threads through other writers (watch set)", async () => {
  const { client, calls, popupCalls } = client401Then({ watching: true });
  await assert.rejects(() => client.repo("o/r").watch.set(true, { noPopupAuth: true }), (err) => {
    assert.equal(err.status, 401);
    return true;
  });
  assert.equal(calls.length, 1);
  assert.equal(popupCalls(), 0);
});

test("noPopupAuth threads through issue comments", async () => {
  const { client, calls, popupCalls } = client401Then({ event: 1 });
  await assert.rejects(
    () => client.repo("o/r").issues.comment(3, "hi", { noPopupAuth: true }),
    (err) => {
      assert.equal(err.status, 401);
      return true;
    },
  );
  assert.equal(calls.length, 1);
  assert.equal(popupCalls(), 0);
});
