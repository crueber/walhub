# 15 — Testing strategy
> Source: MASTER_RUST_SPEC.md §17 (testing strategy, FaultStore, sim), §4.8 (round-trip budgets as test assertions), §20 (discrepancies as test-worthy behaviors) · Status: normative for the walhub Go implementation.

walhub keeps the Rust system's testing philosophy unchanged: correctness claims are **proven by simulation**, not by unit tests alone; the object-store trait has **one contract suite run against every backend**; and protocol work is judged on **counted round trips**, asserted at the transport layer. The Go port replaces the tooling (cargo → go test, just → make) but copies every tier, every fault mode, and every budget — and adds two gates the Rust repo did not have: a per-package **coverage floor** (§1, §7) and a **web/JS test tier** (§6.5).

Test framework: stdlib `testing` only. No testify, no gomock (dependency policy, see 01_overview.md). Assertions are `if got != want { t.Fatalf(...) }`; subtests are `t.Run`; parallel cases use `t.Parallel()`. Directory layout follows the canonical tree (02_concurrency.md is the canonical concurrency playbook referenced throughout).

---

## 1. Tiers

| Tier | Command | What runs | Bar |
|---|---|---|---|
| fast | `make test` → `go test -short -count=1 ./...` + `node --test web/test/` | every unit/integration test honoring `testing.Short()`; hermetic: in-memory store, `t.TempDir()` caches, the real `git` binary; plus the headless JS tests (§6.5) | < 1 min; a watchdog (`timeout 300`) wraps the Go recipe |
| store contract | `make contract` (memory + filesystem, always); `make contract-s3` / `make contract-gcs` for credentialed backends | ONE suite `contract.Run(store, prefix)` (§2) | every case green on every backend: memory + filesystem on every run, S3/GCS whenever their env is set |
| sim | `make sim` → `go test -count=1 -timeout 15m ./internal/sim/...` | fault-injection over one truth store, one `FaultStore` **per instance link** (§3, §4) | the consistency proof; deterministic under a seed |
| e2e | `make e2e` → `tests/e2e.sh` | real git clients against the running walhub server (smart HTTP, receive/upload-pack, bundle-uri, WAL) **plus the first-run/bootstrap lifecycle scenarios (§5.3)** | ~50 s; run whenever git-path or boot-path code changes |
| lint/vet | `make vet` → `go vet ./...` && `gofmt -l` && `go build ./...` | Go equivalent of Rust's `-D warnings`: `go vet` output, any `gofmt -l` line, or any build warning fails the gate | zero findings |
| race | `make race` → `go test -race -short -count=1 ./...` | full fast tier under the race detector (§6) | zero races |
| cover | `make cover` | per-package statement coverage of every `internal/...` package, ≥ 95% fail-under (§7) | CI-enforced; new code lands with tests (D7) |
| web | `make test-web` → `node --test web/test/` | headless JS logic tests + fetch-based server smoke (§6.5) | zero npm dependencies; wired into `make test` and CI |
| slow | `make test-slow` → `go test -run 'Slow' -count=1 ./...` | `TestSlow*` soaks: 20 k-ref push, 466 k-ref refs render, long bundle chains | nightly-ish; never in fast |

Rules carried over verbatim from §17:
- **Never run the whole test tree unbounded.** Every recipe is wrapped in a watchdog. The Makefile resolves `timeout` (GNU coreutils) and falls back to `gtimeout` (macOS); neither present → run unwrapped with a warning, never hang a contributor's session (the `T5`/`T15` variables in §7).
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
| `TestContract_Filesystem` | always | filesystem backend (D4) rooted at `t.TempDir()` |
| `TestContract_S3` | `WALHUB_TEST_S3_ENDPOINT` set (local rustfs from `make dev-store`) | S3 backend, `force_path_style=true`, dev keys |
| `TestContract_GCS` | `WALHUB_TEST_GCS_BUCKET` set | GCS JSON-API backend |

The filesystem backend (Divergence D4) joins memory as **always-run**: `make contract` exercises both on every invocation, so the conditional-write machinery (sidecar `.lock` + flock + stat-compare + atomic rename; `renameat2(RENAME_NOREPLACE)` create-if-absent with the portable fallback; `"<size>:<mtime_ns>"` version tokens doubling as ETags) is contract-proven exactly like S3 conditional PUT is. The suite runs the same case list against it with no special-casing — if the filesystem backend needs a suite tweak that S3/GCS do not, that is a backend bug, not a suite bug. Platform note: CI is Linux, so the `renameat2` path is what CI proves; the portable fallback is additionally unit-tested in `internal/store` (forced by a test-only hook), not by this suite.

