# walhub — Go Implementation Specification

The Go rewrite of the walgit design. The behavioral reference is
[`../MASTER_RUST_SPEC.md`](../MASTER_RUST_SPEC.md) (the Rust implementation's complete specification);
this directory specifies **how to build walhub in Go** — and where walhub deliberately diverges: a Chi
router, a SolidJS SPA frontend (D-WEB-6), zero-config first run with a setup UI, a local
filesystem store backend, and Make-based tooling. Everything not listed as a divergence follows the Rust
spec exactly.

**Read `01_overview.md` first.** Everything else is per-subsystem and can be read (and implemented) in the
order that matches your work.

## The four laws of this rewrite

1. **Minimal dependencies.** The backend allows exactly three third-party modules: `github.com/go-chi/chi/v5`
   (the router — core only, all middleware hand-rolled), `github.com/BurntSushi/toml` (config), and
   `golang.org/x/net` (h2c). Everything else is stdlib or hand-rolled (SigV4, GCS JSON API, protobuf wire
   codec, JWKS, SSE, Prometheus exposition, LRU, singleflight). The frontend budget (amended
   2026-09-02 by explicit user request — DEVIATIONS.md D-WEB-6): runtime npm dependencies are exactly
   `solid-js` + `@solidjs/router`; state management is Solid's own signals/stores + context (no additional
   state library); styling is Tailwind CSS v4 (CSS-first `@import "tailwindcss"` + `@tailwindcss/vite`,
   no config file, no CDN, dark mode by default); still no TypeScript (plain JSX/JS). Dev-time tooling:
   `vite` + `vite-plugin-solid` + `@tailwindcss/vite` build the SPA into `web/dist/`, and `esbuild`
   bundles the modular SDK (`web/sdk/src/*.js`) into the shipped `web/dist/repos.js`. If a design reaches
   for another dependency, it is a spec bug.
2. **Goroutines for performance, zero deadlocks.** I/O parallelism everywhere (striped uploads, batch
   workers, SSE fan-out, sweeps); every concurrency recommendation carries a `### Concurrency` subsection
   naming the hazard and the avoidance. [`13_concurrency.md`](13_concurrency.md) is the canonical playbook —
   lock ordering, try-lock rules, singleflight, bounded parallelism, channel ownership, drain.
3. **Modular core, extensible edges.** Core contracts (store, WAL, git, HTTP, auth) are frozen;
   route registration, policy effects, event sinks, auth providers, task kinds, and CLI subcommands are
   registries. [`14_extensibility.md`](14_extensibility.md) maps future GitHub-like features onto those
   seams without touching core.
4. **User friendliness is a feature.** `walhub` runs with NO config on sane defaults (`0.0.0.0:8080`, local
   filesystem store, first push creates the repo); the `/setup` UI exposes every option with a validated
   save-to-disk; an invalid config file boots into setup-only recovery instead of refusing to start.
   Backend code ships with near-100% test coverage (≥ 95% per package, CI-enforced); the JS is tested with
   Node's built-in runner.

## Compatibility contract

Bucket formats (key layout, protobuf wire encoding, JSON files), the git wire protocol, and TOML config
key names stay **byte-compatible with the Rust implementation**. A bucket written by walgit MUST be
readable by walhub and vice versa.

## The documents

| Doc | Topic | Implements |
|---|---|---|
| [`01_overview.md`](01_overview.md) | Product, principles, package tree, dependency budget, build order | — |
| [`02_storage_protobuf.md`](02_storage_protobuf.md) | Bucket keys, protobuf wire codec (hand-rolled), log framing, store interface, CAS helper | `internal/store` |
| [`03_store_backends.md`](03_store_backends.md) | S3 (hand-rolled SigV4), GCS (JSON API), **filesystem**, leases, striped I/O, round-trip budgets | `internal/store` |
| [`04_git.md`](04_git.md) | git subprocess layer: ingest, refs, pkt-line, receive-pack, upload-pack, repack, bundles | `internal/git` |
| [`05_wal_engine.md`](05_wal_engine.md) | Sync levels, publish/CAS ladder, checkpoints, replay, remote reader, tasks | `internal/wal` |
| [`06_server_http.md`](06_server_http.md) | Chi routing, middleware, git/LFS/static endpoints, auth (3 modes), **bootstrap + setup UI/API** | `internal/server` |
| [`07_api.md`](07_api.md) | JSON API wire contract, SSE envelope, tasks, caching, render recipes | `internal/api` |
| [`08_bundles.md`](08_bundles.md) | Slots, chains, backfill, blobless family, lists, D17 forcing | `internal/bundle` |
| [`09_events.md`](09_events.md) | WAL → webhook bridge: cursor, delivery, wake-ups | `internal/events` |
| [`10_maintenance.md`](10_maintenance.md) | Maintainer loop, compaction, base rebuild, follow, fsck/repair | `internal/maintain` |
| [`17_ssh.md`](17_ssh.md) | SSH git transport: x/crypto/ssh listener, key auth, command framing | `internal/sshd` |
| [`11_config_cli.md`](11_config_cli.md) | Every config key (optional file), per-repo settings, env overrides, CLI reference | `internal/config`, `cmd/walhub` |
| [`12_web_ui.md`](12_web_ui.md) | SolidJS SPA + SDK (D-WEB-6), **setup UI**, `node --test` suite | `web/` |
| [`13_concurrency.md`](13_concurrency.md) | **The concurrency playbook** — referenced by every other doc | cross-cutting |
| [`14_extensibility.md`](14_extensibility.md) | Core-vs-extension seams; issues/PRs/multi-user roadmap | cross-cutting |
| [`15_testing.md`](15_testing.md) | Test pyramid, store contract, FaultStore sim, coverage gate, Make targets | `internal/*/…test` |
| [`16_packaging.md`](16_packaging.md) | Build, container, **zero-config quickstart**, nginx edge, local rig, CI | repo root |

## Implementation order (parallel-friendly)

```
Wave 1 (independent):  02 storage+proto ─┬─ 03 backends (incl. filesystem) ── 15 store contract
                       11 config+cli    ─┘
Wave 2 (needs 02+03):  04 git  ─┬─ 05 wal ─┬─ 06 server (chi + setup) ── 07 api
                                │          ├─ 08 bundles
                                │          ├─ 09 events
                                │          └─ 10 maintenance
Wave 3 (needs 06):     12 web ui + setup frontend (the SDK can start in wave 1 — it only needs 07's
                       shapes on paper)
Always:                13 concurrency (read before writing any goroutine), 14 extensibility (before
                       freezing any interface), 16 packaging (last)
```

`04` and `05` are the heart; `13` is the law. A feature branch per doc works: each doc names its package,
its interfaces, and its tests, and the cross-references are by file name.

## Decisions & deviations from the Rust design

- **Frontend is a SolidJS + Tailwind v4 SPA (2026-09-02, explicit user request — DEVIATIONS.md D-WEB-6).**
  Law 1's frontend budget above supersedes the earlier zero-dependency vanilla-ESM reading: runtime npm
  dependencies are exactly `solid-js` + `@solidjs/router`, styling is Tailwind v4 CSS-first (dark by
  default, no CDN), still no TypeScript, and the SPA is vite-built into `web/dist/` (the SDK stays
  dependency-free and esbuild-bundled).
