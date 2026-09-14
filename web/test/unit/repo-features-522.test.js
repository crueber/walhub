// web/test/unit/repo-features-522.test.js — Forgejo #522: the pure
// feature-flag helpers (web/src/lib/repoFeatures.js). Fail-open is the
// whole contract — only an explicit `false` disables — and the TOML
// helpers round-trip the `[features]` section without touching the rest
// of the settings doc.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  FEATURE_KEYS,
  allFeatures,
  resolveFeatures,
  isFeatureDisabled,
  extractFeatures,
  withFeatures,
} from "../../src/lib/repoFeatures.js";

test("FEATURE_KEYS carries the six wire flags in order", () => {
  assert.deepEqual(FEATURE_KEYS, ["issues", "pulls", "releases", "forks", "watch", "star"]);
});

test("resolveFeatures fails open: only explicit false disables", () => {
  assert.deepEqual(resolveFeatures(undefined), allFeatures());
  assert.deepEqual(resolveFeatures(null), allFeatures());
  assert.deepEqual(resolveFeatures("nope"), allFeatures());
  assert.deepEqual(resolveFeatures({}), allFeatures());
  assert.deepEqual(resolveFeatures({ issues: false }), {
    ...allFeatures(),
    issues: false,
  });
  // Sloppy values never disable (fail-open, not truthy-check).
  assert.deepEqual(resolveFeatures({ issues: 0, pulls: "", star: null }), allFeatures());
});

test("isFeatureDisabled gates tabs/pills fail-open", () => {
  assert.equal(isFeatureDisabled(undefined, "issues"), false);
  assert.equal(isFeatureDisabled(null, "issues"), false);
  assert.equal(isFeatureDisabled({}, "issues"), false);
  assert.equal(isFeatureDisabled({ features: null }, "issues"), false);
  assert.equal(isFeatureDisabled({ features: { issues: false } }, "issues"), true);
  assert.equal(isFeatureDisabled({ features: { issues: false } }, "pulls"), false);
  assert.equal(isFeatureDisabled({ features: allFeatures() }, "star"), false);
});

test("extractFeatures reads booleans, nulls everything else", () => {
  assert.deepEqual(extractFeatures(""), {
    issues: null,
    pulls: null,
    releases: null,
    forks: null,
    watch: null,
    star: null,
  });
  const doc = 'description = "hi"\n[features]\nissues = false\nstar = true\n[bundles]\nmin_commits = 5\n';
  assert.deepEqual(extractFeatures(doc), {
    issues: false,
    pulls: null,
    releases: null,
    forks: null,
    watch: null,
    star: true,
  });
  // Non-boolean spellings stay null (the server rejects them; the
  // editor never writes them).
  assert.equal(extractFeatures('[features]\nissues = "no"\n').issues, null);
  // Section-nested lookalikes outside [features] are ignored.
  assert.equal(extractFeatures('[bundles]\nissues = false\n').issues, null);
});

test("withFeatures appends the canonical block to a doc without one", () => {
  const out = withFeatures('description = "hi"\n', { ...allFeatures(), issues: false });
  assert.ok(out.includes('description = "hi"'), "top-level key untouched");
  assert.ok(out.includes("[features]\nissues = false\npulls = true\nreleases = true\nforks = true\nwatch = true\nstar = true\n"));
  // The result parses back to the same flags.
  assert.deepEqual(resolveFeatures(out && parseBlock(out)), { ...allFeatures(), issues: false });
});

test("withFeatures replaces an existing block in place, drops nothing else", () => {
  const doc = 'description = "hi"\n[features]\n# a comment\nissues = true\n[bundles]\nmin_commits = 5\n';
  const out = withFeatures(doc, { ...allFeatures(), issues: false, star: false });
  assert.ok(out.startsWith('description = "hi"\n[features]\n'), "position kept");
  assert.ok(out.includes("[bundles]\nmin_commits = 5\n"), "sibling section untouched");
  assert.ok(!out.includes("# a comment"), "stale block body replaced wholesale");
  assert.equal(extractFeatures(out).issues, false);
  assert.equal(extractFeatures(out).star, false);
  assert.equal(extractFeatures(out).pulls, true);
});

test("withFeatures on an empty doc yields the bare block", () => {
  assert.equal(withFeatures("", allFeatures()), "[features]\nissues = true\npulls = true\nreleases = true\nforks = true\nwatch = true\nstar = true\n");
});

// Minimal [features] reader for the round-trip pin above (the real
// extractFeatures is asserted directly elsewhere).
function parseBlock(text) {
  const lines = text.split("\n");
  let inFeatures = false;
  const out = {};
  for (const line of lines) {
    const header = line.match(/^\s*\[([^\]]*)\]\s*$/);
    if (header) {
      inFeatures = header[1] === "features";
      continue;
    }
    if (!inFeatures) continue;
    const m = line.match(/^([a-z]+) = (true|false)$/);
    if (m) out[m[1]] = m[2] === "true";
  }
  return out;
}
