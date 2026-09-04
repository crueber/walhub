// web/src/lib/setup.js — §2.10: client-side setup-form validation mirroring the
// server validator (11_config_cli.md §5 named rules: ranges, enum membership,
// URL/listen formats, cross-field rules). The server response is authoritative;
// these rules give the operator inline hints and a client gate before
// POST /api/v1/setup/test and PUT /api/v1/setup.
// Pure functions — importable in Node (setup-form tests).

// Duration: `ms|s|m|h|d|w` suffix or bare integer (= seconds). Size: `B|KiB|MiB|GiB|TiB`
// (case-insensitive, binary) or bare integer (= bytes). `0`/`"0B"`/`"0s"` = disabled.
const DURATION_RE = /^(\d+)(?:\.(\d+))?(ms|s|m|h|d|w)$/;
const SIZE_RE = /^(\d+)(?:\.(\d+))?(B|KiB|MiB|GiB|TiB)$/i;
const MULT = { ms: 0.001, s: 1, m: 60, h: 3600, d: 86400, w: 604800 };
const SIZE_MULT = { b: 1, kib: 2 ** 10, mib: 2 ** 20, gib: 2 ** 30, tib: 2 ** 40 };

export function parseDuration(v) {
  const s = String(v ?? "").trim();
  if (s === "" || s === "0") return 0;
  if (/^\d+$/.test(s)) return Number(s); // bare integer = seconds
  const m = DURATION_RE.exec(s);
  if (!m) return NaN;
  return Number(`${m[1]}${m[2] ? `.${m[2]}` : ""}`) * MULT[m[3]];
}
export function parseSize(v) {
  const s = String(v ?? "").trim();
  if (s === "" || s === "0") return 0;
  if (/^\d+$/.test(s)) return Number(s); // bare integer = bytes
  const m = SIZE_RE.exec(s);
  if (!m) return NaN;
  return Number(`${m[1]}${m[2] ? `.${m[2]}` : ""}`) * SIZE_MULT[m[3].toLowerCase()];
}

const ABS_PATH_RE = /^(?:\/|\\\\|\/\/|[A-Za-z]:[\\/])/;

function asList(v) {
  if (Array.isArray(v)) return v.map(String);
  if (v === undefined || v === null) return [];
  return String(v).split(/[\s,]+/).filter(Boolean);
}
function trimStr(v) {
  return v === undefined || v === null ? "" : String(v).trim();
}

// listen "host:port" — host may be an IPv6 literal in brackets
function parseListen(v) {
  const s = String(v ?? "").trim();
  const m = /^\[([0-9a-fA-F:]+)\]:(\d+)$/.exec(s) || /^([^:\s]+):(\d+)$/.exec(s);
  if (!m) return null;
  const port = Number(m[2]);
  if (!Number.isInteger(port) || port < 1 || port > 65535) return null;
  return { host: m[1], port };
}

function isLoopback(host) {
  if (host === "localhost") return true;
  if (/^127\./.test(host)) return true;
  return host === "::1" || host === "[::1]";
}

/** Format a server duration value (Go compound "1h0m0s", spec "5m", or bare
    seconds) in the single-suffix spec spelling the form hints use. */
export function fmtSpecDuration(v) {
  const s = String(v ?? "").trim();
  if (s === "") return "";
  if (/^\d+(?:\.\d+)?(ms|s|m|h|d|w)$/.test(s)) return s; // already spec spelling
  let total = NaN;
  if (/^-?\d+(\.\d+)?$/.test(s)) {
    total = Number(s); // bare = seconds
  } else {
    total = 0;
    const parts = s.matchAll(/(\d+(?:\.\d+)?)(ms|us|µs|[hms])/g);
    let matched = false;
    for (const [, n, unit] of parts) {
      matched = true;
      total += Number(n) * { h: 3600, m: 60, s: 1, ms: 0.001, us: 1e-6, µs: 1e-6 }[unit];
    }
    if (!matched) return s; // unparseable → show as-is
  }
  if (!Number.isFinite(total)) return s;
  const steps = [["w", 604800], ["d", 86400], ["h", 3600], ["m", 60], ["s", 1]];
  for (const [unit, mul] of steps) {
    if (total >= mul && Number.isInteger(total / mul)) return `${total / mul}${unit}`;
  }
  return `${total}s`;
}

