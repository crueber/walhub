# AGENTS.md — operating manual for walhub (humans and agents)

**What this repo is:** the implementation workspace for **walhub** — a Go rewrite of the walgit git host.
`docs/MASTER_RUST_SPEC.md` specifies the system's behavior completely (it describes the Rust reference
implementation and is normative for behavior); `docs/go/*.md` specify how to build it in Go. When you are
asked to implement, review, or extend walhub, this file tells you how to work — and the same rules bind
human contributors; humans additionally start at [`README.md`](README.md) for setup and day-to-day
workflow. Reading order: `docs/go/README.md` → `docs/go/01_overview.md` → your task's doc →
`docs/go/13_concurrency.md`. Work on the collaboration layer (issues, PRs, review, checks,
notifications, orgs) additionally starts at `docs/features/README.md` — its primitives are frozen
contracts, exactly like `14_extensibility.md`.

> **Pre-1.0, no backwards compatibility within walhub.** Change the shape and delete the old shape in the
> same change — no aliases, shims, deprecated flags, or "still accepted for" branches. The ONE exception is
> bucket compatibility with the Rust implementation (below): formats on the bucket are append-only and
> shared.

## 1. The laws (violating any of these is a rejected change)

1. **Dependency budget is law.** Backend third-party modules: `github.com/go-chi/chi/v5` (the router),
   `github.com/BurntSushi/toml` (config), `golang.org/x/net` (h2c), `golang.org/x/crypto` (SSH server
   transport only — amended 2026-09-02 by explicit user request for SSH git transport; see
   `docs/go/17_ssh.md` decision 17.1). Frontend (amended 2026-09-02 by
   explicit user request — the user directed the SolidJS SPA + Tailwind replacement of the vanilla-ESM
   UI; agents must treat this as approved, see `DEVIATIONS.md` D-WEB-6): runtime npm dependencies are
   exactly **`solid-js` + `@solidjs/router`**; state management is Solid's own signals/stores + context
   (NO additional state library); styling is **Tailwind CSS v4** (CSS-first `@import "tailwindcss"`,
   `@tailwindcss/vite` plugin, no config file, no CDN, dark mode by default). Still NO TypeScript
   (plain JSX/JS). Dev-time tooling: `vite` + `vite-plugin-solid` + `@tailwindcss/vite` build the SPA
   into `web/dist/`, and `esbuild` (unchanged) bundles the modular SDK (`web/sdk/src/*.js`) into the
   shipped `web/dist/repos.js` — the SDK itself stays dependency-free. Anything else needs a written
   amendment in the relevant doc's "Decisions & deviations" section BEFORE the code lands. Hand-roll
   instead: S3 SigV4, GCS JSON API, protobuf wire codec, JWKS/JWT verification, SSE, Prometheus text
   exposition, LRU caches, singleflight, CLI dispatch, CORS and all other middleware (chi core only —
   no chi/cors, no chi/middleware packages).
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
    `-race` mandatory. Frontend: JS tested with Node's built-in `node --test` (logic modules
    headless-testable, DOM kept thin). Never merge with skipped tests; never weaken a budget assertion to
    make it pass.
12. **Documents change with the code.** Every doc has a `Decisions & deviations from the Rust design`
    section; decisions are appended there with a one-line rationale, never silently overridden. If code and
    doc disagree, fix one of them in the same change — say which and why in the commit.

## 2. Working rules (agents and humans — the same bar)

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

Performance claims (scale, latency shape) are **evidence-backed**: measured
entries with reproduction harnesses live in [`docs/EVIDENCE.md`](docs/EVIDENCE.md).
A transport/storage feature that makes a hot-path claim gets an entry there —
measured on the real code path, named backend, both population sizes, harness
committed to `internal/devtools/`.

### Field lessons (each one shipped a real failure — do not relearn them)

- **Env overlay keys are exact paths.** `WALHUB__STORE__S3__ENDPOINT`, never `WALHUB__STORE__ENDPOINT` —
  a missing segment is recorded as an unknown key and silently ignored; `walhub config dump` (or
  `check`) is how you catch it. rustfs/MinIO need `store.s3.force_path_style = true` (virtual-host
  addressing 403s); healthchecks pin `127.0.0.1` (busybox `localhost` = `::1`, rustfs binds IPv4);
  pnpm 11 needs `allowBuilds: { esbuild: true }` in `web/pnpm-workspace.yaml` (the package.json
  `pnpm` field is dead). The compose examples encode all of this.
- **`--data-dir` flag syncing must preserve the env overlay.** It re-points only the flag-derived
  paths (`DataDir`, `Store.Root`, `Cache.Dir`); re-deriving `FirstRunDefaults` there silently
  discarded every `WALHUB__*` override on zero-config boots (shipped and reverted once).