Envs unset → `t.Skip` with the exact env name in the message. **S3 and GCS are both first class: the contract must pass on either when its env is set; making one backend pass and treating the other as optional is a bug** (§17). CI runs memory **and filesystem** always (both env-free) and S3 via the rustfs container (`WALHUB_TEST_S3_ENDPOINT`); GCS runs wherever a bucket credential exists (`WALHUB_TEST_GCS_BUCKET`). These harness envs keep the plain `WALHUB_` prefix (single underscore, like `WALHUB_SIM_SEED`); the `WALHUB__SECTION__KEY` double-underscore scheme of 11_config_cli.md §4.2 is for config overrides only. A prefix (default `test/<runid>/`) isolates concurrent runs on shared backends; cleanup deletes every listed key under the prefix afterward.

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

Memory and filesystem backends are always exercised, no env needed — `TestContract_Memory` and `TestContract_Filesystem` are part of the fast tier (`make contract`).

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

All sim tests skip in `-short` mode (`if testing.Short() { t.Skip }`) so `make test` stays under a minute; `make sim` runs them with a generous `-timeout`.

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

- The server is started once per run (`make dev` or an explicit URL); e2e talks to `WALHUB_E2E_BASE_URL` (default `https://walgit.localhost:8080`) with `WALHUB_TOKEN`. The bootstrap scenarios (§5.3) instead boot their own throwaway servers with a temp `--data-dir` each.
- **Never touch the user's git config.** Every git invocation runs with a private `GIT_CONFIG_GLOBAL` pointing into the test sandbox; credentials are staged there (credential helper or URL-in-token), `GIT_TERMINAL_PROMPT=0` is always exported, and the sandbox is cleaned by the trap. lib-auth.sh is the ONLY place that composes these env vars; test bodies just call its helpers.
- Flows covered (~20 s): clone over bundle-uri (advertisement, chain tokens, remainder fetch), push + ref update + WAL visibility, second clone reusing the bundle, fetch after push from another client, LFS round-trip, 503 edge-fallback shape, drain behavior (SIGTERM mid-clone completes or fails cleanly with the documented status).
- Go harness for unit-level git tests: `internal/git` tests run the real binary with exact argv (04_git.md), same stdin/stdout copy discipline; never go-git.

### 5.3 First-run / bootstrap e2e scenarios (Divergences D5, D6)

Zero-config first run and the setup UI/API are first-class behaviors (06_server_http.md §setup, 11_config_cli.md), so they are pinned by e2e scenarios that boot real server processes. Each scenario gets a fresh temp `--data-dir` (`WALHUB_DATA_DIR` or `--data-dir`), a fresh port from the ephemeral range, lib-auth's env isolation, and a trap that kills the server and removes the dir; they run as part of `make e2e` (~30 s added) and in CI. Fixtures (config fragments + expected-error lists) live in `tests/bootstrap/`, the scenarios are shell functions in `tests/e2e.sh`, and unit coverage of the setup handlers stays in `internal/server` (table-driven httptest per D7) — the e2e layer proves only what unit tests cannot: a real restart, a real file write, and real 503 routing:

| Scenario | Steps | Asserts |
|---|---|---|
| `bootstrap_no_config` | start `walhub serve` with a clean `--data-dir` and no `walhub.toml` anywhere | boots with built-in defaults (D5): `server.listen = "0.0.0.0:8080"`, filesystem store rooted at `<data-dir>/store`, `auth.mode = "none"`, `auto_create_on_push = true`; `/healthz` + `/readyz` are 200; `/setup` serves the UI with the setup banner; the log contains the loud auth warnings; a git push against the anonymous server succeeds |
| `bootstrap_invalid_config` | write a deliberately invalid `walhub.toml` into the data dir (unknown key and a type error, one at a time), then boot | **setup-only mode**: `/setup`, `/healthz`, `/readyz` answer; every other route (a git endpoint, `/api/v1/...`) returns 503 with a pointer to `/setup`; `GET /api/v1/setup` reports file state invalid and the exact validation errors; the UI renders those errors verbatim |
| `bootstrap_setup_save_restart` | from the invalid (or absent) config: `PUT /api/v1/setup` with a corrected config, then restart the process | save returns 200 and lists which keys need a restart; `<data-dir>/walhub.toml` exists afterwards and is byte-valid TOML; after restart the server boots normally (no banner, no 503s, saved values effective — verified via `GET /api/v1/setup` effective values and a git round-trip) |
| `bootstrap_setup_api_auth` | matrix over the access rule (D6): (a) no config file; (b) config present + `auth.mode = "none"`; (c) config present + token auth; (d) case (c) with `WALHUB_SETUP_TOKEN` set | (a) and (b): `GET/PUT /api/v1/setup` open, no credentials; (c): unauthenticated → 401, with the admin token → 200; (d): `WALHUB_SETUP_TOKEN` authorizes it; `POST /api/v1/setup/test` follows the same rule and never writes the file |