/** Format a server byte count in the spec spelling ("64GiB"), largest unit
    that divides evenly; bare bytes otherwise. */
export function fmtSpecSize(v) {
  const n = Number(v);
  if (v === null || v === undefined || s0(v) === "" || !Number.isFinite(n) || n < 0) return s0(v);
  const steps = [["TiB", 2 ** 40], ["GiB", 2 ** 30], ["MiB", 2 ** 20], ["KiB", 2 ** 10], ["B", 1]];
  for (const [unit, mul] of steps) {
    if (n >= mul && Number.isInteger(n / mul)) return `${n / mul}${unit}`;
  }
  return `${n}B`;
}

function s0(v) {
  return v === null || v === undefined ? "" : String(v);
}

const STRATEGY_KEYS = ["name", "kind", "base", "schedule", "keep", "backfill_max", "chain", "filter", "refs", "min_commits"];
const TOKEN_KEYS = ["principal", "token", "token_env", "write", "admin"];

/** Render the server's parsed array-of-struct values (bundles.strategy,
    server.auth.tokens) as the [[name]] TOML fragment the textarea edits —
    `name` is the field's toml name, `known` its struct's key order. Keys are
    normalized to the TOML spellings (the schema surfaces Go field names) and
    round-trip through the overrides channel. */
export function tomlFragment(list, name, known = []) {
  if (!Array.isArray(list) || list.length === 0) return "";
  const val = (v) => {
    if (typeof v === "string") return JSON.stringify(v);
    if (Array.isArray(v)) return `[${v.map((x) => JSON.stringify(String(x))).join(", ")}]`;
    return String(v);
  };
  const norm = (k) => known.find((s) => s.replace(/_/g, "").toLowerCase() === k.replace(/_/g, "").toLowerCase()) ?? k;
  return list
    .map((t) => {
      const entries = Object.entries(t).map(([k, v]) => [norm(k), v]);
      entries.sort(([a], [b]) => {
        const ia = known.indexOf(a), ib = known.indexOf(b);
        return (ia === -1 ? known.length : ia) - (ib === -1 ? known.length : ib) || a.localeCompare(b);
      });
      const body = entries
        .filter(([k, v]) => v !== undefined && v !== null && v !== "" && v !== false && v !== 0 && known.includes(k))
        .map(([k, v]) => `${k} = ${val(v)}`)
        .join("\n");
      return `[[${name}]]\n${body}`;
    })
    .join("\n\n");
}

/** Whether a field row applies to the effective server.auth.mode — FIELDS
    entries without `modes` are unconditional. */
export function fieldAppliesToMode(field, mode) {
  return !field?.modes || field.modes.includes(mode);
}

// --- field metadata: type + enum + example per 11_config_cli.md §2 ------------
//
// `ex` is a WORKING example value: it passes validateSetup on its own and is
// accepted by the server validator in isolation (web/test/unit/setup-form.test.js
// enforces both). The setup page shows it under the label, always visible —
// unlike a placeholder it does not vanish while typing. `note` is a short
// consequence/companion hint for fields whose validity depends on other fields.

