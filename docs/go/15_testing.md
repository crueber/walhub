# 15 — Testing strategy
> Source: MASTER_RUST_SPEC.md §17 (testing strategy, FaultStore, sim), §4.8 (round-trip budgets as test assertions), §20 (discrepancies as test-worthy behaviors) · Status: normative for the walhub Go implementation.

walhub keeps the Rust system's testing philosophy unchanged: correctness claims are **proven by simulation**, not by unit tests alone; the object-store trait has **one contract suite run against every backend**; and protocol work is judged on **counted round trips**, asserted at the transport layer. The Go port replaces the tooling (cargo → go test, justfile) but copies every tier, every fault mode, and every budget.

Test framework: stdlib `testing` only. No testify, no gomock (dependency policy, see 01_overview.md). Assertions are `if got != want { t.Fatalf(...) }`; subtests are `t.Run`; parallel cases use `t.Parallel()`. Directory layout follows the canonical tree (02_concurrency.md is the canonical concurrency playbook referenced throughout).

---

## 1. Tiers

| Tier | Command | What runs | Bar |
|---|---|---|---|
| fast | `just test` → `go test -short -count=1 ./...` | every unit/integration test honoring `testing.Short()`; hermetic: in-memory store, `t.TempDir()` caches, the real `git` binary | < 1 min; a watchdog (`timeout 300`) wraps the whole recipe |
| store contract | `just test` includes it (memory); `just contract-s3` / `just contract-gcs` for backends | ONE suite `contract.Run(store, prefix)` (§2) | every case green on every backend that has credentials |
| sim | `just sim` → `go test -count=1 -timeout 15m ./internal/sim/...` | fault-injection over one truth store, one `FaultStore` **per instance link** (§3, §4) | the consistency proof; deterministic under a seed |
| e2e | `just e2e` → `tests/e2e.sh` | real git clients against the running walhub server (smart HTTP, receive/upload-pack, bundle-uri, WAL) | ~20 s; run whenever git-path code changes |
| lint/vet | `just lint` → `go vet ./...` && `gofmt -l` && `go build ./...` | Go equivalent of Rust's `-D warnings`: `go vet` output, any `gofmt -l` line, or any build warning fails the gate | zero findings |
| race | `just race` → `go test -race -short -count=1 ./...` | full fast tier under the race detector (§6) | zero races |
| slow | `just test-slow` → `go test -run 'Slow' -count=1 ./...` | `TestSlow*` soaks: 20 k-ref push, 466 k-ref refs render, long bundle chains | nightly-ish; never in fast |

Rules carried over verbatim from §17:

- **Never run the whole test tree unbounded.** Every recipe is wrapped in a watchdog. The justfile resolves `timeout` (GNU coreutils) and falls back to `gtimeout` (macOS); neither present → run unwrapped with a warning, never hang a contributor's session.
- **Known-flaky tests are named with their cause, never assertion-loosened.** Port both known flakes with their causes as comments: a base published without `has_commit_graph` ~1 in 3 under the full e2e suite (passes alone); a base-rebuild kill-resume race ~1 in 7 on a shared test-abort constant. If a flake appears, name it, quarantine it behind `-run`, and fix the cause.
- Tests are hermetic unless noted: e2e is the only tier that talks to a live server; slow soaks use the local rustfs dev store.

---

## 2. The store contract suite

**One suite, every backend.** `internal/store/contract/contract.go` exposes:

```go
// Run exercises every observable guarantee of store.ObjectStore against `store`,
// using keys under `prefix` ("" for memory). All state is cleaned up before return.
func Run(t *testing.T, store store.ObjectStore, prefix string)
```

Wiring (`internal/store/contract/backend_test.go`):

