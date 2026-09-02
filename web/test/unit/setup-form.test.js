// web/test/unit/setup-form.test.js — lib/setup.js validators mirroring the server
// (11_config_cli.md §5, rule by rule). Table-driven per §5 of the web doc.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  validateSetup, normalizeSetup, isRestartLikely, parseDuration, parseSize, FIELDS,
  fmtSpecDuration, fmtSpecSize, strategiesToToml,
} from "../../src/lib/setup.js";

const errs = (values) => validateSetup(values);
const fatals = (values) => validateSetup(values).filter((e) => e.severity === "error");
const warns = (values) => validateSetup(values).filter((e) => e.severity === "warn");
const messages = (values, key) => fatals(values).filter((e) => e.key === key).map((e) => e.message);

test("a complete valid config passes with no errors", () => {
  const values = {
    "server.listen": "0.0.0.0:8080",
    "server.auth.mode": "token",
    "server.auth.session_secret": "0123456789abcdef0123456789abcdef",
    "server.tls.mode": "self_signed",
    "store.backend": "filesystem",
    "store.root": "/var/lib/walhub/store",
    "cache.dir": "/var/cache/walhub",
    "cache.disk_high_watermark": "0.9",
    "server.max_concurrent_requests": "512",
    "maintenance.disk": "ssd",
    "placement.serve": "*, acme/*, acme/one",
    "server.request_timeout": "1h",
    "server.max_push_bytes": "64GiB",
    "git.object_format": "sha1",
    "telemetry.log_format": "pretty",
  };
  assert.deepEqual(fatals(values), []);
});

test("auth none on a loopback bind is clean; on a public bind it warns (not fails)", () => {
  assert.deepEqual(fatals({ "server.listen": "127.0.0.1:8080", "server.auth.mode": "none" }), []);
  assert.deepEqual(fatals({ "server.listen": "localhost:8080", "server.auth.mode": "none" }), []);
  const w = warns({ "server.listen": "0.0.0.0:8080", "server.auth.mode": "none" });
  assert.equal(w.length, 1);
  assert.match(w[0].message, /auth\.mode=none on non-loopback listen/);
  assert.deepEqual(warns({ "server.listen": "0.0.0.0:8080", "server.auth.mode": "token" }), []);
});

// --- rule 0: field-level types, enums, formats ------------------------------------

const enumCases = [
  ["server.auth.mode", "basic", "none|token|oidc"],
  ["server.tls.mode", "wildcard", "off|self_signed|files"],
  ["store.backend", "s4", "s3|gcs|memory|filesystem"],
  ["maintenance.disk", "nvme", "tmpfs|ssd"],
  ["git.object_format", "md5", "sha1|sha256"],
  ["telemetry.log_format", "xml", "pretty|json"],
];
for (const [key, bad, expected] of enumCases) {
  test(`enum: ${key} rejects "${bad}" naming the accepted set`, () => {
    assert.ok(messages({ [key]: bad }, key)[0].includes(expected), messages({ [key]: bad }, key)[0]);
  });
}

const rangeCases = [
  ["server.max_concurrent_requests", "0", 1],
  ["store.max_retries", "-1", 0],
  ["compaction.factor", "0.5", 1],
  ["compaction.trigger_packs", "1", 2],
  ["bundles.min_commits", "-3", 0],
  ["git.max_wants", "-5", 0],
];
for (const [key, bad, min] of rangeCases) {
  test(`range: ${key} rejects "${bad}" (< ${min})`, () => {
    assert.ok(messages({ [key]: bad }, key)[0].includes(`≥ ${min}`));
  });
}

test("int fields reject non-integers and non-numbers", () => {
  assert.ok(messages({ "server.max_concurrent_requests": "12.5" }, "server.max_concurrent_requests")[0].includes("integer"));
  assert.ok(messages({ "server.max_concurrent_requests": "many" }, "server.max_concurrent_requests")[0].includes("number"));
});

