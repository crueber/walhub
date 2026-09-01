# 05 — WAL engine (internal/wal)

> Source: MASTER_RUST_SPEC.md §6 (6.1–6.9), §3.2 (consistency model) · Status: normative for the walhub Go implementation.

This is the heart of the system. Everything here lives in `internal/wal` (module `git.packden.us/crueber/walhub`), uses only stdlib plus the two allowed third-party modules (`golang.org/x/net`, `github.com/BurntSushi/toml`), and talks to the object store through the `internal/store` interface (see 03_store_backends.md). Git operations are always the `git` binary with exact argv (see 04_git.md). Wire formats — the protobuf messages, the key layout, the framed log segments — are byte-compatible with the Rust implementation; see 02 for the hand-rolled protobuf codec. The concurrency playbook (single-flight, try-lock patterns, bounded channels, shutdown paths) is 13_concurrency.md; this doc references it instead of re-deriving it.

## 5.0 The consistency model (from §3.2, restated as Go rules)

The manifest object `repos/<owner>/<repo>/manifest.pb` is the linearization point. It carries a `revision` field, a monotonic counter of successful writes. All rules:

1. **CAS or die.** A manifest write is always `store.Put(ctx, key, body, store.PutUpdate(version))` or `store.PutCreate`. A 412 (`store.ErrPreconditionFailed`) is the *normal* contention signal: re-sync, re-verify, retry. It is never an error outcome in metrics.
2. **Ambiguous errors.** A non-412 error during the manifest CAS may mean "the write landed and the response was lost". The writer MUST re-read the manifest fresh (the `casLanded` re-read); if its log segment is listed, the commit is committed. See §5.3 step 7.
3. **Orphans are harmless.** A written-but-uncommitted log segment (a writer crashed between the log PUT and the CAS) is not damage: later writers burn past its seq (§5.4), and it is swept or absorbed after a later commit.
4. **Monotonic revision guard.** A reader/publisher that holds a manifest in memory MUST ignore any freshly fetched manifest with `revision <= held.revision` (it can only be a stale cached read from after the local publish); update only the freshness timestamp. Keep an in-memory guard copy in the handle for this.
5. **Local-after-CAS, refs-first.** Applying a committed ref transaction to the local bare repo happens AFTER the CAS succeeds, under the sync mutex; the new manifest version is advertised only after local refs are written. If local application fails, withdraw the advertised version (reset `state.ManifestVersion` to 0 and bump nothing) so the next sync replays — but still answer the push `ok`, because the bucket CAS is the truth.

## 5.1 Registry, handles, and lock discipline (§6.1)

### 5.1.1 Registry

```go
type Registry struct {
    st        store.Store              // object store (03)
    cfg       *config.Config
    cacheRoot string                   // cache.dir; repo dir = <root>/<owner>/<name>.git

    mu    sync.Mutex                   // guards the maps below only
    repos map[string]*RepoHandle       // key "<owner>/<name>"
    opens singleflight.Group          // per-repo open single-flight (13)
    tasks *TaskTable                   // §5.8
    blocks *BlockCache                 // process-wide remote-reader block cache (§5.7)
    listing *listingCache              // 30s-cached repo list (§5.1.5)
    wg    sync.WaitGroup               // registry-owned background goroutines
    ctx   context.Context              // registry lifetime; cancel = shutdown
}
```

Constructor: `NewRegistry(ctx, st, cfg)`; shutdown: `Close()` cancels `ctx` and waits on `wg`.

#### Concurrency

- `mu` guards map bookkeeping only and is never held across I/O. Hazard: holding `mu` while opening a repo deadlocks all repos behind one slow GET. Avoidance: the open itself runs under `opens` (single-flight, 13_concurrency.md), then locks `mu` briefly to publish the handle.
- Every goroutine the registry spawns (evictor, listing refresher, task janitor) takes `ctx` and exits on `ctx.Done()`; `Close()` joins them via `wg`. Who owns the channels: the registry owns and closes the shutdown only.

### 5.1.2 RepoHandle

```go
type RepoHandle struct {
    ID string // "owner/name"

    // Manifest state. Guarded by syncMu (all writers) + read under RLock.
    manifest   *pb.Manifest
    version    string    // store version tag for the next CAS
    freshAt    time.Time // last freshness check
    heldRev    uint64    // monotonic revision guard (rule 5.0.4)

    syncMu trySync.Mutex // serializes the refs phase (§5.2); try-lock-first
    packMu trySync.Mutex // serializes pack reconciliation; NEVER held with syncMu
    rw     rw.TryRWMutex // pack removals vs readers — try-write-only (§5.1.3)

    state     *PersistedState   // walgit-state.json, see below
    stateMu   sync.Mutex        // guards state file read-modify-write

    progress  *Broadcast[Packet] // per-repo progress broadcast, cap 1024 (§5.8)
    pub       *Publisher          // §5.3, spawned lazily
    remoteIdx *RemotePacks        // §5.7, per manifest revision
    checkpoints map[uint64]time.Time // seq -> created_at cache
    effCfg    effectiveConfigCache   // per settings revision
}
```

`open(id)` fast path: `mu.Lock`, map hit, return. Slow path (single-flight `opens.Do(id, ...)`):

1. GET `manifest.pb`. Absent → `ErrNotFound`.
2. Open-or-init the local dir `<cache.dir>/<owner>/<name>.git`: if missing, `git init --bare` plus `git config` for the manifest's `object_format` (`sha1` → default, `sha256` → `extensions.objectformat=sha256`). Exact argv per 04_git.md.
3. Load persisted state (`walgit-state.json`); corrupt/missing → zero-value defaults. Never fail the open on state corruption — the bucket is the truth and the state file is only an accelerator.
4. If `state.AppliedSeq < manifest.HeadSeq`, run `applyDelta` immediately using the manifest already in hand (no second GET).

