# 08 — Bundles (bundle-uri subsystem)

> Source: MASTER_RUST_SPEC.md §11 (bundle subsystem), §7.8 (creation primitives) · Status: normative for the walhub Go implementation.

`internal/bundle` turns calendar slots into client-compatible bundle lists: the 6-field UTC cron, as-of
content resolution, chain math, gates, building (incremental packs, `git bundle create` fulls, store-side
compose), retention/backfill, list rendering and serving, v2 advertisement, and D17 forcing. Every build
is a narrated task. Git argv is specified here, implemented by `internal/git` (04_git.md); WAL folds come
from `internal/wal` (05_wal_engine.md); `BundleList` proto + `cas_update` from `internal/store`
(02_storage_protobuf.md); compose/presign from 03_store_backends.md; unit scheduling from 10_maintenance.md.

## 8.1 The client contract (ground truth)

| Client state | git does | server work |
|---|---|---|
| Fresh clone (bundle-uri on) | newest weekly + dailies + hourlies newer than it, then fetch | one negotiation: ≤ 1 h of objects |
| Fresh `clone --filter=blob:none --depth=1 --sparse` | git STILL downloads advertised bundles first (the server cannot see the filter at bundle-uri time) — recipes pass `-c transfer.bundleURI=false` | upload-pack answers: seconds |
| Days stale on main | newest applicable cumulative incremental + fetch | ~30 min of objects |
| Days stale on a branch | same + `want branch` | the branch's own commits (rebase-safe: they sit on newer main the chain already delivered) |
| Very stale (older than the oldest kept weekly) | full + chain | as fresh clone |
| bundle-uri off, unbounded zero-have fetch of a `bundles.require` repo | refused with the fix text | none (hour-list fallback: 1 per 6 h with a WARNING) |

Everything below exists to make this table true.

## 8.2 Package layout and seams

```
git.packden.us/crueber/walhub/internal/bundle
  cron.go       6-field UTC cron: parse, validate, Next, Between
  strategy.go   Strategy config struct + validation (config check + per-repo settings)
  resolve.go    as-of resolution against the WAL; base_for_incremental chain walk
  plan.go       slot planning, verdicts, SkippedSlot settle, retention, backfill order
  build.go      one slot build: task, lease, gates, pack/bundle-create/compose, publish
  header.go     bundle header rendering (v2/v3 bytes), PACK-magic offset scan
  list.go       BundleList CAS updates, git-config text rendering, clone/catchup split
  serve.go      proxy vs presigned URI selection + signing-failure fallback
  advertise.go  v2 bundle-uri advertisement, band-2 narration, D17 guard + tracker
```

Interfaces the package consumes (implemented elsewhere; keep these seams — 14_extensibility.md):

```go
// internal/wal (05_wal_engine.md)
type WalView interface {
    RefsAsOf(ctx context.Context, repo string, at time.Time) (Refs, uint64, error) // tips + as_of_seq
    RefsAtSeq(ctx context.Context, repo string, seq uint64) (Refs, error)
    FirstStateAt(repo string) (time.Time, bool) // earlier slots are "unavailable"
}
// internal/git (04_git.md) — §7.8 primitives; exact argv is normative here
type Primitives interface {
    BundleCreate(ctx, repoDir, outPath string, refs []string) (size, packOffset int64, err error)
    PackDelta(ctx, repoDir string, wants, excludes []string, filter string, w io.Writer) error
    CountCommits(ctx, repoDir string, tips, notTips []string) (int, error) // rev-list --count
}
// internal/store (02/03): Get/CAS on bundles/list.pb, Put(Create), Delete, Compose, Presign
```

`Refs` is `[]store.Ref` (name, oid, peeled), sorted by name. `Primitives` runs every git call with
`GIT_TERMINAL_PROMPT=0` and the repo dir as GIT_DIR via `internal/git`'s configured binary.

## 8.3 Slots: the 6-field UTC cron (hand-rolled)

Every strategy has a `schedule`; each fire time is a **slot**. Slots are pure calendar facts: a backfilled
Tuesday bundle contains Tuesday's main, so chains and tokens line up no matter when the maintainer runs.

**Supported syntax (exact, normative):**

