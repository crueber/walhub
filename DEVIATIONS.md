# walhub — Register of Decisions & Deviations from the Rust Design

This file consolidates, deduplicated, every decision recorded in the `## Decisions & deviations from the Rust design` sections of `docs/go/01…16` and the law amendments in `AGENTS.md` (`docs/go/README.md` "four laws" is framing). Each entry merges the bullets that appear, often verbatim, in 3–5 docs. Pure restatements of the Rust baseline (compat keepers such as unchanged TOML keys, the two-level compose, retained S3 race semantics, or 09's "everything else carried over verbatim") are **not** deviations and are excluded.

**Precedence:** where an entry below contradicts `docs/MASTER_RUST_SPEC.md`, this file wins for walhub. Where two docs contradicted each other, the resolution is recorded under "Rulings" at the end.

**Status values:** `in force` | `superseded by D-…` (chain preserved). Sources use doc numbers (`01`–`16`), `README`, `AGENTS`. Last updated: **2026-09-01**.

## 1. Product & UX

- **D-UX-1 — Zero-config first run (Divergence D5).** A missing config file boots on first-run defaults (`listen = 0.0.0.0:8080`, `store.backend = "filesystem"` rooted at `<data-dir>/store`, `auth.mode = "none"` with loud warnings, `auto_create_on_push = true`; other keys per Rust defaults); data dir via `--data-dir`/`WALHUB_DATA_DIR` (default `~/.local/share/walhub`, `/var/lib/walhub` in containers). A missing *explicitly named* config (`--config`/env pointer) stays fatal (exit 2); `/dev/null` still forces defaults+env.
  Rationale: user friendliness is a first-class law; supersedes the Rust fail-closed "run `walgit init` first" boot (exit 2 is now bad-argv only).
  Sources: 01, 03, 06, 11, 15, 16, README L4, AGENTS L10. Status: in force.
- **D-UX-2 — Setup UI + API first-class (Divergence D6).** `/setup` plus `GET|POST|PUT /api/v1/setup{,/test}` save `<data-dir>/walhub.toml` atomically (tmp + fsync + rename, mode 0600) after full validation, reporting restart-required vs read-live keys. Invalid config ⇒ **setup-only mode** (only `/setup`, `/healthz`, `/readyz` answer; rest 503). Access open while unsecured (no/invalid config or auth none), else admin; optional `WALHUB_SETUP_TOKEN` gate.
  Rationale: one-click path from zero-config to real auth; new package `internal/setup`.
  Sources: 01, 06, 11, 12, 15, 16, README, AGENTS L10. Status: in force.
- **D-UX-3 — Auth-`none` allowed on any bind.** Rust validation rule 1 (auth `none` requires a loopback listen, fail-closed) is replaced by a loud warning on any bind (logs, setup UI, `readyz`) with zero refused requests. All other validation stays fail-closed.
  Rationale: the D-UX-1 default (`0.0.0.0:8080` + auth none) depends on it; AGENTS L9 records the supersession.
  Sources: 01, 06, 11, 16, AGENTS L9. Status: in force.

## 2. Dependency budget

- **D-DEP-1 — Backend budget: exactly three modules (Divergence D1).** `github.com/go-chi/chi/v5` (core only), `github.com/BurntSushi/toml`, `golang.org/x/net` (h2c). Chain: earlier two-module budget (toml + x/net) → three modules when chi was adopted. The store layer spends none of it.
  Rationale: chi's path matching replaced the hand-rolled router without touching route inventory or handler behavior; TOML format/parser never migrated.
  Sources: 01, 03, 06, 07, 11, README L1, AGENTS L1. Status: in force (supersedes two-module budget).
- **D-DEP-2 — Frontend budget: zero npm runtime dependencies; one devDependency (`esbuild`).** No TypeScript, no framework, no state library, no VDOM.
  Chain: earlier ≤6-npm-dependency budget (`solid-js`, `@solidjs/router`, `marked`; dev `vite`, `vite-plugin-solid`, `typescript`) → superseded by the zero-runtime budget with the single esbuild devDependency.
  Sources: 01, 12, 15, 16, README L1, AGENTS L1. Status: in force (supersedes SolidJS set).
- **D-DEP-3 — Hand-rolled inventory (no library where a crate was used).** S3 SigV4 (~150 lines, validated against AWS test vectors); GCS JSON-API HTTPS client; protobuf wire codec; JWKS/JWT verification (`crypto/rsa`/`crypto/ecdsa`); self-signed TLS (`crypto/x509`+`crypto/ecdsa`, replacing rcgen); Prometheus text exposition; weighted LRU caches; `singleflight.Group`; CORS and all other middleware (chi core only); `weighted semaphore` + errgroup in `internal/store` (ruling C-1: hand-rolled, no x/sync); cron parser (08); hand-rolled h2c only via `x/net`.
  Rationale: budget is law — hand-roll instead; each hand-rolled piece is behavior-identical to the Rust original.
  Sources: 01, 02, 03, 06, 07, 08, 12, 13, README L1, AGENTS L1. Status: in force.

## 3. Web stack

- **D-WEB-1 — SPA is vanilla standard ECMAScript (Divergence D2).** Native ES modules + import map, `<template>`, ~40-line hand-rolled reactive core, hand-rolled router; wire contract untouched (the SDK defines the wire, not the framework).
  Chain: React → SolidJS (explicit user decision, superseded) → vanilla ESM (2026-08-31, current). `tsc --noEmit` gate and the vite/pnpm script chain are gone with TypeScript.
  Sources: 01, 12, 16. Status: in force (supersedes SolidJS-over-React).
- **D-WEB-2 — SDK authored as submodules, esbuild-bundled (amendment to D2).** The SDK lives at `web/sdk/src/*.js` and is bundled by esbuild (the single devDependency, via `make web`) into shipped `web/dist/repos.js`; the SPA itself remains unbuilt raw ESM; the `/repos.mjs` twin is deleted and `dist/.keep` is committed for the embed.
  Chain: "no build at all" → user-directed one dev-time build for the modular SDK (2026-08-31, second pass). `marked` replaced by hand-rolled markdown-lite (same allowlist sanitizer, unchanged preview-fidelity stance).
  Sources: 01, 12, 15, 16, AGENTS L1. Status: in force (amends the zero-build reading of D2).
- **D-WEB-3 — Asset serving: raw modules, no-cache + strong ETag, on-the-fly gzip middleware.** Supersedes vite's chunk-hash immutable `/assets` scheme with br+gz precompressed siblings; behavioral contract (fresh content on deploy, cheap revalidation, compressed text) preserved.
  Sources: 12. Status: in force (supersedes vite asset scheme).
- **D-WEB-4 — JS testing normative: `node --test`, zero npm test deps.** Headless pure-ESM logic modules + fetch-based server smoke tests, strict logic/DOM separation, wired as `make test-web` into `make test` and CI; tests import source, never the build.
  Sources: 01, 12, 15, AGENTS L11. Status: in force.
- **D-WEB-5 — UI hand-rolled renderers stay.** Unified-diff parser (~120 lines, §2.8 grammar), mini code tokenizer instead of shiki, ~40-line allowlist markdown sanitizer; GFM extras beyond markdown-lite unimplemented (code view is the exact-text path).
  Sources: 12. Status: in force (carried unchanged from the SolidJS draft).
- **D-WEB-6 — SolidJS SPA + Tailwind v4 replace the vanilla-ESM UI (2026-09-02, EXPLICIT USER REQUEST).** The user directed: "replace the baseline Javascript with a SolidJS SPA with a router and state management… some kind of basic tailwind set up that is part of the build management. Nothing can be on a CDN… Dark mode by default." Frontend budget: runtime = exactly `solid-js` + `@solidjs/router`; state management = Solid signals/stores + context (no additional state library); styling = Tailwind CSS v4 CSS-first (`@import "tailwindcss"` + `@tailwindcss/vite`, no tailwind.config) with dark as the default theme; still NO TypeScript (plain JSX/JS); build = vite + vite-plugin-solid (+ @tailwindcss/vite) into `web/dist/`; the SDK stays dependency-free and esbuild-bundled (the `/repos.js` contract is unchanged); zero CDN assets (system font stack; everything embedded).
  Chain: React → SolidJS (superseded) → vanilla ESM (2026-08-31, user) → **SolidJS + Tailwind (2026-09-02, user — supersedes the vanilla decision)**. Asset serving reverts to the vite chunk-hash scheme for built assets (content-hashed `/assets/*` immutable; `index.html` no-cache + ETag), superseding D-WEB-3's raw-module no-cache rule; D-WEB-3's on-the-fly gzip behavior is preserved. D-WEB-4 (node --test, zero npm test deps) and D-WEB-5 (hand-rolled renderers) remain in force — the pure logic modules (diff/markdown/sanitize/highlight/setup-form/SSE-frame/SDK) survive the port unchanged.
  Sources: AGENTS L1, 12, 16. Status: in force (user-directed; supersedes D2's vanilla reading and D-WEB-3's caching class).

## 4. Tooling & CI

- **D-TOOL-1 — Make replaces just (Divergence D3).** Every dev/CI entry point is a Make target (`build fmt vet test race cover test-slow sim contract contract-fs/s3/gcs e2e image dev dev-store clean ci`); the justfile is deleted; `lint` renamed `vet` (commands unchanged); `just dev-local` → `make dev`.
  Rationale: make is ubiquitous — no extra tool for a first contribution.
  Sources: 01, 15, 16. Status: in force.
- **D-TOOL-2 — Coverage gate ≥ 95 % per `internal/...` package (Divergence D7).** `make cover` with per-package `-coverprofile` + `covergate` checker, CI-fail-under; `cmd/` main glue excluded; table-driven `httptest` for every handler; review bar near 100 %.
  Rationale: new — the Rust spec had no coverage gate.
  Sources: 01, 15, 16, README L4, AGENTS L11. Status: in force.
- **D-TOOL-3 — Go test & lint gates.** Stdlib `testing` with `-short` as the tier switch (no build tags / `#[ignore`]); `go vet` + `gofmt -l` + `go build` replace cargo `-D warnings`; concurrency tier adds deadlock canary (full `runtime.Stack` dumps), `-race`, `-count=100` stress, goroutine-leak and op-budget assertions in place of Rust's ignored-soak tier.
  Sources: 13, 15. Status: in force.
