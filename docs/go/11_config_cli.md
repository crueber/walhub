# 11 — Configuration & CLI (`internal/config`, `cmd/walhub`)

> Source: MASTER_RUST_SPEC.md §15 (configuration reference, per-repo settings D24), §16 (CLI reference), §19 (porting notes: clap/exit codes) · Status: normative for the walhub Go implementation.

The binary is `walhub` (module `git.packden.us/crueber/walhub`). It reads the same config files, honors the same environment variables, emits the same exit codes, and exposes the same subcommand surface as the Rust `walgit`, so operators' `walgit.toml`, deployment scripts, and shell habits carry over unchanged. Config parsing uses `github.com/BurntSushi/toml` — the only TOML dependency allowed, and one of exactly three backend dependencies in the budget (`go-chi/chi/v5`, `BurntSushi/toml`, `golang.org/x/net`). One deliberate divergence: **a config file is now OPTIONAL** — a first run with no file boots on built-in defaults (§2.3), and the default file location lives under the data dir (§3.1). All other CLI mechanics (dispatch, flag parsing, duration/size parsing) are hand-rolled or stdlib.

---

## 1. Layering and packages

```
cmd/walhub/            main.go: build-info version, global --config and --data-dir, dispatcher, exit-code mapping
internal/config/       file+env loading, decoding, validation, per-repo settings, effective-config dump
internal/config/       (files) config.go, env.go, validate.go, size.go, repo_settings.go, check.go
```

`internal/config` owns everything the rest of the system consumes:

```go
// internal/config/config.go
type Config struct {  // field layout mirrors the §2 key table 1:1; TOML tags keep the Rust key names
    Server    ServerConfig
    Store     StoreConfig
    Cache     CacheConfig
    Wal       WalConfig
    Maintenance MaintenanceConfig
    Placement PlacementConfig
    Compaction CompactionConfig
    Bundles   BundlesConfig
    LFS       LFSConfig  `toml:"lfs"`
    Upstream  UpstreamConfig
    Git       GitConfig
    Telemetry TelemetryConfig
    Events    EventsConfig
}
// Load(filePaths []string, env func(string) string) (*Config, []Override, error) — file, then env
// overlay, then Validate (fail-closed). Returns applied-but-ignored overrides for `config check`.
```

Every consumer receives `*config.Config` (read-only after load; the server treats it as immutable). Duration and byte-size keys are decoded from TOML strings (`"1h"`, `"64GiB"`, `"0B"`) into `time.Duration` / `int64` by the same parser (`size.go`): unit suffixes `ms|s|m|h|d|w` for durations (`d`/`w` only in config strings, never in Go literals) and `B|KiB|MiB|GiB|TiB` (case-insensitive, binary) for sizes; a bare integer for a size means bytes, a bare integer for a duration means seconds. `0`/`"0B"`/`"0s"` mean "disabled" where the table says so. This parser is shared with the per-repo settings merge so `[bundles] min_bytes` accepts the same syntax in both places.

## 2. The complete key table

Copied from Rust spec §15.1; key names, defaults, and meanings are **normative and unchanged**. The Go `Config` struct paths use the same section/field names.