```
schedule := alias | fields
alias    := "@yearly" | "@annually" | "@monthly" | "@weekly" | "@daily" | "@midnight" | "@hourly"
fields   := field 1*sp field 1*sp field 1*sp field 1*sp field 1*sp field    (exactly 6)
field    := term ("," term)*
term     := ("*" | range) ["/" step]
range    := n | n "-" n            ; n ≤ m required
n        := decimal; bounds: sec 0-59, min 0-59, hour 0-23, dom 1-31, mon 1-12, dow 0-7 (7 ≡ 0 Sun)
```

- Semantics of a term: `*` = all values; `n` = {n}; `a-b` = {a..b}; `*/s` = all values stepped by s;
  `a-b/s` = {a, a+s, … ≤ b}; `a/s` = {a, a+s, … ≤ max} (vixie-cron expansion).
- Field values union across comma terms; duplicates collapse.
- A time fires when sec, min, hour, mon match AND the day matches, where the day rule is: if dom and dow
  are BOTH restricted (at least one term ≠ `*` in each), either may match (vixie-cron OR); if exactly one
  is restricted, it must match.
- Aliases expand to: `@yearly`/`@annually` = `0 0 0 1 1 *`; `@monthly` = `0 0 0 1 * *`;
  `@weekly` = `0 0 0 * * 0`; `@daily`/`@midnight` = `0 0 0 * * *`; `@hourly` = `0 0 * * * *`.
- REJECTED (validation error, named field): month/weekday names (`jan`, `mon`), `@every <dur>`, 5-field
  cron, ranges a > b, step 0, out-of-bounds values.

```go
type Cron struct{ /* sec,min,hour,dom,mon,dow as uint64 bitmasks */ }
func ParseSchedule(s string) (Cron, error)
func (c Cron) Next(after time.Time) (time.Time, error)            // strictly after; ErrNoFire after 5 y
func (c Cron) Between(start, end time.Time) ([]time.Time, error)  // fire times in [start,end]; cap 10 000
```

`Next` is the classic cron-advance algorithm at second resolution: truncate to the second, +1 s, then
advance the smallest non-matching field and reset all lower fields to their first matching value; UTC only
(no DST). Validation runs fail-closed in `config check` (exit 1) and at per-repo settings publish
(invalid = 400, nothing published — see 11_config_cli.md).

## 8.4 Content resolution and creationToken

- A slot's content is the repo's refs **as of that instant**: `RefsAsOf(repo, slot)` — the highest WAL seq
  whose `created_at ≤ slot` (point-in-time replay, §6.6; see 05_wal_engine.md). The fold returns the tip
  set and `as_of_seq`.
- `creationToken = slot epoch seconds` (uint64), recorded on the entry and rendered as
  `bundle.<id>.creationToken`. Token and content are functions of (slot, WAL): a deleted or corrupt bundle
  rebuilt later is identical in content and token.
- A slot earlier than `first_state_at` is **unavailable**: never built, never recorded, never backfilled.
- A cut the WAL cannot replay (predates `min_seq` with no usable checkpoint) is a **no-state** verdict
  (`as_of_seq = 0`, reason `no state as of the slot`) — see §8.7. The weekly compose of refs at the base's
  seq uses `RefsAtSeq`; when that errors and no ref has moved since, the checkpoint unit writes one on the
  spot and the compose retries once (§11.3).

## 8.5 Strategies and validation

```go
type Strategy struct {
    Name        string   // unique, e.g. "weekly"
    Kind        string   // "full" | "incremental"
    Base        string   // required for incremental: name of the base strategy
    Schedule    Cron
    Keep        int      // fulls only; fulls listed (default 2); keep on an incremental = config error
    BackfillMax int      // missing slots per pass; 0 = unlimited
    Chain       bool     // dailies default true
    Filter      string   // "" | "blob:none"
    Refs        []string // glob overrides of the global ref rule (§8.8)
    MinCommits  int      // default: bundles.min_commits (25)
}
```

Defaults (`bundles.strategy[]` absent) = weekly full + daily + hourly:

| name | kind | schedule | extras |
|---|---|---|---|
| weekly | full | `@weekly` | keep 2, backfill_max 1 |
| daily | incremental | `@daily` | base weekly, chain true, backfill_max 7 |
| hourly | incremental | `@hourly` | base daily, chain false, backfill_max 48 |

Validation (fail-closed, both host config and per-repo settings): incremental requires `base` naming an
existing strategy; the base graph must be acyclic; **a whole chain shares one `filter`** (every strategy
reachable through base links must carry the same filter, else error); `keep` on an incremental is an error;
`min_commits` ≥ 0.

