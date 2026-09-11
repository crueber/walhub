// web/test/unit/orgs.test.js — org helpers (Forgejo #348): create-form
// validation mirroring identity.ValidOrg, the create body, and the
// owners/detailed is_org badge predicate.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  isOrgRow,
  orgCreateBody,
  validateOrgName,
} from "../../src/lib/orgs.js";

test("validateOrgName accepts legal slugs, normalized", () => {
  assert.deepEqual(validateOrgName("acme"), { org: "acme" });
  assert.deepEqual(validateOrgName("  Acme-9 "), { org: "acme-9" });
  assert.deepEqual(validateOrgName("a"), { org: "a" });
  assert.deepEqual(validateOrgName("x".repeat(39)), { org: "x".repeat(39) });
});

test("validateOrgName rejects empties and charset/length misses", () => {
  for (const bad of ["", "   ", null, undefined]) {
    const v = validateOrgName(bad);
    assert.ok(v.error, `empty ${String(bad)} must error`);
    assert.equal(v.org, undefined);
  }
  // NOTE: "UPPER" is absent above on purpose — it normalizes to
  // "upper" (valid, covered in the parity test below).
  for (const bad of ["acme!", "has space", "under_score", "dot.name", "x".repeat(40), "org/dir", "a@b.c"]) {
    const v = validateOrgName(bad);
    assert.ok(v.error, `${bad} must error`);
    assert.match(v.error, /1–39/);
  }
});

test("validateOrgName lowercases before checking (server parity)", () => {
  assert.deepEqual(validateOrgName("ACME"), { org: "acme" });
  assert.deepEqual(validateOrgName("Acme-9"), { org: "acme-9" });
});

test("orgCreateBody carries exactly org/display_name/description", () => {
  assert.deepEqual(orgCreateBody({ org: "  Acme ", display_name: "Acme", description: "d" }), {
    org: "acme",
    display_name: "Acme",
    description: "d",
  });
  assert.deepEqual(orgCreateBody({}), { org: "", display_name: "", description: "" });
  assert.deepEqual(orgCreateBody(null), { org: "", display_name: "", description: "" });
  // Non-strings coerce to "" — inputs stay controlled.
  assert.deepEqual(orgCreateBody({ org: 7, display_name: null, description: ["x"] }), {
    org: "",
    display_name: "",
    description: "",
  });
});

test("isOrgRow reads the server is_org bit, legacy-tolerant", () => {
  assert.equal(isOrgRow({ name: "acme", is_org: true }), true);
  assert.equal(isOrgRow({ name: "jane", is_org: false }), false);
  assert.equal(isOrgRow({ name: "old" }), false); // legacy server: no field
  assert.equal(isOrgRow({ name: "n", is_org: null }), false);
  assert.equal(isOrgRow({ name: "n", is_org: 1 }), false); // strict bit
  assert.equal(isOrgRow(null), false);
  assert.equal(isOrgRow(undefined), false);
});
