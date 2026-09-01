# 01 — Overview

> Source: MASTER_RUST_SPEC.md §1, §2, §3 (incl. §3.5 Continuity lineage), §19, §21 · Status: normative for the walhub Go implementation.

walhub is the Go rewrite of walgit. The behavioral reference is [`../MASTER_RUST_SPEC.md`](../MASTER_RUST_SPEC.md)
("the Rust spec"); this document is the entry point of the Go specification. It states what walhub **is**, the
**laws** it is judged against, the **package layout**, the **dependency budget**, and the **build order** —
everything a reader needs before opening any other doc. Wherever a Rust/tokio mechanism is translated, the Go
equivalent is named; every rename or behavior change from the Rust spec is listed in the final section.

---

## 1. Product definition

**A share-nothing git host, fast for monorepos, with an object store as the *only* source of truth — one
binary anyone can run against a bucket, predictable enough that tooling can build on it.** (Rust spec §1.1,
verbatim; the product does not change with the language.)

walhub serves git over smart HTTP (protocol v0 and v2) plus bundle-uri, LFS, a browsing web UI and a JSON API with a
dependency-free SDK. Durable state is **only** an object-store bucket (S3-compatible or GCS). Every process instance is a
disposable cache: wipe all instances and you lose warmth, nothing else. There is no database, no queue, no Redis, no node
identity, no leader election, no gossip. Coordination happens only through bucket primitives: compare-and-swap on one tiny
manifest object, content-addressed immutable objects, and CAS+TTL leases.

It implements the architecture Cursor described in *Git at any scale* (their system "Continuity"), changed
where necessary to run on machines **smaller than the repository** (a few vCPU, tens of GiB of RAM, disk =
that memory). The additions over the source design — the **remote reader**, the **history pack**, and
**bundle-uri as the clone path** — are inherited unchanged; see §7.

---

## 2. Feature surface