## 8.6 Chain math: `base_for_incremental`

The single function that decides what an incremental slot is cut on — the one place that knows both rules:

```
baseFor(s, slot, entries):
  ownPrev   := newest entry of s with entry.slot < slot          # skipped slots have no entry
  base      := newest entry of s.Base with entry.slot <= slot    # walk up the chain when empty:
            #   if s.Base has no such entry, repeat with s.Base's base (nearest ancestor that does)
  if s.Chain && ownPrev != nil && (base == nil || slot == base.slot || ownPrev.slot > base.slot):
      return ownPrev          # cut on THIS strategy's previous bundle
  return base                 # may be nil → slot is "blocked" (waiting for the first base bundle)
```

- Plain rule: an incremental is cut on the newest bundle of its BASE strategy (daily on latest weekly,
  hourly on latest daily) — never on its own kind's previous bundle, except when `chain`. Chain continues
  while the own-previous bundle is **strictly newer** than the newest base bundle.
- **The tie rule:** at the weekly/daily tie (same fire instant — e.g. both `@weekly` and `@daily` fire
  Sunday 00:00:00 UTC, hence same tips by deterministic replay) the slot being cut has
  `slot == base.slot`, so the chain continues through its own link. Content-equivalent either way: the
  base's tips equal the own-previous bundle's tips at the tie instant.

Worked week (W = weekly bundle of Sunday 00:00, token = Sunday epoch):

| daily slot | ownPrev | newest base | rule applied | cut on |
|---|---|---|---|---|
| Sun (tie) | Sat daily | W (same slot) | tie → own link | Sat daily |
| Mon | Sun daily (Sun token) | W (Sun token) | ownPrev NOT strictly newer → base | W |
| Tue … Sat | previous daily | W | ownPrev.slot > W.slot | previous daily |
| next Sun (tie) | Sat | W′ | tie → own link | Sat |

Consequences that are all intended: the daily chain is unbroken in content for catch-up clients (Monday's
prerequisites = W's tips = Sunday daily's tips, which the client already has); the Sunday daily's token
equals the weekly's token, so a fresh clone ("newest weekly + bundles **newer** than it") skips it as
redundant; after an idle stretch the next built daily re-bases on the weekly and carries one multi-day
delta. Hourlies against dailies behave identically at the midnight tie.

## 8.7 Gates and closed slots

Gates apply to **incrementals only; fulls are never gated**. Evaluation order per slot:

1. **No-state** — `RefsAsOf` not replayable → verdict (below), `as_of_seq = 0`.
2. **Unchanged gate** — the tip set (name+oid pairs) equals the newest `BundleEntry` of the same strategy →
   `skipped (unchanged since <id>)`. Pure in-memory comparison; an idle night must not cut 24 identical
   bundles.
3. **`bundles.min_commits`** (default 25, per-strategy override) — commits since the base measured by
   `git rev-list --count <tip oids…> --not <base-tip oids…>` over the local commit-graph (commits/trees are
   local on every maintaining host). Below the gate the verdict is `too-small: N commits (min M)` and the
   next slot builds on the same base (nothing lost — base resolution naturally re-picks the same base
   because the skipped slot produced no entry). Optional `bundles.min_bytes` is parsed but not enforced.

**Closed slots are final.** A slot whose as-of instant is more than **120 s** in the past (the close
grace), once measured too-small / no-state / unchanged, is recorded in `BundleList.skipped`:

```protobuf
message SkippedSlot { string strategy; uint64 slot; string base_id;
                      uint64 as_of_seq;  // 0 = no state
                      string reason;     // "too-small: N commits (min M)" | "no state as of the slot"
                                         // | "unchanged since <bundle-id>"
                      Timestamp at; }
```

Keyed final per (strategy, slot, base_id); every host and every restart skips such slots in O(1). The open
(current) slot — as-of within 120 s of now — is never recorded; it is re-measured each pass. A **new base
bundle for the slot re-opens it**: the settle step drops records whose `base_id` no longer matches the
slot's current base resolution, whose slot now has an entry, or whose slot left the plan window (§8.10).

Verdicts are **batched**: one pass collects all verdicts and commits them in a single CAS (§8.11) — the
pass budget is `list GET → ≤ 1 retention CAS → ≤ 1 verdict-batch CAS` (≤ 3 requests, §4.8). A lost CAS
loses only this pass's verdicts; they are re-measured idempotently.