| Test | Condition | Backend |
|---|---|---|
| `TestContract_Memory` | always | `store.NewMemory()` |
| `TestContract_S3` | `WALHUB_TEST_S3_ENDPOINT` set (local rustfs from `just dev-store`) | S3 backend, `force_path_style=true`, dev keys |
| `TestContract_GCS` | `WALHUB_TEST_GCS_BUCKET` set | GCS JSON-API backend |

Envs unset → `t.Skip` with the exact env name in the message. **S3 and GCS are both first class: the contract must pass on either when its env is set; making one backend pass and treating the other as optional is a bug** (§17). CI runs memory always and S3 via the rustfs container; GCS runs wherever a bucket credential exists. A prefix (default `test/<runid>/`) isolates concurrent runs on shared backends; cleanup deletes every listed key under the prefix afterward.

Case list — table-driven subtests, these exact names, copied semantics from the Rust `run_contract`:

| Test function | Asserts |
|---|---|
| `TestContract_PutCreateWinsOnce` | `Create` on an absent key succeeds; a second `Create` on the same key returns `PreconditionFailed` (412-class); content of the winner is intact |
| `TestContract_PutUpdateCAS` | `Update(wrongVersion)` → `PreconditionFailed` with `current` filled (S3 does the follow-up HEAD); `Update(rightVersion)` → ok; version token after update differs; callers never parse tokens, only compare |
| `TestContract_GetIfNoneMatch` | GET with `if_none_match` = current version → `NotModified{version}`; with a stale version → full object |
| `TestContract_GetIfMatch_Mismatch` | GET with `if_match` ≠ current version → `PreconditionFailed`; with the current version → object |
| `TestContract_RangeReads` | half-open `[start,end)` ranges; last byte; out-of-range end clamps (no error); `start ≥ size` → empty body; `size` in the returned meta is the **whole** object, not the range |
| `TestContract_HeadAndAbsent` | `head` on a present key returns size + version; on an absent key returns "absent" (Go: `(*ObjectMeta)(nil), nil` — see 03_store.md for the exact signature); `get` on absent → `NotFound` |
| `TestContract_Delete` | unconditional delete of an absent key is **Ok** (idempotence); conditional delete with the right version deletes; with a wrong version → `PreconditionFailed`; on S3 the conditional delete is the documented emulated HEAD-then-delete (§20.9) — the suite asserts the *result*, not the wire shape |
| `TestContract_ListOrderingAndPrefixIsolation` | `list(prefix)` returns keys lexicographic, strictly after `start_after`; keys of sibling prefixes never leak; a lazy stream (Go: an iterator func `func(yield func(ObjectMeta) bool)`) pages correctly |
| `TestContract_ListPrefixes` | delimiter listing returns distinct `"<prefix><segment>/"`, sorted, files of a directory not returned |
| `TestContract_LargeStreamedRoundtrip` | a multi-MiB `Stream` body (KNOWN length required) round-trips with a SHA-256 checksum match; content-type survives |
| `TestContract_MultipartPath` | a body above `store.multipart_threshold` (64 MiB default; the suite uses a smaller threshold injected for test speed) lands as one object with identical bytes; a mid-multipart failure aborts and leaves no part objects listed |
| `TestContract_Compose` | compose of 1..=32 sources in order = byte-concat; honors `Create` (second compose of the same dest → 412) / `Update` / `Overwrite`; sources remain in place; > 32 sources → `InvalidArgument`; on S3 this is the emulated multipart-of-ranges path (`compose_is_native() == false`) — the assertion is on the resulting bytes, never on the mechanism |
| `TestContract_LeaseSteal` | protocol-level case on top of CAS: absent lease → `Create` (epoch 0); present and `now ≥ expires_at + 2s` → rewrite `epoch+1` via `Update`; early steal attempt → 412 (lost race); release deletes; deleting an already-stolen/absent lease is Ok (§4.9) |

Memory backend is always exercised, no env needed — `TestContract_Memory` is part of the fast tier.