The complete capability list is identical to the Rust spec §1.3; it is restated here so this doc stands alone.
Bucket formats, the git wire protocol, and config key names stay byte-compatible (README "Compatibility
contract").

| Area | Capabilities |
|---|---|
| git smart HTTP | v0 and v2 (`ls-refs` with prefixes/symrefs/peel/unborn; `fetch` with want/have, `filter`, `shallow`/`deepen`/`deepen-since`/`deepen-not`, `want-ref`, `sideband-all`, `no-progress`, `include-tag`, `wait-for-done`), `receive-pack` (create/delete/update refs, force detection, atomic, deletes, tags incl. peeled, push options, report-status, side-band-64k), `<owner>/<repo>[.git]` namespaces, sha1 and sha256 repositories, gzip request bodies, `object-info`-less (not implemented) |
| bundle-uri | v2 capability + `bundle-uri` command, static lists `bundles/list` (clones) and `bundles/catchup` (fetch recipes), per-strategy bundle objects with ETag/Range/immutable, calendar-slot scheduling with backfill, chained incrementals, blobless (`blob:none`) family on a filtered list, optional presigned store URLs, `bundles.require` refusal of unbounded zero-have clones with the exact fix in the error |
| LFS | Batch API (`operation=upload\|download`, transfer `basic`), basic transfer GET/HEAD/PUT + `verify`, sha256-addressed objects in the bucket, size+sha256 verification, `max_object_bytes` cap, optional read-through from an upstream LFS server (stream + persist), presigned or proxied serving |
| web UI | GitHub-shaped owners/repos pages, tree/blob/commits/commit pages, README rendering, markdown preview, syntax-highlighted blobs, unified/split diffs, branch/tag picker (SSE), clone menu with setup recipes, WAL health dashboard (manifest, packs, bundle chain + plan, compactions, segments, ops), tasks overlay, settings tab (scheduled tasks, push policy editor + validate + dry-run, effective config editor + history + revert). *Rust renders this in React; walhub renders the same surface in SolidJS — see 12_web_ui.md.* |
| JSON API | Repository-scoped read API (`refs`, `refs/{branches\|tags}` paged/streamed, `resolve`, `tree`, `blob`, `commits`, `commit`), repo summary/create/delete, per-repo settings (get/put/delete/effective/history/describe/validate), policy (get/put/delete/validate/dry-run), overview (WAL health), tasks + attachable SSE streams, ops (maintenance actions), owner listing, discovery document, `me`; two lanes (bearer/same-origin and cross-origin browser) |
| SDK | `repos.js` (IIFE → `window.repos`) + `repos.mjs` (ESM), dependency-free, maps the whole API, picks the lane, performs the popup auth flow, unwraps the SSE envelope |
| auth | `none` (loopback only, everyone anon+write+admin), `token` (static tokens as Bearer or Basic password), `oidc` (any OpenID Connect issuer: browser sign-in, verified ID tokens as bearers, `wgt_` access tokens minted at `/_auth/tokens`, static tokens for robots); admin flags independent of write; trusted-forwarder identity forwarding; real 401 semantics so git erases dead credentials |
| policy | Per-repo `policy.json` rule language: groups, ordered rules, ref/principal/path match, `protect` (AND-combined, enforced), `history`/`size` (parsed, not enforced), named-rule rejections on the wire |
| settings | Per-repo TOML overrides (`[bundles]`, `[maintenance]`, `[compaction]`, `[upstream]`, `[integrations]`, ≤ 16 KiB) published **into the WAL** (inline on the manifest + SETTINGS log entries with history) |
| events | A bridge (its own role) tails the WAL from a durable cursor and POSTs `ref` events (create/update/delete) to a webhook, at-least-once per batch, dedup key `(repo, seq, ref_name)`, HMAC signature, bucket-notification wake-up + periodic sweep |
| maintenance | One self-healing loop per maintainer: checkpoints, object repair from upstream, weekly full bundle (base rebuild on SSD hosts, then bucket-side compose), chained dailies/hourlies, geometric compaction, rev-index retrofit, periodic connectivity audit (`fsck`); placement-driven assignment; resumable base rebuilds and imports |
| CLI | `serve`, `compact`, `bundle run/plan/compose/rm`, `repo create/list/info/policy/settings`, `wal ls/show/materialize/add-pack/annotate-pack/rev-index`, `synth`, `import` (two modes, resumable), `mirror`, `config check/dump`. Binary name: `walhub`; `walhub serve` is the server (Rust: `walgit` / `walgit-server`) |
| stores | S3 + S3-compatible (AWS, MinIO, rustfs, R2, Ceph), GCS (JSON API over HTTPS — Rust uses a gRPC+JSON hybrid, see §9.1), in-memory (tests) — one contract suite runs against all |
| ops | `/healthz`, `/readyz` (prewarm/drain aware), Prometheus `/metrics`, structured logs (pretty/JSON) with request ids and trace correlation, lock-wait instrumentation, runtime watchdog, graceful two-phase drain |

Explicitly **not** in scope (Rust spec §1.3): code review, merge queues, CI, issues, fork networks, PRs.
(Commit-trailer conventions exist so external merge-queue tools can integrate; walhub only renders trailers.)

---

## 3. Acceptance bars

"Done" means these numbers, unchanged from Rust spec §1.4:

| Claim | Bar |
|---|---|
| Cold instance useful in seconds | `ls-remote` of the largest repo < 1 s on a fresh instance, even while its packs install |
| CI clone of a 57 GiB / 73 M-object / 466 k-ref monorepo | `clone --filter=blob:none --depth=1 --sparse --single-branch -c transfer.bundleURI=false` in seconds (reference: 2075 s → 8 s) |
| Developer catch-up | a days-stale `fetch` on main = exactly the missed bundle slots + < 1 h of objects from upload-pack |
| Fresh clone | bytes through the server ≈ one hour of pushes (reference: 32.7 GB static, 2.8 MB through upload-pack) |
| Web UI on the monorepo | tree/blob/commits render without packs on disk, ~100–200 ms warm |
| Push | acknowledged only after the bucket ACKs; one CAS per batch; the maintaining host writes |
| Consistency | push then fetch anywhere sees it; concurrent pushers: exactly one winner (simulation suite proves) |
| Security | every route authenticated in token/oidc mode; fail-closed config; dead credential ⇒ real 401 |
| Data completeness | every object reachable from an advertised ref is in a live pack; weekly fsck + repair keep it so |

The last two proof-bearing rows are covered by 15_testing.md (simulation/contract suites) and 03_store_backends.md (round-trip budgets).

---

## 4. Non-goals

From Rust spec §1.5, unchanged:

- Millions of tiny repositories (the long tail is served, not tuned for).
- A 30 GB base repack on a tmpfs host (that is the SSD host's weekly job).
- Forking git or inventing an object format; all weirdness happens *around* git.

One forward-looking caveat: GitHub-like features (multi-user accounts, issues, PRs, review) are **not** a
non-goal forever — they are explicitly deferred. walhub reserves clean seams for them today so they never
require a core rewrite; see §9.3 and 14_extensibility.md.

---

## 5. The ten principles (restated for Go)

A rewrite MUST preserve all ten (Rust spec §2); every design decision answers to them. The numbering and the
law are identical; each entry adds the Go mechanism that enforces it.

1. **No state outside the object store.** Disk and memory are caches. If every instance is wiped, what is
   lost must be "warmth". No local queues, flag files, or env-encoded data. In Go: no goroutine may persist
   state anywhere but the store; scratch dirs live under `cache.dir` and are disposable.
2. **The manifest CAS is the only commit point.** Immutable objects are never overwritten (put mode `Create`
   for content-addressed objects). The only overwritable objects: `manifest.pb`, `bundles/list.pb`,
   `leases/*`, bucket-root `maintain/<host>.pb`, `events/cursor.json`, `policy.json`, `fsck.pb`, render
   cache. Nothing is visible before the manifest CAS; everything after it is idempotent and replayable.
3. **Side effects are readers of the WAL, never steps of a write.** Events, mirrors, notifications tail the
   log from a durable cursor. A webhook failure must never fail a push. In Go: the events bridge is its own
   goroutine (or role) with a context-scoped shutdown; the push path has no webhook dependency at all.
4. **Every read revalidates; there is no "eventually".** Each request starts with a conditional GET of the
   manifest (skippable only within `wal.freshness_ttl`). Push acknowledged ⇒ the next request anywhere sees
   it. In Go: the hand-rolled backends implement conditional GET (`If-None-Match` → 304) natively; the
   in-process monotonic revision guard still applies after a local publish.
5. **Serve from the parts that fit; never a bigger box, never a hard-coded host.** No hostname may appear in
   application code; placement is configuration (`[placement]` globs).
6. **Never block a request goroutine; bulk bytes never share a lane with the control plane.** Go translation of the tokio
   rule: git/fs work runs on a bounded worker pool, never on the `net/http` serve path; pack bytes use dedicated
   transports/permits. The concrete discipline is in §9.2 and 13_concurrency.md — this is a hard, incident-proven rule.
7. **No LIST on a hot path; count the round trips.** Probe (404s are free), don't list. Every protocol change
   is judged on sequential bucket round trips. The Go backends keep the exact per-operation budgets of
   03_store_backends.md.
8. **Standalone first; the edge announces, the app never assumes.** The full product works with nothing in
   front. Any capability an edge takes over is announced per request in `X-Walgit-Capabilities` (header name
   kept verbatim; see the Decisions section at the end).
9. **No silent waiting.** Long work is a task with an id, a log and a progress stream, narrated to git
   (sideband 2) and browsers (SSE). In Go: a per-repo broadcast pub-sub with lag-tolerant subscribers; tasks
   publish regardless of listeners (13_concurrency.md).
10. **Keep it small; upstream git does git things.** Stock `git` binary (`os/exec` with exact argv) for
    repack/bitmaps/bundle creation/upload-pack wherever it can read the packs; in-process git engines only
    where measured and correct (the remote-served-base fetch); protobuf wire format is append-only.

---

## 6. System architecture

### 6.1 Components

```mermaid
flowchart LR
  subgraph clients
    G["git / git-lfs"] --- B["browsers / SDK / scripts"]
  end
  subgraph fleet["walhub fleet — one binary (cmd/walhub), roles by config"]
    S["serve role<br/>internal/server + internal/api"]
    M["maintain role<br/>internal/maintain + internal/bundle"]
    E["events role<br/>internal/events"]
  end
  subgraph core["shared core (per-repo handle)"]
    WAL["internal/wal<br/>WAL engine: sync levels, publish/CAS ladder,<br/>checkpoints, replay, remote reader, tasks"]
    GIT["internal/git<br/>stock git subprocess: ingest, refs,<br/>receive-pack, upload-pack, repack"]
    POL["internal/policy<br/>push rules"]
  end
  K[("object store bucket<br/>internal/store — the only durable state")]
  N["optional nginx/CDN edge:<br/>TLS, auth_request cache, byte offload"]
  W[webhook consumer]
  U[upstream git host e.g. GitHub]
  G --> N --> S
  B --> N --> S
  S --> WAL
  M --> WAL
  E --> WAL
  WAL --> GIT
  WAL --> POL
  WAL <--> K
  GIT <--> K
  E --> W
  M --> U
  S -. "push broker forward (optional)" .-> S
```

- **One binary**, `walhub` (Go module `git.packden.us/crueber/walhub`), roles by config: `server.roles = ["serve",
  "maintain", "events"]`; empty = all three (one-box shape). Any number of `serve` instances may point at one bucket.
  Per repo, exactly one maintainer (placement globs) performs object work and writes; other hosts serve refs-level reads.
- **The bucket is the repository.** Layout under `repos/<owner>/<repo>/`: `manifest.pb` (CAS'd), `log/<seq>.pb` segments,
  `wal/<checksum>.pack|.idx|.rev|.bitmap|.commit-graph`, `checkpoints/<seq>/`, `bundles/`, `leases/`, `policy.json`,
  `lfs/objects/`, `events/cursor.json`, `fsck.pb`, `cache/api/v1/`; plus bucket-root `maintain/<host>.pb` heartbeats.
  Complete schema: 02_storage_protobuf.md.
- **Write path (push):** our receive-pack indexes the incoming pack locally (`git index-pack` in a scratch git-dir),
  checks connectivity, evaluates push policy, uploads `pack ∥ idx ∥ log entry` to the bucket, then CASes the manifest
  (group-committed per repo per instance). Client sees `ok` only after the bucket ACKs. On CAS loss: re-read, re-validate
  every old value, retry with jittered backoff (Rust spec §3.1–3.2; ladder in 05_wal_engine.md).
- **Read path:** one conditional GET of `manifest.pb` per request → 304 serve local / 200 apply the tail.
  What "apply" means depends on the request's **sync level** (Rust spec §6.2, specified in 05_wal_engine.md):
  refs only; refs + packs-that-fit; full materialization; or objects via the remote reader.
- **Placement is configuration** (`[placement] serve/serve_exclude/maintain/maintain_exclude` globs). A host that does
  not *serve* a repo answers object work (upload-pack, receive-pack, LFS transfer, prefetch) with **503 +
  `Retry-After: 15`** and a pkt-line `ERR walgit: <repo> is served by <host>; retry shortly` **before any sync**;
  refs-level reads (info/refs, ls-refs, API refs/resolve/overview, UI, bundle lists) stay available everywhere. Routing
  to hosts is by repo prefix only (`/<owner>/<repo>` — git, bundles, LFS, API, UI all live under it).
- **Push broker (optional):** a host that maintains nothing may forward receive-pack bodies to a single-writer broker
  (`wal.push_broker_url` + `wal.push_broker_token`; the broker lists the token in `server.auth.tokens` and its principal
  in `server.auth.trusted_forwarders`; end-user identity travels in `X-Walgit-Principal`). Fallback to local publish if
  the broker is down (the buffered body is replayable up to `wal.push_broker_buffer_bytes`). Publish is CAS-safe, so the
  broker is an optimization, never a dependency.
- **Edge (optional):** TLS, one cached `auth_request` verdict per credential, repo-prefix routing, and byte
  offload via `X-Accel-Redirect` — only when the edge announces `X-Walgit-Capabilities: accel-redirect,
  client-authorization` per request and walhub answers `X-Walgit-Store-Url/-Authorization/-Key`. An edge's
  fallback for a placed repo is read-only and refs-level; git RPC and LFS writes have no fallback.

### 6.2 Consistency model (inherited verbatim)

- The manifest is a single object with CAS; its `revision` field is a monotonic counter of successful writes. A 412 is
  the normal contention signal: re-sync, re-verify, retry.
- Non-412 store errors during the manifest CAS are **ambiguous** (the write may have landed and the response was lost):
  the writer re-reads the manifest fresh ("cas_landed") and treats "my log segment is listed" as committed. A
  written-but-uncommitted log segment (orphan) is harmless: later writers burn past its seq (seqs are not dense), and it
  is swept after a later commit.
- Readers guard against a stale manifest read *after* their own local publish: a manifest older than the one held in
  memory is ignored (monotonic revision guard).
- Local application of a committed ref transaction happens **after** the CAS, under the refs lock; the new manifest
  version is advertised only after local refs are written; if local application fails the version advertisement is
  withdrawn so the next sync replays — but the push is still answered `ok`, because the bucket CAS is the truth.

### 6.3 The machine model

- Instance = a few vCPU, tens of GiB RAM, disk ≈ RAM (tmpfs), CPU may be throttled between requests.
  `cache.max_bytes` (default 20 GiB) is the budget for everything on disk in **budget** mode.
- Object store facts (GCS-class): GET/PUT small object 60–80 ms; conditional GET → 304 ≈ 15 ms; 404 free; LIST slow/paged
  (never hot path); CAS overwrite serialized ~1 write/s per object; range read ~100 MB/s per connection (stripe for more);
  compose = 1 request, ≤ 32 sources.
- Operations needing real disk or hours of CPU (full `repack -adb` of a monorepo, weekly 30 GB bundle) run on the **SSD
  host** (`maintenance.disk = "ssd"`, `cache.mode` auto-resolves to `disk`) using the same binary and protocol.

Rust spec §3.4's process inventory, translated to goroutines. Every loop takes a `context.Context` from the
root (SIGTERM → `signal.NotifyContext`), so drain cancels everything once.

| Goroutine(s) | Role | Go shape |
|---|---|---|
| HTTP serve | serve | `net/http` server with h2c (`golang.org/x/net/http2h2c`), ALPN TLS (`crypto/tls`), `TCP_NODELAY` per conn; one goroutine per request |
| Runtime watchdog (1 s tick) | all | `time.Ticker`; gauges tasks running + inflight; warns "runtime stalled" when a tick is > 2.5 s late (`inflight = 0` at a late tick ⇒ platform paused the process) |
| **Bulk pool** (4 workers) | serve/maintain | fixed-size worker goroutines fed by a buffered channel of capacity = worker count; pack materialization ONLY — never on request goroutines |
| Prewarm | serve | bounded parallelism (`cache.prewarm_parallelism`, default 2) over `cache.prewarm[]`; `/readyz` 503 until done or `cache.prewarm_ready_timeout` |
| Maintainer loop | maintain | `time.Ticker` at `maintenance.interval` (60 s): heartbeat, then one bounded unit per assigned repo |
| Upstream-follow loop | maintain | ticker at `maintenance.follow_interval` (30 s) |
| Events bridge | events | wake-up channel fed by `POST /_events/notify` + sweep ticker at `events.sweep_interval` |
| Legacy compact/bundle loops | — | when `maintain` is absent but `compact`/`bundle` role present: 60 s passes calling the respective ops (kept for role decomposition) |
| Drain (SIGTERM) | all | phase 1: stop starting maintenance units, interrupt the running unit, serving + `/readyz` stay up, bounded 30 s; phase 2: `/readyz` 503 + `Retry-After`, new fetch/push/LFS refused 503, in-flight capped at `server.drain_timeout`, exit |

Instance identity: `WALHUB_INSTANCE_NAME`/`WALHUB_INSTANCE_ID` → `name/id`; else `HOSTNAME/pid`; else UUID
(env prefix changed from `WALGIT_`; see Decisions). Appears as lease holder, manifest `writer`, task `hostname`,
heartbeat `host`, and the `Server` header (`walhub/<version> (<kind>; <name>[/<instance>])`, kind =
`serverless|ssd|dev`).

#### Concurrency

- **Hazard:** 9+ long-lived loops and a per-request goroutine model can deadlock via cross-lock ordering,
  unbounded buffering, or a worker pool that starves the loops feeding it.
- **Avoidance (canonical rules, see 13_concurrency.md):** lock order is always refs-phase lock → pack lock,
  and the pack lock is taken with `TryLock` (pack removal defers to the next sync instead of blocking); the
  bulk pool's channel is bounded at the worker count so a full pool applies backpressure to the *submitter*,
  never to request goroutines; every channel names its owner and closer in the doc that introduces it; every
  goroutine exits on context cancel; no request goroutine ever waits on a task it did not start.

---

## 7. Lineage: Continuity, and the 7 deviations

walhub inherits walgit's lineage (Rust spec §3.5); the deltas are **design law**, not notes.

**What the Continuity post establishes, and walhub keeps as-is:**

- *Why packfiles make hosting hard:* repository data is a DAG; every git operation is a random walk over
  gigabytes of delta-compressed packs whose physical layout has no correlation with the logical graph.
  Networked filesystems fail on this; object-level DHT storage fails on round trips. Hence: keep real git
  repositories on fast local disks and never put the pack bytes on a network filesystem.
- *The three good choices (inherited):* (1) don't distribute git itself — work at the packfile level;
  (2) store data as actual git repositories on local NVMe so upstream git does the work; (3) keep all copies
  **consistently** in sync — git clients and CI break on eventual consistency.
- *Continuity's core (inherited):* a **write-ahead log in object storage is the source of truth**. A push = an immutable
  WAL entry (packfile + ref transaction) that is **never acknowledged before it is fully persisted**; visibility happens
  only when a tiny index object is updated with an atomic compare-and-swap — **the CAS is the consensus**. Any server can
  be primary; a losing pusher refetches and retries (exactly one winner). Local copies are warm caches, materialized from
  the WAL when missing; no routing table, no elections, no relational database. Every read revalidates (conditional GET;
  304 ≈ 15 ms). Compaction is amortized: only the primary repacks, and the result is *published into the WAL*. The WAL is
  full provenance: every push and every compaction is an entry, replayable to any point. Reads scale linearly with
  replicas; push throughput is bounded only by the store's CAS latency (group commit + push broker exist to push against
  the same cap).

**The 7 deviations (each is a decision in force; a rewrite MUST keep all of them):**

1. **No gossip.** Continuity gossips UDP hints so replicas can skip the conditional GET after a push; walhub
   has **no node-to-node networking at all** (share-nothing, no node identity). Every read revalidates via
   one conditional GET — correctness never depends on a lost datagram.
2. **Placement is configured, not rendezvous-hashed.** Assignment is explicit `[placement]` globs because the
   deployment shape (one big SSD host + many small tmpfs hosts) is a human decision and an edge must be able
   to route by repo prefix deterministically. A push broker plays the "preferred primary" role as an
   optimization, with CAS-safety as the correctness backstop.
3. **The manifest is richer than a WAL pointer.** It carries the denormalized live pack set, the checkpoint
   pointer, log segment inventory, per-repo settings inline, and a revision counter — so a refs-level sync
   needs exactly one GET to know *what* to fetch (round-trip rule: carry state in the object you already
   fetch).
4. **Checkpoints fold the log.** Explicit CHECKPOINT entries (ref snapshot + pack inventory at a seq) mean
   cold start = snapshot + tail, never full replay, and old log segments are GC'd behind `min_seq`.
5. **Serving machines smaller than the repository** — the reason this implementation exists as a separate product: the
   **remote reader** (web UI/API on a repo whose packs can never fit the instance, objects faulted by 1 MiB range reads
   through the manifest's pack indexes), the **history pack** (commits + trees kept local next to a bitmap'd base so
   history walks never touch blob bytes), and **bundle-uri as the clone path** (fresh clone = static bucket files;
   upload-pack only answers the small remainder).
6. **Leases for cross-instance exclusivity.** CAS is enough for pushes; CAS+TTL leases exist for *maintenance*
   work (compaction, bundle builds) where two workers would duplicate hours of CPU.
7. **Receive-pack is ours.** walhub implements receive-pack itself (index-pack in a scratch dir, connectivity,
   policy, report-status) so pushes become WAL entries + group-committed CAS batches rather than a local ref
   update with replication.

Everything else — the CAS commit point, WAL entries as immutable objects, cache-not-truth local copies,
linearizable pushes, provenance — is inherited and MUST survive the rewrite.

---

## 8. Canonical package tree

Go module: `git.packden.us/crueber/walhub`. These paths are normative; every other doc designs against them.

| Package | Responsibility |
|---|---|
| `cmd/walhub` | The single binary: subcommand dispatch (serve, compact, bundle, repo, wal, synth, import, mirror, config), signal handling, exit codes 0/1/2/3 |
| `internal/config` | `walhub.toml` loading (`github.com/BurntSushi/toml`), env overrides (`WALHUB_*`), defaults, validation, per-repo settings parsing |
| `internal/store` | The object store interface + backends (memory, S3 via hand-rolled SigV4, GCS via JSON API), CAS helper, bucket key layout, the hand-rolled protobuf wire codec, log framing |
| `internal/wal` | The WAL engine: per-repo handles, sync levels, publish/CAS ladder, group commit, checkpoints, replay, remote reader, narrated tasks |
| `internal/git` | The git subprocess layer: exact-argv `os/exec` calls — ingest/index-pack, refs, connectivity, receive-pack, upload-pack, repack/bitmaps, bundle creation |
| `internal/bundle` | The bundle-uri subsystem: strategies, calendar slots, backfill, chained incrementals, blobless family, `bundles/list` + `bundles/catchup` |
| `internal/policy` | The per-repo push policy rule language: parse, validate, evaluate, dry-run |
| `internal/server` | HTTP middleware, routing (`net/http.ServeMux` patterns), git smart-HTTP + LFS + static endpoints, auth (none/token/oidc), setup recipes, h2c/TLS listener |
| `internal/api` | The JSON API wire contract, SSE envelope, tasks surface, two-lane auth, render caches |
| `internal/events` | The WAL → webhook bridge: durable cursor, delivery + HMAC signing, bucket-notification wake-ups, sweep |
| `internal/maintain` | The maintainer loop: checkpoints, compaction, base rebuild, fsck/repair, upstream follow, leases |
| `web/` | The SolidJS SPA + the dependency-free `repos.js`/`repos.mjs` SDK, embedded into the binary |

Cross-cutting: `internal/store`, `internal/wal`, `internal/git` are the frozen core; `internal/server`,
`internal/api`, `internal/events`, `internal/maintain`, `cmd/walhub` are consumers wired through the
registries of §9.3.

---

## 9. Go rewrite north stars

### 9.1 Minimal dependencies

The entire Go backend allows exactly **two** third-party modules — no exceptions without a written amendment:

| Module | Why it earns its place |
|---|---|
| `golang.org/x/net` | h2c serving (HTTP/2 without TLS) — `net/http` cannot do it alone |
| `github.com/BurntSushi/toml` | Config parsing; hand-rolling a TOML parser is a spec bug |

Everything else is stdlib or hand-rolled:

- S3 SigV4 signing (`crypto/hmac`, `crypto/sha256`) — validated against AWS official test vectors (03_store_backends.md).
- GCS via the JSON API over plain HTTPS — **no gRPC client** (the Rust gRPC+JSON hybrid collapses to JSON API only).
- Prometheus text exposition, SSE encoding, JWT/JWKS verification (`crypto/rsa`, `crypto/ecdsa`,
  `encoding/base64`), protobuf-wire-compatible encoding (02_storage_protobuf.md; **no**
  `google.golang.org/protobuf`), singleflight/errgroup equivalents, CLI subcommand dispatch, HTTP routing
  (Go 1.22+ `ServeMux` patterns), weighted LRU caches.
- git is ALWAYS the subprocess `git` binary with exact argv — never go-git or a VCS library.

Frontend budget (12_web_ui.md only): **≤ 6 npm direct dependencies** (SolidJS, NOT React; router; at most a
markdown and a diff renderer); prefer platform primitives (fetch, streams, ES modules).

### 9.2 Goroutine-first concurrency, zero deadlocks

Goroutines are the performance model — I/O parallelism everywhere: striped per-object uploads, group-commit
batch workers, SSE fan-out, sweep loops, striped range reads for the remote reader. The discipline:

- **Never block a request goroutine on bulk work.** Git subprocesses, connectivity walks, index-pack, and pack materialization
  run on bounded pools (`errgroup`-style semaphores or the 4-worker bulk pool), never inline.
- **No unbounded buffering.** Every channel/queue has a stated bound; backpressure lands on the submitter.
- **Every goroutine has a shutdown path** — a `context.Context` from the root; drain cancels it.
- **Lock order is fixed and small.** Per repo handle: sync lock then pack lock; the pack lock is acquired with `TryLock` —
  a blocked writer must never stall a clone; defer to the next sync instead (Rust spec §19's try-lock rule, kept verbatim).
- **Single-flight on shared fetches.** Concurrent fetches of one block/manifest/render entry collapse into one
  round trip (hand-rolled singleflight; see 03_store_backends.md, 07_api.md).

[13_concurrency.md](13_concurrency.md) is the canonical playbook; every other doc's `### Concurrency`
subsection references it rather than re-explaining.

#### Concurrency

- **Hazard:** Go's `sync.RWMutex` blocks *new readers* while a writer waits — the mirror image of the tokio
  hazard the Rust spec warns about; a naive pack-eviction write lock stalls every clone.
- **Avoidance:** pack removal takes the pack lock with `TryLock`; on failure it skips eviction and lets the
  next sync retry. Readers never wait on writers for correctness — only for progress they would redo.

### 9.3 Modularity seams for future GitHub-like features

Multi-user accounts, issues, PRs, and review are deferred, not forbidden. The core contracts below are frozen
NOW so those features land as extensions:

| Seam | Contract today | Future feature plugs in via |
|---|---|---|
| Auth principals | `internal/server` resolves every request to a **principal** (`name`, `write bool`, `admin bool`); `none`/`token`/`oidc` are providers behind one interface (06_server_http.md) | A `users` provider backed by a DB, without touching middleware or git routes |
| Policy | `internal/policy` evaluates per-repo rules; effects are an ordered-rule table, not hardcoded verbs (Rust spec §14, doc in 04_git.md context) | New rule effects (e.g. `require_review`) append to the table |
| Events | `internal/events` consumes WAL tail and posts to **one webhook sink** behind a sink interface (09_events.md) | Multiple sinks, internal queues, notification preferences |
| API route registration | Every JSON route registers through a single mux-registration function in `internal/api`; git/LFS/static routes likewise in `internal/server` (06_server_http.md, 07_api.md) | `/issues`, `/pulls` route groups register alongside, sharing middleware and principals |
| Task kinds | Long work is a narrated task with a `(repo, kind)` lock (05_wal_engine.md); kinds are a registry | `merge`, `import-pr` kinds with zero core changes |
| CLI subcommands | `cmd/walhub` dispatches from a subcommand table (11_config_cli.md) | New operator tooling without touching core packages |

Rule: core (`store`, `wal`, `git`) never imports `api`, `server`, `events`, or `maintain`; extension features
may import core and register into seams. 14_extensibility.md maps each feature onto these seams in detail.

### 9.4 Implementation sequencing (bottom-up, parallel-friendly)

Each layer is testable against the Rust spec alone before the next exists. Waves may run in parallel inside a
wave; arrows are hard dependencies.

| Wave | Docs (implement in this order within the wave) | Needs |
|---|---|---|
| 1 | 02_storage_protobuf.md (store interface + proto codec + keys) ∥ 11_config_cli.md (config + CLI skeleton); then 03_store_backends.md (S3/GCS) and 15_testing.md's store contract suite | — |
| 2 | 04_git.md (git subprocess layer) ∥ 05_wal_engine.md (sync refs, publish with group commit, checkpoints); then 13_concurrency.md rules are already in force from the first goroutine | 02 + 03 |
| 3 | 06_server_http.md (HTTP + auth), then 07_api.md (JSON API + SSE); 08_bundles.md, 09_events.md, 10_maintenance.md can start as soon as 05 is done | 04 + 05 |
| 4 | 12_web_ui.md (SPA; the SDK can start in wave 1 — it only needs 07's shapes on paper) | 06 |
| last | 16_packaging.md (build, container, nginx edge, compose) | all |

Always-on: read 13_concurrency.md before writing any goroutine; read 14_extensibility.md before freezing any interface.
Port early, from 15_testing.md: the simulation suite and the round-trip budget assertions — they encode the design's
correctness AND its cost model (Rust spec §19's highest-value-tests note, kept).

---

## 10. The 30-second quickstart

Adapted to walhub naming (config keys unchanged; file name `walhub.toml` is primary, `walgit.toml` still accepted — see
Decisions at the end).

```toml
# walhub.toml
[store]
backend = "s3"            # s3 | gcs | memory
bucket   = "walhub"
prefix   = ""             # optional key prefix

[server]
listen = "127.0.0.1:8080"

[server.auth]
mode = "token"
[[server.auth.tokens]]
principal = "alice"
token     = "whdev-local-token"
write     = true
admin     = true
```

```console
# serve (all three roles on one box; for local play, a memory store also works:
#   store.backend = "memory" — data lives only for the process lifetime)
$ walhub serve --config walhub.toml

# push something
$ cd my-repo
$ git remote add origin http://alice:whdev-local-token@127.0.0.1:8080/alice/my-repo.git
$ git push -u origin main

# clone elsewhere — a fresh clone reads the bundle list, not upload-pack
$ git clone http://alice:whdev-local-token@127.0.0.1:8080/alice/my-repo.git

# browse http://127.0.0.1:8080/alice/my-repo — UI, API, and WAL health on the same prefix
```

`walhub config check --config walhub.toml` validates before serving (exit codes: 0 ok, 1 error, 2 usage,
3 crash). Full key reference: 11_config_cli.md; auth modes and the OIDC flow: 06_server_http.md.

---

## 11. Glossary (Rust spec §21, restated in Go terms)

| Term | Meaning |
|---|---|
| manifest | `repos/<o>/<r>/manifest.pb` — the CAS'd linearization point: head_seq, live pack set, checkpoint pointer, log segments, settings, revision. Decoded by `internal/store`'s hand-rolled protobuf codec |
| WAL / log | append-only `LogEntry` stream in `log/<seq>.pb` segments; kinds PUSH, COMPACT, REF_UPDATE, CHECKPOINT, SETTINGS |
| sync level | how much of the repo a request materializes: Refs < Serve < Full; Objects = Serve-or-remote-reader |
| remote reader | object access over 1 MiB range reads + pack indexes when the pack set cannot fit the instance (`internal/wal`) |
| history pack | tier-2-derived commits+trees pack kept local so history walks never touch blob bytes |
| base (tier 2) | one bitmap'd pack (+ side files) fully representing the repo at a checkpoint; rebuilt only on SSD hosts, weekly |
| slot / creationToken | a calendar fire time of a bundle strategy; token = slot epoch; content = WAL state as of it (`internal/bundle`) |
| chain | dailies/hourlies cut on the previous bundle of their own strategy while newer than the base |
| lease | CAS+TTL protobuf object; the only cross-instance mutex (`internal/store` primitives, used by `internal/maintain`) |
| task | a narrated unit of long work: id, (repo,kind) lock, packet stream, per-instance record (`internal/wal`) |
| group commit | N concurrent pushes → one log PUT + one manifest CAS (`wal.batch_window`/`wal.max_batch`); the batch worker is a goroutine with a bounded queue |
| burn | advancing past an orphaned log slot after a crashed writer (seqs are not dense) |
| D17 | the bundle-uri forcing policy for unbounded zero-have fetches |
| bulk pool | the Go translation of the Rust "bulk runtime": 4 dedicated worker goroutines for pack materialization, fed by a bounded channel; never runs on request goroutines |
| singleflight | hand-rolled in-flight dedup: concurrent fetches of one key collapse into one round trip |
| h2c | HTTP/2 cleartext; walhub serves it via `golang.org/x/net` when `server.http2 = true` |

---

## Decisions & deviations from the Rust design

- Binary `walhub`, module `git.packden.us/crueber/walhub`, `walhub serve` replaces `walgit-server` — the product name is the only user-visible rename; behavior is identical.
- Config file primary name is `walhub.toml` with `walgit.toml` still accepted as a fallback alias — keys and semantics stay byte-compatible, so existing buckets and docs keep working (11_config_cli.md owns the alias).
- Environment prefix becomes `WALHUB_*` (e.g. `WALHUB_INSTANCE_NAME`, overrides `WALHUB__SECTION__KEY=value`) with the legacy `WALGIT_*`/`WALGIT__*` spellings accepted as fallback aliases so walgit-era deploy scripts keep working; when both are set for the same key `WALHUB_` wins. Env vars are not bucket state — no compatibility cost (11_config_cli.md owns the ladder).
- HTTP header names `X-Walgit-Capabilities`, `X-Walgit-Principal`, `X-Walgit-Store-Url/-Authorization/-Key` are kept **verbatim** — they are part of the edge↔app contract and the nginx example; renaming would strand mixed walgit/walhub fleets and existing edge configs.
- `Server` header becomes `walhub/<version> (<kind>; <name>[/<instance>])` — cosmetic identity; kind taxonomy unchanged.
- OIDC access-token prefix stays `wgt_` — tokens are config-issued (not bucket state) and the SDK/docs reference the prefix; renaming buys nothing and breaks recognition.
- GCS backend is JSON-API-over-HTTPS only (the Rust gRPC+JSON hybrid drops gRPC) — honors the two-module dependency budget; the store interface (§4.1 semantics) is unchanged, and conditional-GET/CAS map to `If-None-Match`/generation preconditions.
- Protobuf is encoded by a hand-rolled wire codec on the same field numbers (no `google.golang.org/protobuf`) — dependency budget; wire format is frozen and tested against fixtures (02_storage_protobuf.md).
- Prometheus exposition is a hand-rolled text renderer with the metric names of Rust spec §8.10 kept exactly — dashboards/alerts depend on the names (07_api.md).
- Package layout fixed to the §8 tree — `internal/wal` exists as its own package (Rust spread WAL code across crates) so the engine has one seam; consumers never import git internals directly.
- Future forge features (multi-user, issues, PRs) are deferred with the §9.3 seams reserved — the Rust spec lists them as non-goals only; the seams make the deferral cheap instead of permanent.