## 8.8 Refs in bundles

- `bundles.main_only = true` (default) → `HEAD` + `refs/heads/main`. Deliberate: 466 k refs in every
  incremental would bloat everything; branch deltas ride on main.
- `main_only = false` → `refs/heads/*`, `refs/tags/*`, `HEAD`; `bundles.extra_refs` globs added;
  a strategy's own `refs = [...]` overrides all of the above.
- Glob matching is `path.Match` semantics on the full ref name; unmatched globs are not an error.
- The builder resolves each selected ref's tip as of the slot; a ref whose tip cannot be resolved is
  **skipped with a notice** (task narration), never a repo failure.

## 8.9 Building

### 8.9.1 Every build is a task, and takes the strategy lease

- Task kind `bundle`, params `{strategy, slot}`; narrated (notices + progress bars) per 05_wal_engine.md
  §tasks; join semantics per (repo, kind). `POST …/ops/bundle?strategy=&slot=` and `walhub bundle run`
  both go through it.
- Cross-instance exclusion: lease `leases/bundle-<strategy>.pb` (repo-scoped), acquired before any work.
  TTL 30 min with a 5 min heartbeat loop for the duration of the build. Heartbeat 412 (`LeaseLost`) → the
  build context is cancelled (killing the git subprocess via `exec.CommandContext`), the task records
  failure "lease lost; another host took over", the slot stays missing and is retried next pass.
- Slot content is deterministic, so losing a race wastes work but never correctness; the second builder's
  `Create` upload of the same content key simply fails harmlessly.

### 8.9.2 Incremental (needs packs local — the assigned maintainer)

Header (own render, §8.9.4 header rules, with prerequisites) ∘ `git pack-objects` stdout:

```
git pack-objects --revs --delta-base-offset --stdout -q [--filter=blob:none]
stdin:  <tip-oid>\n  …            (as-of tips of this slot)
       ^<base-tip-oid>\n …        (prerequisites = the base bundle's tips)
```

**Self-contained, never `--thin`.** Measured why: thin deltas against a 32 GB base cost the client 48 s +
420 MB of appended bases; self-contained is +39 % bytes but −35 % client index time. Static bytes are the
cheap resource. The builder streams: render header → tee `pack-objects` stdout into (temp file, sha1,
size) → publish (§8.9.6). Blobless strategies add `--filter=blob:none` (self-contained filtered pack).

### 8.9.3 Weekly full when the repo fits the instance

`git bundle create <tmpfile> --stdin` fed the selected ref lines (`HEAD`, `refs/heads/main`, …); blocking;
returns size + the byte offset of the `PACK` magic (scanned with an 8-byte overlap window — the header/pack
split used elsewhere for composition). Upload the file (§8.9.6).

### 8.9.4 Header rendering (exact bytes) and the big-repo weekly compose

Header rendering needs no git. Byte-exact:

```
v2: "# v2 git bundle\n"  + ("-<oid> \n" per prerequisite, note trailing space)
                        + ("<oid> <name>\n" per ref, HEAD first then refs sorted by name)  + "\n"
v3: "# v3 git bundle\n"  + "@object-format=<sha1|sha256>\n"
                        + "@filter=blob:none\n"          (filtered family only)
                        + prerequisites + refs + "\n"    (as above)
```

v2 is used for sha1 unfiltered; v3 whenever the repo is sha256 or the strategy carries a filter. Composed
fulls carry no prerequisites.

**Weekly full (big repo) — the Sunday unit.** On an ssd host (`maintenance.disk = "ssd"`, pack set fits
disk mode) the missing weekly slot first yields the **BaseRebuild** when due (§13.3; see 10_maintenance.md)
→ `compact base=1 force=1`, published as a tier-2 COMPACT entry. The slot itself then **composes**
header ∘ base pack — `ComposeFullFromBase`:

```go
refs    := wal.RefsAtSeq(repo, baseSeq)          // refs at the base pack's seq (checkpoint written on
                                                 // the spot when none exists and no ref moved since)
header  := renderHeader(v2-or-v3, prereqs=nil, refs)                  // a few hundred bytes
sum     := sha1(header ∘ localBasePackBytes)     // streamed from the LOCAL base pack file
scratch := "wal/_compose/<o>/<r>/<strategy>-<slotUTC>.header"
store.Put(ctx, scratch, header, Create)
store.Compose(ctx, "bundles/<s>/<slotUTC>-<sum>.bundle", scratch, basePackKey)  // zero bucket bytes
store.Delete(ctx, scratch)                                            // best-effort
```

