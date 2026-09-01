# 10 — Maintenance (internal/maintain)

> Source: MASTER_RUST_SPEC.md §13 (13.1–13.5), §7.9, §4.9, §6.5, §6.8, §15.1 (maintenance.*, compaction.*, maintenance.disk, placement.*) · Status: normative for the walhub Go implementation.

The maintainer is a self-healing background engine: every 60 s it looks at each repo it is assigned to and performs exactly one bounded unit of work — or none. It repairs integrity before building anything, folds packs geometrically, rebuilds weekly bases, audits connectivity, and follows an upstream mirror. Everything it does is narrated as a task, lease-guarded against other instances, and observed via heartbeats and Prometheus metrics. In Go this is a goroutine tree in `internal/maintain` with no third-party dependencies beyond the project-wide allowance.

## 1. Package layout

```
internal/maintain/
  maintain.go     // Maintainer: loop goroutine, assignment, pass scheduler, heartbeat
  plan.go         // unit selection per repo (priority list, wrong-host planning)
  units.go        // unit interfaces + registry; one file per unit group if preferred:
  checkpoint.go   // unit 1 (thin wrapper over internal/wal checkpoint writer)
  repair.go       // unit 2 (upstream fetch-as-pack; §7.9 argv)
  bundles.go      // unit 3 (delegates slot planning to internal/bundle)
  compact.go      // unit 4 (geometric fold) + base rebuild (phase machine)
  rebuild.go      // unit 3b BaseRebuild phase machine
  revindex.go     // unit 5
  fsck.go         // unit 6 + repair trigger
  follow.go       // upstream-follow loop (own goroutine, NOT a unit)
  heartbeat.go    // MaintainerHeartbeat writer + purge + tickers
```

Dependency direction: `maintain` → `wal`, `git`, `bundle`, `store`, `config`, `policy`(read-only), `events`(none). It never imports `api`/`server`. The follow loop shares `git` helpers with `repair` but runs on its own goroutine owned by `Maintainer` (see §8).

## 2. Configuration surface (names unchanged from the Rust spec)

| Key | Default | Used by |
|---|---|---|
| `maintenance.interval` | `"60s"` | pass cadence |
| `maintenance.checkpoints` | `true` | unit 1 enabled |
| `maintenance.max_pack_bytes` | `"0B"` | capacity cap; `0` = cache budget (`cache.max_bytes` in budget mode; unlimited in `disk` mode) |
| `maintenance.disk` | `"tmpfs"` | `"ssd"` allows base rebuilds and larger working sets |
| `maintenance.host` | instance id | heartbeat object name `maintain/<host>.pb` |
| `maintenance.fsck_interval` | `"7d"` | unit 6 cadence; `0` = off |
| `maintenance.follow_interval` | `"30s"` | follow loop cadence; `0` = off |
| `compaction.enabled/factor/trigger_packs/trigger_bytes/lease_ttl/retention_superseded` | `true/2/16/1GiB/10m/7d` | unit 4 |
| `[placement] maintain` / `maintain_exclude` | `["*"]` / `[]` | assignment globs (`owner/name`, `owner/*`, `*`) |
| `server.roles` | `[]` (all) | `maintain` (implies compact+bundle) gates the loop |
| `upstream.git` / `upstream.token_env` / `upstream.follow` | — | units 2, follow loop |

Example (`walgit.toml`):

```toml
[maintenance]
interval   = "60s"
disk       = "ssd"            # this host may rebuild bases
max_pack_bytes = "40GiB"      # declared capacity; 0 = cache budget
fsck_interval  = "7d"

[compaction]
enabled = true
factor  = 2

[placement]
maintain        = ["*"]
maintain_exclude = ["archive/*"]
```

Env override forms work as elsewhere: `WALGIT__MAINTENANCE__INTERVAL="30s"`.

## 3. The Maintainer loop

### 3.1 Structure

`Maintainer` owns:

- one **loop goroutine**: ticker at `maintenance.interval`; each tick = one **pass**.
- one **follow goroutine** (§8), started/stopped independently.
- a `context.Context` derived from the server's; `context.CancelFunc` is the ONLY shutdown signal. Drain (SIGTERM) cancels it: the running unit's git subprocess receives SIGKILL-equivalent termination via its `exec.CommandContext`, and no new unit starts.