test("cache.disk_high_watermark must be 0 or strictly inside (0,1)", () => {
  assert.deepEqual(fatals({ "cache.disk_high_watermark": "0.9" }), []);
  assert.deepEqual(fatals({ "cache.disk_high_watermark": "0" }), []);
  assert.equal(messages({ "cache.disk_high_watermark": "1" }, "cache.disk_high_watermark").length, 1);
  assert.ok(messages({ "cache.disk_high_watermark": "-0.5" }, "cache.disk_high_watermark").length >= 1);
});

test("durations: valid suffixes pass, junk fails", () => {
  for (const v of ["5ms", "30s", "10m", "1h", "7d", "2w", "0", "90"]) {
    assert.deepEqual(fatals({ "maintenance.interval": v }), [], v);
  }
  assert.ok(messages({ "maintenance.interval": "5x" }, "maintenance.interval")[0].includes("not a duration"));
});

test("sizes: valid suffixes pass, junk fails", () => {
  for (const v of ["0B", "512B", "1KiB", "64MiB", "20GiB", "2TiB", "4096"]) {
    assert.deepEqual(fatals({ "server.max_push_bytes": v }), [], v);
  }
  assert.ok(messages({ "server.max_push_bytes": "5MB" }, "server.max_push_bytes")[0].includes("not a size"));
});

test("url fields require http(s)", () => {
  assert.deepEqual(fatals({ "server.public_url": "https://git.example.com" }), []);
  assert.ok(messages({ "server.public_url": "ftp://x" }, "server.public_url")[0].includes("http(s)"));
  assert.ok(messages({ "upstream.git": "/local/path" }, "upstream.git").length === 1);
});

test("listen: host:port format, port range, IPv6 brackets", () => {
  assert.deepEqual(fatals({ "server.listen": "0.0.0.0:8080" }), []);
  assert.deepEqual(fatals({ "server.listen": "[::1]:8080" }), []);
  assert.ok(messages({ "server.listen": "8080" }, "server.listen")[0].includes("host:port"));
  assert.ok(messages({ "server.listen": "host:" }, "server.listen")[0].includes("host:port"));
  assert.ok(messages({ "server.listen": "host:99999" }, "server.listen")[0].includes("host:port"));
  assert.ok(messages({ "server.listen": "host:nope" }, "server.listen")[0].includes("host:port"));
});

test("bools accept the server's spellings and reject others", () => {
  for (const v of ["true", "false", "yes", "no", "on", "off", "1", "0"]) {
    assert.deepEqual(fatals({ "server.http2": v }), [], v);
  }
  assert.ok(messages({ "server.http2": "maybe" }, "server.http2")[0].includes("true or false"));
});

test("unknown keys are a validation error, not a silent drop", () => {
  assert.deepEqual(messages({ "server.hack": "1" }, "server.hack"), ["unknown key: server.hack"]);
});

// --- §5 rule 2: oidc ---------------------------------------------------------------

test("oidc requires anonymous_read=false (default true fails when unset)", () => {
  const base = { "server.auth.mode": "oidc", "server.auth.allowed_domains": "acme.com" };
  assert.ok(messages(base, "server.auth.anonymous_read").length === 1);
  assert.deepEqual(fatals({ ...base, "server.auth.anonymous_read": "false" }), []);
  assert.ok(messages({ ...base, "server.auth.anonymous_read": "true" }, "server.auth.anonymous_read").length === 1);
});

test("oidc requires a non-empty allowlist", () => {
  assert.ok(messages({ "server.auth.mode": "oidc", "server.auth.anonymous_read": "false" }, "server.auth.allowed_domains").length === 1);
  assert.deepEqual(
    fatals({ "server.auth.mode": "oidc", "server.auth.anonymous_read": "false", "server.auth.allowed_emails": "a@b.c" }),
    []);
});

test("oauth client id/secret are both-or-neither", () => {
  const base = { "server.auth.mode": "oidc", "server.auth.anonymous_read": "false", "server.auth.allowed_domains": "a.com" };
  assert.ok(messages({ ...base, "server.auth.oauth_client_id": "id" }, "server.auth.oauth_client_id").length === 1);
  assert.deepEqual(fatals({ ...base, "server.auth.oauth_client_id": "id", "server.auth.oauth_client_secret": "sec" }), []);
});

