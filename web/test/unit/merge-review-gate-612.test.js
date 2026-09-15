// web/test/unit/merge-review-gate-612.test.js — Forgejo #612:
// The UI treated ANY CHANGES_REQUESTED as merge-blocking (mergeState's
// review arm, the disabled button, the "blocked:" tooltip, the amber
// "changes requested" line) while the server merges it when no
// required-reviews policy rule exists (GitHub-like, no rule → no block —
// server behavior stays). Fix: the client gates the reviews block on
// rule presence — new pure requiredReviewsApplies(policy, baseRef) in
// pull-state.js (the #561/#588 pattern), computed in Pull.jsx off the
// same fetched policy the requiredChecks() walk reads, passed as a
// requiresReviews prop/signal into mergeState + blockers(). The #588
// "Changes requested" sidebar headline stays informational either way;
// the amber line + tooltip claim blocked only when actually blocking.
// Server behavior untouched; the rejected server-blocks-always
// alternative stays rejected per #586 Decision-1b.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import {
  requiredReviewsApplies,
  mergeabilityDisplay,
  MERGEABILITY_BAD_CLS,
} from "../../src/lib/pull-state.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const PULL = srcOf("../../src/pages/Pull.jsx");
const MERGEBOX = srcOf("../../src/components/MergeBox.jsx");
const LIB = srcOf("../../src/lib/pull-state.js");

const REV_RULE = (refs) => ({
  name: "pr-gate",
  ...(refs === undefined ? {} : { match: { refs } }),
  effect: { "required-reviews": { min_approvals: 1 } },
});
const PROTECT_RULE = {
  name: "protect",
  match: { refs: ["refs/heads/main"] },
  effect: { protect: { require_checks: ["ci"] } },
};

// --- Half 1: the policy-derived gate input (real behavior) ---

test("no policy means no gate: null/undefined/empty all read false", () => {
  for (const p of [null, undefined, {}, { rules: [] }, { version: 1 }]) {
    assert.equal(requiredReviewsApplies(p, "refs/heads/main"), false);
  }
});

test("rules without a required-reviews effect never gate", () => {
  assert.equal(requiredReviewsApplies({ rules: [PROTECT_RULE] }, "refs/heads/main"), false);
  assert.equal(
    requiredReviewsApplies({ rules: [{ name: "g", effect: { "required-reviews": null } }] }, "refs/heads/main"),
    false,
    "null effect value is absence, not presence",
  );
});

test("a matching required-reviews rule gates; a ref-mismatched one does not", () => {
  assert.equal(
    requiredReviewsApplies({ rules: [REV_RULE(["refs/heads/main"])] }, "refs/heads/main"),
    true,
  );
  assert.equal(
    requiredReviewsApplies({ rules: [REV_RULE(["refs/heads/main"])] }, "refs/heads/other"),
    false,
  );
});

test("empty/missing match.refs applies to all refs (the requiredChecks convention)", () => {
  assert.equal(requiredReviewsApplies({ rules: [REV_RULE([])] }, "refs/heads/main"), true);
  assert.equal(requiredReviewsApplies({ rules: [REV_RULE(undefined)] }, "refs/heads/main"), true);
  assert.equal(requiredReviewsApplies({ rules: [REV_RULE(["refs/heads/main"])] }, ""), false);
  assert.equal(requiredReviewsApplies({ rules: [REV_RULE([])] }, ""), true);
});

test("one matching rule among many suffices; baseRef missing matches nothing scoped", () => {
  const policy = { rules: [PROTECT_RULE, REV_RULE(["refs/heads/other"]), REV_RULE(["refs/heads/main"])] };
  assert.equal(requiredReviewsApplies(policy, "refs/heads/main"), true);
  assert.equal(requiredReviewsApplies({ rules: [PROTECT_RULE, REV_RULE(["refs/heads/other"])] }, "refs/heads/main"), false);
});

// --- Half 2: mergeState + blockers consume the gate (source pins) ---

test("mergeState review arm requires the gate (fail-closed unless explicitly false)", () => {
  assert.ok(
    MERGEBOX.includes('props.reviewDecision === "CHANGES_REQUESTED" && props.requiresReviews !== false'),
    "review blocks only when requiresReviews is not explicitly false",
  );
  assert.ok(MERGEBOX.includes("requiresReviews"), "the prop is threaded, not forked");
});