Assignment per pass: enumerate repos from the registry/catalog, keep those matching any `[placement] maintain` glob and none of `maintain_exclude`. Globs: `owner/name` exact, `owner/*`, `*`. Per-repo settings (D24, merged over host config by the `wal` engine's `with_settings`) are evaluated fresh each pass — a repo that turns compaction off stops triggering.

### 3.2 Pass semantics

Each pass, per assigned repo, in registration order:

1. Compute the desired-state snapshot from `(config ⊕ repo settings, WAL state)`.
2. `Select()` the single most important missing unit via the exact priority list (§4).
3. Run it as a **bounded task** through `internal/wal`'s task registry (kinds: `checkpoint`, `repair`, `compact`, `bundle`, `rev-index`, `fsck`, `follow` — see 6.8): `task.Begin(repo, kind)`; a second start of the same `(repo, kind)` joins the running task (single-flight, §7).
4. **Wait cap 1 hour** (`ctx, cancel := context.WithTimeout(passCtx, time.Hour)`). If the timeout fires: the task is NOT killed — the pass moves on and the task remains discoverable in the task table (its goroutine owns its own lifetime; the pass only releases interest). Concretely: `Select` → wrap unit in a task → `go run(unit)`; the pass `select`s on `taskDone(ctx)` vs `time.After(1h)`; on timeout it logs "still running; will re-check next pass" and counts the unit as `running` in metrics.
5. Repeat 2–4 until `Select()` returns idle, **with one re-plan exception**: after a bundle unit that built nothing (no slot settled, retention no-op), re-plan once so a pending BaseRebuild or compaction triggered by the same pass is not starved by bundle bookkeeping. Cap: at most 48 skip-outcomes through stale slots per repo per pass (guard against pathological bundle planning); hitting it ends the repo's turn.
6. Record `walgit_maintain_pass_seconds{host}` and `walgit_maintain_units_total{host,kind,outcome}` (outcome ∈ `ok|error|timeout|wrong-host|held|idle`), `walgit_maintain_unit_seconds{kind}`.

The pass NEVER blocks on bulk store I/O for unselected repos: selection reads only manifest/checkpoint refs already synced at refs level (cheap), plus local disk state (pack directory sizes, rev/bitmap presence via `stat`).

### 3.3 Lease discipline (every unit)

Per §4.9: each unit acquires its lease BEFORE any work and releases it after (`defer`). The compact/base-rebuild lease is `leases/compact.pb`, TTL `compaction.lease_ttl` (10 m), **no heartbeat** (TTL + release is the design; a rebuild longer than the TTL relies on the phase marker + idempotent publish, §6). Bundle units use `leases/bundle-<strategy>.pb` (TTL per strategy; **ruling C-3: normalized coord semantics — the 2 s skew tolerance and epoch+1 on steal/heartbeat apply to ALL leases**; the Rust bundle-lease quirks are deliberately not copied, and the Lease wire format is untouched so bucket compatibility is unaffected). A unit that finds its lease held (not stealable, i.e. `now < expires_at + 2s`) reports outcome `held` and moves on — never waits, never retries within the pass. Acquire uses the CAS ladder (absent → `Create` epoch 0; present+expired+skew → `Update` with `epoch+1`; 412 → lost race → `held`).

## 4. Unit priority (EXACT selection order)

`Select(repo, snapshot)` walks this list top-down and returns the first unit whose trigger predicate holds. There are no weights, no scoring: order IS priority. `snapshot` = the pass's desired-state view: manifest (`head_seq`, `packs[]`, `checkpoint`), repo settings, `fsck.pb` (cached from last audit), bundle plan (computed lazily by the bundles unit only), local pack-dir state.

| # | Unit | Trigger predicate (ALL parts must hold) | Lease | Task kind |
|---|---|---|---|---|
| 1 | **Checkpoint** | `maintenance.checkpoints` (or repo setting) AND any §6.5 trigger fired: entries since last cp ≥ `wal.snapshot_every_entries` (256) ∥ tail bytes (segments with `last_seq > cp.seq`) > `wal.checkpoint_tail_bytes` (8 MiB) ∥ age ≥ `wal.checkpoint_interval` (1 h, measured from `checkpoint.created_at`, or `manifest.updated_at` when no checkpoint). `0` value disables a given trigger. | none (checkpoint writes are idempotent Create+immutable) | `checkpoint` |
| 2 | **Repair** | `fsck.pb` exists AND `len(missing) > 0` AND `repaired_seq == 0` AND `upstream.git` configured. Runs §7.9 `fetch_objects_as_pack` (§6 below). | none (add-pack is CAS-safe) | `repair` |
| 3 | **Bundles** | `bundles.enabled` (or repo setting). Applies retention, settles closed slots, plans slots (§5). The **plan** yields a concrete op: a missing slot on a Full strategy on an **ssd** host whose base is due (§6 triggers) yields **BaseRebuild**; otherwise `bundle strategy=<s> slot=<n>`. Outcome states per slot: `built|missing|pending|blocked|too-small|skipped|unavailable|wrong-host`. | `leases/bundle-<strategy>.pb` for the strategy actually built | `bundle` |
| 4 | **Compaction** | `compaction.enabled` AND (tier-0 pack count ≥ `compaction.trigger_packs` (16) OR tier-0 bytes > `compaction.trigger_bytes` (1 GiB)) AND ≥ **2 fresh packs** (a single pack never folds into itself) AND the pack set fits this host (§5 wrong-host). | `leases/compact.pb` | `compact` |
| 5 | **Rev-index** | ∃ live pack with `!has_rev` AND `object_count ≥ 250 000` (oldest first, file-local build). Push packs below the threshold intentionally stay rev-less (git builds the reverse index in memory cheaply for small packs; without a `.rev`, git rebuilds it per `pack-objects` — 2.85 s per fetch at 60 M objects). | none (write of `wal/<checksum>.rev` is Create-immutable; `annotate_pack` is manifest-only CAS) | `rev-index` |
| 6 | **Fsck audit** | `maintenance.fsck_interval` > 0 AND due: never audited ∥ `repaired_seq != 0` since last audit ∥ `now − report.at ≥ fsck_interval`. Runs over a **complete local copy only** (§7). | none (report is Overwrite; audit is read-only) | `fsck` |
| 7 | Idle | — none of the above — | — | — |

Tie-breaking inside unit 5 (multiple packs qualify): oldest `seq` first. Inside unit 3 (multiple strategies missing slots): strategies in config order = priority; within a strategy the oldest missing slot first.

### 4.1 Wrong-host planning

Before units 3–6 can run for a repo, compute **fits = Σ live pack bytes ≤ capacity**, where capacity = `maintenance.max_pack_bytes` if > 0, else (budget mode: `cache.max_bytes`; disk mode: unlimited/0). If `!fits`, units 3, 4, 5, 6 are **`wrong-host`**: never attempted here, reported as outcome `wrong-host`, and the repo is skipped for those units this pass. Checkpoint (unit 1) and repair (unit 2) always run — they must not require the object bytes locally (checkpoint is refs-level by design; repair fetches from upstream into a scratch repo, not the serving copy).

`maintenance.disk = "tmpfs"` (default): the host never rebuilds bases — the bundles unit's BaseRebuild arm requires `disk == "ssd"`. On a tmpfs host a due base slot is planned but reported `wrong-host` (a capacity-fit ssd peer's heartbeat shows the repo assigned; the slot stays `missing` until someone eligible takes it — this is the intended pressure mechanism).