- **Setup values round-trip through the overrides channel.** The setup API accepts exactly the
  spellings the TOML file accepts: sizes (`64MiB`), `d`/`w` durations, floats, unsigned counters,
  and `[[name]]` TOML fragments for struct slices (`bundles.strategy`, `server.auth.tokens`) decoded
  by the field's toml name. `configCoerce` must use the shared `config` parsers — a UI save failing
  "not an int"/"unsupported type" means the coerce path regressed.
- **Schema lookups are by key name, never by section group.** Group layout changes (auth was split
  out of `server`); `effectiveValue`/`restartKeys` and any new schema consumer match keys directly,
  or `requires_restart` and diffs silently pollute with keys that did not change.
- **Server values render in the spec spelling** (`64GiB`, `1h` — `fmtSpecSize`/`fmtSpecDuration`);
  the setup page's editable values and its per-field examples (`FIELDS[].ex/note`) share one
  spelling, and every example is tested to validate (client) and accepted by `/api/v1/setup/test`
  (server).
- **`node --test`: pass files or globs, never a directory.** Node 22 executes a directory positional
  as a module (`Cannot find module …/web/test`); Node 24 accepts both. All entry points (`make
  test-web`, Woodpecker, the GH workflow) use `web/test/unit/*.test.js`.
- **Tests that spawn git pin their identity.** Annotated tags and `commit-tree` need an
  author/committer; runners and containers have no global git config. `TestMain` in `internal/git`
  and `internal/maintain` sets `GIT_AUTHOR_*`/`GIT_COMMITTER_*` for spawned subprocesses only.
  Reproduce environment gaps with `env -i PATH=… HOME=/tmp/nohome go test …`.
- **Fresh clones compile.** `web/dist/.keep` is tracked because `go:embed all:dist` fails on a
  missing directory — never gitignore it away, and build the web BEFORE `go test` in CI (the embed
  shell is what `/setup` 200s on).
- **GHCR publishing is the GitHub workflow's job.** Every push to the GitHub mirror
  (`github.com/crueber/walhub`, auto-pushed from this Forgejo origin) runs
  `.github/workflows/docker.yml`: test job → buildx publish of `ghcr.io/crueber/walhub`
  (`latest`/`main`/semver/`sha-*`, linux/amd64). The test gate means a broken main publishes
  nothing. README "Run with docker compose" documents pulling.
- **Canonical host redirect bites headless UI drives.** `canonicalBrowserHost`
  (`docs/go/06_server_http.md §2.2 #2`) 302s browser-looking loopback GETs from `127.0.0.1:<port>`
  to `walgit.localhost:<port>` — curl fails the browser test and never redirects, so "curl says 200"
  proves nothing about a browser path. Drive the UI against the canonical host (or expect the hop).

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
internal/sshd       SSH git transport (x/crypto/ssh): sessions, key auth, command parsing
internal/config     walhub.toml (optional) + WALHUB__ env overrides, per-repo settings, validation
internal/policy     push policy rule language (protect/history/size effects)
web/                SolidJS SPA (JSX, no TypeScript; solid-js + @solidjs/router; Tailwind v4, dark by
                    default) built by vite into web/dist/, plus the dependency-free modular SDK esbuild-
                    bundled to web/dist/repos.js — `make web` runs both builds and `make build` depends
                    on it. The binary embeds dist/: the SPA shell at / and every UI route, hashed
                    assets at /_ui/assets/* (immutable), the bundle at /repos.js (module MIME is
                    load-bearing); /setup is a SPA route (open in setup-only mode too)
Dockerfile          three-stage image (web build → static Go binary → alpine+git runtime, nonroot)
compose*.y(ml)      compose.standalone.yml = walhub alone (filesystem store); compose.yaml = walhub +
                    rustfs + create-bucket (the S3 rig, also `make dev-store`); both build from source
.github/workflows   docker.yml on the GitHub mirror: test job → publish ghcr.io/crueber/walhub
.woodpecker/        the Forgejo-origin CI pipeline (vet → web → test → js-test → … → image)
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
8. **Real browser when you touched anything browser-facing** (`web/`, static/UI serving, auth flows,
   setup): load `/`, a repo page, and `/setup` in actual Chromium and check the console. Module
   scripts are MIME-enforced and import-map driven — "curl says 200" has shipped a blank page before
   (the uiAsset stub incident, 2026-09-01). A headless-Chrome CDP drive counts; a DOM-level fetch
   smoke does not. In this workspace the browser daemon is the hub-managed `chrome-cdp` process on
   :9222 — drive it over the CDP WebSocket (navigate, evaluate, screenshot); remember the canonical
   host redirect (§2 field lessons) when pointing it at a loopback port.
9. If you appended a decision: the doc's "Decisions & deviations" section updated in the same commit.