`create(id, objectFormat)`: `PutCreate` of `Manifest{FormatVersion:1, Repo:id, ObjectFormat, HeadSeq:0, MinSeq:0, Packs:nil, LogSegments:nil, Revision:1, Writer:instanceID}`. 412 → `ErrAlreadyExists`. Then init the local repo and write initial state.

`delete(id)`: drop the handle from the map (new opens fail immediately), then **delete the manifest first** — that is the linearization point — then page through all keys under `repos/<owner>/<repo>/` deleting (LIST is slow and paged; delete pages sequentially, it is not a hot path), then `os.RemoveAll` the local dir.

### 5.1.3 The rw lock and the TRY-WRITE-ONLY rule

The Rust spec's `rw` lock protects the on-disk pack set against readers: readers take a read guard for the whole duration of object access (a clone's guard can live for a whole stream), and pack removal takes a write guard. The Rust spec has a hard invariant: **never take this lock as a blocking write — only try-write.** Reason (from the spec, keep the war story): a queued writer blocks ALL new readers; one 24-minute clone once starved every `info/refs` for minutes.

#### The Go hazard

Go's `sync.RWMutex` is writer-preferring *only for waiters that have already called `Lock()`*: once a writer is queued, every subsequent `RLock` blocks. A single long-held read guard (a clone streaming for minutes) plus one queued `Lock()` therefore blocks every new `RLock` — precisely the starvation the invariant forbids. Worse, the documented deadlock pattern (recursive RLock while a writer waits) is reachable here, because object readers can re-enter lookup while holding a guard.

#### The Go pattern (normative)

Do NOT use `sync.RWMutex` for this. Use a **try-lock-only write** built from a `sync.Mutex` plus an atomic, with `runtime_procPin`-free plain atomics:

```go
// package rw: a reader-friendly RWMutex whose writers NEVER queue.
// Writers: TryWriteLock only. Readers: RLock may block (it never starves,
// because no writer is ever queued).
type TryRWMutex struct {
    mu       sync.Mutex // protects the counters below; held briefly
    readers  atomic.Int64
    writer   atomic.Bool
}

func (l *TryRWMutex) RLock() {
    for {
        l.mu.Lock()
        if !l.writer.Load() {
            l.readers.Add(1)
            l.mu.Unlock()
            return
        }
        l.mu.Unlock()
        // A try-writer is mid-flight; yield briefly. This cannot starve:
        // writers are rare and transient.
        time.Sleep(50 * time.Microsecond)
    }
}
func (l *TryRWMutex) RUnlock() { l.readers.Add(-1) }

func (l *TryRWMutex) TryWriteLock() bool {
    l.mu.Lock()
    if l.writer.CompareAndSwap(false, true) && l.readers.Load() == 0 {
        l.mu.Unlock()
        return true // caller owns the write lock; call WriteUnlock
    }
    l.mu.Unlock()
    return false // readers active — leave checksums pending, retry next sync
}
func (l *TryRWMutex) WriteUnlock() { l.writer.Store(false) }
```

Semantics preserved: while any read guard is alive no pack is removed; a removal that loses the race leaves `pending_pack_removals` intact for the next sync (the persisted state carries the pending list, so removal survives restarts). Never call a blocking write on this type; if someone needs to *wait* for quiescence (only maintenance repair may), do it as a loop with `TryWriteLock` + `time.Sleep` + context check, which keeps new readers flowing the whole time.

Alternative (equally acceptable, simpler): a semaphore channel of capacity 1 — readers `select`-receive from a broadcast-free counter channel — but the mutex+atomic form above matches the Rust semantics 1:1 and is the reference. A channel-based tryLock (`select { case ch <- token: ... default: }`) is also fine; whichever you use, it lives in one small package with tests, and 13_concurrency.md catalogs it.

#### Concurrency

- Hazard: writer starvation of readers (clone blocks info/refs). Avoidance: try-write-only, as above; removals are also bounded to one `packMu` holder at a time so two removers cannot livelock the try-lock.
- Hazard: reader re-entry deadlock (lookup → decode → lookup). Avoidance: the `TryRWMutex` above has no queued writer, so re-entrant `RLock` merely increments the count; document that nested `RLock` is allowed but must be balanced. (A Go `sync.RWMutex` would deadlock here.)

### 5.1.4 Persisted state file

Path: `<repo dir>/walgit-state.json`, plain `encoding/json`:

```go
type PersistedState struct {
    ManifestVersion     string   `json:"manifest_version"`      // store version tag for CAS
    AppliedSeq          uint64   `json:"applied_seq"`
    Revision            uint64   `json:"revision"`              // monotonic revision guard
    PacksRevision       uint64   `json:"packs_revision"`
    PendingPackRemovals []string `json:"pending_pack_removals"` // checksums awaiting try-write removal
    RemoteServed        []string `json:"remote_served"`         // checksums served remotely (no local .pack)
}
```

`packsReady() = state.PacksRevision == state.Revision && len(state.PendingPackRemovals) == 0`. Corrupt/missing file → defaults (`AppliedSeq 0` etc.) — the repo re-applies the delta from the manifest, which is always correct. Writes are atomic: write `walgit-state.json.tmp` then `os.Rename` (same directory, same filesystem). The `stateMu` serializes read-modify-write; the file is written under it, and writes must not be attempted while holding `syncMu` alone (take `stateMu` inside `syncMu` — the only lock-order rule in the handle: `syncMu → packMu → rw.TryWrite → stateMu` is the allowed nesting order; never acquire in reverse).