test("the page state() forwards requiresReviews (function-or-value, like its siblings)", () => {
  assert.ok(MERGEBOX.includes("requiresReviews:"), "state() passes the gate into mergeState");
  assert.ok(
    MERGEBOX.includes('typeof props.requiresReviews === "function" ? props.requiresReviews() : props.requiresReviews'),
    "getter-or-value unwrapping matches the checksBlockers()/reviewDecision() idiom",
  );
});

test("amber blockers() lists 'changes requested' only when actually blocking", () => {
  assert.ok(
    MERGEBOX.includes('props.reviewDecision?.() === "CHANGES_REQUESTED" && requiresReviews()'),
    "the amber entry follows the same gate",
  );
  assert.ok(MERGEBOX.includes("blocking merge: {blockers().join"), "amber line kept");
});

test("tooltip cannot claim blocked when not blocking (derives from blockers only)", () => {
  assert.ok(MERGEBOX.includes("blocked: ${b.join"), "disabled tooltip keeps machine wording");
  const tooltipStart = MERGEBOX.indexOf("const tooltip = () => {");
  assert.ok(tooltipStart > 0, "tooltip found");
  const tooltip = MERGEBOX.slice(tooltipStart, MERGEBOX.indexOf("};", tooltipStart));
  assert.ok(!tooltip.includes("CHANGES_REQUESTED"), "no separate review claim in the tooltip");
  assert.ok(tooltip.includes('"merge pull request"'), "unblocked falls through to the merge label");
});

test("enabled() gate + dirty/clean arms untouched (only the review arm changed)", () => {
  assert.ok(MERGEBOX.includes('state() === "mergeable" && canMerge()'), "merge-button enable logic untouched");
  assert.ok(MERGEBOX.includes('if (props.mergeable?.state === "dirty") return "blocked"'), "dirty arm intact");
  assert.ok(
    MERGEBOX.includes('props.mergeable?.state === "clean" || props.mergeable?.state === "behind"'),
    "clean/behind arm intact",
  );
  assert.ok(!MERGEBOX.includes('from "../lib/pull-state.js"'), "no helper import (#592 single-headline rule holds)");
});

// --- Page wiring: policy → gate → box; headline stays informational ---

test("Pull.jsx derives requiresReviews from the fetched policy + base ref and passes it", () => {
  assert.ok(PULL.includes("requiredReviewsApplies"), "page uses the shared helper, no inline re-implementation");
  assert.ok(PULL.includes("const requiresReviews = () => requiredReviewsApplies(getPolicy(), pr()?.base?.ref"), "gate reads policy + base ref");
  assert.ok(PULL.includes("requiresReviews={requiresReviews}"), "gate reaches the MergeBox");
});

test("sidebar headline still reads the raw decision (informational either way)", () => {
  assert.ok(PULL.includes("reviewDecision: summary()?.decision"), "headline input is the raw decision, ungated");
  assert.deepEqual(mergeabilityDisplay("blocked", { reviewDecision: "CHANGES_REQUESTED" }), {
    text: "Changes requested",
    cls: MERGEABILITY_BAD_CLS,
    sub: null,
  });
});

test("exactly one gate definition lives in pull-state.js", () => {
  const defs = LIB.match(/function requiredReviewsApplies\(/g) ?? [];
  assert.equal(defs.length, 1, "exactly one gate definition, no fork");
});

// --- Contracts: no new deps, Tailwind untouched, docs amended ---

test("no new runtime deps; no new CSS", () => {
  const pkg = JSON.parse(srcOf("../../package.json"));
  assert.equal(Object.keys(pkg.dependencies ?? {}).length, 4, "no new runtime deps");
  assert.ok(!MERGEBOX.includes("style="), "MergeBox carries no inline style");
});

test("law 12: the #612 amendment lands in docs/go/12_web_ui.md", () => {
  const doc = srcOf("../../../docs/go/12_web_ui.md");
  assert.ok(doc.includes("Forgejo #612"), "amendment present");
});
