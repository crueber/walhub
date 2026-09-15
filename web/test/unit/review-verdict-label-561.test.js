// web/test/unit/review-verdict-label-561.test.js — Forgejo #561: the PR
// page rendered review/decision statuses as raw wire values
// (APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED, …). Display-only fix:
// one shared reviewVerdictLabel(state) in web/src/lib/pull-state.js maps
// the five known wire values to human labels at the three user-facing
// render sites in Pull.jsx (summary-bar decision badge, reviewer chips,
// review card chips). The wire contract (internal/review/model.go) is
// untouched — option values, chip-class mapping, Show guards, title
// attributes, and API payloads stay byte-identical.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import { reviewVerdictLabel } from "../../src/lib/pull-state.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const PULL = srcOf("../../src/pages/Pull.jsx");
const LIB = srcOf("../../src/lib/pull-state.js");

test("known wire values map to display labels", () => {
  assert.equal(reviewVerdictLabel("APPROVED"), "Approved");
  assert.equal(reviewVerdictLabel("CHANGES_REQUESTED"), "Changes requested");
  assert.equal(reviewVerdictLabel("COMMENTED"), "Commented");
  assert.equal(reviewVerdictLabel("REVIEW_REQUIRED"), "Review required");
  assert.equal(reviewVerdictLabel("DISMISSED"), "Dismissed");
});

test("unknown values pass through as-is, missing reads Unknown", () => {
  assert.equal(reviewVerdictLabel("SOME_FUTURE_STATE"), "SOME_FUTURE_STATE");
  assert.equal(reviewVerdictLabel(null), "Unknown");
  assert.equal(reviewVerdictLabel(undefined), "Unknown");
  assert.equal(reviewVerdictLabel(""), "Unknown");
});

test("exactly one label mapping lives in pull-state.js", () => {
  const defs = LIB.match(/function reviewVerdictLabel\(/g) ?? [];
  assert.equal(defs.length, 1, "exactly one mapping definition");
});

test("all three render sites use the helper", () => {
  assert.ok(
    PULL.includes('{reviewVerdictLabel(summary()?.decision ?? "REVIEW_REQUIRED")}'),
    "summary-bar decision badge renders the label",
  );
  assert.ok(PULL.includes("{who} · {reviewVerdictLabel(r.state)}"), "reviewer chips render the label");
  assert.ok(
    PULL.includes("reviewVerdictLabel(rv.state)"),
    "review card chips render the label on the non-dismissed branch",
  );
});

test("no raw wire value renders at a user-facing site", () => {
  assert.ok(!PULL.includes("{summary()?.decision ?? "), "decision badge no longer renders raw");
  assert.ok(!PULL.includes("{who} · {r.state}"), "reviewer chips no longer render raw");
  assert.ok(!PULL.includes(": rv.state}</span>"), "review cards no longer render raw");
});

test("wire values stay byte-identical (values, comparisons, guards, payloads)", () => {
  assert.ok(
    PULL.includes('value="COMMENTED"') &&
      PULL.includes('value="APPROVED"') &&
      PULL.includes('value="CHANGES_REQUESTED"'),
    "FinishReview option values intact",
  );
  assert.ok(PULL.includes("state: getVerdict()"), "review submit payload intact");
  assert.ok(PULL.includes('r.state === "APPROVED"'), "stale Show guard still keyed on the wire value");
  assert.ok(PULL.includes('reviewVerdictChip(summary()?.decision ?? "REVIEW_REQUIRED")'), "chip-class mapping still keyed on wire");
  assert.ok(PULL.includes("reviewVerdictChip(r.state)"), "reviewer chip classes still keyed on wire");
  assert.ok(PULL.includes("reviewVerdictChip(rv.state ?? rv.kind)"), "review card chip classes still keyed on wire");
});