### 5.1.5 Listing cache

`list()` returns `[]string` of repo IDs. Cached for 30 s (`listingCache{at time.Time; repos []string; mu sync.Mutex}`). Refresh: `listPrefixes("repos/")` → per owner, `listPrefixes("repos/<owner>/")` → for each candidate repo, HEAD `manifest.pb` in parallel (bounded semaphore, 8) — never a full object walk; absent manifest = deleted repo, skip. `list()` never errors on partial refresh failures; it serves the last good snapshot and logs.

### 5.1.6 Eviction (`evictIdle`, periodic)

Two modes:

- **Budget mode** (`cache.mode = "budget"`): evict least-recently-used repos idle longer than `cache.evict_idle_after` while the sum of repo-dir sizes exceeds `cache.max_bytes` (default 20 GiB).
- **Disk mode** (`cache.mode = "disk"`): `syscall.Statfs` on `cache.dir`; evict only when `used/total > cache.disk_high_watermark`; target = `used − (used − (watermark−0.10))·total`. Keep evicting (LRU) until below the target.

Eviction of one repo MUST take **both** the repo's `syncMu` **try-lock** and the `rw.TryWrite`; **if either fails, skip that repo this pass** (never evict under readers, never block the evictor). Order: `syncMu` first, then `rw.TryWrite` — consistent with the global nesting order. After both are held: close/remove the handle from the map (subsequent opens recreate), then `os.RemoveAll` the dir, then release. Directory size accounting (needed by budget mode): `filepath.WalkDir`; symlinks counted by link size (`lstat`); hard links counted once per `(dev, ino)` pair — keep a `map[uint64]struct{}` keyed by `dev<<32|ino` per walk.

#### Concurrency

- Hazard: evictor deadlocks on a busy repo or starves readers. Avoidance: dual try-lock, skip on failure, one pass per interval, and the evictor never takes `packMu` (materialization is protected by the sync lock already held). The evictor holds locks only across `os.RemoveAll` — that is disk I/O, not network, and it is try-lock-guarded.

## 5.2 Sync levels (the heart of the read path)

Every request calls a sync first. Levels, unchanged from the spec:

| Level | Brings | Used by |
|---|---|---|
| **Refs** | checkpoint `RefSnapshot` + every log entry's ref txn → local `packed-refs`. No packs. | info/refs, ls-refs, bundle lists, web refs/resolve/overview, log reading |
| **Serve** (`Sync`) | Refs + the pack set as this instance can hold it: tiers < 2 and HISTORY packs local; a tier-2 base as side-files (idx/rev/bitmap/commit-graph) + the `.pack` linked from a store mount (`cache.store_mount`) or remote-served (no local copy); midx over history + base | upload-pack, receive-pack, bundle builds, prewarm |
| **Full** (`SyncFull`) | Refs + every live pack local (striped downloads); refused with `ErrTooLarge` when the set exceeds the cache budget | base rebuilds, full repacks |
| **Objects** (`SyncObjects`) | Serve when the pack set fits the budget; else Refs + the remote reader → `ObjectAccess{Local,Remote}` | web API object endpoints |

```go
type SyncLevel int
const (
    SyncRefs SyncLevel = iota
    SyncServe
    SyncFull
    SyncObjects
)
// Returns a read guard; packs cannot be removed while held.
func (h *RepoHandle) Sync(ctx context.Context, lvl SyncLevel) (*ReadGuard, error)
```

Algorithm (per sync):

**1. Freshness check.** Unless within `wal.freshness_ttl` (default 0 = always check) of `freshAt`: known version → conditional GET of `manifest.pb` (`304 Unchanged` → done); unknown version → unconditional GET (absent → `ErrNotFound`). Under `syncMu`, **try-lock first**; on failure measure queue wait and block — and if the wait exceeds `telemetry.lock_wait_warn`, WARN with lock name, repo, and wait ms (§5.9). After a successful fetch apply the monotonic revision guard: `manifest.Revision <= heldRev` → discard body, update `freshAt` only.

**2. applyDelta.** If `manifest.Checkpoint != nil && manifest.Checkpoint.Seq > state.AppliedSeq` → GET `refs.pb` at the checkpoint key, write it directly into `packed-refs` (sorted, `head_target` applied to HEAD; `peeled` entries written as `<oid> peeled` lines per 04_git.md), set `state.AppliedSeq = checkpoint.Seq`, and bump the checkpoint-time cache. Then replay the log tail: fetch ALL segment objects overlapping `(applied_seq, head_seq]` in parallel (chunks of 16 goroutines, each segment GET a goroutine), decode each with partial-tail tolerance (§5.3 framing: stop at the first incomplete trailing frame, not an error), merge all entries, sort by `seq`, and apply in order:

- `PUSH` / `REF_UPDATE`: collect all ref txns and write them **once** via the offline ref apply — a full `packed-refs` rewrite (no `git` process needed; works before packs exist). Atomicity: build the new refs map in memory from the current `packed-refs` parse, apply every txn (symbolic → record `head_target`; zero `new_oid` → remove; else set, honoring recorded `new_peeled`), then write `packed-refs.tmp` + rename.
- `COMPACT`: append `entry.Supersedes` checksums to `state.PendingPackRemovals`.
- `CHECKPOINT` / `SETTINGS`: no ref effect (settings ride the manifest).
- Record time bookkeeping: `first_entry_time` / `last_entry_time` from entry `created_at` across the replayed range — this feeds checkpoint provenance and bundle as-of cuts (see 11). Persist state (`applied_seq = head_seq`, `revision = manifest.Revision`) after refs are renamed.