**Concurrency (store contract).** Hazard: the S3/GCS cases hit real HTTP against a container that may be slow or absent. Avoidance: each case is sequential inside one `Run` (the suite is deterministic; parallelism is exercised by the sim, not here), every case keys under a run-unique prefix, and cleanup uses one goroutine draining a bounded channel of keys with a `context.WithTimeout(30s)` so a hung backend cannot wedge the test binary. See 13_concurrency.md for channel-ownership rules.

---

## 3. FaultStore — the sim's heart, ported

Package `internal/store/fault`. One FaultStore wraps the inner store **per instance link**; N simulated instances share ONE truth store (memory), each seeing the truth only through its own fault store.

### 3.1 The fault plan

```go
// Probabilities are 0..=1 floats; Default = no faults at all.
type Plan struct {
    // Uniform latency added to every op, before it is applied. nil = none.
    Delay [2]time.Duration // low, high
    // Latency added to reads (get/head) AFTER the inner op: the answer was taken
    // at an earlier instant and arrives late — a conditional GET racing a local
    // publish. Honors OnlyKeys.
    DelayAfter *time.Duration
    // Retryable before the op is applied (get/head/put/delete/list/compose).
    PErrBefore float64
    // Mutation applied, then Retryable returned (put/delete/compose only) —
    // the class that breaks PUT-then-CAS protocols.
    PErrAfter float64
    // Conditional PUT/DELETE answers PreconditionFailed without applying.
    PCASFail float64
    // get with if-none-match answers NotModified regardless of the real
    // version (a replica that never sees anyone else's writes).
    PStale304 float64
    // get body streams end early with Retryable after some bytes.
    PTruncate float64
    // The op's context never completes.
    PHang float64
    // Every op hangs forever (hard partition). Pending ops keep hanging.
    BlackHole bool
    // Keys containing any of these substrings answer NotFound on get/head
    // (object lost / not yet visible). Mutations still go through.
    DenyKeys []string
    // Keys containing any of these substrings panic on first touch, once per
    // pattern (a crash mid-protocol). A pattern may be scoped to one op as
    // "put:manifest.pb" (ops: get/head/put/delete/compose).
    PanicOnceKeys []string
    // Restrict every probabilistic fault to keys containing one of these
    // substrings (nil = all keys). BlackHole/Deny/Panic are unaffected.
    OnlyKeys []string
}
```

Presets (same semantics as §17):

| Preset | Fields | Meaning |
|---|---|---|
| `Chaos(rate)` | `Delay [0,5ms]`, `PErrBefore=rate`, `PErrAfter=rate/2`, `PCASFail=rate/2`, `PStale304=rate/2`, `PTruncate=rate/2`, `PHang=0` | moderate uniform chaos, the "safety mode" dice |
| `BlackHole()` | `BlackHole: true` | hard partition: nothing ever returns |
| `StaleForever()` | `PStale304: 1.0` | asymmetric partition of the replica kind: writes go through, the instance never learns anything new |

### 3.2 Decide order (per op, normative — port exactly)

The wrapper rolls the dice **in this order**; first match wins:

1. `ops` counter increments (every op, even ones that will fault).
2. **panic-once**: first matching pattern in `PanicOnceKeys` (op-scoped patterns checked as `op:key`, else substring) not yet in the fired set → record it, bump `panics`, `panic(...)` in the request goroutine. In Go a panic must be converted at the instance boundary: the sim's `Instance` wraps every call with a recover that turns it into a synthetic process death — restart the instance (§4). This is the "process crash mid-protocol" injection.
3. **black_hole** → `Hang` (pending ops keep hanging; healing the plan does NOT unhang them — that is the point of a crash).
4. **deny_keys** on non-mutations (get/head) → `Denied` → `NotFound`.
5. **delay**: uniform in `[Delay[0], Delay[1]]` via the wrapper's own seeded RNG.
6. Scope check: `OnlyKeys` set and key matches none → `Proceed` with no faults.
7. Dice, in order: `PHang` → `PErrBefore` → `PCASFail` (only mutation ∧ conditional) → `PErrAfter` (only mutation) → `PStale304` (only read ∧ conditional) → `PTruncate` (only reads with a body) → `Proceed`.

