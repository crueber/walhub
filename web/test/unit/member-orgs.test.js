import { test } from "node:test";
import assert from "node:assert/strict";

import { normalizeMemberOrgs } from "../../src/lib/orgs.js";

test("normalizeMemberOrgs coerces the rail payload to sorted unique names", () => {
  assert.deepEqual(normalizeMemberOrgs(["zeta", "acme", "beta"]), ["acme", "beta", "zeta"]);
  assert.deepEqual(normalizeMemberOrgs(["acme", "acme", " beta "]), ["acme", "beta"]);
});

test("normalizeMemberOrgs drops non-strings and blanks, tolerates odd shapes", () => {
  assert.deepEqual(normalizeMemberOrgs(["acme", null, 42, "", "  ", { org: "x" }]), ["acme"]);
  assert.deepEqual(normalizeMemberOrgs(null), []);
  assert.deepEqual(normalizeMemberOrgs(undefined), []);
  assert.deepEqual(normalizeMemberOrgs({ orgs: ["acme"] }), []);
  assert.deepEqual(normalizeMemberOrgs([]), []);
});