**3. Packs** (only `SyncServe` and above). Under `packMu` (never held together with `syncMu` — refs requests must never queue behind a multi-GB materialization): `checkFits` (sum of plan sizes vs budget; over → `ErrTooLarge`, surfaced as HTTP 503 with the bundle-uri fix text per 07), then run a `materialize` **task** (§5.8) that downloads and reconciles, then refresh the local repo (`git` per 04). The whole phase is one task; concurrent callers join the running task (single-flight on `(repo, "materialize")`, 13_concurrency.md) rather than stacking.

**4. ReadGuard.** `rw.RLock()`; return the guard to the caller. Dropping it is the caller's job (`defer g.Release()`).

#### Concurrency

- Hazards: (a) refs syncs serializing behind pack materialization — avoidance: `packMu` is never held while `syncMu` is held, and materialization runs as a joinable task, off request goroutines when the caller passes a bounded worker pool (13); (b) two syncs racing on `packed-refs` — avoidance: the entire refs phase is inside `syncMu`, try-lock-first so queue time is observable; (c) delta fetch stalling on one slow segment — avoidance: all overlapping segments fetched in parallel with a 16-chunk semaphore and a per-request context; (d) partial-frame decode races — avoidance: segments are immutable objects; decode is pure; no lock is held during decode.
- The 16-goroutine fetch uses `errgroup`-style semantics hand-rolled (13: bounded goroutine group with first-error propagation); each goroutine owns its GET's context and there is nothing to close.

## 5.3 Publish path (§6.3)

One **single-flight publisher goroutine per repo**, owned by the handle, spawned on first publish and **respawned if it dies** (any panic/error exit triggers a respawning wrapper). Callers enqueue `PublishRequest{Pack *PreparedPack; Txn *pb.RefTransaction; Meta map[string]string; Synced bool; CreatedAt *time.Time}` and await a one-shot reply channel.

### 5.3.1 Group commit (batch window)

```go
type Publisher struct {
    ch chan *publishJob          // cap 64 == wal.max_batch; who closes: handle on teardown, publisher never
    out chan publishResult       // per-job reply, created by caller, closed by publisher after send
}
```

Loop (single goroutine, `select`):

```go
for {
    job := <-ch                       // recv first request (blocks; ctx-aware via a done channel)
    batch := []*publishJob{job}
    timer := time.NewTimer(wal.batchWindow) // 5ms
collect:
    for len(batch) < wal.maxBatch {
        select {
        case j := <-ch:               // another waiter exists or arrives instantly
            batch = append(batch, j)
        case <-timer.C:
            break collect             // window elapsed
        }
    }
    if !timer.Stop() { <-timer.C }    // drain; safe because we only break on timer.C or fall through
    runBatch(ctx, batch)
}
```

A lone push does NOT wait: the timer runs concurrently with the collect loop, and if no second job arrives the timer fires at 5 ms — but crucially, the *first* job's enqueue → recv is immediate, and `runBatch` starts after the window only when more work is pending. For a lone push on an idle repo the sequence is recv → timer fires (5 ms) → run. That 5 ms is the spec's design (batch window), not a sleep; do NOT "optimize" it into a synchronous path — receive-pack already amortizes it. If telemetry shows lone-push latency matters, the acceptable variant is: after the first recv, do a non-blocking drain of ready jobs only (`default:` branch), and start the timer only if one arrived — that matches "another waiter exists (or one is immediately receivable)". Implement THAT variant: recv → try-drain; if batch empty after drain and `len(ch) > 0` more may come, wait on timer; else commit immediately.

### 5.3.2 Per-attempt ladder (cap `wal.cas_max_retries` = 16)

For the merged batch (each job keeps its own `Txn` and reply):

1. **Sync** (refs+serve) unless every job has `Synced` (receive-pack reuses its own freshness check).
2. **Snapshot** manifest + version; build the ref view as an O(log n) overlay over `packed-refs` (a sorted index + delta map), never an O(refs) scan per push.
3. **Verify each txn** against the current refs. Per update: `new_symbolic_target` set → always ok; `old_oid` all-zero → ref must not exist; else `old_oid` must equal current value → else `Conflict{Name, Expected, Actual}`. Explicit `CreatedAt` must be ≥ the WAL head's last entry time (monotonic); within the batch, each explicit time must be ≥ the previous job's. Valid txns are applied to the working view so later jobs in the batch see earlier writes. If NO job is valid: answer all with per-ref errors and `Seq: 0` (rejections are transport-successes).
4. **Build entries**: `seq = firstSeq + position` where `firstSeq = manifest.HeadSeq + 1`; PUSH entries carry `PackRef{Checksum, PackSize, IdxSize, ObjectCount, Tier:0, Kind:OBJECTS}`; `created_at` = explicit or now; `meta` = caller metadata (principal, request_id, agent, push-options…).
5. **Concurrently** (two goroutine groups, joined before step 6):
   - (a) upload each pack: PUT `wal/<checksum>.pack` and `.idx` **create-if-absent** (`PutCreate`; duplicate creates are success). ≥ 256 MiB and native compose available → striped upload (`put_file_parallel`, 03 §4.7).
   - (b) **claim the log slot** at `head_seq+1`: `PutCreate` of `log/<seq:016x>.pb` containing the framed entries. On 412: someone wrote that slot. Read the manifest fresh — if `head_seq ≥ seq`, the commit landed → re-sync and restart the ladder (count one attempt). Else HEAD the segment: absent → retry the Create; present after **3 probes × 100 ms** → treat as an **orphan** (a crashed writer): **burn** the seq — record it, `seq+1`, fresh segment, retry (cap **8 consecutive burns**, beyond → `ErrCorrupt`). Upload failure fails the whole batch (all jobs get the error; packs may have landed as orphans, harmless).
