// web/test/unit/owners.test.js — owners (/) page helpers (issue #117):
// newest-first ordering proxy + cap slicing.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  MAX_OWNERS,
  MAX_REPOS_PER_OWNER,
  newestFirst,
  orderByActivity,
  orderOwnersByActivity,
  ownerActivity,
  pageSlice,
} from "../../src/lib/owners.js";

test("caps are sane documented defaults", () => {
  assert.equal(MAX_OWNERS, 50);
  assert.equal(MAX_REPOS_PER_OWNER, 10);
});

test("newestFirst reverses server (ascending) order without mutating", () => {
  const server = ["acme", "demo", "jane"];
  const out = newestFirst(server);
  assert.deepEqual(out, ["jane", "demo", "acme"]);
  assert.deepEqual(server, ["acme", "demo", "jane"]); // input untouched
  assert.deepEqual(newestFirst([]), []);
});

test("newestFirst treats non-array input as empty", () => {
  assert.deepEqual(newestFirst(undefined), []);
  assert.deepEqual(newestFirst(null), []);
  assert.deepEqual(newestFirst("acme"), []);
});

test("pageSlice splits shown/extra at the cap", () => {
  const names = ["a", "b", "c"];
  assert.deepEqual(pageSlice(names, 10), { shown: ["a", "b", "c"], extra: 0 });
  assert.deepEqual(pageSlice(names, 2), { shown: ["a", "b"], extra: 1 });
  assert.deepEqual(pageSlice(names, 3), { shown: ["a", "b", "c"], extra: 0 });
});

test("pageSlice defaults cover the owners-page composition", () => {
  const owners = Array.from({ length: 60 }, (_, i) => `o${i}`);
  const { shown, extra } = pageSlice(newestFirst(owners), MAX_OWNERS);
  assert.equal(shown.length, 50);
  assert.equal(extra, 10);
  assert.equal(shown[0], "o59"); // newest-first survives the cap
  const repos = Array.from({ length: 12 }, (_, i) => `r${i}`);
  const rp = pageSlice(newestFirst(repos), MAX_REPOS_PER_OWNER);
  assert.equal(rp.shown.length, 10);
  assert.equal(rp.extra, 2);
});

test("pageSlice non-array input behaves as an empty list", () => {
  assert.deepEqual(pageSlice(undefined, 10), { shown: [], extra: 0 });
  assert.deepEqual(pageSlice(null, 10), { shown: [], extra: 0 });
  assert.deepEqual(pageSlice("a", 10), { shown: [], extra: 0 });
});

test("pageSlice non-positive or non-finite limits show none, count all as extra", () => {
  for (const limit of [0, -1, NaN, Infinity, "x"]) {
    const { shown, extra } = pageSlice(["a", "b"], limit);
    assert.deepEqual(shown, []);
    assert.equal(extra, 2);
  }
  assert.equal(pageSlice(["a", "b", "c"], 1.9).shown.length, 1); // floors
});

// Forgejo #247: server-provided most-recent-commit order, stabilized client-side.
test("orderByActivity sorts newest commit first, unknowns last", () => {
  const rows = [
    { name: "b", last_commit_time: "2026-09-09T12:00:00Z" },
    { name: "a", last_commit_time: "2026-09-10T12:00:00Z" },
    { name: "c" }, // unknown: always last
    { name: "d", last_commit_time: "2026-09-10T12:00:00Z" }, // tie with a
    { name: "e", last_commit_time: null }, // explicit null: unknown too
  ];
  const out = orderByActivity(rows);
  assert.deepEqual(out.map((r) => r.name), ["a", "d", "b", "c", "e"]);
  assert.deepEqual(rows.map((r) => r.name), ["b", "a", "c", "d", "e"]); // input untouched
});

test("orderByActivity treats non-array input as empty, missing times as names", () => {
  assert.deepEqual(orderByActivity(undefined), []);
  assert.deepEqual(orderByActivity(null), []);
  assert.deepEqual(
    orderByActivity([{ name: "b" }, { name: "a" }]).map((r) => r.name),
    ["a", "b"],
  );
});