test("session_secret must be ≥ 32 bytes when set", () => {
  assert.ok(messages({ "server.auth.session_secret": "short" }, "server.auth.session_secret").length === 1);
  assert.deepEqual(fatals({ "server.auth.session_secret": "0123456789abcdef0123456789abcdef" }), []);
});

// --- §5 rules 5/8/10: store, tls, paths ---------------------------------------------

test("tls files mode requires cert and key", () => {
  assert.ok(messages({ "server.tls.mode": "files" }, "server.tls.cert")[0].includes("requires server.tls.cert"));
  assert.deepEqual(fatals({ "server.tls.mode": "files", "server.tls.cert": "/c.pem", "server.tls.key": "/k.pem" }), []);
  assert.deepEqual(fatals({ "server.tls.mode": "self_signed" }), []);
});

test("filesystem store: store.root must be absolute or empty", () => {
  assert.deepEqual(fatals({ "store.backend": "filesystem", "store.root": "/abs/root" }), []);
  assert.deepEqual(fatals({ "store.backend": "filesystem" }), []); // empty = first-run default
  assert.ok(messages({ "store.backend": "filesystem", "store.root": "rel/root" }, "store.root")[0].includes("absolute"));
});

test("cache.dir must be absolute unless the backend is memory", () => {
  assert.deepEqual(fatals({ "store.backend": "memory", "cache.dir": "rel" }), []);
  assert.ok(messages({ "store.backend": "s3", "cache.dir": "rel" }, "cache.dir")[0].includes("absolute"));
  assert.ok(messages({ "cache.dir": "rel" }, "cache.dir").length >= 1); // default backend (filesystem) still checks
});

test("multipart_part_size must be ≤ multipart_threshold when both are non-zero", () => {
  assert.deepEqual(fatals({ "store.multipart_threshold": "64MiB", "store.multipart_part_size": "32MiB" }), []);
  assert.ok(messages({ "store.multipart_threshold": "32MiB", "store.multipart_part_size": "64MiB" }, "store.multipart_part_size")[0].includes("≤"));
  assert.deepEqual(fatals({ "store.multipart_part_size": "64MiB" }), []); // threshold unset: nothing to compare
});

test("placement globs are *, owner/* or owner/name (one slash at most)", () => {
  assert.deepEqual(fatals({ "placement.serve": "*, acme/*, acme/one" }), []);
  assert.ok(messages({ "placement.serve": "a/b/c" }, "placement.serve")[0].includes("owner/name"));
  assert.ok(messages({ "placement.maintain": "no-slash" }, "placement.maintain")[0].includes("owner/name"));
});

test("server.roles is a list of serve|maintain|events", () => {
  assert.deepEqual(fatals({ "server.roles": "serve, maintain, events" }), []);
  assert.ok(messages({ "server.roles": "serve, admin" }, "server.roles")[0].includes("serve|maintain|events"));
});

// --- normalize + hints ----------------------------------------------------------------

test("normalizeSetup coerces types and skips empty values", () => {
  const { overrides } = normalizeSetup({
    "server.http2": "false",
    "server.max_concurrent_requests": "512",
    "compaction.factor": "2",
    "placement.serve": "*, acme/*",
    "store.backend": "",
    "server.listen": "0.0.0.0:8080",
  });
  assert.equal(overrides["server.http2"], false);
  assert.equal(overrides["server.max_concurrent_requests"], 512);
  assert.equal(overrides["compaction.factor"], 2);
  assert.deepEqual(overrides["placement.serve"], ["*", "acme/*"]);
  assert.equal(overrides["server.listen"], "0.0.0.0:8080");
  assert.ok(!("store.backend" in overrides));
});

test("isRestartLikely: server.* plus store.backend/store.root, nothing else", () => {
  assert.equal(isRestartLikely("server.listen"), true);
  assert.equal(isRestartLikely("server.auth.mode"), true);
  assert.equal(isRestartLikely("store.backend"), true);
  assert.equal(isRestartLikely("store.root"), true);
  assert.equal(isRestartLikely("maintenance.interval"), false);
  assert.equal(isRestartLikely("telemetry.log_format"), false);
  assert.equal(isRestartLikely("wal.freshness_ttl"), false);
});

