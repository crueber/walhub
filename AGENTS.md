# AGENTS.md — operating manual for walhub (humans and agents)

**What this repo is:** the implementation workspace for **walhub** — a Go rewrite of the walgit git host.
`docs/MASTER_RUST_SPEC.md` specifies the system's behavior completely (it describes the Rust reference
implementation and is normative for behavior); `docs/go/*.md` specify how to build it in Go. When you are
asked to implement, review, or extend walhub, this file tells you how to work. Reading order:
`docs/go/README.md` → `docs/go/01_overview.md` → your task's doc → `docs/go/13_concurrency.md`.

> **Pre-1.0, no backwards compatibility within walhub.** Change the shape and delete the old shape in the
> same change — no aliases, shims, deprecated flags, or "still accepted for" branches. The ONE exception is
> bucket compatibility with the Rust implementation (below): formats on the bucket are append-only and
> shared.

## 1. The laws (violating any of these is a rejected change)

1. **Dependency budget is law.** Backend third-party modules: `github.com/go-chi/chi/v5` (the router),
   `github.com/BurntSushi/toml` (config), `golang.org/x/net` (h2c). Frontend: **zero** npm dependencies —
   the UI and SDK are vanilla standard ECMAScript modules (no TypeScript, no framework, no bundler, no
   build step). Anything else needs a written amendment in the relevant doc's "Decisions & deviations"
   section BEFORE the code lands. Hand-roll instead: S3 SigV4, GCS JSON API, protobuf wire codec,
   JWKS/JWT verification, SSE, Prometheus text exposition, LRU caches, singleflight, CLI dispatch, CORS
   and all other middleware (chi core only — no chi/cors, no chi/middleware packages).
2. **git is a subprocess, always.** Every git operation shells out to the `git` binary with the exact argv
   specified in `docs/go/04_git.md`. Never link a Go git library. Never deviate from the specified argv
   without updating the doc in the same change.
3. **Concurrency: goroutines yes, deadlocks never.** Every concurrent design carries a `### Concurrency`
   subsection (hazard + avoidance). The lock rules in `docs/go/13_concurrency.md` are binding:
   - Lock order `syncMu → packMu → rw`; never acquire out of order.
   - `rw` writes are **TryLock-only** (pack removal); a blocked writer starves readers, and a clone can
     hold a read guard for an hour. If TryLock fails, defer the removal to the next pass.
   - Never hold a lock across a store/network call unless the doc explicitly says so (it almost never does).
   - Channel rule: the sender owns and closes the channel; receivers never close; every goroutine exits via
     context.
   - Bulk bytes never share an http.Client or a semaphore with control-plane traffic (the 2026-08 incidents
     in `13_concurrency.md §7` are the proof).
4. **The bucket is the repository.** No state outside the object store may survive a restart (disk and
   memory are caches; "if every instance is wiped, what is lost?" must answer "warmth"). The manifest CAS
   is the only commit point. Never ACK a push before the bucket ACKs. No LIST on a hot path — probe, don't
   list. Every read revalidates (conditional GET) — there is no "eventually".
5. **Bucket compatibility with the Rust implementation.** Key layout, protobuf field numbers/wire encoding,
   JSON file shapes, and git wire behavior are byte-compatible (see `docs/go/02_storage_protobuf.md`).
   Golden fixtures in `testdata/` pin this; a change that breaks round-tripping against the fixtures is a
   bug even if both sides are ours. Proto is append-only: never reuse/renumber a field.
6. **Round trips are the cost model.** Happy-path budgets (push ≤ 5 requests, warm refs 1, cold refs 2,
   checkpoint 4) are asserted in the sim (`docs/go/15_testing.md`). A correct change that adds a sequential
   store round trip to a hot path is a regression. Parallelize independent PUTs; let conditional writes be
   the read; verification goes on the failure path.
7. **No silent waiting.** Long work is a task: unique id, `(repo, kind)` single-flight, progress packets,
   attachable SSE stream. A client must never stare at a silent spinner.
8. **Modularity seams are contracts.** New capabilities (issues, PRs, review, multi-user) attach ONLY
   through the registries in `docs/go/14_extensibility.md` (route providers, auth providers, policy
   effects, event sinks, task kinds, CLI subcommands). Core packages (`internal/store`, `internal/wal`,
   `internal/git`) must not import upward (`internal/server`, `internal/api`, feature packages), and
   feature state must never live on local disk or in the WAL without a schema decision.
9. **Fail closed — with one deliberate exception.** Config validation still refuses oidc without an
   allowlist and rejects unparseable policies; an invalid credential gets a real 401 (that is what makes
   git erase it), never a 200 with in-band error — except the exact four-condition case in
   `docs/go/06_server_http.md §8.4`. **Superseded by divergence:** auth `none` no longer requires a
   loopback bind — it is allowed anywhere (zero-config friendliness) with loud warnings in logs and the
   setup UI; the setup UI is the one-click path to real auth.