| Key | Default | Meaning |
|---|---|---|
| `server.listen` | `"127.0.0.1:8080"` | bind address; loopback twin for `*.localhost`; `none` auth on a non-loopback bind warns (divergence, §5) † |
| `server.http2` | `true` | h2c / ALPN (Go: `golang.org/x/net/http2` h2c handler) |
| `server.max_concurrent_requests` | `512` | global in-flight cap (advisory) |
| `server.max_concurrent_per_repo` | `64` | per-repo git semaphore |
| `server.request_timeout` | `"1h"` | documented cap |
| `server.drain_timeout` | `"20s"` | phase-2 drain: in-flight requests finish; new work refused |
| `server.max_push_bytes` | `"64GiB"` | largest accepted push |
| `server.roles` | `[]` | `serve` / `maintain` (implies compact+bundle) / `events`; empty = all |
| `server.auto_create_on_push` | `false` | create a repo on first push † |
| `server.accel_redirect` | `false` | answer byte requests with X-Accel-Redirect (edge-fronted hosts only) |
| `server.public_url` | — | pins absolute URIs (bundle-uri, LFS, recipes, OAuth callback) behind a proxy |
| `server.cors_origins` | `[]` | exact or one leading `*.`; empty = no cross-origin lane |
| `server.tls.mode` | `"off"` | `off` \| `self_signed` (generated once under `<cache.dir>/tls/`, served at `/services/public/ca.pem`) \| `files` |
| `server.tls.cert/key/hostnames` | — | files-mode PEM; self-signed SANs (default localhost, *.localhost, 127.0.0.1, ::1 + public_url host) |
| `server.ssh.listen` | `""` | SSH git transport bind address; empty = disabled (17_ssh.md) |
| `server.ssh.host_key` / `host_key_env` | — | OpenSSH/PEM private key: path / env var NAME; auto-generated ed25519 under `<data-dir>/ssh/` when unset |
| `server.ssh.keys[]` | — | `{principal, key \| key_env, write, admin}`; public keys allowed to clone/fetch (and push with `write`) over SSH |
| `server.auth.mode` | `"none"` | `none` \| `token` \| `oidc` |
| `server.auth.anonymous_read` | `true` | must be false in oidc |
| `server.auth.tokens[]` | — | `{principal, token \| token_env, write, admin}`; robots allowed in oidc mode too |
| `server.auth.admin_emails / admin_domains` | `[]` | oidc admins |
| `server.auth.issuer` | — | discovery at `<issuer>/.well-known/openid-configuration` |
| `server.auth.allowed_domains / allowed_emails` | `[]` | admission (email_verified required) |
| `server.auth.write_domains` | — | omit = every admitted identity writes |
| `server.auth.oauth_client_id / oauth_client_secret` | — | browser sign-in; redirect `<public_url>/_auth/callback` |
| `server.auth.session_secret` | — | HMAC key ≥ 32 bytes for cookie + issued tokens (shared by every host; rotation revokes all) |
| `server.auth.session_ttl` | `"30d"` | sliding (re-issued at ttl/4) |
| `server.auth.access_token_ttl` | `"90d"` | `wgt_` token lifetime |
| `server.auth.audiences` | `[]` | accepted `aud` on bearer ID tokens (∪ oauth_client_id) |
| `server.auth.trusted_forwarders` | `[]` | principals allowed to set `X-Walgit-Principal` (push broker hop) |
| `store.backend` | `"s3"` | `s3` \| `gcs` \| `memory` \| `filesystem` (D4, 03_store_backends.md) † |
| `store.bucket` / `store.prefix` | `"walgit"` / `""` | bucket + key prefix |
| `store.root` | — | filesystem backend root (D4); first-run default `<data-dir>/store` (§2.3) † |
| `store.max_retries` | `8` | retryable store errors, jittered backoff |
| `store.multipart_threshold` / `multipart_part_size` | `"64MiB"` / `"32MiB"` | multipart thresholds |
| `store.s3.endpoint/region/access_key_env/secret_key_env/force_path_style` | AWS defaults | incl. rustfs/MinIO (path style true) |
| `store.gcs.endpoint` | Google endpoint | endpoint override (emulators) |
| `store.gcs.signing_service_account` | — | signer for `signed_url` serving |
| `store.gcs.bulk_clients` / `bulk_concurrency` | `4` / `32` | bulk transport separation (spec §4.6) |
| `cache.dir` | `"/tmp/walgit"` | local cache root (`<dir>/<owner>/<name>.git`; + `tls/`) † |
| `cache.mode` | `"auto"` | `budget` \| `disk` \| `auto` (= disk when `maintenance.disk = "ssd"`) |
| `cache.max_bytes` | `"20GiB"` | budget-mode cap for everything on disk |
| `cache.disk_high_watermark` | `0.9` | disk-mode eviction trigger (low mark = −0.10) |
| `cache.evict_idle_after` | `"6h"` | idle repos evicted (budget cap or watermark) |
| `cache.prewarm[]` / `prewarm_parallelism` / `prewarm_ready_timeout` | `[]` / `2` / `"0s"` | warm repos at startup; `/readyz` gating |
| `cache.ref_advert_entries` / `object_info_entries` / `bundle_list_entries` | `256` / `4096` / `128` | render cache sizes |
| `cache.remote_block_bytes` / `remote_object_bytes` | `"1GiB"` / `"256MiB"` | remote-reader block/decoded-object LRU |
| `cache.shared_render_cache` | `true` | mirror immutable API JSON into the bucket for all instances |
| `cache.store_mount` | — | read-only bucket mount; Serve syncs link tier-2 base packs from it |
| `wal.batch_window` / `max_batch` | `"5ms"` / `64` | group commit |
| `wal.push_broker_url` / `push_broker_token` / `push_broker_buffer_bytes` | — / — / `"64MiB"` | forward receive-pack to the single-writer broker; replayable fallback buffer |
| `wal.snapshot_every_entries` / `checkpoint_interval` / `checkpoint_tail_bytes` | `256` / `"1h"` / `"8MiB"` | checkpoint triggers |
| `wal.cas_max_retries` | `16` | manifest CAS retry cap |
| `wal.fsck_objects` / `check_connectivity` | `true` / `true` | pushed-object verification |
| `wal.freshness_ttl` | `"0s"` | 0 = always revalidate the manifest |
| `wal.prefetch_packs` / `prefetch_max_bytes` | `true` / `"1GiB"` | background Serve sync after refs-only; bound |
| `wal.remote_objects` | `true` | remote reader for too-large repos |
| `maintenance.interval` | `"60s"` | pass cadence |
| `maintenance.checkpoints` | `true` | unit 1 enabled |
| `maintenance.max_pack_bytes` | `"0B"` | declared capacity (0 = cache budget) |
| `maintenance.disk` | `"tmpfs"` | `ssd` = may rebuild bases |
| `maintenance.host` | instance id | heartbeat name |
| `maintenance.fsck_interval` | `"7d"` | 0 = off |
| `maintenance.follow_interval` | `"30s"` | upstream-follow loop cadence (0 = off) |
| `placement.serve` / `serve_exclude` / `maintain` / `maintain_exclude` | `["*"]` / `[]` / `["*"]` / `[]` | globs `owner/name` \| `owner/*` \| `*` |
| `compaction.enabled/factor/trigger_packs/trigger_bytes/lease_ttl/retention_superseded/engine` | `true`/`2`/`16`/`1GiB`/`10m`/`7d`/`"git"` | geometric fold; engine is git |
| `bundles.strategy[]` | weekly full + daily + hourly (see 08_bundles.md) | `{name, kind, base?, schedule, keep?, backfill_max, chain?, filter?, refs?, min_commits?}` |
| `bundles.min_commits` / `min_bytes` | `25` / `"0B"` | incremental gates |
| `bundles.main_only` / `extra_refs` | `true` / `[]` | bundle ref set |
| `bundles.serve_via` / `signed_url_ttl` / `signed_url_for` | `"proxy"` / `"1h"` / `[]` | byte path |
| `bundles.advertise` / `advertise_filtered` | `true` / `false` | v2 advertisement; filtered families only with the patched git |
| `bundles.require` | `[]` | repos whose unbounded zero-have clones must use bundle-uri (D17) |
| `lfs.enabled` / `serve_via` / `signed_url_ttl` / `max_object_bytes` | `true` / `"proxy"` / `"1h"` / `"16GiB"` | LFS |
| `upstream.git` / `upstream.lfs` / `upstream.token_env` / `upstream.follow` | — | repair source; LFS read-through; token env var (never the token); refs kept equal |
| `git.binary` | `"git"` | path to git binary (walhub always execs it; see 04_git.md) |
| `git.upload_pack_engine` | `"auto"` | `auto` = stock git wherever packs are local/mounted; remote-served bases per 04_git.md |
| `git.allow_filter` / `allow_any_sha1_in_want` | `true` / `false` | uploadpack.* |
| `git.object_format` | `"sha1"` | default for new repos |
| `git.commit_graph` / `commit_graph_changed_paths` | `true` / `false` | split commit-graph chain per repo; Bloom filters |
| `git.history_pack` | `true` | base rebuild also publishes the commits+trees pack |
| `git.max_wants` | `0` | refuse a fetch wanting more objects (0 = off) |
| `telemetry.log_format` / `log_filter` | `"pretty"` / `"info,walgit=debug"` | JSON or pretty logs; `RUST_LOG` env wins if set (accept `WALHUB_LOG` too, see §3.4) |
| `telemetry.metrics` | `true` | Prometheus on /metrics |
| `telemetry.lock_wait_warn` | `"1s"` | lock-wait WARN + histogram threshold |
| `events.webhook_url` / `webhook_secret` / `sweep_interval` | — / — / `"5m"` | events bridge (09_events.md) |

