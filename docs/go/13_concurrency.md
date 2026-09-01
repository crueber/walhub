# 13 — Concurrency: the canonical Go playbook

> Source: MASTER_RUST_SPEC.md §6.1 (lock discipline), §6.8 (tasks), §3.4 (process inventory), §8.1 item 9 + §4.6 (transport separation), §19 (porting table — RWMutex hazard row) · Status: normative for the walhub Go implementation.

Every other doc defers its `### Concurrency` questions here. This file is the single authority on goroutine
inventory, lock ordering, single-flight, bounded parallelism, channel ownership, fan-out, drain, and the
concurrency test kit. Where a sibling doc says "see 13_concurrency.md §N", §N below is the exact pattern.

---

## 1. Goroutine inventory of a running instance

Rust/tokio has a runtime that multiplexes futures; Go has goroutines. §3.4's process inventory maps 1:1
onto long-lived goroutines plus per-request goroutines (Go's `net/http` spawns one per connection).
Every background goroutine: (a) is started once from `internal/server` startup in the §8.10 order,
(b) takes a `context.Context` it must honor, (c) is tracked in a `sync.WaitGroup` owned by the server,
(d) never blocks on bulk work. A goroutine with no exit path is a bug.

| # | Goroutine | Trigger | Exit condition | Must never block on |
|---|---|---|---|---|
| 1 | `http.Server.Serve` (+ one goroutine per connection) | startup, after spawn order below | `server.Shutdown(ctx)` in drain phase 2 | pack materialization; anything in the bulk pool |
| 2 | Bulk pool workers (default 4, `internal/store`) | startup; dedicated `chan bulkJob` | phase-1 cancel (finish current job) | request goroutine completion; holding repo locks while queued |
| 3 | Publisher (one per repo, `internal/wal`) | first enqueue; respawned if it dies | repo deleted, or phase-1 cancel after draining its batch | bulk uploads — hand them to pool #2 and await the reply |
| 4 | Prefetch (short-lived) | after a refs-only sync, when `wal.prefetch_packs` allows | Serve sync completes or context canceled | `syncMu` write lock (see §2.4) |
| 5 | Prewarm workers (`cache.prewarm_parallelism`, default 2) | startup | all prewarm repos done, or phase-1 cancel | `/readyz` (it gates readiness, never vice versa) |
| 6 | Maintainer loop | startup, if `maintain` role | phase-1 cancel between passes | a running unit — one bounded unit per pass (§13.1), never wait > 1 h inline |
| 7 | Upstream-follow loop | startup, if `follow` role, every `maintenance.follow_interval` (30 s) | phase-1 cancel | `packMu` while running git fetch — take the lease, then work |
| 8 | Events bridge | startup, if `events` role + sink | phase-1 cancel | webhook POSTs (10 s timeout, context-bounded) |
| 9 | Events catch-up worker | wake-up (notify POST) or sweep timer | catch_up returns | another repo's catch_up — serialized per repo, not globally (§3) |
| 10 | Sweepers: eviction (`evict_idle`), events sweep (`events.sweep_interval`), repo-listing refresh (30 s) | tickers | phase-1 cancel | blocking `rw.Lock()` — eviction is try-lock only (§2.2) |
| 11 | Heartbeats: maintainer `<host>.pb` (120 s mid-pass), lease heartbeats for long holders | start of pass / lease acquire | pass end / lease release or `LeaseLost` | the work the heartbeat describes |
| 12 | Watchdog (1 s tick) | startup | phase-2 | anything — it only reads gauges; late tick (> 2.5 s) ⇒ log "runtime stalled", do not retry |
| 13 | SSE keepalives (per stream, 10 s comments) | SSE attach | client disconnect or phase-1 cancel | the broadcast ring (always non-blocking, §6) |

Spawn order (§8.10): prewarm → events bridge → maintainer → follow loop → watchdog → bind listener.
The listener binds LAST so a slow prewarm never serves a 200 to a repo that is not warm.

```go
// internal/server/run.go — every background goroutine follows this shape
g, ctx := errgroup.WithContext(rootCtx)          // see §4 for errgroup without deps
for _, job := range []struct{ name string; fn func(context.Context) error }{
    {"prewarm", r.Prewarm}, {"events", bridge.Run}, {"maintain", maint.Run},
    {"follow", follow.Run}, {"watchdog", watchdog.Run},
} {
    job := job
    g.Go(func() error { return job.fn(ctx) })
}
g.Go(func() error { return httpSrv.Serve(ln) })  // bound LAST
return g.Wait()
```

### Concurrency
Hazard: a background loop blocking on a store call or repo lock stalls every doc's "one unit per pass"
cadence and can hold a lock at cancel time. Avoidance: every loop's body takes a fresh `ctx` derived from
the server context with its own `context.WithTimeout` (per-unit ≤ 1 h for maintainer units, 10 s for
webhook POSTs); loops check `ctx.Err()` between iterations. Channel ownership is §5; shutdown is §8.

## 2. THE LOCK RULES

Per-repo `RepoHandle` (§6.1) carries exactly three locks. Names are normative across all docs:
`syncMu` (sync.Mutex — refs phase), `packMu` (sync.Mutex — pack reconciliation), `rw`
(sync.RWMutex — pack presence vs readers). Plus process-level: registry map mutex, task-table mutex,
cache mutexes. That is the complete list; inventing a fourth repo lock requires an amendment to this file.

### 2.1 The `rw` try-write-only-for-removals rule (the §19 hazard row)

The invariant, verbatim from §6.1: `rw` is **never taken as a blocking write**. Readers (upload-pack
streaming one object graph) hold `rw.RLock()` for the duration of object access — potentially minutes for a
large clone. A queued writer blocks *all new readers*: one 24-minute clone once starved every `info/refs`
for minutes. Go's `sync.RWMutex` has exactly this hazard (a pending `Lock()` blocks subsequent `RLock()`).

**Rule: pack removal is the ONLY writer, and it must use `rw.TryLock()`.** On failure (any reader active)
the removal checksums stay in `pending_pack_removals` and the next sync retries — this is not an error
path, it is the designed defer. **Canonical primitive: `internal/wal/rw.TryRWMutex`** (mutex + atomics;
race-tested, 100% covered) — the shipped engine uses it because it keeps the try-or-defer removal
protocol on one auditable type; `sync.RWMutex.TryLock` (Go 1.18+) satisfies the same invariant and is
the fallback if `wal/rw` is ever refactored away. Do NOT use `TryRLock`-style polling elsewhere;
readers always block normally.

```go
// removeSuperseded: called under packMu, from reconcile_packs.
func (h *RepoHandle) removeSuperseded(dead []*PackRef) {
    if !h.rw.TryLock() {                       // readers active → defer, never wait
        return                                  // checksums stay pending; next sync retries
    }
    defer h.rw.Unlock()
    for _, p := range dead {
        os.Remove(filepath.Join(h.dir, "objects", "pack", p.FileName())) // temp-name renames elsewhere are atomic
    }
    h.state.Pending = removePending(h.state.Pending, dead)             // then persist walgit-state.json
}
```

Writer starvation of the remover is fine (removals are pure GC); reader starvation of new readers is the
incident. If a future writer-class operation ever appears, it goes through this same TryLock-or-defer
protocol or gets its own mutex — never a blocking `rw.Lock()`.

### 2.2 Lock ordering

Global order, every doc defers to it:

```
registry.mu  →  syncMu  →  packMu  →  rw  →  (store/network calls, never under any of these)
```

Precise rules, each with its incident:

1. **`syncMu` before `packMu` before `rw`.** A goroutine holding `packMu` must not acquire `syncMu`
   (that is the publisher-deadlock incident, §7.3). If you believe you need the reverse, redesign the
   operation, not the order.
2. **Never hold `rw.RLock()` across a call that can take `syncMu`.** A reader that spots a missing pack
   and calls `handle.Sync()` while holding `RLock` deadlocks against the remover sequence
   (`syncMu → packMu → rw.TryLock`) only if the remover already holds `syncMu`/`packMu` and a removal is
   pending — the reader blocks on `syncMu` forever while blocking the TryLock. **Spec: an `RLock` holder
   that needs a sync returns a "stale" error to its caller or defers the sync to after `RUnlock`.**
3. **Never hold `syncMu` across a store call that can take `packMu`** — this is why the spec splits them
   (§6.1: pack_mutex "held WITHOUT the sync_mutex so refs requests never queue behind multi-GB
   materializations"). `sync` holds `syncMu` only for the manifest freshness check + delta apply, then
   releases it before entering the pack phase.
4. **Never hold any repo lock across a store or network call, with ZERO exceptions.** The store layer
   (`internal/store`) can queue behind the global bulk semaphore (§4.6), the semaphore can queue behind a
   multi-GB striped upload, and a lock held across that queue starves every other request on the repo.
   The single permitted long-held token is the **lease** (§4.9), which is a store CAS object, not an
   in-process lock, and is heartbeated.
5. Lock acquisition on a request path is try-first, then measure: take the non-blocking attempt, and on
   failure measure queue time against `telemetry.lock_wait_warn` and record
   `walgit_lock_wait_seconds{lock="sync_mu|rw|pack_mu|gcs_bulk_permit"}` (§6.9). For `sync.Mutex` this
   means a `TryLock` probe before `Lock`. This is instrumentation, not a correctness mechanism — do not
   skip the blocking acquire afterward.

```go
// Canonical sync skeleton (internal/wal/sync.go) — note where each lock begins and ends.
func (h *RepoHandle) Sync(ctx context.Context, lvl Level) (*ReadGuard, error) {
    if !h.syncMu.TryLock() {                                  // rule 5: try-first + measure
        t0 := time.Now(); h.syncMu.Lock()
        lockWaitSeconds.WithLabelValues("sync_mu", h.id.String()).Observe(time.Since(t0).Seconds())
    } else { defer h.syncMu.Unlock() }
    if err := h.freshenManifest(ctx); err != nil { return nil, err }     // store call UNDER syncMu — rule 4?
    // ... no: freshenManifest is the one §6.1-sanctioned exception below. Everything else waits.
    h.syncMu.Unlock()                                          // rule 3: refs phase ends here
    if lvl >= Serve {
        h.packMu.Lock(); defer h.packMu.Unlock()               // pack phase: syncMu NOT held
        if err := h.reconcilePacks(ctx, lvl); err != nil { return nil, err }
    }
    h.rw.RLock()                                               // ReadGuard: caller holds across object access
    return &ReadGuard{h: h}, nil
}
func (g *ReadGuard) Release() { g.h.rw.RUnlock() }
```

**The one sanctioned store-call-under-lock:** `freshenManifest` (conditional GET of `manifest.pb`) runs
under `syncMu`. It is a control-plane GET on the control-plane transport (§4.6 — separate client, separate
permit), sub-second, and its whole purpose is to serialize the refs phase. It is listed here so no future
doc can cite rule 4 against it. Anything slower (striped downloads, pack PUTs, webhook POSTs, git
subprocesses) waits outside every lock.

### 2.3 Eviction takes both, try-only

`evict_idle` (§6.1) needs `syncMu` (to stop in-flight syncs racing the deletion) and `rw` (no readers on a
dying repo). Both are **try-only**: `syncMu.TryLock()` then `rw.TryLock()`; either failure → skip this
repo this round. Never `Lock()` inside eviction — eviction runs on sweeper #10 and blocking there stalls
the whole sweep loop.

### 2.4 Registry and task tables

`DashMap` (§19) → `sync.Map` for the registry's read-mostly open-handle map, sharded `sync.Mutex` +
plain map for the task table (`running` keyed `(repo, kind)` — §6.8) where writes are frequent. Locks on
these are held for map operations only; anything that can call `open()` (which does store I/O) does so
outside the map lock, coordinated by single-flight (§3.1) instead.

### Concurrency
Hazard: `sync.RWMutex` writer starvation blocking new readers (incident §7.1) and lock-order inversion
between `syncMu`/`packMu` (incident §7.3). Avoidance: writers on `rw` are `TryLock`-only; the global order
`syncMu → packMu → rw` is enforced by code review plus the §9 canary test; no lock is held across store
calls except the enumerated `freshenManifest` exception.

## 3. Single-flight patterns (per key)

Five known single-flights. One shared helper, hand-rolled (no `golang.org/x/sync` — the
bounded-parallelism primitives live in `internal/store/errgroup.go`; ruling C-1), used everywhere:

```go
// internal/singleflight/singleflight.go — canonical; every doc's "single-flight" means this.
type Group struct {
    mu sync.Mutex
    fl map[string]*call
}
type call struct { wg sync.WaitGroup; val any; err error }
func (g *Group) Do(key string, fn func() (any, error)) (any, error) {
    g.mu.Lock()
    if g.fl == nil { g.fl = map[string]*call{} }
    if c, ok := g.fl[key]; ok { g.mu.Unlock(); c.wg.Wait(); return c.val, c.err } // joiners get the SAME result
    c := &call{}; c.wg.Add(1); g.fl[key] = c; g.mu.Unlock()
    c.val, c.err = fn()
    c.wg.Done()
    g.mu.Lock(); delete(g.fl, key); g.mu.Unlock()
    return c.val, c.err
}
```

| Key | Protects | §6.1 / §6.8 anchor |
|---|---|---|
| `"open:" + owner + "/" + name` | registry `open(id)`: concurrent first opens of one repo must produce one `git init --bare` + one state load | §6.1 `open` fast path / single-flight |
| `"create:" + id` | registry `create`: duplicate creates collapse; the 412-on-CAS path still decides ownership | §6.1 `create` |
| `"task:" + repo + "," + kind` | task table `Begin`: a second start of `(repo, kind)` JOINS the running task and awaits its bounded completion (`Begin::AlreadyRunning`), then reuses its outcome | §6.8 running map |
| `"block:" + cacheKey + "," + blockNo` | remote-reader block cache: one block download per key; joiners share bytes and hit/miss counters | §6.7 block cache |
| `"render:" + manifestRev + "," + template` | web render cache fill (per-manifest-revision caches) | §6.1 caches |
| `"publish:" + repo` | the publisher task itself: enqueue-and-join rather than a second publisher | §6.3 |

Joiner semantics from §6.8 are normative: a joined task's completion value is reused, and the join is
**bounded** — wrap `c.wg.Wait()` in a channel-select with `ctx.Done()` so a stuck leader cannot pin an
unbounded number of request goroutines:

```go
done := make(chan struct{})
go func() { c.wg.Wait(); close(done) }()
select {
case <-done:  return c.val, c.err
case <-ctx.Done(): return nil, ctx.Err()
}
```

### Concurrency
Hazard: thundering herd (N requests ⇒ N `git init` / N block GETs) and unbounded joiner piles on a hung
leader. Avoidance: one `Group` per process per concern; joiners select on `ctx.Done()`; the publisher
single-flight respawns a dead leader instead of letting joiners wait forever.

## 4. Bounded parallelism everywhere

**No unbounded goroutine spawn, ever.** Every fan-out is an errgroup-with-limit or a weighted semaphore.
**Ruling C-1: no `golang.org/x/sync`** — the canonical primitives are the hand-rolled
`internal/store/errgroup.go` (`Group.SetLimit` semantics) and weighted semaphore, shared by every
package. Where a leaf package needs neither in full, the equivalent is 6 lines:

```go
// internal/store/limits.go — bulk permits (§4.6): control plane and bulk NEVER share a permit.
var bulkPermits = semaphore.NewWeighted(cfg.Store.BulkConcurrency) // queue metrics wrap Acquire

func AcquireBulk(ctx context.Context) (release func(), err error) {
    t0 := time.Now()
    if err := bulkPermits.Acquire(ctx, 1); err != nil { return nil, err }
    bulkQueueSeconds.Observe(time.Since(t0).Seconds())            // walgit_store_bulk_queue_seconds
    bulkInflight.Inc()
    return func() { bulkInflight.Dec(); bulkPermits.Release(1) }, nil
}
```

Fixed inventory of limits (normative; other docs cite these numbers by name):

| Site | Limit | Source |
|---|---|---|
| Bulk pool workers | 4 (dedicated, never request goroutines) | §3.4 bulk runtime |
| Striped download stripes per object | 16 concurrent 32 MiB chunks | §4.7 |
| Concurrent pack downloads (one sync) | 8 pack tasks, each pack's files in parallel | §6.2 reconcile |
| Log-segment fetch chunks | 16 parallel | §6.2 apply_delta |
| Prewarm parallelism | `cache.prewarm_parallelism` (2) | §3.4 |
| Per-repo git concurrency | `server.max_concurrent_per_repo` semaphore **inside the handler** (§8.1 note: no global timeout layer; the semaphore is the guard) | §8.1 |
| Remote-reader idx downloads | 4 concurrent | §6.7 |
| Remote-reader fault batches | 32 objects/round, ≤ 64 rounds | §6.7 |
| Global in-flight requests | `server.max_concurrent_requests` — advisory counter, reject or shed at the edge | §8.1 |
| Publisher batch | `wal.max_batch` (64) within `wal.batch_window` (5 ms) | §6.3 |
| Checkpoint / segment PUT pairing | 2 (`checkpoint.pb ∥ refs.pb`) | §6.5 |

```go
// internal/wal/reconcile.go — 8 concurrent pack downloads, errgroup form.
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(8)
for _, need := range missing {
    need := need
    g.Go(func() error { return downloadPack(ctx, need) })  // inside: striped 16×32 MiB (§4.7)
}
if err := g.Wait(); err != nil { return err }
```

### Concurrency
Hazard: unbounded fan-out (a 100-pack sync spawning 300 goroutines) saturating the store and starving the
control-plane transport. Avoidance: the only two semaphore flavors allowed are `errgroup.SetLimit` and
`semaphore.Weighted`; a raw `go func()` inside a loop over store-I/O is a review-blocking defect. The bulk
permit (`AcquireBulk`) is taken by the *bulk pool worker*, never by the request goroutine that submitted
the job — request goroutines block on the bounded job channel, which is itself the queue metric.

## 5. Channel ownership rules

1. **The sender owns and closes; the receiver never closes.** Closing from the receiver races the sender's
   send (panic on send-to-closed). Range-receive loops end via close, so every close has exactly one owner.
2. **`context.Context` is the universal shutdown.** A goroutine that would otherwise need a close signal
   selects on `ctx.Done()`. Channels that carry results are closed by their single producer after its
   final send or when its context is canceled — never left dangling.
3. **Bounded buffers only.** `make(chan T, n)` with a named, commented `n`; `n = 0` (unbuffered) preferred
   for rendezvous; a buffer exists only to decouple a measured producer/consumer rate (progress ring §6:
   capacity 1024 per §6.1; task replay buffer 200 packets per §6.8; bulk job channel = pool size × 2).
   No `chan` with capacity derived from untrusted input.
4. **The bulk job channel:** owned by `internal/store`'s pool; request goroutines submit with a
   select-on-`ctx.Done()` (so a full pool surfaces as backpressure, not a stuck handler); the pool closes
   it on phase-1 drain after workers have consumed pending jobs (close by the owner after sends stop).
5. **`nil` channel trick** is allowed for disabling a select arm (set to `nil` to park it), but each use
   needs a comment naming the arm it parks.

```go
// Ownership example: progress packets (§6.1 broadcast, capacity 1024).
// Producer: the active task's reporter. Owner of the ring: the RepoHandle (§6). Consumers: SSE streams.
func (h *RepoHandle) report(p Packet) { h.ring.publish(p) }  // never blocks: §6 drop-oldest
```

### Concurrency
Hazard: send-on-closed panics and goroutine leaks from unowned channels. Avoidance: rule 1 + rule 2; the
§9 vet/race mandates catch violations; every goroutine appears in the §1 inventory table (a `go` statement
outside that inventory needs a comment pointing at its section here).

## 6. SSE / broadcast fan-out with slow-consumer policy

§6.1: per-repo broadcast channel (capacity 1024) of progress packets; the active task's reporter is
mirrored into it; tasks publish **regardless of listeners**. §19 maps this to "buffered channels +
drop-oldest or lag-tolerant subscribers". Normative Go policy:

- The publisher (task reporter) **never blocks and never allocates per subscriber**. Publish is
  drop-oldest per subscriber ring: if a subscriber's buffer is full, discard its oldest packet to admit
  the new one, and count `lagged++`.
- Subscribers get a snapshot + live tail: late SSE attachers replay the per-task 200-packet buffer
  (bars deduped by label, §6.8), then follow the ring.
- Keepalive comments every 10 s (goroutine #13) — also non-blocking (a dead TCP stream is detected by
  write error, not by ring pressure).
- Terminal `result`/`error` packets are never dropped by the ring (they are delivered from the task
  record, not the stream), so a lagged client still learns the outcome.

```go
// internal/events/ring.go — small lag-tolerant broadcast; the pattern all fan-outs reference.
type Ring struct {
    mu   sync.Mutex
    subs map[uint64]*sub
    next uint64
}
type sub struct {
    ch   chan Packet // capacity 1024 (§6.1); owned by Ring, closed by Ring on unsubscribe
    drop int
}
func (r *Ring) Subscribe() (id uint64, ch <-chan Packet) {  // receiver gets read-only view
    r.mu.Lock(); defer r.mu.Unlock()
    id = r.next; r.next++
    s := &sub{ch: make(chan Packet, 1024)}
    r.subs[id] = s
    return id, s.ch
}
func (r *Ring) Unsubscribe(id uint64) {                     // receiver-initiated teardown: Ring closes
    r.mu.Lock(); defer r.mu.Unlock()
    if s, ok := r.subs[id]; ok { close(s.ch); delete(r.subs, id) }
}
func (r *Ring) Publish(p Packet) {                          // never blocks the publisher
    r.mu.Lock(); defer r.mu.Unlock()
    for _, s := range r.subs {
        select { case s.ch <- p:
        default:                                             // slow consumer: drop OLDEST, admit newest
            select { case <-s.ch: s.drop++
            default: }
            select { case s.ch <- p: default: }
        }
    }
}
```

The SSE handler is the receiver: it ranges over `ch`, selects on `ctx.Done()`, and calls
`Unsubscribe` (via `defer`) — the Ring, not the handler, performs the `close`, satisfying rule 5.1.
Task progress, events-bridge fan-out, and watchdog gauges all reuse this type; do not write a second
broadcast mechanism.

### Concurrency
Hazard: a slow SSE client (or one paused via TCP zero-window) blocking the publisher and with it the
publish path (group commit's "answer every waiter" step, §6.3). Avoidance: publisher send is
non-blocking with drop-oldest; subscriber teardown is the Ring's job; the publisher holds `r.mu` only for
map iteration (no I/O under it).

## 7. Deadlock autopsy — the four historical incidents

These are the regression tests the §9 kit encodes. Each: mechanism → invariant that prevents it → test.

1. **24-minute clone starved `info/refs` via queued writer.** A `tokio::RwLock.write()` (here: a blocking
   `rw.Lock()`) queued behind a clone's long-lived read guard; every new `RLock` queued behind the writer
   (§6.1, §19). Invariant: **removals are the only writer class and use `TryLock`-or-defer** (§2.1).
   Test: `TestRemovalsNeverBlockReaders` — hold `RLock`, run removal, assert it deferred (pending set
   unchanged) and a concurrent `RLock` succeeds within 50 ms.
2. **200-byte GET behind 32 stripes via shared transport.** A control-plane GET (`bundles/list.pb`) sat
   455–472 s behind a 32-stripe bulk download on a shared HTTP client/permit (§4.6). Invariant: **bulk
   bytes and control-plane objects never share a transport or a permit** — separate `http.Client` (or
   presigned-request path) and separate semaphore per class; classification by key is mandatory even on
   S3. Test: `TestTransportSeparation` — a store with a slow bulk GET in flight must answer a
   control-plane HEAD within the store contract's latency bound (sim harness: `FaultStore` bulk delay 5 s,
   control-plane assert < 1 s).
3. **Publisher deadlock on the local repo lock.** The publisher (holding `packMu` from its own sync step)
   blocked acquiring `syncMu` held by a request goroutine that was itself waiting on the publisher's
   result — a lock-order inversion of §2.2 rule 1. Invariant: **`syncMu` before `packMu`, always; the
   publisher takes locks in one direction only, and its CAS/store work happens outside both** (§2.2).
   Test: `TestPublisherLockOrder` — race 16 publishers × 16 syncs × 16 removals with `-race`; canary
   (below) must not fire.
4. **Late-tick watchdog misread a stall.** The watchdog's 1 s tick fired > 2.5 s late and the old logic
   treated "late tick" as "runtime stalled" without consulting `inflight` (§3.4: `inflight = 0` ⇒ the
   platform paused the process; `inflight > 0` ⇒ a real stall). Invariant: **the watchdog diagnoses only
   from (tick lateness, `walgit_http_inflight`) together, never from lateness alone, and it never takes
   corrective action** — it observes and logs. Test: `TestWatchdogLateTick` — feed a synthetic 3 s tick
   gap with `inflight == 0`, assert the log line says "platform pause", not "stall".

Autopsy checklist for any NEW deadlock (all docs reference this list):
(a) name the goroutines and the exact lock/channel each holds; (b) check the §2.2 order — inversion?
(c) check §2 rule 4 — a lock held across a store call? (d) check §3 — should these two actors be
single-flighted into one? (e) check §5 — close ownership and buffer bounds; (f) reproduce under `-race`
with the §9 canary before writing the fix; (g) add the incident to this list with its invariant.

## 8. Shutdown / drain: two-phase in Go

§3.4's drain maps onto a context tree. The server owns a root context; every subsystem gets a child.
Phase boundaries are explicit, not garbage-collected.

- **Phase 1 (SIGTERM, bounded 30 s):** `cancelPhase1()` — stops starting maintenance units, interrupts the
  running unit at once (task failure recorded 503 "interrupted: instance shut down; will be retried by the
  next pass", §6.8). Serving + `/readyz` stay UP. Background loops (maintainer, follow, events, sweepers,
  heartbeats) exit at their next `ctx.Done()` check. The bulk pool finishes its current job, then exits.
  The publisher drains its current batch, then exits (in-flight requests may still need their replies).
- **Phase 2:** `/readyz` flips 503 + `Retry-After: 15`; new fetch/push/LFS requests refused 503; the
  listener is `Shutdown(ctx)` with a timeout of `server.drain_timeout` on in-flight requests; then exit.

```go
// internal/server/drain.go
ctx1, cancel1 := context.WithCancel(context.Background())     // phase 1: work context
srvCtx, _     := context.WithCancel(ctx1)                     // serving derives from phase-1 context
// SIGTERM:
func onTerm() {
    cancel1()                                                  // phase 1 begins: loops + units see Done
    select {
    case <-backgroundDone:                                     // WaitGroup of §1 rows 2–13
    case <-time.After(30 * time.Second):                        // bounded (§3.4)
        log.Warn("drain phase 1 timed out; forcing phase 2")
    }
    readyz.SetDraining()                                       // 503 + Retry-After: 15
    rejectNewGit()                                             // fetch/push/LFS → 503
    sctx, sc := context.WithTimeout(context.Background(), cfg.Server.DrainTimeout)
    defer sc()
    _ = httpSrv.Shutdown(sctx)                                 // phase 2: drain requests
    os.Exit(0)
}
```

Waitgroup discipline: **the starter adds, the goroutine defers done** — never a shared counter across
restarts. Task interruption IS context cancel: every task body's store calls and `exec.CommandContext`
receive the phase-1 ctx, so an interrupted materialize kills the git subprocess and records the 503
failure (§6.8 drain hooks). Lease release on drain is best-effort but MUST be attempted before exit
(§19 RAII row: `defer` plus an explicit release in the drain path). Watchdog (#12) exits last — in
phase 2, after the listener, so the final log lines are visible.

### Concurrency
Hazard: phase-1 cancel racing a publish batch (answered waiters vs interrupted CAS) and orphaned git
subprocesses. Avoidance: the publisher treats cancel as "finish current batch, stop accepting"; every
subprocess is `exec.CommandContext(ctx, …)`; every long unit checks `ctx.Err()` between steps and records
the 503 interrupted outcome. Never call `os.Exit` while any §1 goroutine can still write state — exit
order is: background done/timeout → listener drained → exit.

## 9. Go test kit for concurrency

Mandates (CI gates; `just test` runs these, §17 fast tier):

1. **Race detector is mandatory, not optional:** every concurrency test runs under `go test -race` in CI;
   a data race in `internal/{store,wal,git,bundle,policy,server,api,events,maintain}` fails the build.
2. **Stress counts:** flaky-prone tests carry `-count=100` in a named Make target (`just test-stress`),
   not the default pass (default pass keeps §17's < 1 min budget; stress runs nightly-ish, matching the
   Rust `slow` tier).
3. **Deadlock canary pattern:** every multi-actor test wraps its body in a watchdog goroutine that
   fails the test on silence:

```go
func withCanary(t *testing.T, d time.Duration, body func()) {
    t.Helper()
    done := make(chan struct{})
    go func() { body(); close(done) }()
    select {
    case <-done:
    case <-time.After(d):
        buf := make([]byte, 1<<20)
        n := runtime.Stack(buf, true)              // full dump: names the guilty goroutine pair
        t.Fatalf("deadlock canary fired after %s\n%s", d, buf[:n])
    }
}
```

   All four §7 incidents have a canary-backed regression test (names there). The canary timeout is 5 s
   for lock tests, 30 s for store-involved tests.
4. **Budget assertion tests count store ops** (§4.8, §17): the sim harness's `FaultStore` counts ops per
   class; assertions like `push ≤ 5`, `warm refs = 1`, `checkpoint = 4` are ported as Go tests over the
   same counting store. Any concurrency change that quietly adds a round trip (e.g. a lock waiting on a
   re-GET) fails the budget test — this is how we keep "depth before count" honest.
5. **Goroutine-leak check:** tests that spawn from the §1 inventory assert `runtime.NumGoroutine()`
   returns to baseline (± 2) after the subsystem's ctx is canceled and `Wait` returns; a leak means a
   missing exit path (§1 table column 4).
6. **Determinism:** no `time.Sleep`-based synchronization in concurrency tests — use channels, the
   canary, or the sim harness's delay injection (§17 FaultStore: pre-op delay, error-after, stale-304,
   black hole). Randomized seeds via `WALHUB_SIM_SEED(S)` mirror the Rust sim.

## Decisions & deviations from the Rust design

- `rw` try-write (§2: canonical primitive `internal/wal/rw.TryRWMutex`, ruling C-2) replaces `tokio::RwLock::try_write` — same try-or-defer removal protocol; the starvation hazard is identical, so the invariant ports unchanged.
- Dedicated **bulk worker pool via bounded channel + weighted-semaphore permits** (`internal/store/errgroup.go`) replaces the tokio
  "bulk runtime 4 workers" — the permit (`gcs_bulk_permit` metric name kept) is acquired inside the pool,
  never on request goroutines, preserving §4.6 transport separation.
- **`errgroup.SetLimit` / weighted semaphore (hand-rolled, `internal/store/errgroup.go`, ruling C-1)** replace `buffer_unordered(n)` and
  `Semaphore::acquire` fan-outs.
- **Hand-rolled `singleflight.Group`** replaces `DashMap`+join semantics and moka's single-flight: one
  20-line helper covers all five per-key sites, honoring §6.8's join-with-bounded-wait rule via
  `ctx`-select; `sync.Map`/sharded maps replace `DashMap` for registry/task tables.
- **Ring-buffer broadcast with drop-oldest** replaces tokio's `broadcast::channel(1024)`: same capacity,
  but lag-tolerant by construction (tokio's `Lagged` error becomes a counter), and terminal result/error
  packets are delivered from the task record so lagged SSE clients never miss outcomes.
- **`context.Context` tree + explicit two-phase drain** replaces tokio's cancellation tokens and the
  runtime's drop-based task abortion: phase-1 cancel interrupts tasks/subprocesses
  (`exec.CommandContext`), phase 2 drains HTTP via `server.Shutdown`; interruption records the same
  503 "interrupted" task outcome as §6.8.
- **Deadlock canary + `-race` + `-count=100` stress + goroutine-leak and op-budget assertions** replace
  the Rust `#[ignore]`-soak tier for concurrency: canary dumps full stacks (`runtime.Stack`) so an
  autopsy starts from the test, not from prod logs.
- Watchdog diagnostics now require `(tick lateness, inflight)` jointly (incident 4); the Rust spec's
  "warns when a tick is late" wording is refined, not contradicted — the spec's own `inflight` clause is
  the authority.
- One enumerated exception to "no lock across a store call": the manifest freshness GET under `syncMu`
  (§2.2), which is §6.1's refs-phase serialization verbatim; it is control-plane-transport-only so the
  §4.6 separation keeps it sub-second.
