// web/test/unit/timezone.test.js — IANA picker data (Forgejo #234):
// the list comes from the runtime ICU, never a bundled tz database.
import { test } from "node:test";
import assert from "node:assert/strict";
import { timeZones, isValidTimeZone } from "../../src/lib/timezone.js";

test("timeZones returns the runtime IANA list (non-empty strings)", () => {
  const zones = timeZones();
  assert.ok(Array.isArray(zones), "must be an array");
  assert.ok(zones.length > 100, `expected hundreds of zones, got ${zones.length}`);
  for (const z of zones) {
    assert.equal(typeof z, "string");
    assert.ok(z.length > 0 && z.length <= 64, `zone length: ${z}`);
  }
  // Spot-check the zones the issue calls out: the picker is the IANA list
  // (plus the unioned "UTC" — valid IANA, but missing from some runtimes'
  // ICU data, e.g. Node's).
  for (const z of ["UTC", "Europe/Berlin", "America/New_York", "Asia/Tokyo"]) {
    assert.ok(zones.includes(z), `missing ${z}`);
  }
});

test("UTC leads the list even where the runtime ICU omits it", () => {
  const zones = timeZones();
  assert.equal(zones[0], "UTC");
});

test("timeZones returns a copy (caller mutation is harmless)", () => {
  const a = timeZones();
  a.push("Fake/Zone");
  assert.ok(!timeZones().includes("Fake/Zone"));
});

test("isValidTimeZone accepts list members and the unset empty string", () => {
  assert.equal(isValidTimeZone("Europe/Berlin"), true);
  assert.equal(isValidTimeZone("UTC"), true);
  assert.equal(isValidTimeZone(""), true);
  assert.equal(isValidTimeZone(undefined), true);
  assert.equal(isValidTimeZone(null), true);
});

test("isValidTimeZone rejects free text outside the list", () => {
  assert.equal(isValidTimeZone("not a zone"), false);
  assert.equal(isValidTimeZone("europe/berlin"), false); // IANA names are exact
  assert.equal(isValidTimeZone("Berlin"), false);
  assert.equal(isValidTimeZone("Fake/Zone"), false);
});

test("isValidTimeZone honors an explicit list argument", () => {
  assert.equal(isValidTimeZone("UTC", ["UTC"]), true);
  assert.equal(isValidTimeZone("Europe/Berlin", ["UTC"]), false);
});