export const FIELDS = [
  // server
  { key: "server.listen", type: "listen", ex: "0.0.0.0:8080", note: "127.0.0.1:8080 = loopback only" },
  { key: "server.http2", type: "bool", ex: "true" },
  { key: "server.max_concurrent_requests", type: "int", min: 1, ex: "512" },
  { key: "server.max_concurrent_per_repo", type: "int", min: 1, ex: "32" },
  { key: "server.request_timeout", type: "duration", ex: "1h" },
  { key: "server.drain_timeout", type: "duration", ex: "30s" },
  { key: "server.max_push_bytes", type: "size", ex: "2GiB" },
  { key: "server.roles", type: "list", enum: ["serve", "maintain", "events"], ex: "serve, maintain" },
  { key: "server.auto_create_on_push", type: "bool", ex: "true" },
  { key: "server.accel_redirect", type: "bool", ex: "true", note: "only honoured behind an edge that announces accel-redirect" },
  { key: "server.public_url", type: "url", ex: "https://git.example.com" },
  { key: "server.cors_origins", type: "list", ex: "https://git.example.com" },
  { key: "server.tls.mode", type: "enum", enum: ["off", "self_signed", "files"], ex: "self_signed", note: "files also requires tls.cert and tls.key" },
  { key: "server.tls.cert", type: "string", ex: "/etc/walhub/tls/fullchain.pem" },
  { key: "server.tls.key", type: "string", ex: "/etc/walhub/tls/privkey.pem" },
  { key: "server.tls.hostnames", type: "list", ex: "git.example.com, walhub.local" },
  { key: "server.auth.mode", type: "enum", enum: ["none", "token", "oidc"], ex: "token", note: "oidc additionally needs issuer, an allowlist, and anonymous_read=false" },
  { key: "server.auth.anonymous_read", type: "bool", ex: "false", modes: ["token", "oidc"], note: "must be false in oidc mode" },
  { key: "server.auth.tokens", type: "toml", modes: ["token", "oidc"], tomlKeys: TOKEN_KEYS, ex: '[[tokens]]\nprincipal = "ci"\ntoken_env = "WALHUB_CI_TOKEN"\nwrite = true', note: "robots/static credentials — one [[tokens]] table each; admin = true grants admin" },
  { key: "server.auth.session_secret", type: "string", ex: "b3f1c0a9d8e27f645c31b0a98d7e6f5c4a3b2c1d0e9f8a7b6c5d4e3f2a1b0c9d", modes: ["oidc"], note: "any random ≥ 32 bytes (openssl rand -hex 32); rotating revokes sessions" },
  { key: "server.auth.session_ttl", type: "duration", ex: "12h", modes: ["oidc"] },
  { key: "server.auth.access_token_ttl", type: "duration", ex: "24h", modes: ["oidc"] },
  { key: "server.auth.issuer", type: "url", ex: "https://id.example.com", modes: ["oidc"], note: "discovery at <issuer>/.well-known/openid-configuration" },
  { key: "server.auth.allowed_domains", type: "list", ex: "example.com", modes: ["oidc"] },
  { key: "server.auth.allowed_emails", type: "list", ex: "alice@example.com", modes: ["oidc"] },
  { key: "server.auth.write_domains", type: "list", ex: "example.com", modes: ["oidc"], note: "omit = every admitted identity may write" },
  { key: "server.auth.oauth_client_id", type: "string", ex: "walhub-web", modes: ["oidc"], note: "client_id and client_secret go together" },
  { key: "server.auth.oauth_client_secret", type: "string", ex: "client-secret-from-the-issuer", modes: ["oidc"] },
  { key: "server.auth.audiences", type: "list", ex: "walhub-web", modes: ["oidc"] },
  { key: "server.auth.trusted_forwarders", type: "list", ex: "edge.internal", modes: ["token", "oidc"] },
  { key: "server.auth.admin_emails", type: "list", ex: "admin@example.com", modes: ["oidc"] },
  { key: "server.auth.admin_domains", type: "list", ex: "example.com", modes: ["oidc"] },
  // server ssh (17_ssh.md) — public keys are user-managed data in the object
  // store (/api/v1/ssh-keys, the /keys page), not config
  { key: "server.ssh.listen", type: "listen", ex: "0.0.0.0:2222", note: "empty = the SSH transport is disabled" },
  { key: "server.ssh.host_key", type: "string", ex: "/var/lib/walhub/ssh/ed25519_host_key", note: "auto-generated there when empty and listen is set" },
  { key: "server.ssh.host_key_env", type: "string", ex: "WALHUB_SSH_HOST_KEY", note: "names the env var holding the key; overrides host_key" },
  // store
  { key: "store.backend", type: "enum", enum: ["s3", "gcs", "memory", "filesystem"], ex: "filesystem", note: "s3/gcs also need store.bucket and their subsection" },
  { key: "store.bucket", type: "string", ex: "walhub-test" },
  { key: "store.prefix", type: "string", ex: "walhub/" },
  { key: "store.root", type: "path", ex: "/var/lib/walhub/store", note: "filesystem backend only" },
  { key: "store.max_retries", type: "int", min: 0, ex: "3" },
  { key: "store.multipart_threshold", type: "size", ex: "64MiB" },
  { key: "store.multipart_part_size", type: "size", ex: "16MiB", note: "must be ≤ multipart_threshold" },
  { key: "store.s3.endpoint", type: "url", ex: "http://rustfs:9000", note: "rustfs/MinIO style; omit for AWS" },
  { key: "store.s3.region", type: "string", ex: "us-east-1" },
  { key: "store.s3.access_key_env", type: "string", ex: "AWS_ACCESS_KEY_ID", note: "names the env var holding the key" },
  { key: "store.s3.secret_key_env", type: "string", ex: "AWS_SECRET_ACCESS_KEY" },
  { key: "store.s3.force_path_style", type: "bool", ex: "true", note: "true for rustfs/MinIO; AWS uses virtual-host addressing" },
  { key: "store.gcs.endpoint", type: "url", ex: "http://localhost:4443", note: "fake-gcs-server in dev; omit for real GCS" },
  { key: "store.gcs.signing_service_account", type: "string", ex: "signer@project.iam.gserviceaccount.com" },
  { key: "store.gcs.bulk_clients", type: "int", min: 1, ex: "4" },
  { key: "store.gcs.bulk_concurrency", type: "int", min: 1, ex: "8" },
  // cache
  { key: "cache.dir", type: "path", ex: "/var/cache/walhub" },
  { key: "cache.mode", type: "enum", enum: ["budget", "disk", "auto"], ex: "budget" },
  { key: "cache.max_bytes", type: "size", ex: "20GiB", note: "everything on disk must fit this in budget mode" },
  { key: "cache.disk_high_watermark", type: "float", min: 0, max: 1, ex: "0.9" },
  { key: "cache.evict_idle_after", type: "duration", ex: "7d" },
  { key: "cache.prewarm", type: "list", ex: "acme/monorepo" },
  { key: "cache.prewarm_parallelism", type: "int", min: 1, ex: "4" },
  { key: "cache.prewarm_ready_timeout", type: "duration", ex: "30s" },
  { key: "cache.ref_advert_entries", type: "int", min: 1, ex: "4096" },
  { key: "cache.object_info_entries", type: "int", min: 1, ex: "4096" },
  { key: "cache.bundle_list_entries", type: "int", min: 1, ex: "128" },
  { key: "cache.remote_block_bytes", type: "size", ex: "1MiB" },
  { key: "cache.remote_object_bytes", type: "size", ex: "4MiB" },
  { key: "cache.shared_render_cache", type: "bool", ex: "true" },
  { key: "cache.store_mount", type: "path", ex: "/mnt/bucket", note: "read-only mount of the store bucket, if you have one" },
  // wal
  { key: "wal.batch_window", type: "duration", ex: "200ms" },
  { key: "wal.max_batch", type: "int", min: 1, ex: "64" },
  { key: "wal.push_broker_url", type: "url", ex: "http://broker:8080", note: "hosts that maintain nothing forward pushes here" },
  { key: "wal.push_broker_token", type: "string", ex: "broker-shared-secret" },
  { key: "wal.push_broker_buffer_bytes", type: "size", ex: "32MiB" },
  { key: "wal.snapshot_every_entries", type: "int", min: 1, ex: "1000" },
  { key: "wal.checkpoint_interval", type: "duration", ex: "10m" },
  { key: "wal.checkpoint_tail_bytes", type: "size", ex: "64MiB" },
  { key: "wal.cas_max_retries", type: "int", min: 1, ex: "5" },
  { key: "wal.fsck_objects", type: "bool", ex: "true" },
  { key: "wal.check_connectivity", type: "bool", ex: "true" },
  { key: "wal.freshness_ttl", type: "duration", ex: "30s" },
  { key: "wal.prefetch_packs", type: "bool", ex: "true" },
  { key: "wal.prefetch_max_bytes", type: "size", ex: "64MiB" },
  { key: "wal.remote_objects", type: "bool", ex: "true" },
  // maintenance
  { key: "maintenance.interval", type: "duration", ex: "1m" },
  { key: "maintenance.checkpoints", type: "bool", ex: "true" },
  { key: "maintenance.max_pack_bytes", type: "size", ex: "2GiB" },
  { key: "maintenance.disk", type: "enum", enum: ["tmpfs", "ssd"], ex: "ssd" },
  { key: "maintenance.host", type: "string", ex: "build-01" },
  { key: "maintenance.fsck_interval", type: "duration", ex: "24h" },
  { key: "maintenance.follow_interval", type: "duration", ex: "5m" },
  // placement
  { key: "placement.serve", type: "globs", ex: "*, acme/*" },
  { key: "placement.serve_exclude", type: "globs", ex: "secret/*" },
  { key: "placement.maintain", type: "globs", ex: "*" },
  { key: "placement.maintain_exclude", type: "globs", ex: "archive/*" },
  { key: "bundles.strategy", type: "toml", tomlKeys: STRATEGY_KEYS, ex: '[[strategy]]\nname = "weekly"\nkind = "full"\nschedule = "0 0 23 * * 0"\nkeep = 2', note: "TOML array of [[strategy]] — replaces the built-in weekly/daily/hourly set" },
  { key: "compaction.enabled", type: "bool", ex: "true" },
  { key: "compaction.factor", type: "float", min: 1, ex: "4" },
  { key: "compaction.trigger_packs", type: "int", min: 2, ex: "8" },
  { key: "compaction.trigger_bytes", type: "size", ex: "512MiB" },
  { key: "compaction.lease_ttl", type: "duration", ex: "10m" },
  { key: "compaction.retention_superseded", type: "duration", ex: "7d" },
  { key: "compaction.engine", type: "enum", enum: ["git", "gix"], ex: "git" },
  // bundles
  { key: "bundles.strategy", type: "toml", ex: '[[strategy]]\nname = "weekly"\nkind = "full"\nschedule = "0 0 23 * * 0"\nkeep = 2', note: "TOML array of [[strategy]] — replaces the built-in weekly/daily/hourly set" },
  { key: "bundles.min_commits", type: "int", min: 0, ex: "25" },
  { key: "bundles.min_bytes", type: "size", ex: "1MiB" },
  { key: "bundles.main_only", type: "bool", ex: "true" },
  { key: "bundles.extra_refs", type: "list", ex: "refs/heads/release" },
  { key: "bundles.serve_via", type: "enum", enum: ["proxy", "signed_url"], ex: "proxy" },
  { key: "bundles.signed_url_ttl", type: "duration", ex: "15m" },
  { key: "bundles.signed_url_for", type: "list", ex: "acme/monorepo" },
  { key: "bundles.advertise", type: "bool", ex: "true" },
  { key: "bundles.advertise_filtered", type: "bool", ex: "true" },
  { key: "bundles.require", type: "list", ex: "acme/monorepo", note: "listed repos refuse full clones that skip bundle-uri" },
  // lfs
  { key: "lfs.enabled", type: "bool", ex: "true" },
  { key: "lfs.serve_via", type: "enum", enum: ["proxy", "signed_url"], ex: "proxy" },
  { key: "lfs.signed_url_ttl", type: "duration", ex: "15m" },
  { key: "lfs.max_object_bytes", type: "size", ex: "10GiB" },
  // upstream
  { key: "upstream.git", type: "url", ex: "https://github.com/acme/widgets.git", note: "source the follow loop pulls refs from" },
  { key: "upstream.lfs", type: "url", ex: "https://github.com/acme/widgets.git/info/lfs" },
  { key: "upstream.token_env", type: "string", ex: "WALHUB_UPSTREAM_TOKEN", note: "names the env var holding the token" },
  { key: "upstream.follow", type: "bool", ex: "true" },
  // git
  { key: "git.binary", type: "string", ex: "/usr/bin/git" },
  { key: "git.upload_pack_engine", type: "enum", enum: ["auto", "git", "gix"], ex: "auto" },
  { key: "git.allow_filter", type: "bool", ex: "true" },
  { key: "git.allow_any_sha1_in_want", type: "bool", ex: "false" },
  { key: "git.object_format", type: "enum", enum: ["sha1", "sha256"], ex: "sha1" },
  { key: "git.commit_graph", type: "bool", ex: "true" },
  { key: "git.commit_graph_changed_paths", type: "bool", ex: "true" },
  { key: "git.history_pack", type: "bool", ex: "true" },
  { key: "git.max_wants", type: "int", min: 0, ex: "100000" },
  // telemetry
  { key: "telemetry.log_format", type: "enum", enum: ["pretty", "json"], ex: "json" },
  { key: "telemetry.log_filter", type: "string", ex: "walhub=debug" },
  { key: "telemetry.metrics", type: "bool", ex: "true" },
  { key: "telemetry.lock_wait_warn", type: "duration", ex: "5s" },
  // events
  { key: "events.webhook_url", type: "url", ex: "http://ci.example.com/hooks/walhub" },
  { key: "events.webhook_secret", type: "string", ex: "hmac-shared-secret" },
  { key: "events.sweep_interval", type: "duration", ex: "1m" },
  // import (docs/features/10 §6 — fail-closed defaults: empty allowlist,
  // dangerous opt-in per import)
  { key: "import.url_allowlist", type: "list", ex: "git.example.com", note: "empty = GitHub always; other hosts need the list or the per-import dangerous confirm" },
  { key: "import.allow_private_networks", type: "bool", ex: "false", note: "true allows loopback/RFC1918 sources" },
  { key: "import.allow_file_urls", type: "bool", ex: "false", note: "true enables file:// sources (tests/fixtures only)" },
  { key: "import.clone_timeout", type: "duration", ex: "30m" },
  { key: "import.git_timeout", type: "duration", ex: "5m" },
  { key: "import.max_bytes", type: "size", ex: "64GiB", note: "set with server.max_push_bytes when constraining" },
  { key: "import.max_refs", type: "int", min: 1, ex: "100000" },
  { key: "import.max_concurrent", type: "int", min: 1, ex: "2" },
];

