# walhub — Go Implementation Specification

The Go rewrite of the walgit design. The behavioral reference is
[`../MASTER_RUST_SPEC.md`](../MASTER_RUST_SPEC.md) (the Rust implementation's complete specification);
this directory specifies **how to build the same system in Go** — with a minimal dependency footprint,
goroutine-first concurrency that cannot deadlock, and seams for growing into a full forge (multi-user,
issues, PRs).

**Read `01_overview.md` first.** Everything else is per-subsystem and can be read (and implemented) in the
order that matches your work.

## The three laws of this rewrite

1. **Minimal dependencies.** The entire backend allows exactly two third-party modules:
   `golang.org/x/net` (h2c) and `github.com/BurntSushi/toml` (config). Everything else is stdlib or
   hand-rolled (SigV4, GCS JSON API, protobuf wire codec, JWKS, SSE, Prometheus exposition, LRU,
   singleflight). The frontend budget: ≤ 6 npm direct dependencies (SolidJS + router + at most a markdown
   and a diff renderer). If a design reaches for another dependency, it is a spec bug.
2. **Goroutines for performance, zero deadlocks.** I/O parallelism everywhere (striped uploads, batch
   workers, SSE fan-out, sweeps); every concurrency recommendation carries a `### Concurrency` subsection
   naming the hazard and the avoidance. [`13_concurrency.md`](13_concurrency.md) is the canonical playbook —
   lock ordering, try-lock rules, singleflight, bounded parallelism, channel ownership, drain.
3. **Modular core, extensible edges.** Core contracts (store, WAL, git, HTTP, auth) are frozen;
   route registration, policy effects, event sinks, auth providers, task kinds, and CLI subcommands are
   registries. [`14_extensibility.md`](14_extensibility.md) maps future GitHub-like features onto those
   seams without touching core.

## Compatibility contract

Bucket formats (key layout, protobuf wire encoding, JSON files), the git wire protocol, and TOML config
key names stay **byte-compatible with the Rust implementation**. A bucket written by walgit MUST be
readable by walhub and vice versa. `walgit.toml` remains a valid config file name.

## The documents

| Doc | Topic | Implements |
|---|---|---|
| [`01_overview.md`](01_overview.md) | Product, principles, package tree, dependency budget, build order | — |
| [`02_storage_protobuf.md`](02_storage_protobuf.md) | Bucket keys, protobuf wire codec (hand-rolled), log framing, store interface, CAS helper | `internal/store` |
| [`03_store_backends.md`](03_store_backends.md) | S3 (hand-rolled SigV4), GCS (JSON API), leases, striped I/O, round-trip budgets | `internal/store` |
| [`04_git.md`](04_git.md) | git subprocess layer: ingest, refs, pkt-line, receive-pack, upload-pack, repack, bundles | `internal/git` |
| [`05_wal_engine.md`](05_wal_engine.md) | Sync levels, publish/CAS ladder, checkpoints, replay, remote reader, tasks | `internal/wal` |
| [`06_server_http.md`](06_server_http.md) | Middleware, routing, git/LFS/static endpoints, auth (3 modes), setup/recipes | `internal/server` |
| [`07_api.md`](07_api.md) | JSON API wire contract, SSE envelope, tasks, caching, render recipes | `internal/api` |
| [`08_bundles.md`](08_bundles.md) | Slots, chains, backfill, blobless family, lists, D17 forcing | `internal/bundle` |
| [`09_events.md`](09_events.md) | WAL → webhook bridge: cursor, delivery, wake-ups | `internal/events` |
| [`10_maintenance.md`](10_maintenance.md) | Maintainer loop, compaction, base rebuild, follow, fsck/repair | `internal/maintain` |
| [`11_config_cli.md`](11_config_cli.md) | Every config key, per-repo settings, env overrides, CLI reference | `internal/config`, `cmd/walhub` |
| [`12_web_ui.md`](12_web_ui.md) | SolidJS SPA (≤ 6 deps) + dependency-free `repos.js` SDK + embedding | `web/` |
| [`13_concurrency.md`](13_concurrency.md) | **The concurrency playbook** — referenced by every other doc | cross-cutting |
| [`14_extensibility.md`](14_extensibility.md) | Core-vs-extension seams; issues/PRs/multi-user roadmap | cross-cutting |
| [`15_testing.md`](15_testing.md) | Test pyramid, store contract, FaultStore sim, wire-compat fixtures | `internal/*/…test` |
| [`16_packaging.md`](16_packaging.md) | Build, container, config examples, nginx edge, local rig, CI | repo root |

## Implementation order (parallel-friendly)

```
Wave 1 (independent):  02 storage+proto ─┬─ 03 backends ── 15 store contract
                       11 config+cli    ─┘
Wave 2 (needs 02+03):  04 git  ─┬─ 05 wal ─┬─ 06 server ── 07 api
                                │          ├─ 08 bundles
                                │          ├─ 09 events
                                │          └─ 10 maintenance
Wave 3 (needs 06):     12 web ui (SDK can start in wave 1 — it only needs 07's shapes on paper)
Always:                13 concurrency (read before writing any goroutine), 14 extensibility (before
                       freezing any interface), 16 packaging (last)
```

`04` and `05` are the heart; `13` is the law. A feature branch per doc works: each doc names its package,
its interfaces, and its tests, and the cross-references are by file name.