Consequences to preserve: `casFail` answers 412 **without applying**; `errAfter` applies the mutation **then** returns `Retryable` (the ambiguous-write class); `stale304` answers NotModified regardless of the true version.

RNG: xorshift64* seeded per link from `WALHUB_SIM_SEED` (deterministic; the Rust wrapper uses the same generator shape — port it byte-for-byte so seeds are reproducible across ports). Guarded by its own mutex; never shares the plan mutex.

### 3.3 API surface

```go
type FaultStore struct { /* inner store.ObjectStore; name; mu-guarded plan; stats; rng; fired map */ }
func New(inner store.ObjectStore, name string, seed uint64) *FaultStore
func (f *FaultStore) Set(plan Plan)          // takes effect for every op issued from now on
func (f *FaultStore) Plan() Plan             // current plan (copy)
func (f *FaultStore) Heal()                  // liveness mode for a core link: zero faults;
                                             // ops already hanging stay hung
func (f *FaultStore) Stats() *Stats          // exact per-op counters
func (f *FaultStore) SetTrace(on bool)       // ring of "name op key: decision" lines
func (f *FaultStore) TakeTrace() []string
```

Stats — atomic counters, one per fault class plus total ops: `Ops, ErrBefore, ErrAfter, CASFail, Stale304, Truncate, Hang, Denied, Panics` (atomic.Uint64). `Stats.Faults()` sums all fault classes; `Stats.Summary()` renders the `k=v` line the sim dumps on failure. Budget assertions read `Stats.Ops` (§4.8: "Simulation asserts exact budgets (`FaultStore::stats().ops`)").

FaultStore implements `store.ObjectStore` fully (get/head/put/delete/list/list_prefixes/compose/signed_get_url/accel_target + the get_bytes/put_bytes helpers of 03_store.md), delegating to `inner` after `decide` says `Proceed`.

### 3.4 The truth oracle

The cluster holds the truth store itself (unwrapped). `Cluster.TruthManifest(repo)` reads `repos/<o>/<r>/manifest.pb` **via the truth store, bypassing every link** — no instance's stale view can contaminate it. `checkTruth` post-run: decode the truth manifest, verify committed log segments form a contiguous replay, verify every pusher's refs are (eventually) reflected, verify no pack referenced by the manifest is missing. `dumpTraces` prints each link's name, `stats.Summary()`, and its last 40 trace lines (reversed, newest first) — the standard failure artifact.

### 3.5 Concurrency

Hazard: `Hang` means a goroutine that never returns; a black-holed link leaks a goroutine per op, and `panic()` in a wrapper goroutine kills the whole test process unless recovered. Avoidance: every FaultStore op takes a `ctx context.Context`; `Hang` is implemented as `<-ctx.Done()` with **no timeout of the wrapper's own** (the caller's timeout is the test's instrument); instance calls use `context.WithTimeout(10s)` so a hung op surfaces as a deadline error the sim treats as a crash. The `fired` panic-once set and the plan are mutex-guarded with a **fixed lock order**: plan mutex → fired mutex → rng mutex (13_concurrency.md); no op ever holds two while calling into `inner`. Trace slices use the same plan mutex; `TakeTrace` swaps under it.

---

## 4. Sim scenarios (`internal/sim`)

One test file per scenario, all prefixed `TestSim_`, all hermetic, all seeded. Harness: a `Cluster` of N instances sharing one truth store and one repo; each instance = walhub server internals + its own FaultStore link; `AddInstance(tweak)`, `Restart(i)` (fresh process state, same link), `RestartKeepDisk(i)` (persistent cache dir survives). A `Pusher` does real receive-pack-style pushes through the instance's git layer with a bounded timeout.

