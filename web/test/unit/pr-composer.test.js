// web/test/unit/pr-composer.test.js — PR composer direction + endpoint
// state (Forgejo #328): label/direction mapping, wire stability, the
// repo→ref state machine, permission filtering, and the direction-mapped
// error copy. Pure `lib/pr-composer.js` plus source pins on PullNew.jsx
// (From/To labels, no base/head display copy, frozen wire keys kept).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import {
  FROM_LABEL,
  TO_LABEL,
  buildOpenCall,
  sameEndpoint,
  isCrossRepo,
  buildRepoOptions,
  withCurrent,
  filterReposByRole,
  applyEndpointAction,
  fmtEndpoint,
  openErrorMessage,
  previewErrorMessage,
} from "../../src/lib/pr-composer.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const pullNew = () =>
  fs.readFileSync(new URL("../../src/pages/PullNew.jsx", import.meta.url), "utf8");

test("labels read From/To with the head/base direction fixed", () => {
  assert.equal(FROM_LABEL, "From");
  assert.equal(TO_LABEL, "To");
});

test("same-repo open call keeps the frozen wire keys, no fork", () => {
  const { baseRepo, payload, cross } = buildOpenCall({
    fromRepo: "o/r",
    fromRef: "refs/heads/feature",
    toRepo: "o/r",
    toRef: "refs/heads/main",
    title: "t",
    body: "",
  });
  assert.equal(baseRepo, "o/r");
  assert.equal(cross, false);
  assert.deepEqual(payload, {
    title: "t",
    base_ref: "refs/heads/main",
    head_ref: "refs/heads/feature",
  });
  assert.ok(!("fork" in payload), "same-repo opens never send fork");
});

test("cross-repo open call targets the To repo and carries fork.repo", () => {
  const { baseRepo, payload, cross } = buildOpenCall({
    fromRepo: "me/r-fork",
    fromRef: "refs/heads/feature",
    toRepo: "o/r",
    toRef: "refs/heads/main",
    title: "t",
    body: "b",
  });
  assert.equal(baseRepo, "o/r");
  assert.equal(cross, true);
  assert.equal(payload.base_ref, "refs/heads/main");
  assert.equal(payload.head_ref, "refs/heads/feature");
  assert.deepEqual(payload.fork, { repo: "me/r-fork" });
  assert.equal(payload.body, "b");
});

test("same endpoint needs same repo AND same ref", () => {
  assert.equal(sameEndpoint("o/r", "refs/heads/main", "o/r", "refs/heads/main"), true);
  assert.equal(sameEndpoint("o/r", "refs/heads/a", "o/r", "refs/heads/b"), false);
  // A shared branch name across two repos is two endpoints, not one.
  assert.equal(sameEndpoint("o/r", "refs/heads/main", "me/r-fork", "refs/heads/main"), false);
  assert.equal(isCrossRepo("o/r", "me/r-fork"), true);
  assert.equal(isCrossRepo("o/r", "o/r"), false);
  assert.equal(isCrossRepo("", "o/r"), false);
});

test("repo options: current first, owner-prefixed, deduped", () => {
  assert.deepEqual(buildRepoOptions("o/r", "o", ["r", "other", "r", ""]), ["o/r", "o/other"]);
  assert.deepEqual(buildRepoOptions("o/r", "o", ["o/r"]), ["o/r"]);
  assert.deepEqual(buildRepoOptions("o/r", "o", null), ["o/r"]);
});

test("withCurrent keeps the selection choosable", () => {
  assert.deepEqual(withCurrent(["o/other"], "o/r"), ["o/r", "o/other"]);
  assert.deepEqual(withCurrent(["o/r", "o/other"], "o/r"), ["o/r", "o/other"]);
});

test("permission filter: To needs write, From needs any resolved role", () => {
  const entries = [
    { repo: "o/r", role: "write" },
    { repo: "o/docs", role: "read" },
    { repo: "o/secret", role: null },
    { repo: "o/admin", role: "admin" },
  ];
  assert.deepEqual(filterReposByRole(entries, "write"), ["o/r", "o/admin"]);
  assert.deepEqual(filterReposByRole(entries, "read"), ["o/r", "o/docs", "o/admin"]);
});

