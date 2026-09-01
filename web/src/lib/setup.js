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

// --- field metadata: type + enum membership per 11_config_cli.md §2 -----------

export const FIELDS = [
  // server
  { key: "server.listen", type: "listen" },
  { key: "server.http2", type: "bool" },
  { key: "server.max_concurrent_requests", type: "int", min: 1 },
  { key: "server.max_concurrent_per_repo", type: "int", min: 1 },
  { key: "server.request_timeout", type: "duration" },
  { key: "server.drain_timeout", type: "duration" },
  { key: "server.max_push_bytes", type: "size" },
  { key: "server.roles", type: "list", enum: ["serve", "maintain", "events"] },
  { key: "server.auto_create_on_push", type: "bool" },
  { key: "server.accel_redirect", type: "bool" },
  { key: "server.public_url", type: "url" },
  { key: "server.cors_origins", type: "list" },
  { key: "server.tls.mode", type: "enum", enum: ["off", "self_signed", "files"] },
  { key: "server.tls.cert", type: "string" },
  { key: "server.tls.key", type: "string" },
  { key: "server.tls.hostnames", type: "list" },
  { key: "server.auth.mode", type: "enum", enum: ["none", "token", "oidc"] },
  { key: "server.auth.anonymous_read", type: "bool" },
  { key: "server.auth.session_secret", type: "string" },
  { key: "server.auth.session_ttl", type: "duration" },
  { key: "server.auth.access_token_ttl", type: "duration" },
  { key: "server.auth.issuer", type: "url" },
  { key: "server.auth.allowed_domains", type: "list" },
  { key: "server.auth.allowed_emails", type: "list" },
  { key: "server.auth.write_domains", type: "list" },
  { key: "server.auth.oauth_client_id", type: "string" },
  { key: "server.auth.oauth_client_secret", type: "string" },
  { key: "server.auth.audiences", type: "list" },
  { key: "server.auth.trusted_forwarders", type: "list" },
  { key: "server.auth.admin_emails", type: "list" },
  { key: "server.auth.admin_domains", type: "list" },
  // store
  { key: "store.backend", type: "enum", enum: ["s3", "gcs", "memory", "filesystem"] },
  { key: "store.bucket", type: "string" },
  { key: "store.prefix", type: "string" },
  { key: "store.root", type: "path" },
  { key: "store.max_retries", type: "int", min: 0 },
  { key: "store.multipart_threshold", type: "size" },
  { key: "store.multipart_part_size", type: "size" },
  { key: "store.s3.endpoint", type: "url" },
  { key: "store.s3.region", type: "string" },
  { key: "store.s3.access_key_env", type: "string" },
  { key: "store.s3.secret_key_env", type: "string" },
  { key: "store.s3.force_path_style", type: "bool" },
  { key: "store.gcs.endpoint", type: "url" },
  { key: "store.gcs.signing_service_account", type: "string" },
  { key: "store.gcs.bulk_clients", type: "int", min: 1 },
  { key: "store.gcs.bulk_concurrency", type: "int", min: 1 },
  // cache
  { key: "cache.dir", type: "path" },
  { key: "cache.mode", type: "enum", enum: ["budget", "disk", "auto"] },
  { key: "cache.max_bytes", type: "size" },
  { key: "cache.disk_high_watermark", type: "float", min: 0, max: 1 },
  { key: "cache.evict_idle_after", type: "duration" },
  { key: "cache.prewarm", type: "list" },
  { key: "cache.prewarm_parallelism", type: "int", min: 1 },
  { key: "cache.prewarm_ready_timeout", type: "duration" },
  { key: "cache.ref_advert_entries", type: "int", min: 1 },
  { key: "cache.object_info_entries", type: "int", min: 1 },
  { key: "cache.bundle_list_entries", type: "int", min: 1 },
  { key: "cache.remote_block_bytes", type: "size" },
  { key: "cache.remote_object_bytes", type: "size" },
  { key: "cache.shared_render_cache", type: "bool" },
  { key: "cache.store_mount", type: "path" },
  // wal
  { key: "wal.batch_window", type: "duration" },
  { key: "wal.max_batch", type: "int", min: 1 },
  { key: "wal.push_broker_url", type: "url" },
  { key: "wal.push_broker_token", type: "string" },
  { key: "wal.push_broker_buffer_bytes", type: "size" },
  { key: "wal.snapshot_every_entries", type: "int", min: 1 },
  { key: "wal.checkpoint_interval", type: "duration" },
  { key: "wal.checkpoint_tail_bytes", type: "size" },
  { key: "wal.cas_max_retries", type: "int", min: 1 },
  { key: "wal.fsck_objects", type: "bool" },
  { key: "wal.check_connectivity", type: "bool" },
  { key: "wal.freshness_ttl", type: "duration" },
  { key: "wal.prefetch_packs", type: "bool" },
  { key: "wal.prefetch_max_bytes", type: "size" },
  { key: "wal.remote_objects", type: "bool" },
  // maintenance
  { key: "maintenance.interval", type: "duration" },
  { key: "maintenance.checkpoints", type: "bool" },
  { key: "maintenance.max_pack_bytes", type: "size" },
  { key: "maintenance.disk", type: "enum", enum: ["tmpfs", "ssd"] },
  { key: "maintenance.host", type: "string" },
  { key: "maintenance.fsck_interval", type: "duration" },
  { key: "maintenance.follow_interval", type: "duration" },
  // placement
  { key: "placement.serve", type: "globs" },
  { key: "placement.serve_exclude", type: "globs" },
  { key: "placement.maintain", type: "globs" },
  { key: "placement.maintain_exclude", type: "globs" },
  // compaction
  { key: "compaction.enabled", type: "bool" },
  { key: "compaction.factor", type: "float", min: 1 },
  { key: "compaction.trigger_packs", type: "int", min: 2 },
  { key: "compaction.trigger_bytes", type: "size" },
  { key: "compaction.lease_ttl", type: "duration" },
  { key: "compaction.retention_superseded", type: "duration" },
  { key: "compaction.engine", type: "enum", enum: ["git", "gix"] },
  // bundles
  { key: "bundles.strategy", type: "toml" },
  { key: "bundles.min_commits", type: "int", min: 0 },
  { key: "bundles.min_bytes", type: "size" },
  { key: "bundles.main_only", type: "bool" },
  { key: "bundles.extra_refs", type: "list" },
  { key: "bundles.serve_via", type: "enum", enum: ["proxy", "signed_url"] },
  { key: "bundles.signed_url_ttl", type: "duration" },
  { key: "bundles.signed_url_for", type: "list" },
  { key: "bundles.advertise", type: "bool" },
  { key: "bundles.advertise_filtered", type: "bool" },
  { key: "bundles.require", type: "list" },
  // lfs
  { key: "lfs.enabled", type: "bool" },
  { key: "lfs.serve_via", type: "enum", enum: ["proxy", "signed_url"] },
  { key: "lfs.signed_url_ttl", type: "duration" },
  { key: "lfs.max_object_bytes", type: "size" },
  // upstream
  { key: "upstream.git", type: "url" },
  { key: "upstream.lfs", type: "url" },
  { key: "upstream.token_env", type: "string" },
  { key: "upstream.follow", type: "bool" },
  // git
  { key: "git.binary", type: "string" },
  { key: "git.upload_pack_engine", type: "enum", enum: ["auto", "git", "gix"] },
  { key: "git.allow_filter", type: "bool" },
  { key: "git.allow_any_sha1_in_want", type: "bool" },
  { key: "git.object_format", type: "enum", enum: ["sha1", "sha256"] },
  { key: "git.commit_graph", type: "bool" },
  { key: "git.commit_graph_changed_paths", type: "bool" },
  { key: "git.history_pack", type: "bool" },
  { key: "git.max_wants", type: "int", min: 0 },
  // telemetry
  { key: "telemetry.log_format", type: "enum", enum: ["pretty", "json"] },
  { key: "telemetry.log_filter", type: "string" },
  { key: "telemetry.metrics", type: "bool" },
  { key: "telemetry.lock_wait_warn", type: "duration" },
  // events
  { key: "events.webhook_url", type: "url" },
  { key: "events.webhook_secret", type: "string" },
  { key: "events.sweep_interval", type: "duration" },
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