---

## 6. Concurrency test kit (mandates from 13_concurrency.md)

Mandatory for every PR that touches shared state:

1. **`-race` in CI** — `make race` runs the full fast tier under the race detector. A data race is a failing build, no exceptions.
2. **Stress tests** — every lock/actor with a plausible contention story gets a stress test: N goroutines × M ops against the shared structure, asserting the invariant (not the interleaving). The sim IS the store/WAL stress test; package-level stress tests cover caches, the SSE fan-out, and the events bridge.
3. **Deadlock canary** — the test binary runs under `timeout 300` (fast tier); a suite that deadlocks fails the recipe, it does not hang CI silently. Additionally the watchdog pattern from §3.4 of the Rust spec is unit-tested: a stalled-tick detector test asserts the "async runtime stalled" warning fires when a tick is > 2.5 s late (simulated with a fake clock).
4. **Every goroutine has a shutdown path** — linters cannot check this; review does. The sim's restart machinery is the executable proof that instances shut down on context cancel.

### 6.5 Web/JS test tier (Divergence D2)

The frontend is vanilla standard ECMAScript (12_web_ui.md): no TypeScript, no framework, no bundler, zero npm dependencies — so its tests need none either. Runner: Node's built-in `node --test` (Node ≥ 20), invoked by `make test-web`, wired into `make test` and `ci`. `web/package.json` is a dev-only manifest (`{"type": "module"}`, no `dependencies`) purely so Node loads the ESM sources directly.

- **Logic/DOM split, normative.** Every testable module — the API client in the single `web/sdk/repos.js` (plain ESM; the old `.mjs` twin and the esbuild build are gone), the SSE parser, the diff parser, markdown-lite, the ~40-line reactive helpers, the setup-form logic — is a pure ES module with zero DOM access; DOM glue lives in entry modules that import them. This is what makes the tier headless: tests import by relative path (import maps are a browser-only mechanism and never appear in test imports).
- **`web/test/*.test.js`** — unit cases with `node:assert` in the Go tier's style: repos.js request shaping and error mapping, SSE event framing, diff parsing, markdown-lite, reactive-helper subscriptions, setup-form validation against the `/api/v1/setup` schema. Same assertion discipline: `if (got !== want) throw`.
- **`web/test/smoke.test.js`** — fetch-based server smoke against `WALHUB_TEST_WEB_BASE_URL` (default `http://127.0.0.1:8080`): `/` serves the app, `/setup` serves, `/api/v1/setup` returns the schema JSON, one SDK call round-trips. Server absent → the smoke cases skip, so `make test` still passes on a cold machine; CI always has a server up for them. There is no build step to test — **the shipped file is the tested file**; Vite/solid-js/marked/esbuild are deleted from the repo, not just bypassed here.

---

## 7. Makefile — every tier is a target

All dev/CI entry points are Make targets (Divergence D3); there is no justfile, no separate task runner, nothing to remember beyond the target list below. The watchdog semantics port unchanged: `T5`/`T15` resolve GNU `timeout` (coreutils), fall back to `gtimeout` (macOS), and run unwrapped with a warning if neither exists — no watchdog is worse than no tests.