- Store-side compose: GCS native `objects.compose` (2 sources); S3 multipart where every non-final part
  must be ≥ 5 MiB → part 1 = UploadPart(header ∘ first (5 MiB − |header|) bytes read from the **local**
  base pack), part 2 = UploadPartCopy(basePackKey, range from that offset to end) — still zero bytes from
  the bucket through the host. Base pack < 5 MiB → single PUT of the locally concatenated file. (The
  `Compose` contract lives in 03_store_backends.md.)
- `sum` = sha1(header ∘ base pack bytes) is computable only because the composing host has the base pack
  locally (it just rebuilt it, or Serve linked/mounted it). No local base pack file → the unit defers to a
  host that has one. ~2 s for 32 GB; a full slot of a repo WITH a base is never a `pack-objects` of its
  history.
- The entry records `seq = baseSeq`, `tips = refs at baseSeq`, `size = |header| + |base pack|`.

### 8.9.5 Blobless family (`filter = "blob:none"`)

A full chain of its own on the same slots: the blobless weekly = header (v3 `@filter=blob:none`, refs at
the base's seq) ∘ the **history pack** of the base (commits + trees — exactly `--filter=blob:none` of the
refs at the base seq; the BaseRebuild builds it anyway; Kind=HISTORY, `derived_from=<base>`, see
04_git.md). Blobless incrementals pack with `--filter=blob:none` (§8.9.2), self-contained. All gates apply
per strategy. The compose is the §8.9.4 call with the history pack key as the base pack source.

### 8.9.6 Publish

1. Temp file under `cache.dir` while hashing (streaming sha1 + size).
2. `put_file_parallel` (03_store_backends.md; single PUT ≤ 128 MiB) → key
   `bundles/<strategy>/<slotUTC>-<sha1-of-content>.bundle`, slot text `20060102T150405Z`. Mode **Create**
   (immutable), store version = the sha1 → HTTP ETag.
3. `bundles/list.pb` updated by CAS, **8 retries** (§8.11): upsert the entry
   `{id, key, strategy, kind, creation_token = slot, seq = as_of_seq, size, base_id, created_at, version,
   tips, slot, filter}`.
4. Retention + object deletes per §8.10.

### Concurrency

Reference 13_concurrency.md for the general playbook; the bundle-specific hazards:

- **Build single-flight per (repo, strategy).** Hazards: two hosts duplicating a 32 GB build; two
  goroutines on one host racing the same slot. Avoidance: in-process singleflight keyed
  `(repo, strategy)` (join → reuse the running build's outcome) + the cross-host lease (§8.9.1). The task
  table's (repo, `bundle`) join additionally serializes different strategies of one repo — accepted, the
  maintainer runs one unit per pass anyway.
- **Lease heartbeat ownership.** The build goroutine owns the lease and spawns the heartbeat goroutine;
  both die by the build's context; heartbeat 412 cancels the build context (git killed via
  `exec.CommandContext`); nobody else ever writes the lease object.
- **The list CAS update loop (8 retries).** Hazard: concurrent publishers (bundle build, import, `bundle
  rm`, verdict batch) racing `bundles/list.pb`. Avoidance: every mutation through `CASUpdate` (read-modify-
  CAS; 412 → re-read and retry immediately, counted to 8; Retryable → jittered 5→100 ms backoff; then
  `RetriesExhausted` reported, retried next pass). Object deletes happen only after the committing CAS,
  filtered by old-vs-new key sets, so a lost race can never delete a re-added object.
- **Never block request goroutines.** List rendering, advertisement, and D17 checks read in-memory/cached
  state (render cache `cache.bundle_list_entries = 128`); builds, retention, and backfill run only in
  maintainer/task goroutines. The D17 tracker is a mutex-guarded map with lazy expiry + a 10 m sweep
  goroutine on the scheduler's context; no unbounded buffering anywhere; the pack stream is `io.Copy` from
  the subprocess (stdin closed, stdout drained, then `Wait`).

## 8.10 Retention and backfill

