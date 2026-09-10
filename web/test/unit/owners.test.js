// web/test/unit/owners.test.js — owners (/) page helpers (issue #117):
// newest-first ordering proxy + cap slicing.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  MAX_OWNERS,
  MAX_REPOS_PER_OWNER,
  activeOwnerNames,
  hasKnownActivity,
  instanceRepoTotal,
  newestFirst,
  orderByActivity,
  orderOwnersByActivity,
  ownerActivity,
  pageSlice,
} from "../../src/lib/owners.js";

test("caps are sane documented defaults", () => {
  assert.equal(MAX_OWNERS, 5); // Forgejo #295: top-5 most-active owners
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
  assert.equal(shown.length, 5);
  assert.equal(extra, 55);
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

test("hasKnownActivity is true only when a real time string is present", () => {
  assert.equal(hasKnownActivity({}), false);
  assert.equal(hasKnownActivity({ bob: null, amy: null }), false);
  assert.equal(hasKnownActivity({ bob: undefined }), false);
  assert.equal(hasKnownActivity(undefined), false);
  assert.equal(hasKnownActivity(null), false);
  assert.equal(hasKnownActivity("bob"), false);
  assert.equal(hasKnownActivity({ bob: "2026-09-10T12:00:00Z", amy: null }), true);
});

test("server order survives first paint: gate re-rank until a known time lands (#283 follow-up)", () => {
  // Mirrors Owners.jsx: server-ordered names pass through untouched while the
  // activity map holds no known times, so the MAX_OWNERS slice keeps the
  // server's top-N (an active owner past the name cap still surfaces).
  const server = ["bob", "alice", "amy", "zed"]; // sort=activity&order=desc
  const rank = (names, activity) =>
    hasKnownActivity(activity) ? orderOwnersByActivity(names, activity) : names;
  assert.deepEqual(rank(server, {}), server);
  assert.deepEqual(pageSlice(rank(server, {}), 2).shown, ["bob", "alice"]);
  // Once a section reports, the client re-rank applies in the same total order
  // (known first, the rest — pending or settled-null — trailing by name).
  assert.deepEqual(
    rank(server, { alice: "2026-09-11T12:00:00Z" }),
    ["alice", "amy", "bob", "zed"],
  );
});

// Forgejo #295: /explore shows the top-5 most-active owners over the #283
// owners/detailed rows (server-ranked, unknowns already last).
test("activeOwnerNames keeps server order, drops owners without activity", () => {
  const rows = [
    { name: "bob", last_commit_time: "2026-09-10T12:00:00Z" },
    { name: "ghost", last_commit_time: null }, // no commits: not active
    { name: "alice", last_commit_time: "2026-09-09T12:00:00Z" },
    { name: "pending" }, // missing time: not active
    { name: "zed", last_commit_time: "2026-09-11T12:00:00Z" },
  ];
  assert.deepEqual(activeOwnerNames(rows), ["bob", "alice", "zed"]);
  assert.deepEqual(rows.length, 5); // input untouched
});

test("activeOwnerNames treats non-array input as empty, skips nameless rows", () => {
  assert.deepEqual(activeOwnerNames(undefined), []);
  assert.deepEqual(activeOwnerNames(null), []);
  assert.deepEqual(activeOwnerNames("bob"), []);
  assert.deepEqual(activeOwnerNames({ owners: [] }), []);
  assert.deepEqual(
    activeOwnerNames([{ last_commit_time: "2026-09-10T12:00:00Z" }, { name: "bob", last_commit_time: "2026-09-10T12:00:00Z" }]),
    ["bob"],
  );
});

test("explore composition: active filter, top-5 slice, uncapped owner total (#295)", () => {
  // Mirrors Owners.jsx: the owners/detailed payload arrives server-ranked
  // (sort=activity&order=desc, unknowns last); the page filters to active
  // owners BEFORE the MAX_OWNERS slice, and the owner total is the
  // payload's row count — never the slice.
  const payload = {
    owners: [
      { name: "o1", last_commit_time: "2026-09-10T12:00:00Z" },
      { name: "o2", last_commit_time: "2026-09-09T12:00:00Z" },
      { name: "o3", last_commit_time: "2026-09-08T12:00:00Z" },
      { name: "o4", last_commit_time: "2026-09-07T12:00:00Z" },
      { name: "o5", last_commit_time: "2026-09-06T12:00:00Z" },
      { name: "o6", last_commit_time: "2026-09-05T12:00:00Z" },
      { name: "o7", last_commit_time: "2026-09-04T12:00:00Z" },
      { name: "ghost", last_commit_time: null }, // opposite of active: never shown
    ],
  };
  const totalOwners = payload.owners.length;
  assert.equal(totalOwners, 8); // true total, uncapped
  const ranked = activeOwnerNames(payload.owners);
  assert.deepEqual(ranked, ["o1", "o2", "o3", "o4", "o5", "o6", "o7"]);
  const { shown, extra } = pageSlice(ranked, MAX_OWNERS);
  assert.deepEqual(shown, ["o1", "o2", "o3", "o4", "o5"]);
  assert.equal(extra, 2); // overflow counts active owners only
});

test("explore composition: fewer than 5 active owners renders fewer sections (#295)", () => {
  const payload = {
    owners: [
      { name: "o1", last_commit_time: "2026-09-10T12:00:00Z" },
      { name: "o2", last_commit_time: "2026-09-09T12:00:00Z" },
      { name: "ghost", last_commit_time: null },
    ],
  };
  const { shown, extra } = pageSlice(activeOwnerNames(payload.owners), MAX_OWNERS);
  assert.deepEqual(shown, ["o1", "o2"]); // no filler
  assert.equal(extra, 0);
  assert.equal(payload.owners.length, 3); // total still counts every owner
});

test("explore cold load costs 1 listing + MAX_OWNERS section fetches (#295)", () => {
  // Headless fetch-count mirror of Owners.jsx: one owners/detailed listing
  // for the ranked rows + totals, then exactly one detailed fetch per
  // SHOWN section (the top-5 slice) — never one per owner on the instance.
  let fetches = 0;
  const stubOwners = (query) => {
    fetches += 1; // the listing
    assert.deepEqual(query, { sort: "activity", order: "desc" });
    return Promise.resolve({
      owners: Array.from({ length: 60 }, (_, i) => ({
        name: `o${i}`,
        last_commit_time: `2026-09-${String((i % 28) + 1).padStart(2, "0")}T12:00:00Z`,
      })),
    });
  };
  const stubSection = () => {
    fetches += 1; // one per mounted section
    return Promise.resolve({ repos: [] });
  };
  return stubOwners({ sort: "activity", order: "desc" }).then((doc) => {
    assert.equal(fetches, 1);
    const { shown } = pageSlice(activeOwnerNames(doc.owners), MAX_OWNERS);
    assert.equal(shown.length, 5);
    return Promise.all(shown.map(stubSection)).then(() => {
      assert.equal(fetches, 1 + MAX_OWNERS); // 6 cold reads, not ~61
    });
  });
});

// Forgejo #307: the instance repo total is the sum of the served repo_count
// fields over the UNCAPPED owners/detailed payload — never the top-5 slice,
// never a per-owner listing walk.
test("instanceRepoTotal sums served repo_counts over the uncapped payload", () => {
  const payload = {
    owners: [
      { name: "o1", repo_count: 3, last_commit_time: "2026-09-10T12:00:00Z" },
      { name: "o2", repo_count: 1, last_commit_time: "2026-09-09T12:00:00Z" },
      { name: "o3", repo_count: 2, last_commit_time: "2026-09-08T12:00:00Z" },
      { name: "o4", repo_count: 1, last_commit_time: "2026-09-07T12:00:00Z" },
      { name: "o5", repo_count: 1, last_commit_time: "2026-09-06T12:00:00Z" },
      { name: "o6", repo_count: 4, last_commit_time: "2026-09-05T12:00:00Z" },
      { name: "ghost", repo_count: 1, last_commit_time: null }, // inactive, still counted
    ],
  };
  assert.equal(instanceRepoTotal(payload.owners), 13); // true total, uncapped
  // The top-5 slice's capped sum (8) is NOT the total — the page must sum
  // the payload rows, not the shown sections.
  const { shown } = pageSlice(activeOwnerNames(payload.owners), MAX_OWNERS);
  assert.equal(shown.length, 5);
  const cappedSum = payload.owners
    .filter((r) => shown.includes(r.name))
    .reduce((s, r) => s + r.repo_count, 0);
  assert.equal(cappedSum, 8);
  assert.notEqual(cappedSum, instanceRepoTotal(payload.owners));
});

test("instanceRepoTotal tolerates missing counts and non-array input", () => {
  assert.equal(instanceRepoTotal(undefined), 0);
  assert.equal(instanceRepoTotal(null), 0);
  assert.equal(instanceRepoTotal("o1"), 0);
  assert.equal(instanceRepoTotal([]), 0);
  // Older servers without the rail contribute 0 rather than NaN.
  assert.equal(
    instanceRepoTotal([
      { name: "a", repo_count: 2 },
      { name: "b" },
      { name: "c", repo_count: null },
      { name: "d", repo_count: "3" },
    ]),
    2,
  );
});
