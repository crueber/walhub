// web/test/unit/repo-review-586.test.js — Forgejo #586: the pure
// self-approval helpers (web/src/lib/repoReview.js). Default-ON is the
// whole contract — unset renders checked (the server default is
// allowed) — and the TOML helpers round-trip the `[review]` section's
// `allow_self_approval` line without touching the rest of the settings
// doc.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  SELF_APPROVAL_LABEL,
  SELF_APPROVAL_HINT,
  extractSelfApproval,
  withSelfApproval,
} from "../../src/lib/repoReview.js";

test("label and hint name the consequence", () => {
  assert.equal(SELF_APPROVAL_LABEL, "Allow self-approval");
  assert.ok(SELF_APPROVAL_HINT.includes("never counts"), "the hint states the gate rule");
});

test("extractSelfApproval reads booleans, nulls everything else", () => {
  assert.equal(extractSelfApproval(""), null);
  assert.equal(extractSelfApproval(undefined), null);
  assert.equal(extractSelfApproval('description = "hi"\n'), null);
  assert.equal(extractSelfApproval("[review]\n"), null);
  assert.equal(extractSelfApproval("[review]\nallow_self_approval = true\n"), true);
  assert.equal(extractSelfApproval("[review]\nallow_self_approval = false\n"), false);
  const doc = 'description = "hi"\n[features]\nissues = false\n[review]\nallow_self_approval = false\n[bundles]\nmin_commits = 5\n';
  assert.equal(extractSelfApproval(doc), false);
  // Non-boolean spellings stay null (the server rejects them; the
  // editor never writes them).
  assert.equal(extractSelfApproval('[review]\nallow_self_approval = "no"\n'), null);
  assert.equal(extractSelfApproval("[review]\nallow_self_approval = 0\n"), null);
  // Section-nested lookalikes outside [review] are ignored.
  assert.equal(extractSelfApproval("[bundles]\nallow_self_approval = false\n"), null);
});

test("withSelfApproval appends the canonical block to a doc without one", () => {
  const out = withSelfApproval('description = "hi"\n', false);
  assert.ok(out.includes('description = "hi"'), "top-level key untouched");
  assert.ok(out.includes("[review]\nallow_self_approval = false\n"));
  assert.equal(extractSelfApproval(out), false);
});

test("withSelfApproval on an empty doc yields the bare block", () => {
  assert.equal(withSelfApproval("", true), "[review]\nallow_self_approval = true\n");
  assert.equal(withSelfApproval("", false), "[review]\nallow_self_approval = false\n");
});

test("withSelfApproval replaces the line in place, drops nothing else", () => {
  const doc = 'description = "hi"\n[review]\nallow_self_approval = true\n[bundles]\nmin_commits = 5\n';
  const out = withSelfApproval(doc, false);
  assert.ok(out.startsWith('description = "hi"\n[review]\n'), "position kept");
  assert.ok(out.includes("[bundles]\nmin_commits = 5\n"), "sibling section untouched");
  assert.equal(extractSelfApproval(out), false);
});

test("withSelfApproval preserves sibling lines in the section", () => {
  const doc = "[review]\n# a comment\nallow_self_approval = false\n";
  const out = withSelfApproval(doc, true);
  assert.ok(out.includes("# a comment"), "sibling line kept");
  assert.equal(extractSelfApproval(out), true);
});

test("withSelfApproval appends the line to a section missing it", () => {
  const doc = "[review]\n# future knob\n[bundles]\nmin_commits = 5\n";
  const out = withSelfApproval(doc, false);
  assert.ok(out.includes("# future knob"), "sibling line kept");
  assert.ok(out.includes("[bundles]\nmin_commits = 5\n"), "next section untouched");
  assert.equal(extractSelfApproval(out), false);
});

test("withSelfApproval round-trips through the features helpers' doc shape", () => {
  const doc = '[features]\nissues = true\npulls = true\nreleases = true\nforks = true\nwatch = true\nstar = true\n';
  const out = withSelfApproval(doc, false);
  assert.ok(out.includes("[features]"), "features block untouched");
  assert.equal(extractSelfApproval(out), false);
});