- **Retention, applied every pass** (not only on publish — never an orphan): keep = fulls listed
  (`keep`, default 2) + **the chain under every kept full** (walk `base_id` links to the root full) +
  non-chain incrementals: the 2 newest per strategy whose base is kept. Entries failing this are removed
  from the list; their objects are deleted **after** the retention CAS commits, only for keys present in
  the old list and absent from the committed new list.
- **Plan windows** (what can be missing at all): fulls — the `keep` newest fire slots ≤ now; chain
  incrementals — fire slots ≥ the newest base-strategy bundle's slot (includes the tie slot); non-chain —
  the 2 newest fire slots ≤ now. Slots before `first_state_at` are unavailable.
- **Backfill:** missing slots built oldest-first, ≤ `backfill_max` per strategy per pass (0 = unlimited;
  weekly default 1, hourly default 48). An outage leaves no holes; a deleted/corrupt bundle is "missing"
  and rebuilt identically. Strategy priority = config order; within a strategy the oldest missing slot
  first. Each pass runs at most one bundle unit per strategy up to its budget, then re-plans
  (10_maintenance.md).

## 8.11 Lists: CAS, rendering, clone vs catchup, serving

`bundles/list.pb` (`BundleList{mode, heuristic, bundles[], updated_at, skipped[]}`) is the one CAS'd
advertisement state. All mutations go through the generic CAS helper (02_storage_protobuf.md):

```go
err := store.CASUpdate(ctx, key, 8, func(l *store.BundleList) (*store.BundleList, error) {
    // mutate; return CASAbort to bail without writing
}) // 412 → re-read + immediate retry (counted); Retryable → backoff 5→100 ms, jittered
```

Mutators: publish-upsert, retention prune, verdict batch, `bundle rm`, import supersede (same-strategy +
dependents). Exhausted retries → the pass reports and retries next pass; nothing is lost.

**List rendering** (git-config text; `mode = all`, `heuristic = creationToken`; entries ascending
creationToken; orphaned incrementals whose base entry is gone are dropped):

```ini
[bundle]
    version = 1
    mode = all          # BundleList.mode
    heuristic = creationToken
[bundle "<id>"]
    uri = https://git.example.com/<o>/<r>/bundles/<strategy>/<file>.bundle   # or presigned
    creationToken = <slot epoch>
    filter = blob:none  # filtered family only
```

- `id` = `<strategy>/<slotRFC3339Z>` (stable across rebuilds; families never mix on one list).
- **`bundles/list` = kept fulls + the chain (for clones); `bundles/catchup` = the same without fulls.**
  Why: with `heuristic = creationToken` git's token walk downloads every full newer than the stored token —
  a fetching client would pull the 32 GB new weekly on the first fetch after it. Every recipe therefore
  records the **catchup** URL in `fetch.bundleURI`; only clones see the fulls.
- `?filter=blob:none` renders the blobless family (entries with `filter = blob:none`); any other filter
  value → 400.

**Serving:** `bundles.serve_via = "proxy"` → URIs are `…/bundles/<strategy>/<file>` through the static
object contract (ETag/Range/immutable; 06_server_http.md); `"signed_url"` → presigned store URIs (TTL
`bundles.signed_url_ttl`, 1 h) for repos in `bundles.signed_url_for`; on any signing error fall back to
proxy URIs and warn once per repo (in-memory flag). List responses are `no-cache` (presigned URIs expire).

## 8.12 v2 advertisement and band-2 narration

- Capability `bundle-uri` in the v2 capability advertisement when `bundles.advertise` (default true) and
  the repo has bundles enabled.
- `command=bundle-uri` (no arguments accepted — any argument → ERR pkt + flush) answers the **clone list
  inline as key=value pkt-lines**, flush-terminated:

```
bundle.version=1
bundle.mode=all
bundle.heuristic=creationToken
bundle.<id>.uri=https://git.example.com/<o>/<r>/bundles/<strategy>/<file>.bundle
bundle.<id>.creationToken=1793000000
```

- `bundles.advertise_filtered` (default false) appends the filtered family's entries with
  `bundle.<id>.filter = blob:none` lines to the same response — only for a patched git that matches
  `bundle.<id>.filter` (§8.13 hazard).
- **Band-2 narration:** a v2 fetch response under active advertisement echoes each advertised bundle
  before the packfile section, one line per plain-list entry ascending token:
  `* bundle-uri: <path> (<human size>, <kind>, seq <seq>, token <token>)`, e.g.
  `* bundle-uri: /acme/monorepo/bundles/weekly/20260830T000000Z-3f2a….bundle (32.3 GB, full, seq 1, token 1793000000)`
  (client renders the `remote:` prefix; size one decimal, 1000-based B/KB/MB/GB/TB). Sideband framing is
  04_git.md's; the line content is assembled here from `BundleList`.