10. **Zero-config first run.** `walhub` with no config boots on sane defaults (`0.0.0.0:8080`, filesystem
    store under the data dir, auth `none`, `auto_create_on_push`) and the `/setup` UI configures
    everything. A present config file is validated before boot; an INVALID file puts the server in
    setup-only mode (everything but setup/health answers 503) until the user saves a fixed one. Setup
    save is validated, written atomically to `<data-dir>/walhub.toml`, and lists restart-required keys.
11. **Tests are the definition of done.** Backend: near-100% coverage, enforced — every `internal/...`
    package holds ≥ 95% statement coverage in CI (`make cover`), table-driven httptest for every handler,
    `-race` mandatory. Frontend: vanilla JS tested with Node's built-in `node --test` (logic modules
    headless-testable, DOM kept thin). Never merge with skipped tests; never weaken a budget assertion to
    make it pass.
12. **Documents change with the code.** Every doc has a `Decisions & deviations from the Rust design`
    section; decisions are appended there with a one-line rationale, never silently overridden. If code and
    doc disagree, fix one of them in the same change — say which and why in the commit.

## 2. Working rules for agents

- **Read your doc first.** Each `docs/go/NN_*.md` is self-sufficient for its package: interfaces, argv,
  wire shapes, concurrency rules, tests. Cross-references are by file name. Do not guess behavior that the
  Rust spec pins down — open `docs/MASTER_RUST_SPEC.md` and search it.
- **Implement in waves** (see `docs/go/README.md`): storage → git/WAL → HTTP/API → subsystems. Independent
  docs can be implemented in parallel by separate agents; the seams between them are the interfaces named
  in the docs (store.ObjectStore, wal.RepoHandle, api route providers). When two agents meet at a seam, the
  interface in the doc wins; propose amendments, don't freelance.
- **Tests are part of the definition of done.** Each package's doc names its tests (contract cases, budget
  assertions, e2e flows). `-race` is mandatory in CI; concurrency-heavy packages get stress tests
  (`-count=100`) and the deadlock canary; `make cover` enforces the ≥ 95% per-package gate. Never merge
  with skipped tests; never weaken a budget assertion to make it pass.
- **Never run unbounded commands.** Wrap test/build invocations in a timeout. `go test ./...` in this repo
  is tiered (see `docs/go/15_testing.md`) — run `make test` / `make race` / `make sim` for the tier you
  need, not the world.
- **Commit discipline.** One logical change per commit; the commit message states the doc section it
  implements and any decision it appends. Format with gofmt (`make fmt`); vet clean (`make vet`); the
  three allowed imports are the only non-stdlib imports a linter should ever flag.
- **Naming.** Binary `walhub`, module `git.packden.us/crueber/walhub`, packages exactly as in
  `docs/go/01_overview.md` (cmd/walhub, internal/{store,wal,git,bundle,policy,server,api,setup,events,maintain,config},
  web/). Wire/bucket identifiers (header names `X-Walgit-*`, config key names, bucket key paths, protobuf
  package `walgit.v1`) keep their Rust-era names — they are wire contracts, not branding.

## 3. Quick orientation (what runs where)

```
cmd/walhub          one binary; no subcommand = serve; roles by config (serve/maintain/events)
internal/store      ObjectStore interface, protobuf+framing codecs, filesystem/S3/GCS/memory backends, leases
internal/wal        the WAL engine: sync levels, publish/CAS, checkpoints, replay, remote reader, tasks
internal/git        the git subprocess layer (ingest, refs, pkt-line, receive/upload-pack, repack)
internal/server     HTTP (chi): middleware, routing, git/LFS/static endpoints, auth, setup recipes
internal/api        JSON API + SSE envelope + render caches (the /{o}/{r}/api[-browser] surface)
internal/setup      bootstrap + setup: first-run defaults, config schema for the UI, validated save
internal/bundle     bundle-uri scheduler: slots, chains, lists, D17
internal/events     WAL → webhook bridge (cursor, delivery, wake-ups)
internal/maintain   maintainer loop: checkpoints, bundles, compaction, fsck/repair, follow
internal/config     walhub.toml (optional) + WALHUB__ env overrides, per-repo settings, validation
internal/policy     push policy rule language (protect/history/size effects)
web/                vanilla ES-module SPA + SDK (zero npm deps, no build), embedded into the binary
```

## 4. Verification ladder (what to run before you say "done")

1. `make fmt && make vet` — clean.
2. Package tests for everything you touched: `make test` (or `go test ./internal/<pkg>/... -race`).
3. `make cover` — the ≥ 95% per-package gate holds with your change included.
4. Store contract suite: `make contract` (memory + filesystem always; S3 via the rustfs rig when you
   touched a backend).
5. `make sim` when you touched `internal/wal` or any publish/sync path (budget assertions included).
6. `make e2e` with real git when you touched `internal/git` or `internal/server` git routes.
7. `make test-web` (node --test) when you touched `web/`.
8. If you appended a decision: the doc's "Decisions & deviations" section updated in the same commit.