| Go test | Ported from | What it proves |
|---|---|---|
| `TestSim_SafetyThenLiveness` | `sim_safety_then_liveness` (seeded) | N pushers × M pushes under `Chaos(rate)` faults: **exactly one winner per competing transaction**, no lost commits, every instance converges to the truth refs; `checkTruth` + liveness checks after faults are healed |
| `TestSim_OrphanedLogSegmentDoesNotBlockWriters` | `liveness_orphaned_log_segment_does_not_block_writers` | crash between the log-segment PUT and the manifest CAS → the segment is an orphan; later writers **burn past its seq** (§6.4), a later commit sweeps it; writers never block on it |
| `TestSim_AfterALostCASResponse` | `liveness_after_a_lost_cas_response` | `errAfter` on the manifest CAS → the write may have landed; the writer re-reads fresh ("cas_landed") and treats "my segment is listed" as committed; no duplicate seq, no lost ref |
| `TestSim_StaleInstanceCannotStarveTheCore` | `liveness_stale_instance_cannot_starve_the_core` | a replica under `staleForever` keeps answering 304; the monotonic revision guard makes a stale manifest read **after a local publish ignored**; the core (other instances) still commits |
| `TestSim_ConcurrentPushersExactlyOneWinner` | `Pusher::push_once` races in `sim_safety_then_liveness` | K instances push conflicting ref updates simultaneously; the truth manifest ends with exactly one manifest revision per CAS and every loser observes the winner's version (412 → re-sync) |
| `TestSim_ReaderWriterReadGuardDuringCompaction` | `liveness_leaked_read_guard_pins_cache_until_drop` | a clone holds the pack-cache **ReadGuard** while compaction removes packs → compaction proceeds (try-write rule: writers never block on a reader lock, they retry); a leaked guard pins the cache only until drop, never deadlocks |
| `TestSim_BaseRebuildResumesAfterKillBetweenAnyTwoPhases` | `base_rebuild_resumes_after_a_kill_between_any_two_phases` | kill the rebuild between any two phases (`copied → repacked → history_pack → commit_graph`); resume continues from the marker iff `manifest.head_seq == started_head_seq`, else restarts; across all attempts **exactly one `git repack` runs** |
| `TestSim_DrainInterruptsRunningUnit` | drain hooks (§6.8) + `sim_task_ownership_under_concurrency_and_owner_crash` | SIGTERM phase 1 interrupts the running maintenance unit; the dropped task records failure 503 "interrupted: instance shut down; will be retried by the next pass"; the next pass retries it; serving stays up through phase 1 |
| `TestSim_CheckpointWriterCrashIsInvisibleAndRepaired` | `sim_checkpoint_writer_crash_is_invisible_and_repaired` | panic-once during checkpoint writes leaves garbage keyed by seq that is never a hazard; the next writer checkpoints idempotently |
| `TestSim_BlackHoledInstanceIsInvisibleToTheCore` | `liveness_black_holed_instance_is_invisible_to_the_core` | a black-holed link never wedges the core or leases: lease steal after `expires_at + 2s` works, compaction proceeds, the partition heals or the instance is restarted |
| `TestSim_HealthyRequestRoundTripBudgets` | `healthy_request_round_trip_budgets` | the §4.8 budgets, counted at the transport layer via the FaultStore link stats (below) |
| `TestSim_LivenessUnderRandomSeeds` | `WALGIT_SIM_SEED(S)` plural | loops seeds (default set: 22 and neighbors; `WALHUB_SIM_SEED` / `WALHUB_SIM_SEEDS` override) through `SafetyThenLiveness` — the randomized consistency proof |

All sim tests skip in `-short` mode (`if testing.Short() { t.Skip }`) so `just test` stays under a minute; `just sim` runs them with a generous `-timeout`.

### 4.1 Budget assertions (counted at the transport layer)