## 8.13 D17 forcing (`bundles.require`) and the stock-git hazard

**D17 forcing** (in the fetch path): an **unbounded zero-have** fetch (no haves, no `deepen*`, no `filter`)
of a repo listed in `bundles.require` is refused with the fix in the error (pkt ERR, exact text):

```
unbounded fetch refused: this repo requires bundle-uri; use bundle-uri per the setup recipe, or pass -c transfer.bundleURI=false for shallow/filtered fetches
```

Bounded zero-have fetches (CI `--depth`/`--filter`) and all fetches with haves proceed.

**One-shot fallback:** a principal that fetched the repo's `bundles/list` within the last hour
demonstrably tried bundle-uri (git never retries a failed bundle download and then falls back) → it gets
**one upload-pack full clone per 6 h** with a loud band-2 WARNING; the next one and everyone else is
refused. In-memory per instance:

```go
type d17Key struct{ repo, principal string }
type d17Entry struct{ listFetch, fallback time.Time }
// guard decision for an unbounded zero-have fetch:
//   allow := now.Sub(e.listFetch) <= 1h && (e.fallback.IsZero() || now.Sub(e.fallback) >= 6h)
//   on allow: e.fallback = now; emit band-2:
//   "warning: full fetch allowed without bundle-uri (fallback, once per 6 h); switch to the bundle-uri recipe"
```

Expiry: lazy on access + a sweep goroutine every 10 m dropping entries older than 6 h; cap 100 000 keys
with drop-oldest. The list GET handler records `(repo, principal)` (06_server_http.md).

**Two lists, never mixed — the stock-git hazard:** stock git never consults `bundle.<id>.filter`, and a
blobless clone STILL downloads advertised bundles first (the server cannot see the client filter at
bundle-uri time). Putting both families on one list makes a blobless clone download the 32 GB unfiltered
full — the filter is defeated precisely for the client that asked for it. Hence the unfiltered chain lives
only on `bundles/list`, the blobless family only on `bundles/list?filter=blob:none`, and
`advertise_filtered` stays false until clients carry the patch (§20 of the Rust spec).

## 8.14 Plan states, CLI ops, metrics

Plan states (gauge `walgit_bundle_plan_slots{repo,strategy,state}`, set after each maintainer pass):

| state | meaning |
|---|---|
| built | live entry exists |
| pending | future slot, or the open slot not yet due |
| missing | in window, no entry, no closed verdict — backfill candidate |
| blocked | incremental with no resolvable base bundle (e.g. before the first weekly) |
| too-small | closed verdict `too-small: …` |
| skipped | closed verdict unchanged / no-state |
| unavailable | slot before `first_state_at` |
| wrong-host | pack set does not fit this host (or base work on a tmpfs host) — never attempted here |

CLI (`cmd/walhub`, dispatch per 11_config_cli.md):

```
walhub bundle run [--repo ID] [--strategy NAME]   # run due builds now (through the task+lease path)
walhub bundle plan acme/monorepo                  # capacityless slot table + maintainers
walhub bundle compose acme/monorepo [--strategy weekly]
walhub bundle rm acme/monorepo <ids…>             # CAS list update + object deletes
```

`bundle plan` prints per strategy the slot table (`slot  kind  status  detail` — detail = bundle id or
verdict reason), the `next:` upcoming units per maintainer heartbeat (`maintain/<host>.pb`), and the
maintainers assigned by `[placement] maintain`. `bundle compose` is the manual §8.9.4 form; without a
resolvable base it errors, pointing at `walhub compact --base`. `bundle rm` removes entries by id and
deletes their objects via the CAS list update.

## 8.15 Operator quick reference

```toml
[bundles]
enabled = true
main_only = true
serve_via = "proxy"
advertise = true
min_commits = 25
require = ["acme/monorepo"]

[[bundles.strategy]]
name = "weekly"
kind = "full"
schedule = "@weekly"
keep = 2
backfill_max = 1

[[bundles.strategy]]
name = "daily"
kind = "incremental"
base = "weekly"
schedule = "@daily"
chain = true
backfill_max = 7

[[bundles.strategy]]
name = "hourly"
kind = "incremental"
base = "daily"
schedule = "@hourly"
backfill_max = 48
```