6. **Build the manifest update**: `head_seq = last_seq`; extend `packs` with new PackRefs (sorted by `seq`); push `LogSegmentRef{key, first_seq, last_seq, size, sealed:true}` (sorted by `first_seq`); set `updated_at`, `writer`, `revision = old+1`.
7. **CAS the manifest** (`PutUpdate(version)`, or `PutCreate` when the repo was empty — `head_seq==0 && len(log_segments)==0`):
   - Ok → committed.
   - 412 → **delete our own log segment** (CAS delete of exactly our version — the store delete must carry the version so we never delete a segment a racing writer is reading), then retry from step 1.
   - Other error → **ambiguous**: fresh manifest re-read (`casLanded`); if it lists our segment → committed (recover the version via HEAD); else NOT committed — **leave the segment in place** (do not delete: a lost-response commit the re-read failed to observe must not be destroyed; a later writer burns past it).
8. **Committed.** Resolve the final version (CAS metadata, else HEAD). Then, **local commit under `syncMu`, refs FIRST, then advertise**: apply each txn (`applyRefTxn`, no old-check) to `packed-refs`; on failure WARN (`walgit_publish_local_apply_failed_total`) and **withdraw** (set `state.ManifestVersion = ""` so the next sync replays; the version is not advertised). Then update state (`manifest_version`, `applied_seq`, `revision`; keep `packs_ready` if it was already true), sweep burned orphans (CAS-delete the burned segments we recorded), note entry times, spawn commit-graph folding for pushed packs **off the critical path** (13), and check the checkpoint trigger (§5.5 → opportunistic background checkpoint). Answer every waiter: valid → `PublishResult{Seq, PerRef: ok}`; invalid → per-ref errors.

**Ordering rule (normative, from §3.2):** local refs are written BEFORE the new manifest version is advertised (held in `state.ManifestVersion`), because advertising first would let a reader cache old refs under the new version. **Withdraw-on-failure rule:** if local application fails, the version is withdrawn so the next sync replays; the push is still answered `ok`.

#### Concurrency

- Hazard: publisher goroutine leak or panic wedging all pushes. Avoidance: one goroutine per repo, `respawn` wrapper (`recover` → log → recreate channel state — the channel is NOT closed on respawn; jobs enqueued during the gap are retained because the channel is owned by the handle), and the loop exits only on handle teardown (`ctx.Done()`). The handle closes `ch` exactly once, at teardown, after the publisher has drained (`sync.WaitGroup` inside `Publisher.Close`).
- Hazard: batch jobs answered out of order / missed. Avoidance: each job carries its own reply channel; `runBatch` guarantees exactly one send per job; a `defer close(reply)` after send makes double-reply a test-detectable panic.
- Hazard: concurrent publishers CASing each other into livelock. Avoidance: there is exactly ONE publisher per repo (single-flight at handle level); the 412 retry ladder is bounded at 16 attempts.
- Hazard: slot-claim probing blocking other repos. Avoidance: probes are per-repo, inside the publisher goroutine; total added latency is bounded by 3×100 ms ×8 burns.
- No request goroutine ever performs a store PUT on the publish path; all bucket I/O is inside the publisher goroutine (the only per-batch parallelism is the two bounded groups in step 5).

### 5.3.3 publishCompact, publishSettings, peeled