`TestSim_HealthyRequestRoundTripBudgets` snapshots `link.Stats().Ops` around each operation and asserts:

| Operation | Assertion | Budget source (§4.8) |
|---|---|---|
| push (per batch, already synced) | `ops ≤ 5` (4 if already synced) | freshness GET → (pack PUTs ∥ log PUT) → manifest CAS |
| warm refs sync | `ops ≤ 1` | 1 conditional GET (0 within freshness TTL) |
| cold refs sync (one tail) | `ops ≤ 2` | manifest GET → (checkpoint refs ∥ tail) |
| checkpoint | `ops ≤ 4` | freshness GET → (refs PUT ∥ checkpoint PUT) → manifest CAS; provenance times come from what the writer already applied, **never a log GET** (the 2026-08-22 regression was 6 requests — this assertion is the regression fence) |

Counting rule: read `Stats.Ops` immediately before and after the awaited op on the **acting instance's link only**; background maintenance on other instances does not count against this instance. Failure messages include the measured count, the budget, and `dumpTraces` output.

### 4.2 Concurrency

Hazards and rules (13_concurrency.md is the playbook; these are the sim-specific applications):

- **Hung goroutines** → every FaultStore op is `ctx`-cancellable; the cluster restarts an instance by abandoning its goroutines, never by waiting on them. The test binary's `-timeout 15m` is the outer watchdog.
- **Panic-injected crashes** → recovered at the instance boundary, never at the wrapper: one request's injected panic must not kill the test process (mirrors the prod rule that one request's panic must not kill the instance).
- **Restart races** → `Restart` closes the instance's channels before spawning the replacement; **who owns and who closes** is fixed: the cluster owns and closes all instance lifecycle channels; instances close only their own task channels (13_concurrency.md §shutdown).
- **Shared truth store** → the memory store serializes under one mutex; sim load is deliberately modest (≤ 8 instances) so the lock never becomes the bottleneck under `-race`.

---

## 5. Wire-compat fixtures and git e2e

### 5.1 Golden wire fixtures

`testdata/wire/` holds reference vectors produced by the Rust implementation, checked in and never regenerated by Go code:

| Fixture | Content | Test |
|---|---|---|
| `manifest.pb.golden`, `log_segment.pb.golden`, `lease.pb.golden`, `checkpoint.pb.golden`, `bundle_list.pb.golden` | protobuf bytes exactly as the Rust writer emitted them (package `walgit.v1`, field numbers per 05_protobuf.md) | `TestWireDecode_Golden`: hand-written wire decode (02_protobuf.md's decoder) parses each to the expected struct field-by-field; re-encode → **byte-identical** to the golden |
| `log_frame_stream.golden` | a multi-entry WAL segment with uvarint framing (§5.3) | `TestWireFrames_Golden`: framing decode → entries; re-frame → byte-identical |
| `ref_vectors.json` | CAS version/ETag fixtures per backend from recorded exchanges | fed to the contract suite's token-equality cases |

Byte-identical re-encoding is the compat bar (11_compat.md): a Go-written manifest must be readable by the Rust reader and vice versa; `cmp` of decoded-and-re-encoded bytes is the test. A Go struct field order or omission that changes field numbers fails here, not in production.

### 5.2 Git e2e with real clients

`tests/e2e.sh` (plus `tests/lib-auth.sh` ported) drives a live walhub server with the real `git` binary — the same pattern as the Rust repo:

- The server is started once per run (`just dev-local` or an explicit URL); e2e talks to `WALHUB_E2E_BASE_URL` (default `https://walgit.localhost:8080`) with `WALHUB_TOKEN`.
- **Never touch the user's git config.** Every git invocation runs with a private `GIT_CONFIG_GLOBAL` pointing into the test sandbox; credentials are staged there (credential helper or URL-in-token), `GIT_TERMINAL_PROMPT=0` is always exported, and the sandbox is cleaned by the trap. lib-auth.sh is the ONLY place that composes these env vars; test bodies just call its helpers.
- Flows covered (~20 s): clone over bundle-uri (advertisement, chain tokens, remainder fetch), push + ref update + WAL visibility, second clone reusing the bundle, fetch after push from another client, LFS round-trip, 503 edge-fallback shape, drain behavior (SIGTERM mid-clone completes or fails cleanly with the documented status).
- Go harness for unit-level git tests: `internal/git` tests run the real binary with exact argv (04_git.md), same stdin/stdout copy discipline; never go-git.

---

## 6. Concurrency test kit (mandates from 13_concurrency.md)

Mandatory for every PR that touches shared state:

1. **`-race` in CI** — `just race` runs the full fast tier under the race detector. A data race is a failing build, no exceptions.
2. **Stress tests** — every lock/actor with a plausible contention story gets a stress test: N goroutines × M ops against the shared structure, asserting the invariant (not the interleaving). The sim IS the store/WAL stress test; package-level stress tests cover caches, the SSE fan-out, and the events bridge.
3. **Deadlock canary** — the test binary runs under `timeout 300` (fast tier); a suite that deadlocks fails the recipe, it does not hang CI silently. Additionally the watchdog pattern from §3.4 of the Rust spec is unit-tested: a stalled-tick detector test asserts the "async runtime stalled" warning fires when a tick is > 2.5 s late (simulated with a fake clock).
4. **Every goroutine has a shutdown path** — linters cannot check this; review does. The sim's restart machinery is the executable proof that instances shut down on context cancel.

---

## 7. justfile sketch

```just
# Resolve GNU timeout (coreutils); absent on macOS where every wrapped recipe
# otherwise dies with exit 127. No watchdog is worse than no tests.
t5  := `if command -v timeout >/dev/null 2>&1; then echo "timeout 300"; elif command -v gtimeout >/dev/null 2>&1; then echo "gtimeout 300"; else echo ""; fi`
t15 := `if command -v timeout >/dev/null 2>&1; then echo "timeout 900"; elif command -v gtimeout >/dev/null 2>&1; then echo "gtimeout 900"; else echo ""; fi`

default:
    @just --list

build:
    go build ./...

fmt:
    gofmt -w $(gofmt -l .)

lint:            # the Go "-D warnings" gate
    test -z "$(gofmt -l .)" || (echo "gofmt needed:" && gofmt -l . && exit 1)
    go vet ./...

test:            # fast tier: hermetic, < 1 min, watchdog-wrapped
    {{t5}} go test -short -count=1 ./...

race:            # full fast tier under the race detector
    {{t15}} go test -race -short -count=1 ./...

test-slow:       # soaks: 20k-ref push, 466k-ref render, ...
    {{t15}} go test -run 'Slow' -count=1 ./...

sim:             # the consistency proof (fault injection); seeded
    {{t15}} go test -count=1 -timeout 15m ./internal/sim/...

contract-s3:     # ONE suite against rustfs (starts it if not answering)
    just dev-store
    WALHUB_TEST_S3_ENDPOINT=http://127.0.0.1:9000 \
    AWS_ACCESS_KEY_ID=walgit-dev AWS_SECRET_ACCESS_KEY=walgit-dev-secret \
    {{t5}} go test -count=1 -run 'TestContract_S3' ./internal/store/contract/...

contract-gcs:
    test -n "$WALHUB_TEST_GCS_BUCKET" || (echo "set WALHUB_TEST_GCS_BUCKET"; exit 1)
    {{t5}} go test -count=1 -run 'TestContract_GCS' ./internal/store/contract/...

e2e:             # real git against the live server; ~20 s
    {{t15}} tests/e2e.sh

dev-store:       # rustfs (S3-compatible) on :9000, fixed dev keys, bucket created
    docker compose up -d rustfs
    @echo "rustfs on :9000 (console :9001); keys walgit-dev / walgit-dev-secret; bucket walgit-test"

dev-store-stop:
    docker compose down

dev-local config="walhub.standalone.toml":   # one-box dev: all roles, TLS, rustfs
    #!/bin/sh
    set -e
    export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-walgit-dev}" AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-walgit-dev-secret}"
    curl -sf http://127.0.0.1:9000/minio/health/live >/dev/null 2>&1 || just dev-store
    go build -o bin/walhub ./cmd/walhub
    ./bin/walhub serve --config {{config}}
```

---

## 8. §20 discrepancies that are test-worthy behaviors

The rewrite copies CODE behavior (§20); each of these gets an explicit assertion so it is not "fixed" silently in the wrong direction:

- §20.2/§20.3: `GET /HEAD` and `/objects/info/packs` are 404; no `/services/git-*` routes — e2e asserts the 404s.
- §20.9: S3 conditional delete is HEAD-then-delete (check-then-act race, accepted) — contract asserts the result only.
- §20.7: bundle leases lack the 2 s skew tolerance and heartbeat sets `epoch = 1` — `TestContract_LeaseSteal` pins the **coord** semantics; the bundle-lease quirk is pinned in the bundle tests (10_bundles.md), not "fixed" by accident.
- §20.12: `report-status-v2` negotiated and parsed, `option` lines never emitted (rejected atomic → plain `ng`) — git e2e pushes an atomic transaction with a rejection and asserts the response shape.
- §20.5: the git layer always execs `"git"` regardless of `git.binary` — decision below changes this; the e2e suite must pass with a wrapper-script `git` to prove the config is honored.

---

## Decisions & deviations from the Rust design

- **Test framework is stdlib `testing` with `-short` as the tier switch, not build tags or `#[ignore]`.** One flag (`-short`) cleanly excludes sim/slow from `go test ./...`, matching Go convention and keeping `just test` hermetic; rationale: zero new dependencies, and `-run` composes naturally.
- **Env prefix `WALHUB_`** replaces `WALGIT_` (e.g. `WALHUB_SIM_SEED`, `WALHUB_E2E_BASE_URL`, `WALHUB_TEST_S3_ENDPOINT`, `WALHUB_TEST_GCS_BUCKET`); rationale: the binary and product are walhub; TOML keys (the compat surface, 11_compat.md) are unchanged.
- **`go vet` + `gofmt -l` + `go build` replace cargo's `-D warnings`** as the lint gate; rationale: Go has no clippy; vet + format is the community-accepted equivalent, enforced via `just lint`.
- **FaultStore lives in `internal/store/fault` and the truth oracle is simply the unwrapped memory store** rather than a separate oracle object; rationale: in the Go design every link is a decorator over one shared `store.ObjectStore`, so "read bytes bypassing every link" needs no extra abstraction.
- **Panic-once faults are recovered at the instance boundary and modeled as process death (restart), keeping the wrapper's `panic()`** like the Rust original; rationale: preserves the exact injection semantics (state mid-protocol) while the test harness decides what "the process died" means.
- **The contract suite adds `TestContract_LeaseSteal`** (not present in the Rust contract, which tested leases elsewhere); rationale: in walhub the lease is pure store protocol (§4.9), so pinning it in the one suite that runs on every backend is cheaper and stronger.
- **`git.binary` config is honored by the Go git layer** (fixing §20.5's hardcoded `"git"`); rationale: it costs one flag and makes the §20.5 e2e proof (wrapper-script git) possible; behavior change is marked in 04_git.md.
- **Deterministic RNG (xorshift64*) ported byte-for-byte** so seeds are reproducible across the Rust and Go implementations; rationale: golden failure repro across ports beats a nicer RNG.
- **Budget counters are read from the FaultStore link stats** (exactly as §4.8 prescribes) rather than from an HTTP middleware counter; rationale: the sim exercises the store path directly, and the FaultStore counts precisely the requests the budget model is about.