```sh
curl -s https://git.example.com/acme/monorepo.git/bundles/list
curl -s 'https://git.example.com/acme/monorepo.git/bundles/catchup?filter=blob:none'

# fresh clone (token host): bundle-uri + catch-up fetch recipe
git -c http.extraHeader="Authorization: Bearer $WALGIT_TOKEN" -c transfer.bundleURI=true \
    -c fetch.bundleURI=https://git.example.com/acme/monorepo.git/bundles/catchup \
    clone https://git.example.com/acme/monorepo.git

# developer blobless shape (MUST be --sparse/--no-checkout: a plain checkout of a monorepo lazily
# fetches ~1.5 M blobs in one promisor fetch)
git clone --filter=blob:none --sparse \
    --bundle-uri='https://git.example.com/acme/monorepo.git/bundles/list?filter=blob:none' \
    -c fetch.bundleURI='https://git.example.com/acme/monorepo.git/bundles/catchup?filter=blob:none' \
    https://git.example.com/acme/monorepo.git
```

Setup hygiene (from the installer, §8.9 of the Rust spec): unset stale
`http.https://<host>/.extraHeader`; set `transfer.bundleURI true`; **unset** global `fetch.bundleURI`
(invalid globally; per-clone only); set `fetch.uriProtocols https`.

## Decisions & deviations from the Rust design

- Cron is a hand-rolled parser in `internal/bundle` with the exact syntax fixed in §8.3 (6-field, lists/
  ranges/steps incl. `a/s`, 5 aliases, vixie dom/dow OR rule, names and `@every` rejected) — stdlib has no
  cron and deterministic calendar slots are required for reproducible backfill.
- `backfill_max` default for the (unspecified) daily strategy is 7 — a week of dailies per pass, between
  the spec'd weekly 1 and hourly 48.
- Unchanged verdicts are recorded as closed `SkippedSlot`s like too-small/no-state — uniform O(1) skipping;
  the spec names only too-small/no-state but the idle-night goal implies it.
- The unchanged gate compares against the newest `BundleEntry` of the strategy regardless of `base_id` —
  required so idle periods collapse across chain re-base points; the spec's "on the same base" phrasing
  only fits the non-chain shape.
- Plan windows are pinned down (fulls: `keep` newest slots; chain: ≥ newest base slot; non-chain: 2 newest
  slots) — derived from the retention rules and required to bound backfill work.
- v3 headers always carry `@object-format=<algo>` (sha1 included), ordered before `@filter` — sha256
  correctness requires it and git accepts both orders and sha1 object-format lines.
- Composed-bundle checksum = sha1(header ∘ **local** base pack bytes) streamed on the composing host — the
  only way to honor both the key layout (`<sha1-of-content>`) and zero bucket bytes through the host.
- Compose uploads the header to a scratch object `wal/_compose/…` (deleted best-effort) and the S3 part-1
  trick (header ∘ first 5 MiB from the local pack) is specified — S3 multipart requires non-final parts
  ≥ 5 MiB and the header is tiny.
- Bundle leases get coord semantics: 2 s skew tolerance and incrementing epoch (§20.7's legacy quirk is not
  copied); the protobuf format is unchanged so buckets stay compatible.
- Bundle lease TTL is fixed at 30 min with 5 min heartbeats — no config key exists for it.
- Task join stays keyed (repo, `bundle`) per §6.8; per-strategy exclusion is enforced by singleflight +
  lease; different-strategy same-repo builds serialize (the maintainer runs one unit per pass anyway).
- The D17 refusal text, fallback WARNING line, and band-2 narration format are spelled exactly (the spec
  quotes only fragments); narration prints the URI path.
- D17 tracker: per-instance memory, 6 h TTL (lazy + 10 m sweep), cap 100 000 entries with drop-oldest —
  a restart resets it, granting one fresh fallback per principal; accepted.
- Entry `id` format is `<strategy>/<slotRFC3339Z>` and the object key's slot text is `20060102T150405Z` —
  keys are discovered via `BundleList` entries, never parsed, so interop is unaffected.
- Orphaned incrementals (base entry no longer listed) are dropped from rendering and pruned with retention.
- All git invocations go through `internal/git`'s configured binary — fixing §20.5 (`git.binary` not
  plumbed) at the seam rather than per-callsite.