Notes:
- `git.upload_pack_engine`: the Rust `gix` engine has no Go equivalent and none is wanted (dependency law). In walhub the accepted values are still `auto | git | gix` for file compatibility, but `gix` is treated exactly as `git` at load time **with a one-time startup WARN naming the key** (ruling C-4: an inert config value must be visible to the operator); `auto` resolves to `git`. See 04_git.md for the upload-pack implementation.
- `telemetry.log_filter` keeps the Rust EnvFilter syntax as a string; the Go logger interprets the `info,walgit=debug` shape by mapping the `walgit` target to `walhub` (see §7.3 of 06_server_http.md — that doc owns logging).
- Bundle strategy array entries (`[[bundles.strategy]]`) decode in declaration order; `bundles.strategy` order is meaningful for §11 default families.
- Rows marked † have a different effective value on a zero-config first run; §2.3 is normative there. The table otherwise lists the compiled-in defaults, which equal the Rust-spec defaults.

### 2.1 Example: minimal config

```toml
# walgit.toml — one-box, loopback, token auth off, S3 default chain
[server]
listen = "127.0.0.1:8080"

[store]
backend = "s3"
bucket = "walgit"

[store.s3]
endpoint = "http://127.0.0.1:9000"   # rustfs/MinIO on the same box
region = "us-east-1"
force_path_style = true
access_key_env = "AWS_ACCESS_KEY_ID"
secret_key_env = "AWS_SECRET_ACCESS_KEY"

[cache]
dir = "/tmp/walgit"
```

### 2.2 Example: standalone-ish (public, token auth)

```toml
[server]
listen = "0.0.0.0:8443"
public_url = "https://git.example.com"

[server.tls]
mode = "self_signed"            # generated once under <cache.dir>/tls/; CA at /services/public/ca.pem

[server.auth]
mode = "token"
anonymous_read = false
tokens = [
  { principal = "alice", token_env = "WALGIT_TOKEN_ALICE", write = true, admin = true },
]

[store]
backend = "gcs"
bucket = "acme-walgit"

[cache]
dir = "/var/cache/walhub"
mode = "disk"                   # real SSD host: full materialization + watermark eviction

[git]
object_format = "sha1"
```

### 2.3 First-run defaults (zero-config boot; divergence D5)

When no config file is found (§3.1), the server boots anyway on these **first-run defaults**. They replace the compiled-in defaults of the table above for the marked keys; every unmarked key keeps its table value. User friendliness is a first-class law: `walhub` with zero configuration must serve git over HTTP on a fresh machine.

| Key | First-run value | Why it differs |
|---|---|---|
| `server.listen` | `"0.0.0.0:8080"` | a first-run server should be reachable, not only loopback |
| `store.backend` | `"filesystem"` | no bucket credentials exist yet; keys map to paths under `store.root` (D4, 03_store_backends.md) |
| `store.root` | `<data-dir>/store` | the filesystem backend's root, inside the data dir |
| `cache.dir` | `<data-dir>/cache` | the data dir owns all writable state — no `/tmp` surprise |
| `server.auth.mode` | `"none"` | zero-friction first run; allowed on ANY bind with a loud warning (§5, rule 1 — the Rust fail-closed loopback rule is superseded) |
| `server.auto_create_on_push` | `true` | `git push` to a fresh server should just work |

All other keys take their table (Rust-spec) defaults. The effective first-run config is exactly what `config dump` prints when no file is present, and what `GET /api/v1/setup` reports as effective values with `file_state = "absent"`.

A first-run boot also surfaces a **setup banner**: the log warns that auth is `none`, and the web UI shows a persistent banner linking `/setup` until a config file exists with auth configured. When `auth.mode = "none"` on a non-loopback bind, the warning is emitted at every startup regardless of first-run state.

## 3. Loading order, env overrides, PORT lockstep

### 3.1 The ladder