const FIELD_BY_KEY = new Map(FIELDS.map((f) => [f.key, f]));

// --- normalize (inputs are strings; the payload wants typed values) -----------

/** Coerce flat {key: inputValue} into {overrides: {key: typedValue}} for the API. */
export function normalizeSetup(values) {
  const overrides = {};
  for (const [key, raw] of Object.entries(values ?? {})) {
    const field = FIELD_BY_KEY.get(key);
    const s = raw === undefined || raw === null ? "" : String(raw).trim();
    if (s === "") continue; // absent = keep default
    let v = s;
    if (field?.type === "bool") v = /^(true|yes|on|1)$/i.test(s);
    else if (field?.type === "int") v = Number(s);
    else if (field?.type === "float") v = Number(s);
    else if (field?.type === "list") v = asList(s);
    else if (field?.type === "globs") v = asList(s);
    overrides[key] = v;
  }
  return { overrides };
}

// --- the validator (mirrors 11_config_cli.md §5, rule by rule) ----------------

/**
 * validateSetup(values) → [{key, message, severity}] — severity "error" fails
 * validation; "warn" mirrors the server's warn-only divergence (auth none on a
 * non-loopback bind) and never blocks.
 */
export function validateSetup(values) {
  const errors = [];
  const effectiveBackend = (() => {
    const b = values?.["store.backend"];
    return b === undefined || b === null || b === "" ? "filesystem" : String(b).trim(); // first-run default
  })();
  const fail = (key, message) => errors.push({ key, message, severity: "error" });
  const warn = (key, message) => errors.push({ key, message, severity: "warn" });
  const get = (key) => {
    const v = values?.[key];
    return v === undefined || v === null ? "" : String(v).trim();
  };
  const num = (key) => (get(key) === "" ? NaN : Number(get(key)));

  for (const [key, raw] of Object.entries(values ?? {})) {
    const field = FIELD_BY_KEY.get(key);
    if (!field) {
      errors.push({ key, message: `unknown key: ${key}`, severity: "error" }); // unknown keys are a validation error, not a silent drop
      continue;
    }
    const s = raw === undefined || raw === null ? "" : String(raw).trim();
    if (s === "") continue;
    const t = field.type;
    if (t === "int" || t === "float") {
      const n = Number(s);
      if (!Number.isFinite(n)) { fail(key, `${key}: must be a number, got "${s}"`); continue; }
      if (t === "int" && !Number.isInteger(n)) { fail(key, `${key}: must be an integer, got "${s}"`); continue; }
      if (field.min !== undefined && n < field.min) { fail(key, `${key}: must be ≥ ${field.min}, got ${s}`); continue; }
      if (field.max !== undefined && n > field.max) { fail(key, `${key}: must be ≤ ${field.max}, got ${s}`); continue; }
    } else if (t === "enum") {
      if (!field.enum.includes(s)) fail(key, `${key}: must be one of ${field.enum.join("|")}, got "${s}"`);
    } else if (t === "bool") {
      if (!/^(true|false|yes|no|on|off|1|0)$/i.test(s)) fail(key, `${key}: must be true or false, got "${s}"`);
    } else if (t === "duration") {
      if (!/^\d+$/.test(s) && !DURATION_RE.test(s)) fail(key, `${key}: not a duration, got "${s}"`);
    } else if (t === "size") {
      if (!/^\d+$/.test(s) && !SIZE_RE.test(s)) fail(key, `${key}: not a size, got "${s}"`);
    } else if (t === "url") {
      if (s !== "" && !/^https?:\/\//.test(s)) fail(key, `${key}: must be an http(s) URL, got "${s}"`);
    } else if (t === "listen") {
      if (s !== "" && parseListen(s) === null) fail(key, `${key}: must be host:port, got "${s}"`);
    } else if (t === "path") {
      // §5 rule 10: cache.dir must be absolute UNLESS the backend is memory
      if (s !== "" && !(key === "cache.dir" && effectiveBackend === "memory") && !ABS_PATH_RE_OK(s)) {
        fail(key, `${key}: must be an absolute path, got "${s}"`);
      }
    } else if (t === "list" && field.enum) {
      for (const item of asList(raw)) {
        if (!field.enum.includes(item)) { fail(key, `${key}: must be one of ${field.enum.join("|")}, got "${item}"`); break; }
      }
    } else if (t === "globs") {
      for (const g of asList(raw)) {
        if (g !== "*" && !/^[^/]+\/(?:\*|[^/]+)$/.test(g)) fail(key, `${key}: placement glob must be "*", "owner/*" or "owner/name", got "${g}"`);
      }
    }
  }

  const listen = parseListen(get("server.listen") || "0.0.0.0:8080");
  const backend = get("store.backend") || "filesystem"; // first-run default
  const authMode = get("server.auth.mode") || "none";

  // §5 rule 1 — auth none on a non-loopback bind: WARN only (deliberate divergence)
  if (authMode === "none" && listen && !isLoopback(listen.host)) {
    warn("server.auth.mode", `auth.mode=none on non-loopback listen ${listen.host}:${listen.port}; anyone who can reach this port can read and write every repository — set server.auth.mode = "token" (or oidc) and restart`);
  }
  // §5 rule 2 — oidc allowlist + oauth pair + session secret length
  if (authMode === "oidc") {
    // the effective default of anonymous_read is true — unset also fails in oidc mode
    const anon = get("server.auth.anonymous_read");
    if (anon === "" || /^(true|1|on|yes)$/i.test(anon)) fail("server.auth.anonymous_read", `server.auth.anonymous_read must be false in oidc mode`);
    if (asList(values["server.auth.allowed_domains"]).length === 0 && asList(values["server.auth.allowed_emails"]).length === 0) {
      fail("server.auth.allowed_domains", `oidc mode requires a non-empty server.auth.allowed_domains or server.auth.allowed_emails allowlist`);
    }
    const id = get("server.auth.oauth_client_id");
    const secret = get("server.auth.oauth_client_secret");
    if ((id === "") !== (secret === "")) fail("server.auth.oauth_client_id", `server.auth.oauth_client_id and oauth_client_secret must be both set or both unset`);
  }
  const secret = get("server.auth.session_secret");
  if (secret !== "" && secret.length < 32) fail("server.auth.session_secret", `server.auth.session_secret must be ≥ 32 bytes when set`);

  // §5 rule 8 — tls files mode needs cert+key
  const tls = get("server.tls.mode") || "off";
  if (tls === "files" && (get("server.tls.cert") === "" || get("server.tls.key") === "")) {
    fail("server.tls.cert", `server.tls.mode = "files" requires server.tls.cert and server.tls.key`);
  }

  // §5 rule 5 — store backend specifics
  if (backend === "filesystem") {
    const root = get("store.root");
    if (root !== "" && !ABS_PATH_RE_OK(root)) fail("store.root", `store.root must be an absolute path or empty, got "${root}"`);
  }
  const thresh = get("store.multipart_threshold");
  const part = get("store.multipart_part_size");
  if (thresh !== "" && part !== "") {
    const t = parseSize(thresh);
    const p = parseSize(part);
    if (Number.isFinite(t) && Number.isFinite(p) && t > 0 && p > t) {
      fail("store.multipart_part_size", `store.multipart_part_size (${part}) must be ≤ multipart_threshold (${thresh})`);
    }
  }

  // §5 rule 10 — cache.dir absolute unless the memory backend
  const cacheDir = get("cache.dir");
  if (cacheDir !== "" && backend !== "memory" && !ABS_PATH_RE_OK(cacheDir)) {
    fail("cache.dir", `cache.dir must be an absolute path, got "${cacheDir}"`);
  }
  // §5 rule 6 — disk_high_watermark ∈ (0,1) or 0
  const wm = get("cache.disk_high_watermark");
  if (wm !== "") {
    const w = Number(wm);
    if (Number.isFinite(w) && w !== 0 && !(w > 0 && w < 1)) {
      fail("cache.disk_high_watermark", `cache.disk_high_watermark must be 0 or strictly between 0 and 1, got ${wm}`);
    }
  }

  return errors;
}

function ABS_PATH_RE_OK(p) {
  return ABS_PATH_RE.test(String(p ?? "").trim());
}

/**
 * Which changed keys plausibly need a restart (§2.10 hint list): everything
 * under server.* plus store.backend / store.root. The save response's
 * requires_restart list is authoritative; this only annotates the UI.
 */
export function isRestartLikely(key) {
  return key === "store.backend" || key === "store.root" || key.startsWith("server.");
}