- **`publish_compact`** (repacks, base rebuilds, `add-pack`): upload pack+idx (+ side-files) create-if-absent → the same slot-claim/CAS ladder with a `COMPACT` entry (`pack`, `supersedes[]`) → manifest packs: remove superseded checksums, add the new one; on commit, append the superseded checksums to `state.PendingPackRemovals` (their removal goes through the same try-write removal as everyone else's). **`add_pack`** installs the file into the local copy first (filename must be `pack-<checksum>.pack`), then publishes it as a `COMPACT` entry superseding nothing (tier from arg).
- **`annotate_pack`**: retrofits `.rev`/`.bitmap`/`.commit-graph` flags onto a live PackRef — manifest-only CAS, **no log entry**, `head_seq` unchanged.
- **`publish_settings`**: validate first — parse the new `settings.toml` over the host config with `github.com/BurntSushi/toml`; invalid → error, nothing published. Then log slot (`SETTINGS` entry with `settings` + meta author/message) + manifest CAS (`manifest.settings` = the new document, revision = previous+1). Happy path is 2 rounds: log PUT → CAS. Invalidates the effective-config cache.
- **Peeling (`new_peeled`)**: the *writer* fills `new_peeled` for `refs/tags/*` updates pointing at annotated tags (follow Tag objects, max 16 hops) BEFORE publish; replicas then advertise `^{}` from the WAL alone. Replay prefers a recorded `new_peeled` and only peels locally when the object happens to be present.

## 5.4 Orphan/burn protocol (§6.4, normative recap)

Log seqs are NOT dense. Full sequence on slot-claim 412:

1. Fresh manifest read: `head_seq ≥ our seq` → the commit landed → re-sync, restart the ladder.
2. Else HEAD the slot: absent → retry the Create.
3. Present → sleep 100 ms, probe again, ×3 total → **burn**: record the seq in the batch's burned list, `seq+1`, start a new segment, retry the claim. Cap 8 consecutive burns → `ErrCorrupt` (operator alarm: something is deeply wrong, likely a stuck writer or clock skew).
4. After OUR commit → CAS-delete the burned segments we recorded ("sweep").
5. After OUR CAS-412 → delete exactly our segment.
6. After an ambiguous CAS error → delete nothing.

The burned list lives in the batch context only; persistence comes from the manifest itself (burned segments are unlisted until swept, and step 4's CAS delete is their only removal).

## 5.5 Checkpoints (§6.5)

**Triggers** (any; 0 disables): entries since last checkpoint ≥ `wal.snapshot_every_entries` (256); tail bytes (Σ `size` of segments with `last_seq > cp.seq`) > `wal.checkpoint_tail_bytes` (8 MiB); age since `checkpoint.created_at` (or `manifest.updated_at` for a checkpoint-less repo) ≥ `wal.checkpoint_interval` (1 h). Evaluated after publishes (opportunistic, in the publisher goroutine but as a background task off the reply path) and by the maintainer's checkpoint unit (10_maintenance.md).

**Write** (refs-level only — works on an instance that could never hold the packs). Idempotent: if `cp.seq == head_seq`, return the existing checkpoint. `seq = head_seq`; snapshot = local refs (sorted, peeled); provenance: `created_at = now`; `first_state_at = previous.first_state_at → first_entry_time → first_seq_published_at → previous.created_at`; `as_of = last_entry_time → previous.as_of → created_at`.

- Round 1: PUT `checkpoints/<seq:016x>/checkpoint.pb` (packs = full live pack set with side-file flags, `refs_key`, `ref_count`, writer) ∥ PUT `checkpoints/<seq:016x>/refs.pb` — both `PutCreate` + immutable, keyed by seq (deterministic; a crash leaves garbage, never a hazard).
- Round 2: CAS manifest (`checkpoint`, `min_seq = seq+1`, trim folded segments — those entirely below `min_seq` — `revision+1`, updated writer/timestamps).

**Cold start fold:** `applyDelta` loads `refs.pb` at the checkpoint and replays only the tail — a fresh instance never replays the whole log.

#### Concurrency

- Round 1's two PUTs run in two goroutines joined by the writer; both are create-only so no coordination is needed. Hazard: two instances checkpointing the same seq simultaneously — benign (same content, create-if-absent idempotent); the manifest CAS in round 2 picks one winner and the loser's CAS-412 restarts the ladder. No locks beyond the publisher's `syncMu` are taken; the checkpoint never blocks a push reply.

## 5.6 Point-in-time replay (§6.6)

- `readLog(from, to?)`: manifest via freshness check; overlapping segments fetched **sequentially** (read-only path, not latency-critical), frames filtered to `[from, to]` inclusive, sorted by `seq`. Never mutates anything.
- `refsAtSeq(seq)` / `refsAsOf(t)`: pure in-memory fold — start from the newest checkpoint usable at the cut (seq-cut: `cp.seq ≤ seq`; time-cut: `checkpoint.as_of ≤ t`), GET its snapshot, then apply entries in order until the cut (per entry: symbolic → `head_target`; zero `new_oid` → remove; else set with `new_peeled`). Time-cut reads to head and breaks by `created_at` (an entry with missing `created_at` never breaks the walk). Error `refs at seq N are not replayable` when the cut predates `min_seq` with no usable checkpoint — the weekly compose needs refs at the base pack's seq, which is why `first_state_at`/`as_of` exist.
- `walhub wal materialize --at-seq N` builds a standalone repo directory from checkpoint + replayed txns + the pack set at that seq (fetched from the store or copied from the local copy), refs applied LAST.

## 5.7 Remote reader (§6.7)

For repos too large to materialize. Used by the web API (`ObjectAccess::Remote`) and by the fetch-path faulter. NOT used by stock `git` upload-pack (clones go through bundle-uri; fetch remainders come from local small packs).

**Block cache** (process-wide, all repos, owned by the Registry):

```go
type BlockCache struct {
    mu      sync.Mutex                  // guards the map
    blocks  map[blockKey]*blockEntry    // key = {globalCacheKey string, blockNo uint64}, 1 MiB blocks
    lru     list.List                   // container/list LRU of blockKey
    inflight singleflight.Group         // per-block single-flight (13)
    bytes   atomic.Int64                // current size; hit/miss counters
    cap     int64                       // cache.remote_block_bytes, default 1 GiB
}
type blockEntry struct{ data []byte; elem *list.Element }
```

`Get(key)` — fast path under `mu` (map hit → move to LRU front); miss → `inflight.Do(key, fetch)` so concurrent misses share one fetch; on insert evict LRU tail until under cap. Hit/miss + bytes counters (metrics per 08).

**RemotePacks** (per manifest revision; rebuilt when the revision changes): for every non-history pack, an in-memory index file — downloaded into `<repo>/remote-idx/<checksum>.idx` (or hard-linked from the local pack dir when Serve installed it), 4 concurrent downloads, then opened (the index parser is 04's). `objects/pack` stays untouched. Registered as a task (`remote-index`); concurrent openers join it.

**Lookup:** `locate(oid)` → first index hit → `(index, offset)`; `lookupPrefix(prefix)` → unique across packs, else `ErrAmbiguous`.

**Read paths:**

- `header(oid)`: kind + inflated size without materializing — walk the delta chain (≤ 256) inflating only delta headers (result size from the first varints).
- `decode(oid)`: iterative resolution with an LRU at every hop: walk OfsDelta (`offset − distance`) / RefDelta (`locate` base id) collecting the chain (**≤ 4096 deep**); base object read via the block cache + incremental `compress/zlib` inflate (the 64-byte entry header is prefetched with the first block read; inflation prefetches `min(size, 64 MiB)` of blocks up front); then fold the chain in reverse with the git delta format (below), caching every intermediate. Decoded-object LRU: `cache.remote_object_bytes` (256 MiB), keyed `(pack index, offset)`.
- **Git delta format** (normative, copy): varint `base_size` (must match the base object's size), varint `result_size`, then commands until the buffer ends: `cmd & 0x80` = copy — offset bits `0x01|0x02|0x04|0x08` (LSB→byte 0..3 of a little-endian offset; omitted bits are 0), size bits `0x10|0x20|0x40`, size `0` → `0x10000`, copy is bounds-checked against the base; `cmd != 0 && cmd & 0x80 == 0` = insert the next `cmd` literal bytes; `cmd == 0` = reserved/error. Total produced bytes must equal `result_size`. Verify with git's own behavior on real packs (15_testing.md).

**Faulter (fetch path — REQUIRED in v1 Go):** given missing oids, decode + write them into the local loose store in parallel batches of 32, in rounds (max 64), so subsequent `git` commands run unchanged. This is what lets the Go upload-pack/fetch path survive without the full native engine.

**Decision — no full gix-equivalent engine in v1.** The Rust implementation embeds a native pack reader/upload-pack engine (gix-class) so Serve-tier repos can be served without shelling out. The Go implementation RECOMMENDS deferring that engine: v1 serves upload-pack via the stock `git` binary over the materialized local copy (Serve sync) and uses the remote reader + faulter to fill gaps for fetches on large repos, per 04_git.md. The remote reader itself (block cache, index files, decode, faulter) IS required in v1 — the web API depends on it. Consequence, stated plainly: a Serve-tier repo whose pack is remote-served still needs `git` to run against something; the faulter + local small packs + bundle-uri clones cover this. Revisit when the Go native pack engine (doc 02's protobuf and 04's index code) is reused for serving.

#### Concurrency

- Hazards: (a) thundering herd on one hot block — avoidance: per-block single-flight before fetch, so N waiters share one GET; (b) unbounded memory — avoidance: both LRUs are capacity-bounded (`cache.remote_block_bytes`, `cache.remote_object_bytes`) and eviction happens under `mu` while holding no other lock; (c) index download stampede at revision change — avoidance: `RemotePacks` is per-revision, built by a `remote-index` task that joiners await; the old revision's object stays alive until the swap (build-then-swap pointer under a mutex, no lock held during downloads); (d) faulter parallelism — bounded batches of 32, max 64 rounds, all under the request's context; (e) delta chain depth — hard cap 4096 (256 for headers) prevents stack growth; the fold is iterative, never recursive.

## 5.8 Tasks (§6.8) — every long thing is narrated

```go
type TaskRecord struct {
    ID        string        // uuid (crypto/rand hex)
    Kind      string        // materialize | remote-index | history-pack | compact | bundle |
                            // checkpoint | fsck | repair | follow | rev-index | sync | rematerialize | prewarm
    Repo      string
    Hostname  string
    Started   time.Time
    Finished  *time.Time
    ElapsedMS int64
    OK        *bool         // nil = running
    Summary   string
    Progress  *Progress     // {Label, Done, Total *uint64, Unit, Percent *float64}
    LogTail   []string      // last 60 notices
    Params    map[string]any
}
```

Task table per instance (owned by the Registry): `running` keyed by `(repo, kind)` — a second start of the same `(repo, kind)` **joins** the running one (`ErrAlreadyRunning` → await its completion up to a bounded wait, then reuse its outcome). Implement `(repo,kind)` single-flight as a keyed mutex map (`sync.Map` of `*keyedLock`) or a `singleflight.Group` keyed `"repo/kind"`; either way, joiners get a channel closed on completion and the recorded result. `recent` (30 per repo, ring buffer); `by_id` (map with janitor eviction of finished records after 1 h).

Packets: `notice {text}`, `progress {label, done, total?, unit, percent?}` (latest bar per label wins), `task {TaskRecord}`; terminal `result {task, value}` | `error {status, message}`. Per-repo broadcast + per-task replay buffer (200 packets, bars deduped by label) so late SSE attachers get history. Keepalive comments every 10 s (SSE encoding per 09).

**Broadcast primitive** (lag-tolerant subscribers):

```go
type Broadcast[T any] struct {
    mu   sync.Mutex
    subs map[uint64]chan T   // per-subscriber bounded chan (cap 16)
    next uint64
    buf  []T                 // replay ring (200)
}
func (b *Broadcast[T]) Subscribe() (id uint64, ch <-chan T, replay []T)
func (b *Broadcast[T]) Unsubscribe(id uint64)
func (b *Broadcast[T]) Send(v T)        // non-blocking send to every sub; full channel = drop (lag-tolerant)
```

`Send` NEVER blocks: `select { case ch <- v: default: }` — a slow SSE consumer drops packets rather than stalling the task (progress bars are lossy by design; the replay buffer covers reconnects). A subscriber channel is owned and closed by `Unsubscribe` only.

Kinds and drain hooks: phase 1 of drain interrupts running ops (cancel their contexts) — an interrupted task records failure `503 "interrupted: instance shut down; will be retried by the next pass"` when draining. Records are instance-memory only (cross-instance exclusivity is the lease, 03 §4.9). `hostname` tells where a task ran; a task vanishing from `running` counts as finished only when the same instance answers (or `recent` shows it with a result).

#### Concurrency

- Hazards: (a) SSE fan-out blocking task goroutines — avoidance: non-blocking `Send` + bounded subscriber channels + replay ring; (b) task table races — avoidance: one `sync.Mutex` per table region (running/recent/by_id) or a single table mutex; operations are O(1) map ops, never held across I/O; (c) joiner leaks — avoidance: joiner channels are closed exactly once by the task's `defer`, and the bounded wait has a timeout fallback to `recent` lookup; (d) drain races — the drain hook holds the table mutex only to flip a `draining` flag and cancel stored contexts; cancellation is the shutdown path for every task goroutine (13).

## 5.9 Lock instrumentation (§6.9)

Every lock acquisition on a request path: **try first; on queueing measure the wait**. Pattern:

```go
func withLockTiming(name string, repo string, try func() bool, lock func(), unlock func()) {
    start := time.Now()
    if try() { recordFast(name); unlock(); return }
    locked := make(chan struct{})
    go func() { lock(); close(locked) }()
    // ... measured wait; on acquire record histogram + WARN if wait > telemetry.lock_wait_warn
}
```

Histogram `walgit_lock_wait_seconds{lock}` (Prometheus text exposition, hand-rolled per 08; locks: `sync_mutex`, `rw`, `pack_mutex`, `gcs_bulk_permit`). WARN past `telemetry.lock_wait_warn` with (lock, repo, wait_ms). Snapshots feed the watchdog and the ops/overview pages (08, 10_maintenance.md). The try-lock-first requirement is why `syncMu` and the rw lock are custom types, not bare `sync.Mutex`/`RWMutex` — the instrumentation hook is part of their API (`TryLock` + `LockMeasured`).

#### Concurrency

- Hazard: the instrumentation goroutine leaking if the lock is never acquired. Avoidance: the waiter goroutine takes the lock exactly once and `close(locked)` in a `defer`; the caller selects on `locked` vs `ctx.Done()` and on abandonment lets the goroutine finish its acquisition before exiting (never abandon mid-lock without the unlock path — the canonical fix is: the goroutine itself unlocks after delivering, per 13_concurrency.md's "acquire-then-handoff" pattern).

## 5.10 Config reference for this doc (keys used above)

`wal.freshness_ttl` (0), `wal.max_batch` (64), `wal.batch_window` (5ms), `wal.cas_max_retries` (16), `wal.remote_objects`, `wal.prefetch_packs`, `wal.prefetch_max_bytes` (1 GiB), `wal.snapshot_every_entries` (256), `wal.checkpoint_tail_bytes` (8 MiB), `wal.checkpoint_interval` (1h), `cache.mode` (budget|disk), `cache.max_bytes` (20 GiB), `cache.evict_idle_after`, `cache.disk_high_watermark`, `cache.store_mount`, `cache.remote_block_bytes` (1 GiB), `cache.remote_object_bytes` (256 MiB), `telemetry.lock_wait_warn`. Example:

```toml
[wal]
freshness_ttl = 0
max_batch = 64
batch_window = "5ms"

[cache]
mode = "budget"
max_bytes = 21474836480
remote_block_bytes = 1073741824
```

Background prefetch (from §6.2): after a refs-only sync, if `wal.prefetch_packs` and this host serves the repo and the Serve plan would put ≤ `wal.prefetch_max_bytes` on disk → kick a background Serve sync (a `sync`-kind task; joinable; context from the registry, not the request).

## Decisions & deviations from the Rust design

- **`rw` lock is a custom `TryRWMutex` (mutex + atomics), not `sync.RWMutex`** — Go's writer-queueing RWMutex would block all new readers behind a queued writer (the exact starvation the spec's try-write-only invariant forbids); custom type keeps readers lock-free and writers try-only.
- **Remote reader block cache single-flight via a hand-rolled `singleflight.Group`** — matches the Rust per-block dedup with zero dependencies; 13_concurrency.md catalogs the pattern.
- **Publisher channel is owned by the handle and never closed on publisher respawn** — respawn replaces the loop, not the mailbox; jobs enqueued mid-respawn are retained, preserving the spec's "respawned if it dies" without losing requests.
- **Group-commit uses recv → try-drain → conditional 5 ms timer** — literal reading of the spec ("another waiter exists, or one is immediately receivable"); a lone push on an idle repo commits without waiting the window.
- **Faulter for the fetch path is required in v1; the full gix-equivalent serving engine is deferred** — v1 serves via stock `git` + materialized packs + bundle-uri; remote reader + faulter cover web API and fetch-gap cases (see 04_git.md for the serving split).
- **Eviction lock order fixed as `syncMu → rw.TryWrite → stateMu`** (global nesting order `syncMu → packMu → rw.TryWrite → stateMu`) — one stated order prevents the classic eviction/refresh deadlock; the spec's "both try-locks, skip on failure" is preserved.
- **`packed-refs` offline apply is done in-process (parse-map-rename), not via `git update-ref`** — the spec requires it to work before packs exist and atomically across many refs; a full-file rewrite with tmp+rename is the boring correct shape (04_git.md owns the format details).
- **Task broadcast drops packets for slow subscribers instead of blocking** — the spec's per-repo broadcast channel (capacity 1024) is mapped to bounded per-subscriber channels + a 200-packet replay ring; lossy progress bars are semantically fine, replay covers SSE reconnects.