1. **Defaults.** The `Config` struct's Go zero values are NOT the defaults; `Config` values are first set from the compiled-in default table (§2). This is a literal table (`defaultConfig()` returning a fully populated `Config`), because the zero value of `time.Duration(0)` or `int64(0)` legitimately means "off" for some keys — the defaults must not be confusable with "off". When no config file exists at all, the first-run defaults of §2.3 are applied on top of the compiled-in table before the remaining legs run.
2. **File (optional).** The config file is looked up in this order, first hit wins:
   - **Explicit `--config PATH`** (or `WALHUB_CONFIG` / legacy `WALGIT_CONFIG` env when `--config` is absent). BurntSushi/toml `DecodeFile` into a fresh struct seeded from defaults; unknown keys are an error listing key + line (fail-closed, same as serde's `deny_unknown_fields` behavior in the Rust binary). **A missing explicitly-named file is fatal (exit 2, `config file not found: <path>`)** — if the operator pointed at a file, silence would be a footgun. Explicit `--config /dev/null` = defaults+env only (file leg is skipped when the path is `/dev/null`).
   - **`<data-dir>/walhub.toml`**, if it exists (data dir: §3.1.1). `<data-dir>/walgit.toml` is accepted as an alias (checked second). If the file exists but is INVALID — parse error or unknown key — boot enters **setup-only mode** (06_server_http.md): only `/setup`, `/healthz`, and `/readyz` answer; everything else returns 503 with a pointer to the errors, and the setup UI displays them. Saving a fixed config (§3.7) then requires a restart.
   - **No file** → zero-config first run on the §2.3 first-run defaults, with the setup banner (§2.3).
3. **Env overlay.** `WALHUB__SECTION__KEY=value` (legacy `WALGIT__SECTION__KEY=value` accepted as a fallback alias; `WALHUB_` wins when both define the same key) on top of the file result (§3.2) — or on top of the defaults when no file was loaded. The overlay applies identically in all three file states; **env always wins over the file** (file ⊕ env, documented).
4. **PORT lockstep.** If `PORT` env is set, it overrides the port portion of `server.listen` AND, when `server.public_url` is a loopback URL (`http://127.0.0.1:PORT`, `http://localhost:PORT`, `http://[::1]:PORT`), rewrites its port to the same value (§3.3).
5. **Validation.** Fail-closed; any violation is exit 2 with the offending key(s) and reason on stderr (§5) — with the single warn-only exception of rule 1 (auth-none loopback, divergence).

#### 3.1.1 The data dir (divergence D5)

All writable state lives under one **data dir**, selected by:

1. the global `--data-dir PATH` flag (accepted in the same argv positions as `--config`);
2. else the `WALHUB_DATA_DIR` env var;
3. else the default: `~/.local/share/walhub` (XDG), or `/var/lib/walhub` when the process detects a container context (`/.dockerenv` present or `KUBERNETES_SERVICE_HOST` set).

The data dir holds `<data-dir>/store/` (filesystem store backend root, `store.root` when `backend = "filesystem"`, D4), `<data-dir>/cache/` (first-run `cache.dir`, §2.3), and the saved config `<data-dir>/walhub.toml` (written by the setup UI, §3.7; `walgit.toml` alias accepted).

The directory is created (mode `0700`) on first boot if missing. `--data-dir` never sets a config value directly — it only relocates the file leg (§3.1 step 2) and provides the default expansion for `store.root` / `cache.dir` in the first-run table; an explicit `store.root` or `cache.dir` in the file or env always wins. Like `--config`, `--data-dir` is peeled before subcommand dispatch (§6.3) and is honored by every subcommand that loads config.

### 3.2 Env override mechanics

Decision: **primary prefix `WALHUB__`, legacy `WALGIT__` accepted as an alias** — new deployments and docs use `WALHUB__`, while deployment scripts, systemd units, and secret manifests written for walgit keep working unchanged, and both binaries can be mixed in one fleet. When both spellings are set for the same key, the `WALHUB__` value wins and the `WALGIT__` one is ignored (reported as an ignored override, never fatal). Same rule for the config-file pointer: `WALHUB_CONFIG` primary, `WALGIT_CONFIG` fallback.

- Pattern: `WALGIT__` + section path joined by `__`, value = TOML value syntax. `WALGIT__STORE__BUCKET=acme` sets `store.bucket = "acme"`. Nesting beyond one level: `WALGIT__SERVER__AUTH__MODE=oidc` sets `server.auth.mode`.
- Array-of-table (`[[bundles.strategy]]`) overrides: `WALGIT__BUNDLES__STRATEGY__0__BACKFILL_MAX=1` (index after the section, zero-based, whole-array replacement semantics like §3.4 placement). An override naming index N when the file provides fewer entries is an error (exit 1): the operator asked to mutate a slot that does not exist.
- The value is parsed as a TOML value (TOML "value" productions only — strings, integers, floats, booleans, arrays, inline tables). `WALGIT__SERVER__ROLES='["serve"]'` is an array; a bare `serve` is a parse error naming the key. Dates/datetimes are not needed by any key and are rejected.
- Scalars under `[bundles.strategy]` like `WALGIT__BUNDLES__MIN_COMMITS=10` go through the same typed decode as the file leg: the string is decoded as TOML, then assigned; a type mismatch with the target field is an error (exit 1) naming the key, the raw value, and the expected type.
- Unknown section/key in an env override: the key is recorded as an **ignored override** and validation reports it; see `config check` (§6) for how this is surfaced and the `--strict` exit-3 path. The process still boots otherwise (a stale `WALGIT__STORE__BKUET` typo must not take down a host — but it must be visible).
- `token_env`-style indirection is unaffected: `server.auth.tokens[].token_env` names another env var read at use time, as in Rust.

### 3.3 PORT override and loopback public_url lockstep

```go
// internal/config/env.go
func applyPort(c *Config, getenv func(string) string) error {
    p := getenv("PORT"); if p == "" { return nil }
    port, err := strconv.Atoi(p)
    if err != nil || port < 1 || port > 65535 { return fmt.Errorf("PORT: %q is not a TCP port", p) }
    host, _, err := net.SplitHostPort(c.Server.Listen)   // error = exit 2
    c.Server.Listen = net.JoinHostPort(host, strconv.Itoa(port))
    if u, err := url.Parse(c.Server.PublicURL); err == nil && u.Host != "" && isLoopbackHost(u.Host) {
        u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
        c.Server.PublicURL = u.String()
    }
    return nil
}
// isLoopbackHost: 127.0.0.0/8, ::1, "localhost". A non-loopback public_url (the normal proxied case)
// is NOT touched: PORT is a local-dev/reverse-proxy-port knob, not a general URL rewriter.
```

### 3.4 Placement all-or-nothing

If ANY `WALGIT__PLACEMENT__*` override is present, the entire `[placement]` section is taken from the environment: the file's `[placement]` table is discarded first, then the four keys are defaulted (`serve=["*"]`, `serve_exclude=[]`, `maintain=["*"]`, `maintain_exclude=[]`) and only the env-provided ones assigned. Rationale: placement lists are the one config where a partial override silently changes which host does what; half-file-half-env would be un-auditable. This mirrors the Rust behavior. (Other sections merge key-by-key; placement is the sole exception.)

### 3.5 Validation is fail-closed

Every rule in §5 runs after the ladder completes. A violation aborts startup (exit 2) or `config check` (exit 1). There is exactly one "warn and continue" path — rule 1 (auth-none on a non-loopback bind), a deliberate divergence from the Rust fail-closed rule (§5) — and the softer ignored-override treatment for unknown-key env overrides (§3.2), because those cannot change behavior. Everything else remains fail-closed.

### 3.6 Concurrency

Config is loaded once, before any goroutine that reads it is started, and is never mutated afterwards. `cmd/walhub` passes `*Config` by value (the struct is read-only by convention) to subsystem constructors. No locks, no atomics, no copy-on-write: the hazard (a subsystem mutating shared config mid-flight) is avoided by construction — nothing in walhub holds a `*Config` it may write to. If a future feature needs hot-reload, it must own its own snapshot (see 13_concurrency.md "immutable snapshot" pattern); it MUST NOT mutate the shared `*Config`.

### 3.7 Setup save semantics (divergence D6)

The setup UI (`/setup`) and its API (`GET /api/v1/setup`, `POST /api/v1/setup/test`, `PUT /api/v1/setup`; endpoint shapes and access rules are 06_server_http.md's job — this doc fixes the file semantics) write the config file:

- **Atomic write.** The setup API serializes the submitted config to TOML (same key names as §2), writes it to `<data-dir>/walhub.toml.tmp`, fsyncs, then `rename`s it over `<data-dir>/walhub.toml`. Readers see either the old or the new file, never a partial one. Permissions `0600` (the file may contain `token_env`-adjacent material; secrets themselves never live in the file — `token_env` names an env var).
- **Validate before write.** `PUT /api/v1/setup` runs the full §5 validation on the submitted values FIRST; invalid input writes nothing and returns the per-key errors (this is also what `POST /api/v1/setup/test` does without saving). Only an all-green submission is written.
- **Restart-required keys.** The save response reports, per key, whether it takes effect without a restart. Restart-required (the default — most keys are consumed once at subsystem construction: store backend, WAL, cache, TLS, auth, listen address, …) vs. read-live (picked up on the next tick/emit): `telemetry.log_format`, `telemetry.log_filter`, `maintenance.interval`, `maintenance.follow_interval`, `wal.freshness_ttl`. walhub does NOT hot-reload the shared `*Config` (§3.6); read-live keys are re-read from the effective config at their point of use, which the setup save updates by writing the file and refreshing the in-memory first-run overlay — a restart is still the supported way to be sure.
- **Edits apply to the current file (§3.4).** Submitted overrides merge onto the file-visible config — compiled-in defaults ⊕ the first existing candidate file, or the zero-config first-run shape when no file exists — so a save changes only the keys the operator touched; keys the form does not carry keep their file values. Env overlays are never baked into the file: the file keeps its own values and env still wins at load (see the env-interaction bullet below).
- **Interaction with env (file ⊕ env, env wins).** The saved file participates in the ordinary ladder (§3.1): env overrides and PORT lockstep still apply on top of it, and an env override of a key the operator just set in the UI will silently win — the setup UI therefore shows the effective value (post-env) for every key and warns when a submitted value is masked by an active `WALHUB__`/`WALGIT__`/`PORT` override. Setup never writes env vars.
- **Invalid-config rescue.** When boot landed in setup-only mode (§3.1 step 2), a successful setup save writes a valid file but does NOT swap the running config; the response says `restart_required: true` for every key and the UI tells the operator to restart. (Boot flow: config present → validate → boot if it all works; config missing → defaults + setup banner; config INVALID → setup-only mode.)

## 4. Per-repo settings (D24)

### 4.1 Shape

Per-repo settings are published INTO the WAL (inline on the manifest + SETTINGS log entries with history) and merged over the host config by `with_settings`. The Go surface:

```go
// internal/config/repo_settings.go
type RepoSettings struct {  // the four allowed sections; zero = "not set, inherit host config"
    Bundles      *BundlesConfig      `toml:"bundles"`
    Maintenance  *MaintenanceConfig  `toml:"maintenance"`
    Compaction   *CompactionConfig   `toml:"compaction"`
    Upstream     *UpstreamConfig     `toml:"upstream"`
    Integrations map[string]toml.Primitive `toml:"integrations"` // accepted, forward-compat, never interpreted
}
func (r *RepoSettings) Merge(base *Config) (*Config, error)  // "with_settings": pointer-set fields override base
```

### 4.2 Rules (normative, unchanged from Rust spec §15.2)

- Allowed sections: `[bundles]`, `[maintenance]`, `[compaction]`, `[upstream]`; `[integrations]` accepted (stored verbatim, forward-compat). Anything else → 400 at publish.
- Size: the serialized settings payload MUST be ≤ 16 KiB; larger → 400.
- `[integrations]` contents are stored verbatim and never interpreted; the 16 KiB budget includes them.
- NOT settable via settings: auth, store, server, wal, cache, `upstream.token_env` (host-only). A `[server]`, `[store]`, `[wal]`, `[cache]`, or `upstream.token_env` key inside repo settings → 400.
- Validation at publish runs the host build's `with_settings` merge + validation (§5) against the would-be effective config; invalid → 400, nothing published. This means a settings payload must pass the same fail-closed validation as a host config, restricted to the merged sections.
- Write = admin (API gating is 06_server_http.md's job; `internal/config` exposes the validator and the merge).
- `GET …/settings/effective` returns only those sections (07_api.md owns the endpoint shape).
- Every instance sees a new settings revision on its next refs-level sync, with zero extra round trips — 05_wal_engine.md implements the manifest-inline + SETTINGS log mechanics; this doc fixes only the payload semantics.

### 4.3 Example

```console
$ cat /tmp/settings.toml
[bundles]
main_only = false

$ walhub repo settings set acme/monorepo --file /tmp/settings.toml -m "bundle all heads"
```

## 5. Validation rules (fail-closed — one warn-only divergence; exit 2 at boot, exit 1 in `config check`)

Each rule is a named function in `internal/config/validate.go`; the list is the contract:

1. **none-mode loopback (DIVERGENCE — warn, not fail).** When `server.auth.mode = "none"` and `server.listen` is NOT on a loopback address (127.0.0.0/8, `::1`, or `localhost` host), startup logs a loud warning — `auth.mode=none on non-loopback listen <addr>; anyone who can reach this port can read and write every repository — set server.auth.mode = "token" (or oidc) and restart` — and continues. The Rust rule (fail-closed, exit 2) is **superseded by divergence**: zero-config first runs bind `0.0.0.0` with auth `none` by design (§2.3), and an operator who sets an explicit file keeps the freedom to do the same, warned. `config check` reports it as a warning, exit 0.
2. **oidc allowlist.** `mode = "oidc"` requires `server.auth.anonymous_read = false` AND at least one of `allowed_domains` / `allowed_emails` non-empty. `oauth_client_id` and `oauth_client_secret` must be both set or both unset. `session_secret`, when set, MUST be ≥ 32 bytes.
3. **bundle strategy validation.** For each `[[bundles.strategy]]`:
   - `kind = "incremental"` requires `base` naming an earlier-declared strategy's `name` (unknown or later-declared base → error);
   - a whole chain shares one `filter`: a strategy and its transitive base chain MUST declare identical `filter` values (absent = none);
   - `keep` on an incremental strategy fails (keep is a full-strategy concept);
   - `schedule` is a valid 6-field cron expression or one of `@hourly|@daily|@weekly|@monthly|@yearly` (the parser lives in 08_bundles.md; validation here only checks it parses);
   - `filter`, when present, is only `"blob:none"` (Rust accepts nothing else);
   - `backfill_max` ≥ 0; `min_commits` ≥ 0; a strategy's `refs` globs compile as `owner`-free git ref globs.
4. **chain-shared filter (restated).** A filtered chain is all-or-nothing: mixing `filter = "blob:none"` with a filterless strategy in one lineage is the classic footgun and is rejected by name in the error.
5. **store**: `store.backend ∈ {s3,gcs,memory,filesystem}`; for s3, `access_key_env`/`secret_key_env` name existing env vars only at use time (not validated at load — the Rust behavior); `multipart_part_size` ≤ `multipart_threshold` when both are non-zero; for filesystem, `store.root` (default `<data-dir>/store`, §2.3) must be an absolute path or empty (empty = first-run default).
6. **sizes/timeouts**: every duration/size key parses (§1); `cache.disk_high_watermark ∈ (0,1)` or 0; negative values nowhere.
7. **placement globs** compile: each entry is `*`, `owner/*`, or `owner/name` (one `/` at most).
8. **tls**: `mode ∈ {off,self_signed,files}`; `files` requires cert+key paths; `self_signed` implies nothing else.
9. **roles**: each role ∈ {serve, maintain, events}; duplicates allowed (idempotent), unknown role → error.
10. **paths**: `cache.dir` is absolute (Rust requires this for the tls/ sibling) unless backend is `memory`.
11. **ssh** (17_ssh.md §3): when any `[server.ssh]` key is set — `listen` must be host:port; each
    `keys[]` entry needs a principal and exactly one of key/key_env (env read at boot; unset →
    error); key lines must parse as authorized_keys; duplicate fingerprints fail. `listen` unset
    with keys configured still validates (the transport stays disabled).

## 6. CLI: `walhub`

### 6.1 Global shape, exit codes, version

- Binary: `walhub`. `walhub [--config PATH] [--data-dir PATH] <command>`; `--config`/`--data-dir` may also appear after the subcommand (`walhub serve --config x.toml`) — both positions are accepted, last wins.
- Config default: the implied default path is **`<data-dir>/walhub.toml`** (data dir: §3.1.1), with `<data-dir>/walgit.toml` accepted as an alias name (checked second; §8). `WALHUB_CONFIG` (legacy `WALGIT_CONFIG`) overrides the default path when `--config` is absent; explicit `--config` beats the env var. A missing file is NOT fatal at the default location — it means zero-config first run (§2.3). A missing explicitly-named file (`--config` or the env pointer) IS fatal: exit 2 with `config file not found: <path>`. An explicit path of `/dev/null` forces defaults+env (§3.1).
- **No subcommand = `serve`** (with the parsed config; identical to `walhub serve`).
- Exit codes (normative; copy of Rust §16):
  - `0` — success;
  - `1` — command/config error (bad flag value, validation failure surfaced by a command, strategy not found, …);
  - `2` — a missing **explicitly-named** config file (`--config`/`WALHUB_CONFIG`/`WALGIT_CONFIG` — never the absent default file, which is the zero-config first run), and argv-level errors (unknown command, malformed `--config`/`--data-dir` use);
  - `3` — `config check --strict` observed ignored overrides (supervisor pre-start check).
  - Mapping: Go's `flag` package calls `os.Exit(2)` on flag errors by default — the dispatcher overrides `flag.Usage`/error handling per subcommand so every argv error funnels to exit 2 uniformly (see §6.3).
- Version: from build info, in order: linker-injected `WALHUB_BUILD_SHA` (set via `-ldflags "-X main.buildSHA=$(git rev-parse --short=12 HEAD)"` at build time, from `runtime/debug.ReadBuildInfo()` VCS info when present (Go embeds `vcs.revision` with `-buildvcs=true`, default when building from a git checkout; take first 12 hex chars), else `dev`. `walhub version` prints it; `serve` logs it at startup.
- Legacy compat: `walgit-server` is not shipped; `walhub serve` is the only server entrypoint. (If operators' units reference `walgit-server`, a `cmd/walgit-server` symlink-compatible binary MAY be added later; it is out of scope here.)

### 6.2 Subcommand table

Dispatch is a hand-rolled table (`cmd/walhub/main.go`), not a CLI framework:

```go
type command struct {
    name  string                        // e.g. "bundle", or "" for the serve default
    help  string
    run   func(ctx context.Context, args []string) error   // ctx cancelled on SIGTERM/SIGINT
}
var subcommands = []command{ /* flat table; "bundle run" is a two-token lookup */ }
```

Implementation notes per group (behavioral truth from Rust spec §16; each command's heavy lifting lives in the doc named in the last column):

| Command | Go implementation notes | See |
|---|---|---|
| `serve` | open store per §2 table, construct AppState, spawn maintainer + follow loops honoring `server.roles`, install SIGTERM/SIGINT handler that triggers the two-phase drain (`server.drain_timeout`), run the HTTP server. Legacy shape: a roles list of exactly `["maintain"]` or exactly `["compact"]`-equivalent runs its 60 s loop and no HTTP listener? NO — Rust runs the HTTP server in all cases; walhub MUST also always run HTTP (it serves /readyz for the supervisor). | 06_server_http.md |
| `compact [REPO\|--all] [--once] [--base]` | `--base` requires `--once`; drives 10_maintenance.md's compaction unit (or the base-rebuild path: full repack -adb + bitmap + commit-graph + history pack, checkpoint at that seq, then `bundle compose`). Exit 0 even when nothing to do. | 10_maintenance.md |
| `bundle run [--repo ID] [--strategy N]` / `bundle plan <repo>` / `bundle compose <repo> [--strategy N]` / `bundle rm <repo> <IDS…>` | thin wrappers over internal/bundle's plan/compose/delete; `run` without `--repo` iterates all repos the placement rules assign to this host. Output: one line per slot decision. | 08_bundles.md |
| `repo create <REPO> [--object-format sha1\|sha256]` / `repo list` / `repo info <REPO>` | create is CAS-based (create-if-absent on the manifest key); `repo info` prints manifest stats, pack inventory, checkpoint, segments as a table. | 05_wal_engine.md |
| `repo policy get\|set --file F\|clear <REPO>` | read/save/clear `policy.json` (14.1 envelope; the file is stored as bytes, validated by internal/policy on set). | 14.1 of MASTER_RUST_SPEC.md / internal/policy |
| `repo settings show <REPO> [--effective]` / `set --file F [-m MSG]` / `clear` / `history` | D24 surface. `show` prints the stored payload; `--effective` prints the `with_settings`-merged host+repo config restricted to the allowed sections. Author = `$USER`. `set` validates via §4.2 before publish. | §4 this doc |
| `wal ls <REPO> [--from N] [--to N]` | log table: seq, kind, pack (12 hex), supersedes count, ref count. Streams entries from the store; no full-log read into memory. | 05_wal_engine.md |
| `wal show <REPO> <SEQ>` | one entry, full: writer, created_at, pack, supersedes, ref updates, push options, checkpoint, meta. | 05_wal_engine.md |
| `wal materialize <REPO> --at-seq N --out DIR` | build a standalone repo at a point in time (checkpoint + replay + pack set fetched from the store or copied from the local cache copy; refs applied last). When history is folded, the error names the oldest rewindable state. | 05_wal_engine.md |
| `wal add-pack <REPO> <pack> [--history-of CHECKSUM] [--tier N]` | publish an existing pack as a COMPACT entry (recovery path: `--tier 0`). | 05_wal_engine.md |
| `wal annotate-pack <REPO> <CHECKSUM> [--rev F] [--bitmap F] [--commit-graph F]` | retrofit side files via manifest-only CAS (no log entry). | 05_wal_engine.md |
| `wal rev-index <IDX> [--out P]` | write a `.rev` from an `.idx` alone, byte-identical to git's output (04_git.md owns the algorithm). | 04_git.md |
| `synth --out DIR --size s\|m\|l [--commits N] [--files N] [--seed N]` | deterministic synthetic repo via `git fast-import`; sizes S=50c/200f, M=2000c/5000f+binary, L=50000c/50000f; output fsck-verified before exit 0. | 15_testing.md |
| `import --from GITDIR owner/name [--reuse-packs] [--refs GLOB]…` | classic import: publish refs + packs (reuse source packs or `pack-objects --all`), then immediately a full bitmap'd repack published as the tier-2 base. | 05_wal_engine.md, 04_git.md |
| `import --direct --from GITDIR owner/name [--packs DIR] [--refs GLOB]… [--bundle=true] [--bundle-strategy S] [--replace] [--force] [--commit-graph=true] [--history-pack=true] [--parallelism 8] [--verify-closure=true]` | bucket-direct import of ready-made packs: verify closure → side files → history pack → striped uploads (HEAD-skip existing, marker-file resumability, `--force` after a moved target) → ref snapshot + checkpoint → manifest CAS (`min_seq = seq+1`; `first_state_at = as_of = now`) → bundle list (supersedes same-strategy + dependents). Re-run on a completed import = no-op. Striped upload parallelism = `--parallelism` goroutines (see 03_store_backends.md §striped). | 05_wal_engine.md, 03_store_backends.md |
| `mirror --from URL --to URL --dir PATH [--ref NAME]… [--interval 30s] [--once] [--force] [--repack-every 1h] [--identity token\|gcloud\|gce]` | external bridge via a local bare buffer repo: fetch from source, ff-only push to destination; bearer token from `$WALGIT_TOKEN` / gcloud / GCE metadata (cached 50 min); `--once` exits non-zero on push failure; geometric repack of the buffer on `--repack-every`. The interval loop and any child git processes take the command's ctx. | 10_maintenance.md |
| `config check [--env-file PATH]… [--strict]` / `config dump` | validate file ⊕ env, print ignored overrides to stderr; `--strict` exits 3 when any override was ignored (supervisor pre-start check). With NO config file present, `config check` validates the effective defaults (§2.3) ⊕ env — it passes (exit 0) unless an env override breaks a rule; this makes it usable as a pre-flight check on a not-yet-configured host. `config dump` prints the effective config as TOML (defaults ⊕ first-run overrides ⊕ file ⊕ env, post-PORT-lockstep, post-placement-all-or-nothing), reporting `file_state` (present / absent / invalid) so scripts can tell the three boot states apart. `--env-file` loads KEY=VALUE pairs into the override set (does not touch the real environment). | §6.3 |

There are NO `fsck`/`repair` subcommands — those are maintainer units (10_maintenance.md); manual recovery is `walhub wal add-pack --tier 0` + ref-delete pushes.

### 6.3 Flag parsing and dispatcher mechanics

- Per-subcommand `flag.NewFlagSet` (stdlib), one instance per invocation. The dispatcher:
  1. peel `--config PATH` and `--data-dir PATH` (and `--help/-h`) from anywhere in argv before dispatch;
  2. look up the subcommand (first token; `bundle`/`wal`/`repo`/`import`/`config` consume a second token from a nested table);
  3. run the subcommand's `run(ctx, remainingArgs)`.
- Errors: a FlagSet parse error or an unknown subcommand prints usage to stderr and exits 2; a runtime error from `run` prints `walhub: <err>` and exits 1. `ctx` is cancelled on SIGTERM/SIGINT — every subcommand's `run` MUST honor it (interval loops, `git` subprocesses, store retries).
- No hidden flags, no config-in-flags tricks: flags never set config values except `--config` and `--data-dir` themselves (`--data-dir` only relocates the file leg and the first-run root defaults, §3.1.1; the env/PORT layering in §3 is the only other writer). This keeps `config dump` honest.
- Output formats: table commands (`wal ls`, `repo list`, `repo info`, `bundle plan`) print human tables to stdout; every command accepts `--json` (added for walhub; not in the Rust CLI) emitting one JSON object per line (JSONL), keys stable and snake_case, for scripting. When a Rust command had a documented exact output shape (e.g. `wal ls` columns), the human table keeps those columns in that order.

### 6.4 Concurrency

- `serve`: see 13_concurrency.md. Config provides only the numbers.
- `mirror`: the `--interval` loop is one goroutine; each fetch/push is a child process; the goroutine exits when ctx is cancelled. The buffer repo directory is owned by this process alone (the Rust spec gives it the same single-owner rule).
- `import --direct`: striped uploads run `--parallelism` goroutines (default 8) over a bounded channel of upload items; the importing goroutine closes the channel; workers exit on channel close or ctx cancel. Marker files make re-runs idempotent, so a ctx cancel mid-import is safe to re-run (05_wal_engine.md owns the marker scheme).
- Everything else is single-goroutine + ctx.

### 6.5 Examples

```console
# one-box: serve with defaults + env overrides
$ WALGIT__STORE__BUCKET=acme-walhub PORT=9000 walhub --config /etc/walhub/walgit.toml serve

# pre-start supervisor check (fail the boot when the file+env combo is wrong)
$ walhub config check --config /etc/walhub/walgit.toml --env-file /etc/walhub/overrides.env --strict || exit 3

# import a big repo, direct mode
$ walhub import --direct --from /srv/git/monorepo.git acme/monorepo \
    --bundle=true --bundle-strategy weekly --parallelism 12

# bundle ops
$ walhub bundle plan acme/monorepo
$ walhub bundle compose acme/monorepo --strategy weekly
$ walhub bundle run --strategy daily

# recovery
$ walhub wal add-pack acme/monorepo /tmp/pack-ab12.pack --tier 0

# synth a test repo
$ walhub synth --out /tmp/big --size l --seed 7
```
```console
# zero-config first run: no file anywhere, boots on §2.3 defaults with setup banner
$ walhub serve
# → server.listen = "0.0.0.0:8080", store.backend = "filesystem" (<data-dir>/store),
#   server.auth.mode = "none" (warning logged), server.auto_create_on_push = true
```

## 7. Worked end-to-end example

```console
# 0. zero-config first run (no --config, no <data-dir>/walhub.toml): §2.3 defaults + env
$ walhub config dump
# → listen = "0.0.0.0:8080", store = filesystem @ ~/.local/share/walhub/store,
#   cache.dir = <data-dir>/cache, auth.mode = "none" (warned), auto_create_on_push = true
# → file_state = "absent"

# 1. defaults + file + env
[server]
listen = "127.0.0.1:8080"
public_url = "http://127.0.0.1:8080"
[store.s3]
endpoint = "http://127.0.0.1:9000"
force_path_style = true

$ WALGIT__STORE__BUCKET=walhub-dev PORT=9999 walhub config dump --config /etc/walhub/walgit.toml
# → server.listen = "127.0.0.1:9999"       (PORT lockstep)
# → server.public_url = "http://127.0.0.1:9999"  (loopback URL, same lockstep)
# → store.bucket = "walhub-dev"            (env override)
# → everything else = defaults

# 2. same config against a real public URL: PORT does NOT touch it
$ WALGIT__SERVER__PUBLIC_URL=https://git.example.com PORT=9999 walhub config dump --config /etc/walhub/walgit.toml
# → server.listen = "127.0.0.1:9999"
# → server.public_url = "https://git.example.com"   (non-loopback: untouched)

# 3. placement all-or-nothing
$ WALGIT__PLACEMENT__SERVE='["acme/*"]' walhub config dump --config /etc/walhub/walgit.toml
# → placement.serve = ["acme/*"], placement.maintain = ["*"] (file's placement table discarded, defaults re-applied)

# 4. typo'd env key: boots anyway, but visible
$ WALGIT__STORE__BKUET=x walhub config check --config /etc/walhub/walgit.toml
# warning: ignored override WALGIT__STORE__BKUET (unknown key store.bkuet)
$ WALGIT__STORE__BKUET=x walhub config check --config /etc/walhub/walgit.toml --strict
# exit 3
```

## 8. Decisions & deviations from the Rust design

- **Env prefix: `WALHUB__` is primary, `WALGIT__` accepted as a legacy alias** (supersedes the earlier "stays `WALGIT__`" decision): new deployments and docs use `WALHUB__`; scripts, systemd units, and secret manifests written for walgit keep working via the alias, and `WALHUB_` wins when both define the same key (§3.2). Compat preserved, naming future-proofed.
- **Config file name/location (superseded in part):** the earlier decision kept `walgit.toml` as the implied default file name. The divergence moves the default location under the data dir — `<data-dir>/walhub.toml` is the default path, `<data-dir>/walgit.toml` is accepted as an alias (checked second, §3.1, §6.1) — so the README's "`walgit.toml` remains a valid config file name" promise still holds for the alias, while first-run and setup-UI files are written as `walhub.toml`.
- **`--json` flag added to table-output commands** (JSONL, stable snake_case keys): Rust's human tables are unscriptable; JSONL costs ~20 lines and no dependency.
- **`gix` as `git.upload_pack_engine` value accepted but treated as `git`**: the Rust gix engine has no Go equivalent and none is wanted (dependency law); file compatibility is preserved.
- **`walgit-server` binary alias not shipped**: `walhub serve` is the entrypoint; units referencing `walgit-server` are a packaging concern (16_packaging.md MAY add an alias later).
- **Version env var is `WALHUB_BUILD_SHA`** (Rust used `WALGIT_BUILD_SHA`): it is build-time, not deployment-facing, so no compat value; VCS build info from the Go toolchain is the primary source anyway.
- **`RUST_LOG` honored (kept) as an override of `telemetry.log_filter`**, and `WALHUB_LOG` accepted as the new-style spelling: zero-cost compat for existing log-tuning scripts.
- **Unknown-key env overrides are soft (ignored + reported) rather than fatal**: matches Rust behavior and keeps a stale variable from taking down a fleet; `--strict` makes them fatal for supervisors.

### Divergence (2026-08-31)

- **D5 — Config is optional; zero-config first run.** The Rust spec boots fail-closed with no config file ("run `walgit init` first"); walhub boots on the first-run defaults of §2.3 instead: `listen = "0.0.0.0:8080"`, `store.backend = "filesystem"` rooted at `<data-dir>/store`, `auth.mode = "none"` (warned), `auto_create_on_push = true`, everything else per the Rust defaults. User friendliness is a first-class law. A missing file at the DEFAULT location is a first run, not an error; a missing explicitly-named file (`--config`/config-env pointer) remains fatal (exit 2), and `/dev/null` still forces defaults+env (§3.1). Data dir (`--data-dir` / `WALHUB_DATA_DIR`, default `~/.local/share/walhub`, `/var/lib/walhub` in containers) owns `store/`, `cache/`, and the saved `walhub.toml` (§3.1.1).
- **D6 — Setup UI + API save semantics (§3.7).** The setup UI writes `<data-dir>/walhub.toml` atomically (tmp + fsync + rename, mode `0600`) after full §5 validation; the save response reports restart-required vs read-live keys; the saved file joins the ordinary ladder so env ⊕ file still holds with env winning. Boot states: config present → validate → boot; absent → defaults + setup banner; INVALID → setup-only mode (only `/setup`, `/healthz`, `/readyz` answer; rest 503) until a fixed config is saved and the process restarted. Endpoint shapes and the setup access rule live in 06_server_http.md.
- **Auth-none loopback rule superseded.** Rust validation rule 1 (auth `none` requires a loopback listen, fail-closed) is replaced by a loud warning on any bind (§5 rule 1); the first-run default `0.0.0.0:8080` + `auth = "none"` depends on it. All other validation stays fail-closed.
- **D4 — `filesystem` added to `store.backend`** (`s3|gcs|memory|filesystem`, §2 note † and §5 rule 5): keys map to paths under `store.root`, first-run default `<data-dir>/store`. 03_store_backends.md owns the backend semantics; this doc owns the key, its validation, and the default.
- **D1 — dependency budget** (restated where it touches this doc): exactly `github.com/go-chi/chi/v5`, `github.com/BurntSushi/toml`, `golang.org/x/net`. TOML stays the config file format and BurntSushi/toml stays the parser — no format migration was entertained.
