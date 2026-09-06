// web/test/unit/setup-advanced.test.js — issue #168: per-section collapsed
// "Advanced" groups for sane-default fields. The grouping is UI-only: hidden
// (collapsed) fields still validate and save byte-identically, so these tests
// pin the grouping metadata AND the save-payload equivalence.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  FIELDS, isAdvanced, splitAdvanced, validateSetup, normalizeSetup,
} from "../../src/lib/setup.js";

const byKey = new Map(FIELDS.map((f) => [f.key, f]));
// Section card a key renders in: server.auth.* has its own card after server.
const sectionOf = (key) => (key.startsWith("server.auth.") ? "auth" : key.split(".")[0]);
const keysIn = (section) => FIELDS.filter((f) => sectionOf(f.key) === section).map((f) => f.key);

// --- the flag + the partition helper -------------------------------------------

test("isAdvanced is true only for the explicit flag", () => {
  assert.equal(isAdvanced({ key: "x", advanced: true }), true);
  assert.equal(isAdvanced({ key: "x" }), false);
  assert.equal(isAdvanced({ key: "x", advanced: false }), false);
  assert.equal(isAdvanced(undefined), false);
  assert.equal(isAdvanced(null), false);
});

test("splitAdvanced partitions rows, preserves order, accepts strings", () => {
  // grounded in the real flags: server.listen is essential, server.http2 advanced
  assert.equal(isAdvanced(byKey.get("server.listen")), false);
  assert.equal(isAdvanced(byKey.get("server.http2")), true);
  const rows = [{ key: "server.listen" }, { key: "server.http2" }, { key: "server.public_url" }];
  const { essential, advanced } = splitAdvanced(rows);
  assert.deepEqual(essential, [{ key: "server.listen" }, { key: "server.public_url" }]);
  assert.deepEqual(advanced, [{ key: "server.http2" }]);
  assert.deepEqual(rows.map((r) => r.key),
    ["server.listen", "server.http2", "server.public_url"]); // input untouched
  // bare key strings work too
  assert.deepEqual(
    splitAdvanced(["server.listen", "server.http2"]),
    { essential: ["server.listen"], advanced: ["server.http2"] },
  );
});

test("splitAdvanced treats unknown keys as essential (never hides)", () => {
  const { essential, advanced } = splitAdvanced([{ key: "server.nope" }, "also.unknown"]);
  assert.deepEqual(essential, [{ key: "server.nope" }, "also.unknown"]);
  assert.deepEqual(advanced, []);
});

// --- per-section verdicts (issue #168 acceptance: every section evaluated) ------

// Sections with a collapsed Advanced group: at least one essential row stays
// visible and at least one sane-default knob hides.
const GROUPED = ["server", "store", "cache", "wal", "maintenance", "compaction",
  "bundles", "lfs", "upstream", "git", "telemetry", "events", "import"];
for (const section of GROUPED) {
  test(`section "${section}" has visible essentials AND a collapsed Advanced group`, () => {
    const keys = keysIn(section);
    assert.ok(keys.length > 0, `${section}: no FIELDS keys`);
    const adv = keys.filter((k) => isAdvanced(byKey.get(k)));
    const ess = keys.filter((k) => !isAdvanced(byKey.get(k)));
    assert.ok(ess.length > 0, `${section}: every row advanced — essentials must stay visible`);
    assert.ok(adv.length > 0, `${section}: no advanced rows`);
  });
}

// Sections evaluated to all-visible: auth (every row is mode-gated and either
// required or security-sensitive) and placement (routing globs have no sane
// default to hide behind).
for (const section of ["auth", "placement"]) {
  test(`section "${section}" is all-visible (no Advanced group)`, () => {
    const keys = keysIn(section);
    assert.ok(keys.length > 0, `${section}: no FIELDS keys`);
    assert.deepEqual(keys.filter((k) => isAdvanced(byKey.get(k))), []);
  });
}

// --- save/test/validate behavior is grouping-independent ------------------------

test("save payload is identical expanded vs collapsed (grouping is UI-only)", () => {
  const values = {
    "server.listen": "0.0.0.0:8080",
    "server.max_concurrent_requests": "512", // advanced
    "server.ssh.host_key": "/var/lib/walhub/ssh/key", // advanced
    "store.backend": "filesystem",
    "store.multipart_threshold": "64MiB", // advanced
    "cache.dir": "/var/cache/walhub",
    "cache.prewarm_parallelism": "4", // advanced
    "wal.checkpoint_interval": "10m",
    "wal.cas_max_retries": "5", // advanced
    "maintenance.interval": "1m",
    "maintenance.fsck_interval": "24h", // advanced
    "compaction.enabled": "true",
    "compaction.factor": "4", // advanced
    "bundles.strategy": '[[strategy]]\nname = "weekly"\nkind = "full"\nschedule = "0 0 23 * * 0"\nkeep = 2',
    "bundles.min_commits": "25", // advanced
    "lfs.enabled": "true",
    "lfs.max_object_bytes": "10GiB", // advanced
    "upstream.git": "https://github.com/acme/widgets.git",
    "upstream.token_env": "WALHUB_UPSTREAM_TOKEN", // advanced
    "git.binary": "/usr/bin/git",
    "git.max_wants": "100000", // advanced
    "telemetry.log_format": "json",
    "telemetry.lock_wait_warn": "5s", // advanced
    "events.webhook_url": "http://ci.example.com/hooks/walhub",
    "events.sweep_interval": "1m", // advanced
    "import.clone_timeout": "30m",
    "import.max_refs": "100000", // advanced
  };
  // "collapsed" = the same flat form state (a closed <details> unmounts
  // nothing); "expanded" = essential + advanced merged back — both must
  // produce the same overrides the test endpoint sees.
  const { essential, advanced } = splitAdvanced(Object.keys(values));
  assert.ok(essential.length > 0 && advanced.length > 0);
  const pick = (ks) => Object.fromEntries(ks.map((k) => [k, values[k]]));
  assert.deepEqual(
    normalizeSetup(values),
    normalizeSetup({ ...pick(essential), ...pick(advanced) }),
  );
  // and the advanced keys actually travel (grouping never drops them)
  const { overrides } = normalizeSetup(values);
  for (const k of advanced) assert.ok(k in overrides, `${k} must still save while collapsed`);
});

test("collapsed advanced fields still validate (hidden ≠ skipped)", () => {
  const bad = {
    "cache.prewarm_parallelism": "0", // min 1
    "wal.cas_max_retries": "0", // min 1
    "server.max_concurrent_requests": "many", // not a number
    "import.max_refs": "-1", // min 1
  };
  for (const [key, value] of Object.entries(bad)) {
    assert.ok(isAdvanced(byKey.get(key)), `${key} must be an advanced field for this test`);
    const errs = validateSetup({ [key]: value }).filter((e) => e.key === key && e.severity === "error");
    assert.equal(errs.length, 1, `${key}=${value} must still fail validation while collapsed`);
  }
  // valid advanced values pass alongside valid essentials
  assert.deepEqual(
    validateSetup({
      "server.listen": "0.0.0.0:8080",
      "server.max_concurrent_requests": "512",
      "cache.prewarm_parallelism": "4",
    }).filter((e) => e.severity === "error"),
    [],
  );
});