test("orderByActivity breaks time ties on (owner, name) like the server", () => {
  const t = "2026-09-10T12:00:00Z";
  const rows = [
    { owner: "b", name: "a", last_commit_time: t },
    { owner: "a", name: "b", last_commit_time: t },
    { owner: "a", name: "a", last_commit_time: t },
  ];
  const out = orderByActivity(rows);
  assert.deepEqual(
    out.map((r) => `${r.owner}/${r.name}`),
    ["a/a", "a/b", "b/a"],
  );
});

test("orderByActivity composes with pageSlice (slice-after-server-sort)", () => {
  const rows = Array.from({ length: 12 }, (_, i) => ({
    name: `r${i}`,
    last_commit_time: `2026-09-${String(i + 1).padStart(2, "0")}T12:00:00Z`,
  }));
  const { shown, extra } = pageSlice(orderByActivity(rows), MAX_REPOS_PER_OWNER);
  assert.equal(shown.length, 10);
  assert.equal(extra, 2);
  assert.equal(shown[0].name, "r11"); // newest commit survives the cap
});

// Forgejo #283: owner sections ordered by most-recent-commit repo.
test("ownerActivity returns the max last_commit_time, null when none known", () => {
  const doc = {
    repos: [
      { name: "b", last_commit_time: "2026-09-09T12:00:00Z" },
      { name: "a", last_commit_time: "2026-09-10T12:00:00Z" },
      { name: "c" }, // unknown: skipped
      { name: "e", last_commit_time: null }, // explicit null: skipped
    ],
  };
  assert.equal(ownerActivity(doc), "2026-09-10T12:00:00Z");
  assert.equal(ownerActivity(doc.repos), "2026-09-10T12:00:00Z"); // bare rows accepted
  assert.equal(ownerActivity({ repos: [{ name: "x" }] }), null);
  assert.equal(ownerActivity({ repos: [] }), null);
  assert.equal(ownerActivity(undefined), null);
  assert.equal(ownerActivity(null), null);
  assert.equal(ownerActivity({}), null);
});

test("orderOwnersByActivity sorts newest owner first, unknowns last", () => {
  const names = ["acme", "demo", "jane", "ghost"];
  const activity = {
    acme: "2026-09-09T12:00:00Z",
    demo: "2026-09-10T12:00:00Z",
    // jane: fetch pending (missing) — last; ghost: settled, no commits (null) — last
    ghost: null,
  };
  assert.deepEqual(orderOwnersByActivity(names, activity), ["demo", "acme", "ghost", "jane"]);
  assert.deepEqual(names, ["acme", "demo", "jane", "ghost"]); // input untouched
});

test("orderOwnersByActivity breaks time ties on name, all-unknown is name order", () => {
  const t = "2026-09-10T12:00:00Z";
  assert.deepEqual(
    orderOwnersByActivity(["b", "a", "c"], { b: t, a: t, c: t }),
    ["a", "b", "c"],
  );
  assert.deepEqual(orderOwnersByActivity(["jane", "demo", "acme"], {}), ["acme", "demo", "jane"]);
  assert.deepEqual(orderOwnersByActivity(["solo"], { solo: t }), ["solo"]);
  assert.deepEqual(orderOwnersByActivity([], {}), []);
});

test("orderOwnersByActivity treats non-array/non-object input as empty", () => {
  assert.deepEqual(orderOwnersByActivity(undefined, {}), []);
  assert.deepEqual(orderOwnersByActivity(null, {}), []);
  assert.deepEqual(orderOwnersByActivity("acme", {}), []);
  assert.deepEqual(orderOwnersByActivity(["b", "a"], undefined), ["a", "b"]);
  assert.deepEqual(orderOwnersByActivity(["b", "a"], null), ["a", "b"]);
});

test("orderOwnersByActivity composes with pageSlice (slice-after-rank)", () => {
  const owners = Array.from({ length: 12 }, (_, i) => `o${i}`);
  const activity = Object.fromEntries(
    owners.map((o, i) => [o, `2026-09-${String(i + 1).padStart(2, "0")}T12:00:00Z`]),
  );
  const { shown, extra } = pageSlice(orderOwnersByActivity(owners, activity), 10);
  assert.equal(shown.length, 10);
  assert.equal(extra, 2);
  assert.equal(shown[0], "o11"); // most recently committed owner survives the cap
});