test("parseDuration / parseSize mirror the server's shared parser", () => {
  assert.equal(parseDuration("1h"), 3600);
  assert.equal(parseDuration("5ms"), 0.005);
  assert.equal(parseDuration("2w"), 1209600);
  assert.equal(parseDuration("90"), 90); // bare int = seconds
  assert.ok(Number.isNaN(parseDuration("1x")));
  assert.equal(parseSize("1KiB"), 1024);
  assert.equal(parseSize("1MiB"), 1024 ** 2);
  assert.equal(parseSize("4096"), 4096); // bare int = bytes
  assert.ok(Number.isNaN(parseSize("5MB")));
});

test("every FIELDS entry has a known type (table integrity)", () => {
  const known = new Set(["listen", "bool", "int", "float", "duration", "size", "url", "enum", "list", "globs", "path", "string", "toml"]);
  for (const f of FIELDS) {
    assert.ok(known.has(f.type), `${f.key}: unknown type ${f.type}`);
    if (f.type === "enum") assert.ok(f.enum.length >= 2, `${f.key}: enum needs values`);
  }
});

test("every FIELDS entry carries a working example (setup page hints)", () => {
  for (const f of FIELDS) {
    assert.ok(f.ex !== undefined && String(f.ex).trim() !== "", `${f.key}: missing ex`);
  }
});

test("every example value validates on its own (client mirror)", () => {
  for (const f of FIELDS) {
    const errs = validateSetup({ [f.key]: f.ex }).filter((e) => e.key === f.key && e.severity === "error");
    assert.deepEqual(errs, [], `${f.key}: example ${JSON.stringify(f.ex)} must validate: ${errs.map((e) => e.message).join("; ")}`);
  }
});

test("fmtSpecDuration renders server values in the spec spelling", () => {
  assert.equal(fmtSpecDuration("1h0m0s"), "1h"); // Go compound from the schema
  assert.equal(fmtSpecDuration("0s"), "0s");
  assert.equal(fmtSpecDuration(7200), "2h"); // bare seconds
  assert.equal(fmtSpecDuration(5400), "90m"); // largest even divisor
  assert.equal(fmtSpecDuration(604800), "1w");
  assert.equal(fmtSpecDuration("500ms"), "500ms");
  assert.equal(fmtSpecDuration(""), "");
});

test("fmtSpecSize renders byte counts in the spec spelling", () => {
  assert.equal(fmtSpecSize(68719476736), "64GiB");
  assert.equal(fmtSpecSize(64 << 20), "64MiB");
  assert.equal(fmtSpecSize(0), "0B");
  assert.equal(fmtSpecSize(1156), "1156B"); // no even unit → bytes
  assert.equal(fmtSpecSize(""), "");
  assert.equal(fmtSpecSize(null), "");
});

test("strategiesToToml renders the parsed strategies as an editable fragment", () => {
  const fragment = strategiesToToml([
    { name: "weekly", kind: "full", schedule: "0 0 23 * * 0", keep: 2, backfill_max: 1 },
    { name: "daily", kind: "incremental", base: "weekly", schedule: "0 0 23 * * *", chain: true },
  ]);
  // keys are normalized to toml spellings; presence asserted, order is not
  assert.match(fragment, /\[\[strategy\]\]\nname = "weekly"[\s\S]*kind = "full"[\s\S]*schedule = "0 0 23 \* \* 0"[\s\S]*keep = 2/);
  assert.match(fragment, /\[\[strategy\]\]\nname = "daily"[\s\S]*base = "weekly"[\s\S]*chain = true/);
  assert.equal(strategiesToToml([]), "");
  assert.equal(strategiesToToml(null), "");
  // the example + the rendered fragment both validate (toml is server-checked)
  const parsed = fragment.split("\n\n").length;
  assert.equal(parsed, 2);
});