test("endpoint state machine: repo change re-keys (ref resets)", () => {
  const start = { repo: "o/r", ref: "refs/heads/feature" };
  assert.deepEqual(applyEndpointAction(start, { type: "select-repo", repo: "me/r-fork" }), {
    repo: "me/r-fork",
    ref: "",
  });
  assert.deepEqual(
    applyEndpointAction(start, { type: "select-repo", repo: "o/r" }),
    start,
    "re-picking the same repo keeps the ref",
  );
  assert.deepEqual(applyEndpointAction(start, { type: "select-ref", ref: "refs/heads/other" }), {
    repo: "o/r",
    ref: "refs/heads/other",
  });
  assert.deepEqual(applyEndpointAction({ repo: "o/r", ref: "x" }, { type: "noop" }), {
    repo: "o/r",
    ref: "x",
  });
});

test("endpoint display: short ref locally, repo@short across repos", () => {
  assert.equal(fmtEndpoint("o/r", "refs/heads/main", "o/r"), "main");
  assert.equal(fmtEndpoint("me/r-fork", "refs/heads/feature", "o/r"), "me/r-fork@feature");
});

test("unreachable-head 422 maps to a specific push-first error", () => {
  const msg = openErrorMessage({ status: 422, message: "head commit not reachable — push first" });
  assert.match(msg, /From branch/);
  assert.match(msg, /push the From branch first/);
  assert.ok(!/422/.test(msg), "no bare status code");
});

test("dup-pair 409 and unknown revisions map specifically", () => {
  assert.match(
    openErrorMessage({ status: 409, message: "open pull request #3 already pairs a and b" }),
    /already compares this From and To/,
  );
  assert.match(
    openErrorMessage({ status: 404, message: 'unknown revision "refs/heads/nope"' }),
    /Unknown revision/,
  );
  assert.match(openErrorMessage(new Error("boom")), /Couldn't open: boom/);
});

test("preview errors name the failing side", () => {
  assert.equal(previewErrorMessage("From", { status: 404, message: "x" }), "No such From branch — check the name.");
  assert.equal(previewErrorMessage("To", { status: 404, message: "x" }), "No such To branch — check the name.");
  assert.match(previewErrorMessage("From", new Error("down")), /Couldn't compare: down/);
});

test("composer speaks From/To in the UI, never base/head display copy", () => {
  const s = pullNew();
  assert.ok(s.includes("FROM_LABEL"), "From label rendered via FROM_LABEL");
  assert.ok(s.includes("TO_LABEL"), "To label rendered via TO_LABEL");
  assert.ok(s.includes("FROM_LABEL} repository"), "From repo dropdown labelled");
  assert.ok(s.includes("TO_LABEL} repository"), "To repo dropdown labelled");
  for (const banned of ["Base ref", "Head ref", "base ref", "head ref", "Base and head", "base and head", "Swap base and head"]) {
    assert.ok(!s.includes(banned), `no display copy "${banned}"`);
  }
});

test("PR page pending-head copy speaks From, never head/base ref", () => {
  const s = fs.readFileSync(new URL("../../src/pages/Pull.jsx", import.meta.url), "utf8");
  assert.ok(s.includes("From branch pending"), "pending-head line uses From terminology");
  for (const banned of ["head ref pending", "Head ref", "Base ref", "base ref", "head ref"]) {
    assert.ok(!s.includes(banned), `no display copy "${banned}"`);
  }
});

test("composer keeps the frozen wire keys and deep-link params", () => {
  const s = pullNew();
  assert.ok(s.includes("base_ref"), "wire base_ref kept");
  assert.ok(s.includes("head_ref"), "wire head_ref kept");
  assert.ok(s.includes("search.base"), "?base= prefill kept");
  assert.ok(s.includes("search.head"), "?head= prefill kept");
});
