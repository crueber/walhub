// web/test/unit/access-save.test.js — Forgejo #391: the shared
// visibility save path (lib/accessSave.js). Failure paths (403/409 →
// reseed + specific error), fresh-version acquisition, and the single
// 409 retry. The component-level reseed (select ← server truth) calls
// reseedVisibility, covered here against stub repo clients.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  friendlyAccessError,
  saveVisibilityOnly,
  reseedVisibility,
} from "../../src/lib/accessSave.js";

function httpError(status, message) {
  const e = new Error(message);
  e.status = status;
  return e;
}

function stubRepo({ versions, putImpl }) {
  let gets = 0;
  const puts = [];
  return {
    stats: () => ({ gets, puts }),
    access: {
      get: async () => {
        const doc = versions[Math.min(gets++, versions.length - 1)];
        if (doc instanceof Error) throw doc;
        return doc;
      },
      put: async (body) => {
        puts.push(body);
        if (putImpl) return putImpl(body, puts.length);
        return { version: body.version + 1 };
      },
    },
  };
}

const BINDINGS = [{ subject: "user:jane@example.com", role: "admin" }];

test("success uses a fresh version and preserves bindings", async () => {
  const repo = stubRepo({
    versions: [{ version: 3, visibility: "public", role_bindings: BINDINGS }],
  });
  const next = await saveVisibilityOnly(repo, "private");
  assert.equal(next.version, 4);
  const { gets, puts } = repo.stats();
  assert.equal(gets, 1);
  assert.deepEqual(puts, [{ version: 3, visibility: "private", role_bindings: BINDINGS }]);
});

test("409 retries once on a freshly re-read version, then succeeds", async () => {
  const repo = stubRepo({
    versions: [
      { version: 3, visibility: "public", role_bindings: BINDINGS },
      { version: 4, visibility: "private", role_bindings: BINDINGS },
    ],
    putImpl: (body, n) => {
      if (n === 1) throw httpError(409, "access.json changed under you; reload");
      return { version: 5 };
    },
  });
  const next = await saveVisibilityOnly(repo, "private");
  assert.equal(next.version, 5);
  const { gets, puts } = repo.stats();
  assert.equal(gets, 2);
  assert.deepEqual(puts.map((p) => p.version), [3, 4]);
});

test("409 twice throws the conflict (loud, never silent)", async () => {
  const repo = stubRepo({
    versions: [{ version: 3, visibility: "public", role_bindings: [] }],
    putImpl: () => {
      throw httpError(409, "access.json changed under you; reload");
    },
  });
  await assert.rejects(saveVisibilityOnly(repo, "private"), (e) => e.status === 409);
  assert.equal(repo.stats().puts.length, 2);
});

test("403 does not retry (one PUT, caller reseeds + notes)", async () => {
  const repo = stubRepo({
    versions: [{ version: 3, visibility: "public", role_bindings: [] }],
    putImpl: () => {
      throw httpError(403, "admin role required");
    },
  });
  await assert.rejects(saveVisibilityOnly(repo, "private"), (e) => e.status === 403);
  assert.equal(repo.stats().puts.length, 1);
});

test("invalid spelling throws before any GET", async () => {
  const repo = stubRepo({ versions: [{ version: 0 }] });
  await assert.rejects(saveVisibilityOnly(repo, "hidden"), /visibility must be/);
  assert.equal(repo.stats().gets, 0);
});

test("friendlyAccessError names the specific cause", () => {
  assert.equal(friendlyAccessError(httpError(409, "x")), "changed under you — reloaded the latest version");
  assert.equal(friendlyAccessError(httpError(403, "x")), "admin role required to change access");
  assert.equal(friendlyAccessError(httpError(401, "x")), "sign in to view access");
  assert.equal(friendlyAccessError(httpError(500, "boom")), "boom");
  assert.equal(friendlyAccessError(null), "");
});

test("reseedVisibility returns server truth, null when unreadable", async () => {
  const ok = stubRepo({ versions: [{ version: 2, visibility: "private" }] });
  assert.equal(await reseedVisibility(ok), "private");
  const denied = stubRepo({ versions: [httpError(403, "nope")] });
  assert.equal(await reseedVisibility(denied), null);
  const weird = stubRepo({ versions: [{ version: 2, visibility: "hidden" }] });
  assert.equal(await reseedVisibility(weird), null);
});