- **D-TOOL-4 — CI is a Makefile + Woodpecker pipeline, not GitHub Actions.** The repo lives on Forgejo; Woodpecker's services block covers the rustfs contract job.
  Sources: 16. Status: in force.

## 5. Storage

- **D-STORE-1 — `filesystem` store backend added (Divergence D4).** Full peer of `s3|gcs|memory`: guarded key→path mapping under `store.root`, `"<size>:<mtime_ns>"` version token/ETag, sidecar `.lock` + flock + stat-compare + atomic rename for conditional writes, `renameat2(RENAME_NOREPLACE)` create-if-absent with portable fallback, stream-concat compose + rename, `os.File.ReadAt` ranges, byte-order walk; no accel/signed URLs; residual TOCTOU ledgered (< S3's). Rust-spec counterpart: none.
  Sources: 01, 03, 11, 15, 16. Status: in force.
- **D-STORE-2 — Contract suite always runs memory AND filesystem (Divergence D4/15).** `TestContract_Filesystem` runs wherever `TestContract_Memory` runs, no env; S3/GCS stay env-gated (`WALHUB_TEST_S3_ENDPOINT`, `WALHUB_TEST_GCS_BUCKET`; CI exercises S3 via the rustfs container). Supplements the `TestContract_LeaseSteal` addition (lease is pure store protocol, pinned in the one all-backends suite).
  Sources: 03, 15. Status: in force.
- **D-STORE-3 — Hand-rolled protobuf wire codec; no protoc codegen at build time.** Fixed-schema codec in `internal/store/proto` on the same field numbers; the canonical `.proto` text stays the wire contract; golden fixtures generated once against a real toolchain prove byte equality; message structs and `Marshal`/`Unmarshal` written by hand (or generated once, output checked in) to keep the build hermetic.
  Sources: 01, 02, README L1, AGENTS L1/L5. Status: in force.
- **D-STORE-4 — GCS is JSON API over plain HTTPS; no gRPC clients.** The Rust gRPC topology (Storage + StorageControl + N bulk gRPC channels) collapses into control/bulk HTTP client pools with identical isolation semantics (`bulk_clients` now counts HTTP pools, same default 4); `SignedGetURL` via IAM signBlob over HTTPS.
  Sources: 01, 03, 14. Status: in force.
- **D-STORE-5 — S3 via presigned/signed plain HTTP; no AWS SDK.** Hand-rolled SigV4 (see D-DEP-3) because the SDK hides conditional-GET paths and adds a dependency; behavior matches Rust exactly.
  Sources: 03. Status: in force.
- **D-STORE-6 — Codec/wire additions (not in the Rust spec).** 32 MiB frame-length cap (bounds corrupt-varint allocation); explicit `ErrKindCorrupt` error kind ("bucket is wrong" vs "key absent"); map entries encoded in sorted key order (deterministic, decoder-irrelevant); `Timestamp` as explicit `{Seconds, Nanos}` struct.
  Sources: 02. Status: in force.
- **D-STORE-7 — Go-idiom interface adaptations in the store layer.** Typed `StoreError` with `errors.Is/As` sentinels (`PreconditionFailed` stays protocol-normal); `GetResult` as a two-variant interface union with `isGetResult()` marker; list callbacks (`fn func(ObjectMeta) error`) instead of lazy iterators; `casUpdate` split into `decode`/`encode`/`f`; memory store keeps one mutex (test double; CAS version behavior is what's exercised).
  Sources: 02. Status: in force.

## 6. HTTP & auth

- **D-HTTP-1 — Router is chi (Divergence D1); middleware is an ordered factory slice.** ~~Go 1.22 `http.ServeMux` + hand-rolled `/{owner}/{repo}[.git]/<sub>` fallback~~ superseded: chi core only (no `chi/cors`, no `chi/middleware`); the owner/repo parsing survives inside the trailing wildcard; middleware is an explicit ordered slice of `func(http.Handler) http.Handler` applied via chi `Use` — the order is load-bearing and data, not tower layers.
  Sources: 01, 06, 11, AGENTS L1. Status: in force (supersedes the ServeMux decision).
- **D-HTTP-2 — SSE writer mechanics.** `http.ResponseController` write deadlines (15 s per packet); keepalive + packet writes share one per-stream mutex (a stalled client must not pin a goroutine; interleaved writes must not tear) — tokio got both for free.
  Sources: 07. Status: in force.
- **D-HTTP-3 — Server Go adaptations.** No timeout/body-limit middleware (matching §20.4's truth about the Rust code; limits stay per-feature: push ingest, settings ≤ 16 KiB, blob ≤ 2 MiB, LFS cap); request-id "span" becomes structured-log record + `trace_id` extraction (slog-based, no otel); gzip request bodies decompressed straight off `r.Body` with `io.LimitReader` at git's own caps; graceful shutdown via `signal.NotifyContext` + two-phase `DrainState`, no shutdown library; install.sh + credential helper served from `embed` templates.
  Sources: 06. Status: in force.
- **D-API-1 — Render cache: revision-stamped entries + one weighted-bytes budget.** Bucket envelope carries `revision`; new config key `cache.render_cache_bytes` (default 256 MiB) replaces Rust's per-use `cache.*_entries` sizes; bucket/LRU split preserved.
  Sources: 07. Status: in force.
- **D-API-2 — API behavioral pins & clarifications.** Discovery document lists only real routes derived from the route table (kills the §20.4 phantom merge-queue advertisement); `%x00` field separators in `--format` argv; commit render = two `git show` invocations (header, then patch+numstat); trailer folding joins with `"\n"` + de-indented line; `?raw` blob responses uncapped (the 2 MiB cap is a JSON-shape rule); cross-host `POST ops/{op}` returns 409/SSE `error` (records are instance-local; no attachable stream).
  Sources: 07. Status: in force.

## 7. Git & WAL engine

- **D-ENG-1 — No gix/upload engine in Go v1; remote-served base refused.** Stock git for every fetch; a remote-served base (Rust: gix engine + remote-reader faulter) gets an explicit pkt ERR + 503/Retry-After naming the fix (`cache.store_mount` or fetch from the serving host). The faulter + remote reader stay for web API and fetch-gap cases. The Rust gix engine carried the 178 GB OOM and wrong-id thin-pack bug; v1 stays boring.
  Sources: 04, 05, 11. Status: in force.
- **D-ENG-2 — Connectivity via stock git pipeline.** `git rev-list --objects --stdin --not --all | git cat-file --batch-check` replaces the gix rev-walk (exact `stop_at_existing_refs`/missing-object semantics, zero walker code); per-ref attribution by bounded per-tip re-runs on failure.
  Sources: 04. Status: in force.
- **D-ENG-3 — v2 wire mechanics.** Fetch requests passed through byte-for-byte after guard parsing (no re-encode bugs); capability advertisement rendered by hand (ls-refs is ours, fetch is stock git's; advertised fetch features are the subset stock git accepts). (report-status-v2 negotiated-but-plain-`ng` is carried over from §20 item 12, not a new deviation.)
  Sources: 04. Status: in force.
- **D-ENG-4 — Caches & pools stdlib.** Ref-cache pending-overlay = copy-on-write slice swap under RWMutex (replaces generation-keyed moka); peel cache = one persistent `git cat-file --batch` per repo (no per-tag subprocess); blocking pool = semaphore-bounded goroutine pool (`git.max_git_procs`) replacing tokio's 4-worker bulk runtime + `spawn_blocking`.
  Sources: 04. Status: in force.
- **D-ENG-5 — WAL engine Go adaptations.** Publisher channel owned by the handle, never closed on respawn (jobs enqueued mid-respawn retained); group-commit = recv → try-drain → conditional 5 ms timer; eviction lock order fixed `syncMu → rw.TryWrite → stateMu`; `packed-refs` offline apply in-process (parse-map-rename, tmp+rename) instead of `git update-ref`.
  Sources: 05. Status: in force.
- **D-ENG-6 — `git.binary` plumbed everywhere.** One field on the git `Layer` (Rust hardcoded `"git"`, §20.5); all git invocations across packages go through it; makes the wrapper-script e2e proof possible.
  Sources: 04, 08, 15. Status: in force.

## 8. Concurrency, events, maintenance, extensibility

- **D-CONC-1 — Concurrency playbook substitutions (13).** Bulk worker pool = bounded channel + semaphore permits (permit acquired inside the pool, `gcs_bulk_permit` name kept); broadcast = ring-buffer drop-oldest (capacity 1024, `Lagged` becomes a counter, terminal packets from the task record); `sync.Map`/sharded maps replace `DashMap`; cancellation = `context.Context` tree + explicit two-phase drain (phase 1 interrupts tasks/`exec.CommandContext`, phase 2 `server.Shutdown`); watchdog diagnostics require (tick lateness, inflight) jointly; one enumerated lock-across-store exception (manifest freshness GET under `syncMu`).
  Sources: 05, 06, 13. Status: in force (ruling C-2: canonical primitive `internal/wal/rw.TryRWMutex`).
- **D-EVT-1 — Events bridge Go adaptations.** No in-process delivery retry (cursor untouched; replay at next wake-up; 10 s bound on request context, `http.Client.Timeout` unset); one bridge goroutine, sequential sinks, whole-batch single POST; non-blocking notify wake (bounded 64, `dropped` report, sweep backstop); sink = interface with one built-in webhook; `/_events/notify` report fields `repo`/`status` with `queued|dropped|ignored`; effective read start `max(cursor+1, min_seq)`.
  Sources: 09. Status: in force (per-sink cursors: see D-EXT-2).
- **D-MAINT-1 — `context.Context` is the only shutdown/timeout mechanism.** Drain cancellation, the 1 h unit wait, and git subprocess kills all hang off one tree; 1 h expiry releases only the pass's interest (task goroutine survives, matching "still running → move on") — replaces tokio `select!`/abort.
  Sources: 10, 13. Status: in force.
- **D-MAINT-2 — Rev-index built in-process from the `.idx`.** Byte-identical writer in `internal/git`; CLI `wal rev-index` shares the code; git's own `--rev-index` is not invoked.
  Sources: 10. Status: in force.
- **D-MAINT-3 — Maintenance pins.** Repair stays lease-less (optional `leases/repair.pb` noted, not required); heartbeats written on the pass goroutine only; stale-heartbeat purge = prefix list at pass start (no timer goroutine); rebuild pre-flight uses `statfs` on `cache.dir`; 48-skip stale-slot cap = plain per-repo-per-pass counter; `upstream.last_round` is instance-memory only.
  Sources: 10. Status: superseded by ruling C-3 (normalized coord semantics for ALL leases).
- **D-EXT-1 — Extensibility doc is new; registries are compiled-in.** No Rust counterpart: frozen contracts stay frozen, everything else registers (route/auth/task/CLI/policy/sink providers) — no dynamic loading, no cgo, no reflection DI. Auth becomes a provider chain (`none`/`token`/`oidc` frozen as first members, exact §8.8 resolution); policy effects are an open registry (`review-required`/`ci-required` honestly scoped to receive-pack); per-repo private-read ACLs explicitly deferred (seam named, not spec'd).
  Sources: 14. Status: in force.
- **D-EXT-2 — Extensible state/wire changes.** `events/cursor.json` generalizes to `events/cursors/<sink>.json` (key family added to the frozen overwritable list); bridge serialization weakens process-wide → per-repo single-flight (seq order + CAS cursor make cross-repo ordering meaningless); issues/PRs use sidecar state families, never WAL kinds (WAL kinds are closed); PRs = `refs/heads/pull/<n>/{head,merge}` maintainer-computed convention, DESIGN SKETCH; `issues/index.json` / `pulls/<n>/index.json` added to the frozen overwritable-key list. **Wave A amendment (2026-09-04, docs/features/01):** the identity families join the frozen overwritable list — `users/*/profile.json`, `users/*/invitations/index.json`, `orgs/*/org.json`, `orgs/*/members.json`, `orgs/*/teams/*.json`, `repos/<o>/<r>/access.json`; invitation objects stay Create-only immutable (delete-on-transition). The named `require_read` hook is specified (01 §4.1) and the api seam authenticates before dispatch.
  Sources: 14. Status: in force (refines 09's single-cursor carryover).

## 9. Naming & compat keepers that *changed* identity

- **D-NAME-1 — Product rename with wire keepers.** Binary `walhub`, module `git.packden.us/crueber/walhub`, `walhub serve` replaces `walgit-server` (one binary, no twin, no alias shipped); `Server` header → `walhub/<version> (<kind>; <name>[/<instance>])`; metric prefix `walgit_*` → `walhub_*` (names/semantics otherwise preserved); build-time env `WALHUB_BUILD_SHA` (no compat value). Keepers: `X-Walgit-*` header names, `wgt_` token prefix, `walgit_session` cookie in the auth cache key, proto package `walgit.v1` — edge contracts and stored credentials, not branding.
  Sources: 01, 03, 11, 16, AGENTS L1/L4. Status: in force.
- **D-CFG-1 — Config file: `<data-dir>/walhub.toml` default, `walgit.toml` alias.** Checked second (§3.1, §6.1); first-run and setup-UI files are always written as `walhub.toml`; TOML key names unchanged.
  Chain: earlier "keep `walgit.toml` as the implied default" → superseded in part by the data-dir relocation (2026-08-31).
  Sources: 01, 11, 16. Status: in force (supersedes walgit.toml-default).
- **D-CFG-2 — Env prefixes: `WALHUB__SECTION__KEY` primary, `WALGIT__` alias; `WALHUB_` wins on conflict.** Single-underscore `WALHUB_*` names remain the harness/process envs (`WALHUB_SIM_SEED`, `WALHUB_DATA_DIR`, test endpoints).
  Chain: earlier "stays `WALGIT__`" (and 15's original `WALGIT_` harness prefix) → superseded by the 2026-08-31 divergence.
  Sources: 01, 11, 15, 16. Status: in force (supersedes WALGIT__-primary).
- **D-CFG-3 — `gix` accepted as `git.upload_pack_engine` but treated as `git`.** Accepted values stay `auto | git | gix` for file compatibility; `auto` resolves to `git`. (ruling C-4: treated as `git` with a one-time startup WARN naming the key — 04's stance adopted).
  Sources: 04, 11. Status: in force.
- **D-CFG-4 — CLI & env additions.** `--json` flag on table-output commands (JSONL, stable snake_case); `RUST_LOG` kept as override of `telemetry.log_filter` with `WALHUB_LOG` as the new spelling (`walgit` log target mapped to `walhub`); unknown-key env overrides soft-ignored + reported, fatal only under `--strict`.
  Sources: 11. Status: in force.

## 10. Packaging

- **D-PKG-1 — Container & release.** Runtime image alpine (git ≥ 2.47), not debian-slim + tini (distroless/static lacks git; busybox `wget` covers HEALTHCHECK; zombie reaping via `os/exec` wait + orchestrator init); git-lfs omitted (client-side tool); startup git < 2.47 is fatal (exit 1); Go release flags `-trimpath -buildvcs=stamp -ldflags "-s -w"` replace thin LTO + unwind-panic profile (per-request/per-pass `recover` reproduces "one panic must not kill the instance"); Nix flake not ported; `install.sh` served from the Forgejo raw URL (no new public route); dev-rig rustfs credentials keep Rust values (`walgit-dev` / `walgit-dev-secret`, bucket `walgit-test`).
  Sources: 16. Status: in force.
- **D-PKG-2 — Web build stage minimal.** ~~Node 20 + pnpm@10 web stage~~ superseded twice by D2: removed entirely (zero-build), then restored minimal — a node stage exists ONLY to run esbuild (`pnpm run build:sdk` → `web/dist/repos.js`); the frontend is embedded raw; container default store path uses filesystem (D-STORE-1).
  Sources: 16. Status: in force (supersedes Node-20 web stage, twice).

## 11. Bundles

- **D-BUNDLE-1 — Hand-rolled cron parser.** Exact §8.3 syntax (6-field, lists/ranges/steps incl. `a/s`, 5 aliases, vixie dom/dow OR rule; names and `@every` rejected) — stdlib has no cron; deterministic calendar slots required for reproducible backfill.
  Sources: 08. Status: in force.
- **D-BUNDLE-2 — Scheduler pins (spec gaps closed).** `backfill_max` default 7 for the unspecified daily strategy; unchanged verdicts recorded as closed `SkippedSlot`s; unchanged gate compares against the newest `BundleEntry` of the strategy regardless of `base_id` (idle periods collapse across chain re-bases); plan windows pinned (fulls: `keep` newest; chain: ≥ newest base slot; non-chain: 2 newest); orphaned incrementals (base unlisted) dropped from rendering and pruned.
  Sources: 08. Status: in force.
- **D-BUNDLE-3 — Wire/ops pins.** v3 headers always carry `@object-format=<algo>` (sha1 included), ordered before `@filter`; composed-bundle checksum = sha1(header ∘ **local** base pack bytes) streamed on the composing host; compose uploads the header to scratch `wal/_compose/…` and uses the S3 part-1 trick (header ∘ first 5 MiB of the local pack); entry `id` = `<strategy>/<slotRFC3339Z>`, key slot text `20060102T150405Z` (keys discovered, never parsed); D17 refusal/fallback/narration texts spelled exactly, tracker = per-instance memory, 6 h TTL, cap 100 000 drop-oldest; task join stays keyed `(repo, bundle)` with singleflight + lease for per-strategy exclusion; bundle lease TTL 30 min with 5 min heartbeats (no config key). (Skew/epoch: ruling C-3 — normalized coord semantics, 2 s skew + epoch+1, wire format untouched.)
  Sources: 08. Status: in force.

## Rulings (2026-09-01 — the four cross-doc contradictions, resolved)

- **C-1 — `golang.org/x/sync` is NOT a dependency.** The final budget stands (chi, toml, x/net). The canonical bounded-parallelism primitives are the hand-rolled `internal/store/errgroup.go` (`Group` with `SetLimit` semantics + weighted semaphore); doc 13 amended. Status: resolved.
- **C-2 — The `rw` lock: `internal/wal/rw.TryRWMutex` is the canonical primitive.** The shipped, race-tested engine implementation (mutex + atomics, 100% covered) wins; `sync.RWMutex.TryLock` remains a documented fallback satisfying the same invariant. Doc 13 amended. Status: resolved.
- **C-3 — Bundle leases: normalized coord semantics for ALL leases.** 2 s skew tolerance and epoch+1 on steal/heartbeat everywhere (08's stance); the Rust bundle-lease quirks (no skew, epoch = 1) are deliberately not copied. The Lease wire format (proto fields) is untouched, so bucket compatibility is unaffected — the quirks never were a format concern. Doc 10 amended. Status: resolved.
- **C-4 — `gix` downgrade: one-time startup WARN naming the key.** 04's stance adopted (an inert config value must be visible to the operator); doc 11 amended. Status: resolved.
