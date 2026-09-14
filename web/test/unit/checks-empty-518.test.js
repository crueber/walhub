// web/test/unit/checks-empty-518.test.js — issue #518: a PR whose head
// sha has zero reported check contexts rendered the checks card with the
// wire pending state (amber pill, reads as CI-in-flight) and no reporting
// guidance. The wire contract (zero ⇒ pending) is load-bearing for the
// merge gate and stays untouched — this is a client display fix keyed off
// the combined view's statuses array. The rule lives in the pure
// lib/checks-empty.js module (no Solid, no DOM); the pages only wire it.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import { isZeroChecks, zeroChecksTitle, requiredCheckBlockers } from "../../src/lib/checks-empty.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const CHECKS = srcOf("../../src/pages/Checks.jsx");
const PULL = srcOf("../../src/pages/Pull.jsx");
const DETAIL = srcOf("../../src/pages/CheckDetail.jsx");

test("isZeroChecks: only an actually-empty statuses array is empty", () => {
  // Loading / unknown shapes must NOT claim the empty state (the caller
  // keeps the loading/real pill instead).
  assert.equal(isZeroChecks(null), false);
  assert.equal(isZeroChecks(undefined), false);
  assert.equal(isZeroChecks({}), false);
  assert.equal(isZeroChecks({ state: "pending" }), false);
  // The wire zero shape: pending state, empty statuses.
  assert.equal(isZeroChecks({ state: "pending", statuses: [], total_counts: {} }), true);
  assert.equal(isZeroChecks({ statuses: [] }), true);
});

test("isZeroChecks: one pending context is in-flight, never empty", () => {
  // The acceptance line: ≥1 context (any state, including pending) keeps
  // the real combined state — the amber pill stays meaningful.
  assert.equal(
    isZeroChecks({ state: "pending", statuses: [{ context: "ci/build", state: "pending" }] }),
    false,
  );
  assert.equal(
    isZeroChecks({ state: "success", statuses: [{ context: "ci/build", state: "success" }] }),
    false,
  );
  assert.equal(
    isZeroChecks({
      state: "failure",
      statuses: [
        { context: "a", state: "success" },
        { context: "b", state: "failure" },
      ],
    }),
    false,
  );
});

test("zeroChecksTitle: configured only when required is known-empty", () => {
  assert.equal(zeroChecksTitle([]), "No checks configured");
  assert.equal(zeroChecksTitle(["ci/build"]), "No checks reported yet");
  // CheckDetail fetches no policy — unknown required must not assert a config.
  assert.equal(zeroChecksTitle(undefined), "No checks reported yet");
  assert.equal(zeroChecksTitle(null), "No checks reported yet");
});

test("requiredCheckBlockers: zero-contexts + required reads <context> (missing)", () => {
  assert.deepEqual(requiredCheckBlockers([], []), []);
  assert.deepEqual(requiredCheckBlockers(["ci/build"], []), ["ci/build (missing)"]);
  assert.deepEqual(
    requiredCheckBlockers(["a", "b"], [{ context: "a", state: "success" }]),
    ["b (missing)"],
  );
  assert.deepEqual(requiredCheckBlockers(["a"], [{ context: "a", state: "pending" }]), ["a (pending)"]);
  assert.deepEqual(requiredCheckBlockers(["a"], [{ context: "a", state: "failure" }]), ["a (failure)"]);
  assert.deepEqual(requiredCheckBlockers(["a"], [{ context: "a", state: "success" }]), []);
});

test("Checks.jsx: CheckPill goes neutral on zero, keeps the real pill otherwise", () => {
  assert.ok(CHECKS.includes("isZeroChecks(getView())"), "pill keys off the statuses array, not the state string");
  assert.ok(CHECKS.includes("no checks"), "neutral pill label replaces the amber pending one");
  assert.ok(CHECKS.includes("checks: no contexts reported"), "neutral title replaces the pending title");
  assert.ok(CHECKS.includes("stateDot(getView().state)"), "non-empty shas keep the real combined pill");
});

test("Checks.jsx: ZeroChecksBlock carries reporting guidance, not new copy", () => {
  assert.ok(CHECKS.includes("ZeroChecksBlock"), "shared block exported for the PR card + detail page");
  assert.ok(CHECKS.includes("wct_"), "guidance names the CI token shape");
  assert.ok(CHECKS.includes("POST …/checks/statuses/"), "guidance names the report endpoint");
  assert.ok(CHECKS.includes('href="/api#checks-ci"'), "guidance links the existing reporting docs");
  assert.ok(CHECKS.includes("/checks"), "guidance links the checks page");
});

test("Pull.jsx: checks card swaps rows for the block on zero, blockers intact", () => {
  assert.ok(PULL.includes("ZeroChecksBlock"), "card renders the empty-state block");
  assert.ok(PULL.includes("required={requiredChecks()}"), "block titles configured vs reported from policy");
  assert.ok(PULL.includes("isZeroChecks(getCombined())"), "zero keys off the fetched combined view");
  assert.ok(
    PULL.includes("requiredCheckBlockers(requiredChecks(), getCombined()?.statuses)"),
    "blockers resolve through the pinned helper (missing vs failing intact)",
  );
  assert.ok(PULL.includes("blocking merge: {checksBlockers().join"), "merge-block line still surfaces alongside");
});

test("CheckDetail.jsx: same empty-state treatment, shared pill cache key", () => {
  assert.ok(DETAIL.includes("ZeroChecksBlock"), "detail page renders the block on zero");
  assert.ok(DETAIL.includes("isZeroChecks(getCombined())"), "detail zero keys off the combined view");
  assert.ok(
    DETAIL.includes("checks:${ctx.full}:${sha()}"),
    "detail reuses the CheckPill data key — one combined fetch, no extra request",
  );
});