```make
T5  := $(shell command -v timeout >/dev/null 2>&1 && echo "timeout 300" || command -v gtimeout >/dev/null 2>&1 && echo "gtimeout 300")
T15 := $(shell command -v timeout >/dev/null 2>&1 && echo "timeout 900" || command -v gtimeout >/dev/null 2>&1 && echo "gtimeout 900")

build: ## compile everything
	go build ./...

fmt: ## format Go sources
	gofmt -w $$(gofmt -l .)

vet: ## the Go "-D warnings" gate: gofmt clean, go vet, go build
	test -z "$$(gofmt -l .)" || (echo "gofmt needed:" && gofmt -l . && exit 1)
	go vet ./... && go build ./...
test: test-go test-web ## fast tier = Go fast tests + web tests
test-go: ## fast Go tier: hermetic, < 1 min, watchdog-wrapped
	$(T5) go test -short -count=1 ./...
test-web: ## headless JS logic tests + fetch smoke (§6.5); zero npm deps
	node --test web/test/
race: ## full fast tier under the race detector
	$(T15) go test -race -short -count=1 ./...

cover: ## coverage gate: >= 95% statements, every internal/... package (§7.1)
	@mkdir -p .cover && rm -f .cover/*.out
	@fail=0; for pkg in $(INT_PKGS); do \
	  prof=".cover/$$(echo $$pkg | tr '/' '-').out"; \
	  $(T5) go test -short -count=1 -coverprofile="$$prof" $$pkg || exit 1; \
	  go run ./internal/devtools/covergate -min 95 -profile "$$prof" -pkg "$$pkg" || fail=1; \
	done; exit $$fail

test-slow: ## soaks: 20k-ref push, 466k-ref render, ...
	$(T15) go test -run 'Slow' -count=1 ./...

sim: ## the consistency proof (fault injection); seeded
	$(T15) go test -count=1 -timeout 15m ./internal/sim/...

contract: ## ONE suite against every always-run backend (memory + filesystem)
	$(T5) go test -count=1 -run 'TestContract_(Memory|Filesystem)' ./internal/store/contract/...
contract-fs: ## filesystem backend only (D4)
	$(T5) go test -count=1 -run 'TestContract_Filesystem' ./internal/store/contract/...

contract-s3: ## env-gated: ONE suite against rustfs (starts it if not answering)
	$(MAKE) dev-store
	WALHUB_TEST_S3_ENDPOINT=http://127.0.0.1:9000 AWS_ACCESS_KEY_ID=walgit-dev AWS_SECRET_ACCESS_KEY=walgit-dev-secret \
	$(T5) go test -count=1 -run 'TestContract_S3' ./internal/store/contract/...
contract-gcs: ## env-gated: GCS JSON-API backend
	test -n "$$WALHUB_TEST_GCS_BUCKET" || (echo "set WALHUB_TEST_GCS_BUCKET"; exit 1)
	$(T5) go test -count=1 -run 'TestContract_GCS' ./internal/store/contract/...

e2e: ## real git + bootstrap lifecycle (§5.2, §5.3) against live servers; ~50 s
	$(T15) tests/e2e.sh
image: ## container image (16_packaging.md)
	docker build -t walhub:dev .

dev: ## one-box dev run: rustfs up, build, serve on :8080 with zero-config defaults
	$(MAKE) dev-store
	go build -o bin/walhub ./cmd/walhub
	./bin/walhub serve
dev-store: ## rustfs (S3-compatible) on :9000, fixed dev keys, bucket created
	docker compose up -d rustfs
dev-store-stop:
	docker compose down
clean:
	rm -rf bin .cover && find . -name '*.test' -delete
ci: ## what CI runs, in order (fast first, proof last)
	$(MAKE) vet test race cover contract sim
```

Normative notes: the old `lint` target is `vet` (same three commands); the old `dev-local` recipe became `make dev`, which needs no `--config` because a bare serve boots with the D5 zero-config defaults — export the rustfs dev creds (`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` as in `contract-s3`) before `dev` when the store backend is S3. `test-web` runs `node --test web/test/` (§6.5) and is part of `make test` — a Go-only green build is not green. S3/GCS contract targets stay env-gated; memory + filesystem (`contract`) never skip.

### 7.1 The coverage gate (Divergence D7)

CI enforces **≥ 95% statement coverage, per `internal/...` package**; `cmd/` (main glue only) is excluded from the gate. The mechanism is deliberately boring, stdlib-only:

1. `cover` runs one `go test -short -count=1 -coverprofile=<pkg>.out` per package discovered by `go list ./internal/...` — profiles are never merged; each package is judged alone. The checker (`internal/devtools/covergate`, ~40 lines of Go invoked with `go run`; an awk equivalent is acceptable) reads the raw profile (`mode: set`), **skips every block in a `// Code generated` file** (the generated protobuf sources of 02_storage_protobuf.md would otherwise dominate the denominator), computes `Σ numStmts(hit > 0) / Σ numStmts` over the remaining blocks, and exits non-zero below 95.0, naming the package, its percentage, and its five lowest-covered files so the failure is actionable without opening the profile.

**The rule behind the gate: new code lands with tests.** 95% is the CI floor, not the review bar — the expectation is **near 100%**: every new function lands with table-driven tests (table-driven `httptest` for every handler), and a diff that lowers a package below the floor fails CI before review even starts. An uncovered line needs a named reason in the PR (unreachable defensive branch, `exec`-of-real-git shim); "hard to test" is not a reason, it is a sim scenario waiting to be written.

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