### 4.2 Heartbeats

Object: bucket-root `maintain/<host>.pb`, protobuf `MaintainerHeartbeat` (§5.2; Overwrite put, never WAL state):

```protobuf
message MaintainerHeartbeat {
  string host = 1;
  repeated string repos = 2;     // assigned globs resolved to repo ids
  repeated string exclude = 3;   // maintain_exclude globs
  uint64 max_pack_bytes = 4;
  string disk = 5;               // "tmpfs" | "ssd"
  google.protobuf.Timestamp started_at = 6;
  google.protobuf.Timestamp last_pass_at = 7;
  string last_unit = 8;          // "<repo> <kind> <detail>"
  uint64 passes = 9;
}
```

Cadence (Go):

- written **before** each pass (fields updated: `last_pass_at = now`, `passes++`, `last_unit` cleared) and **after** each pass (`last_unit = "<repo> <kind> <detail>"` of the last executed unit).
- a **mid-pass ticker** `time.NewTicker(120 * time.Second)` fires while any unit is running; each tick rewrites the object (same content, fresh put — Overwrite semantics, no CAS) so a pass lasting hours still looks alive. The ticker is created when the first unit of a pass starts and `Stop()`ed when the last one ends (§7).
- **Purge**: heartbeats older than 24 h are deleted — by a `maintain`-role host only, as a cheap list at the start of each pass (prefix `maintain/`), delete keys whose `last_pass_at` (or mtime fallback) is older than 24 h.
- **Alive** = `last_pass_at` within 600 s. The settings/overview surface (and any balancer) uses this; a host whose loop goroutine panicked stops being "alive" 10 minutes later without any explicit tombstone.

Metrics: `walgit_maintainer_heartbeat_timestamp{host}` (gauge set on each write), pass/unit metrics from §3.2.

## 5. Unit 3 — Bundles (planning only here; building lives in 04/07 docs)

The unit calls into `internal/bundle`'s planner (same code the `bundle plan` CLI uses, gauges `walgit_bundle_plan_slots{repo,strategy,state}`). The maintainer's responsibilities are the ordering and the BaseRebuild fork:

1. Retention pass (prune entries past `keep`, delete objects, respect the list generation).
2. Settle closed slots (slots whose fire time has passed and have no bundle): compute the plan.
3. Choose the oldest `missing` slot, strategies in config order.
4. If the chosen strategy is Full (kind `full`, no chain), the host is ssd, and the base-rebuild trigger (§6.1) holds → run **BaseRebuild** (unit 3b) instead of a direct `bundle create`; the slot composes from the new base afterwards (`compose_full_from_base`, zero bytes through the host).
5. Else run `bundle strategy=<s> slot=<n>` (incremental or full as planned).

One re-plan exception after a build-nothing outcome (§3.2 step 5) keeps compaction/base triggers visible when bundles did only bookkeeping.

## 6. Unit 4 — Compaction (geometric fold) and unit 3b — Base rebuild

### 6.1 Compaction unit

Under `leases/compact.pb` (TTL `compaction.lease_ttl`, 10 m):

```bash
git -C <repo.git> repack -d --geometric=<compaction.factor> --write-midx \
    --keep-pack=pack-<base-checksum>.pack \
    --keep-pack=pack-<history-checksum>.pack
```

- `--keep-pack` for the base (tier 2) and history packs: a fold NEVER touches them (the invariant that keeps serving cheap: everything newer than the base lives in small packs).
- Diff the pack set before/after → new packs + removed checksums.
- Publish as a tier-1 `ENTRY_KIND_COMPACT` entry (`pack` = new PackRef, `supersedes` = folded checksums) via `publish_compact` (§6.3). On commit, superseded checksums join `pending_pack_removals`; the publisher's own pack sync removes them locally like everyone else's.
- **Retention/GC:** superseded packs are kept `compaction.retention_superseded` (7 d — the provenance window for `walgit wal materialize --at-seq`); the GC sweep (part of this unit's post-work or the next pass) deletes `wal/<checksum>.{pack,idx,…}` older than that AND no longer in any manifest. `walgit_repo_pack_bytes`-style gauges may be added; the REQUIRED series here is `walgit_maintain_units_total{host,kind="compact",outcome}`.

#### Concurrency

Hazard: two hosts fold simultaneously, or a fold races a push's pack upload.
Avoidance: (a) the `leases/compact.pb` lease is the cross-instance mutex — one fold at a time, CAS ladder, steal only after `expires_at + 2s`; (b) locally, the fold runs under the repo handle's `pack_mutex` taken with **try-lock semantics** (`sync.Mutex` wrapped in a `TryLock` check — Go 1.18+): if the publish path holds it, report `held` and defer to the next pass rather than queueing (readers must never queue behind maintenance); (c) pack uploads are Create-immutable so a concurrent push's pack can never collide; the fold's `supersedes` list is built from the manifest snapshot taken under `sync_mutex` before `repack` starts — packs added after that snapshot are simply not in `supersedes` (they stay live; the next trigger folds them). See 13_concurrency.md for the canonical playbook.

### 6.2 Base rebuild unit (resumable phase machine)

Trigger predicates (any):

- no base exists (no tier-2 pack) but packs do; OR
- more than one tier-2 pack exists; OR
- the base lacks a bitmap (`has_bitmap == false`); OR
- **the base predates the window**: window start = the previous weekly slot's fire time, or the repo's `first_state_at` (checkpoint provenance) when the repo is younger than one slot. Bar = WAL `head_seq` at that instant. **Rebuild iff `base_seq ≤ max(bar, 1)`** — `max(bar,1)` makes an empty-at-slot-time repo (bar 0) still rebuildable at seq 1. Pushes landing DURING a rebuild do not re-trigger it (their seqs are above `bar`); next week's slot does.

Pre-flight: **disk free** under `cache.dir` must exceed the live pack set (`statfs` on `cache.dir`; free < Σ pack bytes → report `blocked`, try next pass). The serving copy keeps answering fetches throughout: all heavy work happens in a scratch copy.

Algorithm (also driven by `walgit compact --base`):

1. **Scratch copy** `<cache.dir>/_rebuild/<owner>/<repo>.git` — reflink when the FS supports it (XFS/btrfs `FICLONE` ioctl), else a plain copy. Seconds on XFS; the serving pack set is never mutated.
2. **Phase marker** `<cache.dir>/_rebuild/<owner>/<repo>.json`:

   ```json
   {
     "started_head_seq": 1042,
     "phase": "repacked",
     "new_packs": ["3f9a…"],
     "history":  "7c21…",
     "commit_graph": "e0d2…"
   }
   ```

   Fields: `started_head_seq` (manifest head at copy time), `phase` ∈ `copied → repacked → history_pack → commit_graph`, `new_packs[]` (checksums produced by the repack), optional `history` (history pack checksum) and `commit_graph` (trailing chain checksum). Written after EACH phase completes (atomic `write-temp` + `rename`); the pack files themselves are the durable evidence.

3. **Phases:**
   - `copied`: scratch exists with an objects dir.
   - `repacked`: in the scratch: delete stray `*.keep` markers, then `git repack -a -d --threads=0 --write-bitmap-index --write-midx [--keep-pack …]`. Record new pack checksums (diff the scratch pack dir before/after).
   - `history_pack`: when `git.history_pack` (default true): `git pack-objects --filter=blob:none --revs --delta-base-offset --stdout -q` over all ref oids piped into `git index-pack --stdin` inside the scratch → `pack-<sha>.pack/.idx/.rev` + a `.history` marker naming the base. Record `history`.
   - `commit_graph`: `git commit-graph write --reachable --split=replace [--changed-paths]`; the trailing chain layer is copied out as `wal/<checksum>.commit-graph`. Record `commit_graph`.
4. **Resume rule:** on unit start, read the marker. Continue from the recorded phase **IFF** `manifest.head_seq == started_head_seq` **AND** the scratch has an objects dir. Otherwise (a push landed — the repacked pack would be missing the new objects) **delete the scratch and the marker, start over** from phase 1. This is why the rule is head-seq equality, not "marker exists": the scratch is a snapshot of one manifest generation.
5. **Publish (idempotent):** upload `pack+idx+rev+bitmap+commit-graph` **create-if-absent** (duplicate creates are success; a pack already live under the same checksum is skipped), then `publish_compact` superseding the pack set as it existed at rebuild start (superseding already-superseded packs is harmless — the manifest CAS removes what it finds). Only at publish are the new files linked into the serving copy (`link`/`copy` into `objects/pack` + `annotate` flags). A crash mid-publish is safe: create-if-absent + CAS = retriable exactly.
6. **Kill safety:** draining (SIGTERM → context cancel) kills the running git subprocess; the marker's phase is whatever completed. Git writes packs via temp+rename, so a half-written pack never looks complete. A kill between ANY two phases resumes with exactly one `git repack` across all attempts (this is the simulation-proven invariant; see 13_concurrency.md §kill-resume).

#### Concurrency

Hazards: (a) rebuild vs push → scratch goes stale; (b) rebuild vs GC deleting `wal/` objects the scratch references via alternates (not used here — scratch is a full copy, but the publish step reads scratch packs while another host could theoretically prune); (c) two rebuilds.
Avoidance: (a) the `started_head_seq` resume rule converts staleness into a clean restart, checked at every phase entry; (b) publish is create-if-absent and the manifest CAS decides liveness — a GC only deletes objects no manifest references, and our new pack IS referenced by the COMPACT entry before its own GC window opens (`retention_superseded`); (c) `leases/compact.pb` guards base rebuild and geometric fold alike — one writer per repo. The scratch directory is per-repo and the unit is single-flight per repo (§7), so no scratch contention.

## 7. Single-flight, drain, and the heartbeat ticker lifecycle

- **One unit at a time per repo** (single-flight): the task registry keys running tasks by `(repo, kind)`. Because the pass executes units sequentially per repo, the registry's join semantics are a backstop for cross-loop entry (the follow loop, manual API ops, CLI `--once`): a second `Begin(repo, kind)` JOINS the running task and awaits it (bounded wait, then reuse its outcome). Implement the registry as `map[key]*runningTask` under a `sync.Mutex` with a per-task `done` channel; joiners receive on it. Cross-INSTANCE exclusivity is the lease, not the registry.
- **Drain interrupts:** the maintainer's context is cancelled on SIGTERM phase 1. Every unit's git subprocesses run under `exec.CommandContext(unitCtx)` so cancellation kills them; the unit returns an interrupted error; the task records failure with the standard "interrupted: instance shut down; will be retried by the next pass" semantics (§6.8). No unit ever outlives the context.
- **The 120 s heartbeat ticker lifecycle:** owned by the pass goroutine. Created lazily when the first unit of a pass starts, `Stop()`ed (and drained via its `C` channel being left unread after stop) when the pass's last unit ends; the ticker goroutine is the pass goroutine itself (`select` on `ticker.C` alongside unit completion), NOT a separate goroutine — so there is exactly one writer to the heartbeat object at any moment (the mid-pass tick and the pre/post-pass writes all happen on the pass goroutine). Ownership: pass goroutine creates, stops, and writes; the follow loop NEVER writes the maintainer heartbeat (it records its outcome only in `upstream.last_round` state, §8).
- **Why the follow loop must never be behind a base rebuild:** ingress (a `git fetch` from upstream that developers' pushes depend on) must not wait behind a 30-minute repack. Hence: separate goroutine, separate cadence, no shared lock with any unit. The follow loop takes no lease and no `(repo, kind)` task slot that a unit could hold (its task kind `follow` is distinct); it only touches the serving copy through the ordinary publish path (single-flight publisher, §6.3), which is push-shaped work, not maintenance-shaped work. If a base rebuild is mid-phase, follow still runs: its scratch has alternates into the serving objects and its publish is an ordinary PUSH that lands ahead of/behind the rebuild's COMPACT entry in the log without ordering hazards (both are serialized by the manifest CAS; the rebuild's resume rule re-checks `head_seq` so a follow-induced advance restarts the scratch, which is correct and rare).

## 8. Upstream follow loop (own goroutine — NOT a unit)

`[upstream] follow = ["refs/heads/main"]` with `upstream.git` + `upstream.token_env`. Runs on its own goroutine, every `maintenance.follow_interval` (30 s; 0 = off), independent of the unit pass:

1. **Pack-set fit requirement:** negotiation needs the object base. Compute the same `fits` as §4.1; if the pack set does not fit this host, skip with a warning (outcome `failed`, detail `pack set does not fit`) — do NOT half-negotiate against a partial copy.
2. **Fetch into the persistent scratch** `<cache.dir>/follow/<owner>/<name>.git` (bare; created once with `objects/info/alternates` → the serving objects dir so the delta fetch reuses local objects):
   ```bash
   git -C <follow-scratch> \
     -c fetch.unpackLimit=1 -c transfer.unpackLimit=1 \
     -c fetch.writeCommitGraph=false -c gc.auto=0 -c protocol.version=2 \
     fetch --no-tags <upstream-url> '+refs/heads/main:refs/follow/refs/heads/main'
   ```
   Before the fetch, stage WAL ref values into the scratch: `git update-ref --stdin` setting `refs/follow/<ref>` to the WAL's current oids (this is what makes the fetch a delta request rather than a blind one). Token: config-pair credential helper reading `$<upstream.token_env>` (default `WALGIT_UPSTREAM_TOKEN`); always `GIT_TERMINAL_PROMPT=0`.
3. **Compare:** read tips back via `git for-each-ref refs/follow/`. No ref moved → in-sync, discard the fetched pack, record `in-sync`. Moved → run the **follow op** as an ordinary publish:
   - ingest the fetched pack (fsck per `wal.fsck_objects`, `thin=false`), connectivity check per config,
   - **fast-forward only**: each ref update must satisfy `old_oid` is an ancestor of `new_oid` (standard ff rule); **a rewound upstream is refused** and logged EVERY round until a human decides (the refusal is not sticky-silenced — the operator must see it every 30 s until the config changes),
   - refs deleted upstream are left alone (deletion is a human's call),
   - publish as a PUSH entry with `meta: principal="upstream", upstream=<url>, agent="walgit follow"`.
4. **Policy is NOT evaluated** for follow publishes (follow is configuration, not a principal). The only way to stop following a ref is to remove it from `upstream.follow`.
5. **Status per instance** (in-memory, guarded by a mutex; NOT the WAL): `upstream.last_round = {at, outcome: in-sync|published|refused|failed, detail, upstream: {ref→oid}, ours: {ref→oid}}`, surfaced by `GET …/settings/describe`. Metrics: `walgit_follow_rounds_total{repo,outcome}`, `walgit_follow_refs_total{repo}`.

Example operator flow (mirror a GitHub repo into walhub):

```toml
[upstream]
git       = "https://github.com/acme/widget.git"
token_env = "WALGIT_UPSTREAM_TOKEN"        # export WALGIT_UPSTREAM_TOKEN=ghp_…
follow    = ["refs/heads/main"]
```

`curl -s https://walhub.example.com/api/repos/acme/widget/settings/describe | jq .upstream.last_round`
→ `{"at":"2026-09-01T10:04:30Z","outcome":"published","detail":"refs/heads/main 8c1f→9d2a", …}`.

#### Concurrency

Hazard: follow publish racing a base rebuild's publish, or two hosts following the same repo.
Avoidance: follow publish goes through the same single-flight publisher as pushes (manifest CAS serializes everything; no extra lock). Two hosts following: allowed by design (idempotent ff-only publishes — the loser's publish becomes a no-op ref-verify conflict recorded as `in-sync` on the next round), but the placement convention is one maintaining host per repo; the fit-check in step 1 keeps oversized repos on their rightful host. The follow goroutine NEVER takes `leases/compact.pb` and NEVER runs while holding a unit task slot (distinct kind `follow`), so it cannot deadlock against the pass. See 13_concurrency.md.

## 9. Unit 6 — Fsck audit, and unit 2 — Repair

### 9.1 Fsck audit

- Runs **only on a host whose local copy holds the whole pack set** (never over a linked/remote base — the audit's value is proving the live set, not a partial view). Predicate beyond the interval: the serving copy is at `Full` sync (all manifest packs linked locally).
- Command: `git fsck --connectivity-only --no-dangling` over the serving copy (`GIT_DIR=<cache.dir>/<owner>/<repo>.git`).
- Report: Overwrite put of `fsck.pb` (repo-relative; protobuf `FsckReport` §5.2): `{seq, at, host, missing[≤100 000 oids], missing_total, problems, elapsed_secs, repaired_seq}`. `repaired_seq` is 0 unless repair ran. Gauge `walgit_repo_missing_objects{repo}` set from `missing_total`.
- Manual trigger: `POST …/api/ops/fsck?connectivity=1` (same code path, same task kind).

### 9.2 Repair unit

Due right after checkpoint (priority 2) when `fsck.pb` lists missing oids, `repaired_seq == 0`, and `upstream.git` is configured. Uses the §7.9 repair helper (`fetch_objects_as_pack`):

1. Scratch bare repo `<cache.dir>/_repair/<owner>/<repo>.git` (per pass; delete after).
2. Optional inline credential helper: `-c credential.helper= -c credential.helper='!f() { echo username=x-access-token; echo password='$TOKEN'; }; f'` style config-pair (token from `upstream.token_env`).
3. Fetch the missing oids in **500-oid batches**:
   ```bash
   git -C <repair-scratch> \
     -c fetch.negotiationAlgorithm=noop -c protocol.version=2 \
     fetch --no-tags --no-write-fetch-head --quiet --depth=1 \
     <upstream> <oid1> <oid2> … (≤ 500)
   ```
4. `git pack-objects --no-reuse-delta --compression=6` over exactly the requested oids (no closure) → `pack-<sha>.pack/.idx`.
5. **Verify EVERY requested oid is in the resulting idx** (read the idx — see doc 03/04 for the index format; a linear scan of fanout tables suffices). A refused want (upstream lacks it, shallow filter excluded it, etc.) is an ERROR — never a silent hole; the batch fails, the unit reports `error`, and the report's `repaired_seq` stays 0 so the next pass retries.
6. Publish via `add-pack` as a **tier-0 COMPACT entry superseding nothing**; set `repaired_seq` in a fresh `fsck.pb` Overwrite write (so the next pass re-audits and the repair does not re-fire). Counter `walgit_repair_objects_total{repo}` incremented per verified oid.

#### Concurrency

Hazard: a repair publish racing a push that references the same (now-restored) objects; two hosts repairing simultaneously.
Avoidance: publish is the ordinary CAS ladder (duplicate pack create = success; COMPACT entry is serialized by the manifest CAS). Two hosts: the lease is omitted for repair by design (it is cheap and idempotent), but the `repaired_seq == 0` predicate self-disarms: the first host to write the new `fsck.pb` wins, the other's publish is a harmless duplicate (create-if-absent). If the implementer prefers belt-and-braces, a `leases/repair.pb` MAY be added — deviation noted in Decisions. The repair scratch is per-repo and single-flight via the task registry.

## 10. Unit 5 — Rev-index

- Trigger: a live pack with `!has_rev` and `object_count ≥ 250 000`, oldest first. One pack per unit run.
- Build **file-locally**: `git rev-index` equivalent — write the `.rev` from the `.idx` alone (the CLI `walgit wal rev-index <IDX> [--out P]` is byte-identical to git's output; the unit calls the same internal function from `internal/git`). No `git` subprocess needed for the EWAH encode (implement the format writer in `internal/git`; see doc 04 for the `.rev` layout).
- Install: write to a temp file in `objects/pack`, rename to `pack-<checksum>.rev`, then `annotate_pack` (manifest-only CAS — `has_rev = true`, no log entry, `head_seq` unchanged). If the pack vanished meanwhile (superseded by a fold), abandon silently — outcome `ok` with a notice.
- `--keep`? No: rev-index never touches pack bytes.

## 11. Unit 1 — Checkpoint

Thin wrapper: evaluate the §6.5 triggers; when fired, call `internal/wal`'s checkpoint writer (refs-level, idempotent: returns the existing checkpoint if `cp.seq == head_seq`). Provenance chain (`created_at`/`first_state_at`/`as_of`) is the WAL engine's job — the unit only decides WHEN. Metrics: `walgit_checkpoints_total{outcome}`, `walgit_checkpoint_seconds`, lag gauges `walgit_checkpoint_lag_entries`, `walgit_checkpoint_age_seconds`.

## 12. Metrics summary (maintain-owned series)

| Series | Type | Meaning |
|---|---|---|
| `walgit_maintain_pass_seconds{host}` | histogram | pass duration |
| `walgit_maintain_units_total{host,kind,outcome}` | counter | outcomes: `ok\|error\|timeout\|wrong-host\|held\|idle` |
| `walgit_maintain_unit_seconds{kind}` | histogram | unit duration |
| `walgit_maintainer_heartbeat_timestamp{host}` | gauge | last heartbeat write |
| `walgit_repo_missing_objects{repo}` | gauge | from last `fsck.pb` |
| `walgit_repair_objects_total{repo}` | counter | verified oids restored |
| `walgit_follow_rounds_total{repo,outcome}` / `walgit_follow_refs_total{repo}` | counter | follow loop |
| `walgit_bundle_plan_slots{repo,strategy,state}` | gauge | bundle planning states |
| `walgit_checkpoint_lag_entries` / `walgit_checkpoint_age_seconds` | gauge | checkpoint lag |

## 13. End-to-end example

Operator view of one pass on an ssd host (`walhub serve --config walgit.toml`):

```
12:00:00 pass start host=ip-10-0-4-9 assigned=142 repos
12:00:00 acme/widget      unit=checkpoint  outcome=ok        (tail 9.2MiB > 8MiB)
12:00:01 acme/legacy      unit=fsck        outcome=ok        (7d due; 0 missing)
12:00:09 acme/monorepo    unit=compact     outcome=ok        (18 tier-0 packs folded → 3)
12:03:11 acme/monorepo    unit=bundle      outcome=ok        (weekly missing → BaseRebuild phase=commit_graph)
12:26:40 acme/huge        unit=rev-index   outcome=timeout   (still running; discoverable via /api/tasks)
12:26:40 acme/imported    unit=repair      outcome=held      (compact lease held by ip-10-0-4-10)
12:26:41 … remaining repos idle; pass 41.2s; heartbeat rewritten
```

## 14. Testing bar (what "correct" means for the implementer)

- Unit selection: a table-driven test over snapshots asserting the EXACT priority order, the wrong-host skips, the `disk=tmpfs` base-rebuild refusal, and the ≥2-fresh-packs compaction guard.
- Resume: kill the base rebuild between every pair of phases (inject cancellation after each phase write); assert the resumed run executes exactly one full `git repack` and publishes the same checksums as an uninterrupted run (marker + head_seq equality).
- Repair: a repo with `fsck.pb.missing = [oid]` and a configured upstream heals; a want the upstream refuses fails the batch and leaves `repaired_seq = 0`.
- Follow: rewound upstream → `refused` every round; ff move → published with `principal="upstream"`.
- Heartbeat: the 24 h purge removes a stale host's object; `alive` is false 601 s after the last write.

## Decisions & deviations from the Rust design

- **`context.Context` is the only shutdown/timeout mechanism** (vs tokio's `select!`/abort): drain cancellation, the 1 h unit wait (`context.WithTimeout`), and git subprocess kills (`exec.CommandContext`) all hang off one tree; the 1 h expiry releases only the pass's interest — the task goroutine survives and stays discoverable in the task table, matching the Rust "still running → move on" contract without a second cancellation path.
- **Rev-index is built in-process from the `.idx`** (byte-identical writer in `internal/git`) instead of shelling to git: fewer subprocesses, and the CLI `wal rev-index` shares the code; git's own `--rev-index` path is not invoked.
- **Repair remains lease-less** (Rust code has no repair lease either); an OPTIONAL `leases/repair.pb` is noted but not required — the `repaired_seq == 0` predicate plus create-if-absent publishes make concurrent repairs idempotent.
- **Bundle leases keep their historical quirks** (no 2 s skew tolerance, heartbeat epoch = 1) for bucket-format compatibility with the Rust implementation; a written amendment is required to "fix" them.
- **Heartbeat writes happen on the pass goroutine only** (ticker handled in the same goroutine's `select`): Go makes a second goroutine tempting, but a single writer eliminates read-modify-write races on the heartbeat object without a lease.
- **Wrong-host planning uses `statfs`-based free-space checks for the rebuild pre-flight** where Rust likely uses the store's cache accounting; `statfs` on `cache.dir` is the honest measure of "can the scratch copy land here".
- **The 48-skip stale-slot cap is enforced per repo per pass** as a plain counter (the Rust spec states the cap without an owner; the pass goroutine owns it).
- **Follow-loop status (`upstream.last_round`) is instance-memory only**, matching §13.4's "status kept per instance"; a host restart clears it, and `settings/describe` simply omits it until the next round completes.
- **Purge of stale heartbeats is a prefix list at pass start** rather than a dedicated timer goroutine: one fewer goroutine, and a 60 s cadence is plenty for a 24 h horizon.