- **Test framework is stdlib `testing` with `-short` as the tier switch, not build tags or `#[ignore]`.** One flag (`-short`) cleanly excludes sim/slow from `go test ./...`, matching Go convention and keeping `make test` hermetic; rationale: zero new dependencies, and `-run` composes naturally.
- **Env prefix `WALHUB_`** replaces `WALGIT_` (e.g. `WALHUB_SIM_SEED`, `WALHUB_E2E_BASE_URL`, `WALHUB_TEST_S3_ENDPOINT`, `WALHUB_TEST_GCS_BUCKET`); rationale: the binary and product are walhub; TOML keys (the compat surface, 11_config_cli.md) are unchanged. *(Refined by the 2026-08-31 divergence below: config **overrides** use `WALHUB__SECTION__KEY` with the `WALGIT__` alias per 11_config_cli.md §4.2; these single-underscore names remain the harness/process envs.)*
- **`go vet` + `gofmt -l` + `go build` replace cargo's `-D warnings`** as the lint gate; rationale: Go has no clippy; vet + format is the community-accepted equivalent, enforced via `make vet`.
- **FaultStore lives in `internal/store/fault` and the truth oracle is simply the unwrapped memory store** rather than a separate oracle object; rationale: in the Go design every link is a decorator over one shared `store.ObjectStore`, so "read bytes bypassing every link" needs no extra abstraction.
- **Panic-once faults are recovered at the instance boundary and modeled as process death (restart), keeping the wrapper's `panic()`** like the Rust original; rationale: preserves the exact injection semantics (state mid-protocol) while the test harness decides what "the process died" means.
- **The contract suite adds `TestContract_LeaseSteal`** (not present in the Rust contract, which tested leases elsewhere); rationale: in walhub the lease is pure store protocol (§4.9), so pinning it in the one suite that runs on every backend is cheaper and stronger.
- **`git.binary` config is honored by the Go git layer** (fixing §20.5's hardcoded `"git"`); rationale: it costs one flag and makes the §20.5 e2e proof (wrapper-script git) possible; behavior change is marked in 04_git.md.
- **Deterministic RNG (xorshift64*) ported byte-for-byte** so seeds are reproducible across the Rust and Go implementations; rationale: golden failure repro across ports beats a nicer RNG.
- **Budget counters are read from the FaultStore link stats** (exactly as §4.8 prescribes) rather than from an HTTP middleware counter; rationale: the sim exercises the store path directly, and the FaultStore counts precisely the requests the budget model is about.

**Divergence (2026-08-31):**

- **D3 — Make replaces just.** Every dev/CI entry point is a Make target (§7: `build`, `fmt`, `vet`, `test`, `race`, `cover`, `test-slow`, `sim`, `contract`, `contract-fs`, `contract-s3`, `contract-gcs`, `e2e`, `image`, `dev`, `dev-store`, `dev-store-stop`, `clean`, `ci`); the justfile is deleted. The old `lint` target is renamed `vet` (supersedes the `just lint` wording above; commands unchanged), and `just dev-local` becomes `make dev`. Rationale: make is ubiquitous — no extra tool to install for a first contribution — and the watchdog/watch-what-you-run semantics port one-to-one.
- **D7 — Coverage gate.** `make cover` enforces **≥ 95% statement coverage on every `internal/...` package** (`cmd/` excluded), via per-package `-coverprofile` + the `covergate` checker (§7.1), CI-gated. Review bar is near 100%: new code lands with tests — table-driven `httptest` for every handler. This is new (the Rust spec had no coverage gate); it does not supersede any prior decision.
- **D4 — Filesystem store joins the contract suite as an always-run backend.** `TestContract_Filesystem` runs wherever `TestContract_Memory` runs, no env needed; S3 and GCS stay env-gated (`WALHUB_TEST_S3_ENDPOINT`, `WALHUB_TEST_GCS_BUCKET`) and CI exercises S3 via the rustfs container. Supplements (does not supersede) the `TestContract_LeaseSteal` decision — the suite's "one suite, every backend" rule now has four backends, two of them unconditional.
- **D2 — Web/JS tests without a toolchain.** The JS tier is `node --test` over headless pure-ESM logic modules plus fetch-based smoke tests (§6.5), wired into `make test` and `ci`; zero npm dependencies, no bundler, single `web/sdk/repos.js`. New tier; no prior decision affected.
- **D5/D6 — Bootstrap and setup lifecycle is e2e-tested.** Four scenarios (§5.3: no-config defaults boot; invalid-config setup-only 503s; setup save → restart → normal; setup API auth matrix incl. `WALHUB_SETUP_TOKEN`) run in `make e2e` and CI. New; no prior decision affected.
