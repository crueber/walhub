# Performance evidence

Measured claims about walhub's behavior under load — not estimates. Every entry
in this document was produced by a harness committed to the repo (reproduction
command included), on the real code path, and states the backend it ran on.
Entries are added when a feature's scale behavior needs pinning (explicitly
requested or when a review questions a hot path).

## Method standards (what qualifies as an entry)

- **Measured, not modeled.** The harness runs the real code path — no mocks for
  the layer under test. Fixtures may be synthetic; the measurement is not.
- **Named backend and hardware.** Absolute numbers are environment-bound
  (this workstation is a QEMU VM); the *shape* claims — flat vs linear vs
  quadratic, per-op vs per-request — are the durable part of every entry.
- **Reproducible.** Each entry names its harness (package + env var + command)
  and the harness lives in `internal/devtools/` or the package's own tests.
- **Both sizes.** Entries measure at a "normal" population and an order(s) of
  magnitude beyond, so flat-vs-growing is visible in the table itself.
- **Hot paths vs convenience paths are called out.** A UI page taking 100×
  longer than an auth lookup is a different kind of fact than an auth lookup
  degrading.

## Index

| # | Date | Area | Question | Verdict |
|---|---|---|---|---|
| E1 | 2026-09-03 | SSH key registry (`internal/server/sshkeys.go`) | 10k vs 1M registered keys: what happens to auth, the keys page, and writes? | Auth flat (O(1) per handshake); keys page linear in per-user keys; writes constant. Scale-safe to 1M+. |
| E2 | 2026-09-04 | Identity authz read path (`internal/identity`) | What does one role resolution cost, and can it explode with team count? | O(1) per request: 1 conditional access.json GET + 1 per referenced team; steady state transfers zero bodies (304-class). Cannot explode: bounded binding list, exact-key probes, no LIST. |
| E3 | 2026-09-04 | Issue list/read path (`internal/issues`) | Is the issue list index-first O(1), what does a thread read cost, and is the LIST fallback bounded? | List flat (2 GETs, 0 LIST at any population when the index is complete); thread read linear in thread length; fallback bounded (1 LIST + ≤2000 header GETs, page ≤100). Cannot explode: every scan is prefix-bounded and paged. |
| E4 | 2026-09-04 | PR mergeability + merge task (`internal/pulls`) | Does mergeability stay bounded as open PRs accumulate, what does a merge cost, and is git-pool usage bounded? | Recompute flat (3 GETs + 1 PUT, 0 LIST, 6 git calls at 1 and 50 open PRs); merge bounded (12 GETs + 5 PUTs, 0 LIST, 11 git calls, peak concurrency 1); 16 concurrent readers collapse to 1 merge-tree. Cannot explode: no LIST on any path, page-bounded sidecars, pool-gated git. |
| E5 | 2026-09-04 | Review summary + required-reviews gate (`internal/review`) | What does a review-summary recompute cost, what does the merge-time gate scan cost, and why can't either explode? | Recompute linear in review/thread count (9 GETs + 1 PUT + 2 LISTs at 5 reviews/2 threads; 122 + 1 + 2 at 100/20); gate scan linear in review count (8 + 0 + 1 at 5; 103 + 0 + 1 at 100), own deadline, never trusts the summary. Cannot explode: scans are prefix-bounded to one PR's low-volume collaboration subtree, no git on any path, no cross-PR fan-out. |
| E6 | 2026-09-04 | Check report path + combined view + required-checks gate (`internal/checks`) | What does one CI report cost, what does the combined view cost, and why can't either explode as context count grows? | First report 2 GETs + 2 PUTs + 1 LIST; re-report 4 GETs + 3 PUTs + 1 LIST (1 context); combined 1 LIST + 1 GET per context, 0 PUTs; gate = policy GET + 1 LIST + 1 GET per context under a 15 s deadline. Cannot explode: every scan is prefix-bounded to one sha, reports are CI-rate, no git on any read path, index writes are best-effort. |
| E7 | 2026-09-04 | Notification fan-out + webhook delivery loop (`internal/notify`) | What is the write amplification of one emission, what does the unread dedup save on replay, and what does a delivery pass cost? | One emission = 2 GETs + 2 PUTs per recipient + 3 GETs + 2 PUTs fixed (thread, watchers, seq reservation, activity); deduped replay writes flat (2 PUTs, zero notification/index writes at any recipient count; probes are 3+n GETs); delivery pass = 1 LIST + ~3 GETs + 1 PUT per event + 1 cursor CAS, idle pass writes nothing. Cannot explode: 100-recipient sync cap with task fallback, 5 s budget, per-hook cursors. |

---

## E1 — SSH key registry at 10k and 1M keys (2026-09-03)

**Area:** SSH git transport, key auth (`internal/server/sshkeys.go`,
`internal/sshd`). Spec: `17_ssh.md` §3/§5.

**Question:** the registry stores one object per key in two families
(`ssh-keys/k/<fp>` for auth lookup, `ssh-keys/u/<principal>/<fp>` for the UI
list). What happens to (a) SSH auth, (b) the keys page, and (c) writes when the
host accumulates 10,000 keys — and 1,000,000?

**Method.** Harness: `internal/devtools/keybench` (`go test -tags evidence
./internal/devtools/keybench/ -run TestSSHKeyRegistryScale -v -timeout 30m`;
the `evidence` build tag is what keeps the harness out of CI and `go vet`).
Real ed25519 keypairs (1 per key — distinct fingerprints), registered through
the real `Add` path over the **memory store**, keys spread over 100 principals
(the population shape: many users, a handful of keys each — the benchmark's
`user0` deliberately holds n/100 keys to also probe the pathological end).
Measured: 1000 auth lookups (mean/max), one keys-page list, one add. Memory
store isolates the registry's algorithmic shape from network RTT; see
"over the network" below for the S3/GCS constant.

**Results.**

| path | 10,000 keys | 1,000,000 keys | shape |
|---|---|---|---|
| setup (n adds, 2 PUTs each) | 0.27s (~27µs/add) | 29.9s (~30µs/add) | constant per add |
| **SSH auth lookup** (1 GET by fingerprint) | mean **5.9µs**, max 221µs | mean **5.6µs**, max 29µs | **flat** — O(1), no LIST |
| keys-page list (one principal) | 5.1ms for 100 keys | 1.51s for 10,000 keys | linear in **per-principal** key count |
| store footprint | ~20k objects | ~2M objects | grows with total keys |

**Analysis.**

- **Auth is flat because the fingerprint IS the storage key.** `LookupBy-
  Fingerprint` is one exact-key GET + one in-memory rights resolution
  (`PrincipalForName`). No LIST, no scan, no cache to warm: 5.9µs at 10k keys,
  5.6µs at 1M. An SSH handshake runs one lookup, and handshakes are rare
  relative to HTTP git traffic — auth cost per handshake is invariant at any
  population size. This is the property that matters most, and it holds.
- **The keys page is linear in per-user keys, not total keys.** `List` walks one
  principal's prefix and decodes each entry (the listing entry carries the full
  record; LIST returns only metadata, so one GET per entry). The bound is
  keys-per-principal — 5.1ms for a 100-key principal, 1.51s for a 10,000-key
  principal, at every total population. Real distributions (1–20 keys/user)
  sit at the low end. If a deployment ever needed more, the fix is mechanical:
  make the listing entry self-sufficient so LIST alone answers (index format
  change, not a redesign) — noted as the scale-out lever, not built.
- **Writes are constant** — 2 PUTs per add (record PutCreate + index
  overwrite), fingerprint uniqueness enforced by the store's PutCreate
  (duplicate → 409 at the API). ~30µs/add on the memory store across the whole
  1M-key run.

**Over the network (S3/GCS).** The memory-store numbers isolate the shape;
every auth lookup then costs one GET (~60–80ms typical per-object latency) and
the keys page costs (1 + K) GETs. Flat in total keys, per the table — at 1M
keys an SSH handshake is still exactly one GET.

**Operational notes at 1M keys** (what grows — storage and housekeeping, not
latency):

- ~2M store objects, ~1 GB of small objects. Trivial for S3/GCS. On the
  **filesystem backend** that is 2M inodes across `ssh-keys/k/` and the
  per-principal `u/` dirs — ext4 lookups stay O(log n), but inode budget and
  backup size feel it.
- No key expiry: keys are identity and accumulate (same as every git host).
- One rollback window: if the listing-entry PUT fails after the record PUT
  commits, an orphan k-doc survives — unlistable, and it blocks that
  fingerprint's re-registration until removed. One store call wide; at any
  scale this is a rounding-error event, but it is the one thing a cleanup tool
  would have to do.
- The store-backed registry is only half the key story; the server's SSH host
  key is machine-local (data dir, not the bucket) — see `17_ssh.md` §3.

**Verdict.** Scale-safe where it matters: auth is O(1) per handshake at every
population size; the UI path is bounded by per-user key counts; writes are
constant. The 10k → 1M jump costs storage footprint and housekeeping, not
user-visible latency. No redesign required; the UI-list self-sufficiency
change is the documented lever if keys-per-principal ever grows large.

---

## E2 — Identity authz read path: conditional GET per request + LRU (2026-09-04)

**Area:** identity + permissions, role resolution (`internal/identity` —
`access.go` LRU, `gate.go` CheckRead/Resolve). Spec: `features/01` §4/§4.1.

**Question:** every read request (git `info/refs`, upload-pack, LFS reads,
repo-scoped API reads) resolves the caller's role. What does one resolution
cost in store round trips, what does steady state transfer, and can the
query count explode as teams/bindings grow?

**Method.** Harness: `TestAuthzReadPath` in
`internal/identity/evidence_test.go` (`go test ./internal/identity/ -run
TestAuthzReadPath -v`; the access-LRU hit/miss counters log per run). Real
`Service.Resolve`/`CheckRead` over the **memory store** wrapped in a
counting decorator (total GETs, conditional vs full, bodies transferred vs
304-class `NotModified`). Memory store isolates the resolution algorithm's
shape from network RTT; the backend contract (`IfNoneMatch` → `NotModified`,
pinned by the store contract suite on every backend) makes the shape
backend-independent — over the network each probe is one sub-second
control-plane GET and each hit transfers no body. Fixture: one private repo
with 3 bindings (direct + 2 team refs), two teams, one org.

**Results.**

| path | store GETs | bodies transferred | shape |
|---|---|---|---|
| cold resolve (nothing cached) | 3 (access.json + 2 teams) | 3 | O(bindings): 1 + teams referenced |
| warm resolve ×50 (steady state) | 150 (3/request) | **0** | flat — every probe 304-class |
| after one team edit | 3 | 1 (the changed team) | lazy invalidation, then flat again |
| anonymous public read | 1 (access.json only) | 1 cold / 0 warm | no team probes for anon |

**Analysis.**

- **O(1) per request, and it cannot explode.** A resolution is exactly one
  conditional GET of `access.json` plus one conditional GET per *referenced*
  team — bounded by the binding list length, which is itself bounded
  (validated subjects, sorted array, human-rate writes; a repo with ten
  thousand bindings would be an operator error, not a scaling law). Every
  probe is an exact key. There is no LIST anywhere on this path
  (team/org enumeration lives on collaboration pages and admin sweeps only).
- **Steady state transfers nothing.** Entries are stamped by the store CAS
  version; revalidation is a 304-class probe. The harness's 50 warm
  resolutions cost 150 probes and 0 bodies — the LRU counters read
  `hits=50 misses=1` for the access object. A changed version invalidates
  lazily (exactly the ref→sha LRU pattern, 07 §5): one body on the next
  probe, then flat again. Staleness is bounded by one request, never a TTL.
- **Writes stay off the path.** Resolution never writes (synthesis is
  read-only; materialization is the idempotent `access-bootstrap` op and the
  CAS PUT path). Revocation latency is one in-flight request by design.

**Over the network (S3/GCS/filesystem).** Same shape with per-probe RTT:
one extra sub-second control-plane GET per `info/refs` for anonymous hot
clones of public repos (01 §4.1 budget), never a LIST, never a body on
revalidation. Bulk/control-plane transport separation is untouched — these
are control-plane keys (`.json`) on the control-plane path.

**Verdict.** The authz read path is constant per request at any population
size: bounded probes, exact keys, zero steady-state bytes. No redesign
required; if binding lists ever grew large the lever is a per-team roster
projection, not a new index.

---

## E3 — Issue list/read path: index-first O(1), bounded LIST fallback (2026-09-04)

**Area:** issues list + thread reads (`internal/issues` — `ListIssues`,
`GetThread`, `scanHeaders`, `CompactIndex`). Spec: `features/02` §§2/7.

**Question:** the default UI (issue list, thread page) must stay at O(1)
store requests as repos accumulate issues and threads grow; the P4 LIST
fallback that covers index staleness must itself be bounded so a degraded
read cannot explode.

**Method.** Harness: `TestEvidenceIssueReadPath` in
`internal/issues/evidence_test.go`
(`go test ./internal/issues/ -run TestEvidenceIssueReadPath -v`). Real
`Service.ListIssues`/`GetThread` over the **memory store** wrapped in a
counting decorator (GET/LIST/PUT per call). Memory store isolates the
algorithmic shape from network RTT; every counted op is one bucket round
trip on any backend. Populations: 10 issues (short thread: 5 comments) and
300 issues (long thread: 60 comments + opened = 61 events).

**Results.**

| path | n=10 | n=300 | shape |
|---|---|---|---|
| list, default page (index-first) | 2 GET, 0 LIST | 2 GET, 0 LIST | **flat** — index + P2 counter, no LIST |
| thread read, full first page | 7 GET, 1 LIST (6 events) | 62 GET, 1 LIST (61 events) | linear in **thread length** |
| list, index deleted (fallback) | 12 GET, 1 LIST | 302 GET, 1 LIST | linear in **repo size**, bounded (below) |

**Analysis.**

- **The default list is flat because completeness is checked, not hoped
  for.** `ListIssues` reads `issues/index.json` plus the P2 counter
  (`meta/next_num`) — 2 GETs — and serves the window from the index alone
  when every allocated number has a card. The counter check is what makes
  "index-first" honest: a lost index update (10-CAS-fail drop, crash
  between the header CAS and the index CAS) reads as incomplete and falls
  through to the scan instead of silently hiding an issue. Same 2 GETs at
  10 and 300 issues; no LIST on the happy path at any population.
- **A thread read is linear in the thread, not the repo.**
  `GetThread` = 1 header GET + 1 prefix LIST over that thread's `events/`
  + 1 GET per event in the thread (61 events → 62 GETs). Threads are
  human-rate append-only logs (comments, label/state changes, reactions);
  a 10,000-comment thread would cost 10,001 GETs on a full read — which is
  why the response windows at `n ≤ 200` (`after_seq` older-on-demand).
  The scan itself stays exact-key GETs under one thread prefix; it never
  touches another thread's keys. If threads ever grew hostile, the lever
  is serving the tail window from a `startAfter` seq cursor (the `012x`
  key order already supports it) — noted, not built: human threads do not
  need it.
- **The fallback cannot explode because every scan is prefix-bounded and
  paged.** `scanHeaders` issues exactly 1 LIST over `issues/` and at most
  `scanCap = 2000` header GETs; the response page is `n ≤ 100` (default
  50). At 300 issues the degraded list costs 302 GETs — the whole repo,
  once, and only while the index is incomplete (absent, mid-repair, or
  compacted history). The 2000-cap means a 100,000-issue repo's fallback
  still costs ≤ 2001 requests and serves the first 2000 headers' worth of
  pages — degradation is linear-to-a-ceiling, never fan-out.
- **Compaction keeps the happy-path object small.** Past ~256 KiB the
  `issue-index-compact` CAS evicts oldest `closed_recent` first and
  advances `compacted_through` monotonically in the same CAS; evicted
  threads stay listable through the fallback above. Checked inline on
  every index write (bytes in hand — cheaper than sampling), so the index
  cannot grow past ~256 KiB + one card between compactions.

**Over the network (S3/GCS/filesystem).** Same shape with per-op RTT: the
default list is 2 control-plane GETs (`.json` keys); a full thread read is
(2 + E) GETs for E events; a degraded list is (3 + H) for H headers up to
the 2000 cap. No LIST on the happy path; one bounded LIST on the degraded
path. These are all control-plane keys on the control-plane path —
bulk transport separation is untouched.

**Verdict.** The read path is flat where users look (list: 2 GETs forever)
and linear-with-a-ceiling everywhere else (thread in its own length,
fallback in repo size capped at 2000 headers, pages at 100). No redesign
required; the tail-window cursor is the documented lever if threads ever
grow beyond human scale.

---

## E4 — PR mergeability compute cost + merge task round-trips (2026-09-04)

**Area:** pull-request mergeability + merge (`internal/pulls` —
`ComputeMergeable`, `runMerge`, `HandleRefEvent`, task single-flights).
Spec: `features/03` §§4/5/7.

**Question:** mergeability must stay bounded as open PRs accumulate (the
recompute runs on every thread fetch after a push); a merge must cost a
bounded number of bucket round trips (the budgets in `features/15` spirit:
no sequential store round trip added to a hot path); git usage must stay
inside the bounded per-repo pool (no fan-out, no LIST on any path).

**Method.** Harness: `TestEvidenceMergeabilityBudget`,
`TestEvidenceMergeRoundTrips`, `TestEvidenceSinkNoList`,
`TestEvidenceConcurrentRecomputeSingleFlight` in
`internal/pulls/evidence_test.go`
(`go test ./internal/pulls/ -run TestEvidence -v`). Real
`Service.ComputeMergeable`/`runMerge`/`HandleRefEvent` over the **memory
store** wrapped in a counting decorator (GET/PUT/LIST per call) plus a
scripted `GitRunner` (exact argv shapes, call log, concurrency gauge).
Memory store isolates the algorithmic shape from network RTT; every
counted op is one bucket round trip on any backend. The git argv itself is
proven separately against stock git 2.53 (`gitexec_test.go`: real clean
AND conflicting `merge-tree` runs, `commit-tree`, rev-list counts,
reachability pipeline, diff, log ranges). Populations: 1 and 50 open PRs.

**Results.**

| path | 1 open PR | 50 open PRs | shape |
|---|---|---|---|
| mergeability recompute | 3 GETs, 1 PUT, 0 LIST, 6 git calls | 3 GETs, 1 PUT, 0 LIST, 6 git calls | **flat** — sidecar + stamp, no index scan, no LIST |
| merge task (squash→merge path) | 12 GETs, 5 PUTs, 0 LIST, 11 git calls, peak concurrency 1 | — (per-PR cost is population-independent) | **bounded** — re-verify + trial + commit + publish + P3/P4/P8 commit |
| sink scan (one ref event, 5 open PRs) | ≤ 8 GETs, 0 LIST, 0 git (unrelated ref) | same | **flat in index size** — index-authoritative lookup, git only on recompute |
| 16 concurrent recomputes, one head | 1 `merge-tree` total | — | **collapsed** — `"mergeable:"+repo/num` single-flight, joiners share |

**Analysis.**

- **Recompute is flat because the stamp is the invalidation key, not a
  scan.** One recompute reads the thread header + pr.json + writes
  mergeable.json (3 GETs + 1 CAS PUT — the loser's retry converges) and
  runs exactly 6 pool-gated git calls (2 resolves, merge-base, merge-tree,
  rev-list count, plus the ancestry probe). It never reads the shared
  index and never LISTs — open-PR count is irrelevant by construction.
  The thread fetch serves `unknown` + enqueues on mismatch instead of
  computing inline, so a push storm degrades to one background pass per
  repo (`pull-mergeable` batches all dirty PRs), never N reader-driven
  merge-trees.
- **A merge is bounded because every step is one object or one process.**
  12 GETs (thread + sidecar reads, live ref resolves are git, policy.json,
  closer inputs) + 5 PUTs (thread CAS, event Create, pr.json CAS, index
  CAS, mergeable refresh) + 11 git calls (resolves, ancestry, merge-base,
  trial, subject, commit-tree, plus the recompute's share). Zero LIST:
  the duplicate check and the sink both read the index object, never scan
  it. Git runs sequential through the pool (measured peak concurrency 1
  on the sequential task path); the only fan-out in the package is the
  reader single-flight, which collapses by design (16 → 1 measured).
- **The sink scan is index-authoritative, not a sweep.** One ref event
  costs 1 index GET + 1 GET per open PR sidecar (5 open PRs → ≤ 8 GETs
  including the thread reads on head drift) and touches git only for PRs
  whose base/head actually matched. An unrelated ref costs the scan and
  nothing else — no recompute, no git, no LIST.

**Over the network (S3/GCS/filesystem).** Same shape with per-op RTT: a
recompute is 3 control-plane GETs + 1 CAS PUT + 6 local git execs (git
runs on the serving host's materialized copy, not over the network); a
merge is 12 + 5 plus the WAL publish's own manifest ladder (doc 05
budgets, unchanged). No LIST on any pulls path at any population —
collaboration LISTs stay on paginated UI pages (PR list enriches
page-bounded sidecars, ≤ 100 GETs per page).

**Verdict.** Mergeability is flat (3+1, 0 LIST, 6 git), merges are bounded
(12+5, 0 LIST, 11 git, pool-sequential), the sink is index-cheap, and
reader stampedes collapse to one trial merge. No redesign required; the
levers if volume ever surprises are the mergeable batch window (already
per-repo) and the list page size (already ≤ 100).

---

## E5 — Review-summary recompute + required-reviews gate at 5 and 100 reviews (2026-09-04)

**Area:** code review rollup + merge gate (`internal/review`, spec
`docs/features/04_code_review.md` §6).

**Question:** what does a review-summary recompute cost, what does the
merge-time gate scan cost inside the merge task, and why can't either one
explode as review volume grows?

**Method.** Harness: `internal/review/evidence_test.go`
(`TestEvidenceReviewSummaryBudget`, `TestEvidenceGateScanBudget`;
`go test ./internal/review/ -run TestEvidence -v`). Real service paths
over the **memory store** wrapped in an op-counting decorator — the
budgets count store round-trips (GET/PUT/LIST), which is the
round-trip cost model (AGENTS law 6); memory isolates the algorithmic
shape from network RTT. Populations: 5 reviews/2 threads ("normal") and
100 reviews/20 threads (order of magnitude beyond — past any human PR).

**Results.**

| path | 5 reviews / 2 threads | 100 reviews / 20 threads | shape |
|---|---|---|---|
| summary recompute | 9 GETs, 1 PUT, 2 LISTs | 122 GETs, 1 PUT, 2 LISTs | linear in review+thread count |
| gate scan (merge task) | 8 GETs, 0 PUTs, 1 LIST | 103 GETs, 0 PUTs, 1 LIST | linear in review count |

**Analysis.**

- **The recompute is linear because the summary is a fold over the
  immutable set.** 2 LISTs (one prefix LIST over `reviews/`, one over
  `threads/` — both bounded to ONE PR's collaboration subtree) + 1 GET
  per review event + 1 GET per thread header + the requests GET + the
  header read, and exactly 1 CAS PUT for the header write. 5+2+2 = 9;
  100+20+2 = 122 — the formula is exact, no hidden fan-out. The LISTs
  never leave the PR prefix (no cross-PR scan exists anywhere in the
  package), and there are zero git calls on every review path by
  construction (the package has no git seam).
- **The gate scan is the same fold minus the writes, under its own
  deadline.** Policy GET + header GET + sidecar GET + 1 LIST + 1 GET per
  review (5+3 = 8; 100+3 = 103, exact). It runs inside the merge task's
  context with `GateTimeout` (default 15 s); a blown deadline fails
  closed. It NEVER reads `review_summary` — the verdict re-derives from
  the event scan, so a poisoned or racing cache cannot decide a merge
  (pinned by test: the gate passes with a deliberately poisoned summary
  and fails only on the scan's verdict).
- **Why it can't explode.** Three bounds compose: (1) the scan prefix is
  one PR (`pulls/<num>/…`) — review volume is per-PR human output, and a
  100-review PR is already past pathological; (2) no step fans out —
  sequential GETs, one LIST per family, one PUT, zero git; (3) the write
  side is CAS-arbitrated on the PR header, so concurrent submits
  serialize on allocation (16-way submit burst converges with unique
  seqs, measured in `TestConcurrentSubmits`) instead of amplifying reads.
  There is no background dismisser, no cross-PR aggregation, and no
  second writer — the summary is a render cache any racing writer
  recomputes identically, and the gate doesn't trust it anyway.

**Over the network (S3/GCS/filesystem).** Same shape with per-op RTT: a
recompute at 100 reviews is ~122 control-plane GETs + 1 CAS PUT (no bulk
lane, no git); the gate is ~103 GETs inside the merge task's deadline.
Both stay on the control plane and off the git hot path at any
population — the lever if a deployment ever saw thousand-review PRs is
the page/window size, not a redesign.

**Verdict.** Recompute and gate costs are exactly linear in per-PR review
count with constant LISTs (2 and 1) and at most 1 PUT, measured on the
real code path at both populations. No redesign required.

---

## E6 — Check report path + combined view + required-checks gate (2026-09-04)

**Question:** what does one CI status report cost, what does the combined
worst-of view cost, what does the merge-time gate scan cost inside the
merge task, and why can't any of them explode as context count grows?

**Method.** Harness: `internal/checks/evidence_test.go`
(`TestEvidenceReportBudget`; `go test ./internal/checks/ -run
TestEvidence -v`). Real service paths over the **memory store** wrapped
in an op-counting decorator — the budgets count store round-trips
(GET/PUT/LIST), which is the round-trip cost model (AGENTS law 6);
memory isolates the algorithmic shape from network RTT. Populations: 1
context and 20 contexts on one sha (20 is past any sane CI matrix for a
single commit).

**Results.**

| path | 1 context | 20 contexts | shape |
|---|---|---|---|
| first report (per context) | 2 GETs, 2 PUTs, 1 LIST | 230 GETs, 40 PUTs, 20 LISTs (cumulative over 20 reports) | 2 PUTs + (2 + k) GETs + 1 LIST per report, k = contexts on the sha |
| re-report (1 of k) | 4 GETs, 3 PUTs, 1 LIST | 23 GETs, 3 PUTs, 1 LIST | status CAS (GET + PUT) + re-read + index CAS + combined re-read |
| combined view | 1 GET, 0 PUTs, 1 LIST | 20 GETs, 0 PUTs, 1 LIST | exactly 1 LIST + 1 GET per context |
| gate scan (merge task) | policy GET + 1 LIST + k GETs | same shape | under GateTimeout (15 s), fails closed |

**Analysis.**

- **A first report is 2 PUTs (status Create + index CAS) + 2 GETs
  (index read + combined re-read for the broadcast packet) + 1 LIST**
  (the combined re-read's prefix LIST). A re-report adds the failed
  Create attempt and the CAS re-read: 4 GETs + 3 PUTs (one PUT failed
  with 412 — counted as an attempt, not a write) + 1 LIST.
- **The per-report cost grows linearly in k** because the broadcast
  packet carries the fresh combined state, which re-reads all k
  contexts (1 LIST + k GETs, bounded parallel fan-out cap 8). This is
  deliberate: the SSE `check` frame must not lag one write behind.
  Reports are CI-rate (one system per context, human-driven pipelines),
  never git-hot-path, so k GETs per report is the honest price of a
  correct broadcast — measured, not assumed.
- **The combined view is exactly 1 LIST + k GETs, 0 PUTs, 0 git.**
  No commit resolution happens on the read path (POST validates the
  sha; GET trusts it), so reads never touch the git pool.
- **The gate scan is policy GET + statuses LIST + k GETs** (same fold
  as combined, minus the index), under its own 15 s deadline; a blown
  deadline fails closed. It NEVER trusts the index projection — the
  verdict re-derives from the per-context objects, so a stale or racing
  index row cannot decide a merge (same discipline as the review gate's
  distrust of review_summary, E5).
- **Index writes are best-effort.** A lost index CAS (writer died
  between the status write and the index CAS, or 5-attempt exhaustion)
  costs one stale table row until the next report — the per-context
  objects are the backfill truth, and compaction is inline past 256
  KiB / newest 500 shas. The table page degrades to LIST, never to
  wrong.

**Over the network (S3/GCS/filesystem).** Same shape with per-op RTT: a
report at 20 contexts is ~22 control-plane GETs + 2 CAS PUTs + 1 LIST
(no bulk lane, at most 1 git call on POST for sha validation, zero on
every GET); the gate is ~k+1 GETs + 1 LIST inside the merge task's
deadline. All control plane, all off the git hot path — the lever if a
deployment ever saw hundred-context shas is the fan-out cap, not a
redesign.

**Verdict.** Report cost is constant PUTs (2–3) with GETs linear in
per-sha context count; combined and gate reads are exactly 1 LIST + k
GETs with zero writes and zero git. No redesign required.

---

## E7 — Notification fan-out write amplification + webhook delivery loop (2026-09-04)

**Area:** notifications, subscriptions, mentions, webhooks (`internal/notify`).
Spec: `docs/features/06_notifications.md` §4–§5.

**Method.** Harness: `internal/notify/evidence_test.go`
(`TestEvidenceFanoutAmplification`, `TestEvidenceDeliveryLoop`;
`go test ./internal/notify/ -run TestEvidence -v`). Real service paths
over the **memory store** wrapped in an op-counting decorator — the
budgets count store round-trips (GET/PUT/LIST/HEAD), which is the
round-trip cost model (AGENTS law 6); memory isolates the algorithmic
shape from network RTT. Populations: 1 and 10 recipients per emission
(the 100-recipient overflow path is covered by
`TestOverflowDefersToFanoutTask`, which asserts full drain + task
record); delivery measured over 5 queued events against a live sink plus
the true idle pass.

**Results.**

| path | 1 recipient | 10 recipients | shape |
|---|---|---|---|
| one emission (subscribed) | 5 GETs, 4 PUTs | 23 GETs, 22 PUTs | 2 GETs + 2 PUTs per recipient + 3 GETs + 2 PUTs fixed |
| deduped second emission (all still unread) | 4 GETs, 2 PUTs | 13 GETs, 2 PUTs | writes flat (activity only, zero notification/index writes); probes are 3 + n GETs |
| delivery pass, 5 queued, live sink | 14 GETs, 8 HEADs, 6 PUTs, 1 LIST | — | ~3 GETs + 1 PUT per event + 1 cursor CAS; cursor 0 → 5 |
| idle pass (cursor at head) | 3 GETs, 8 HEADs, 0 PUTs, 1 LIST | — | hooks LIST + cursor GET + head probe; writes nothing |

**Analysis.**

- **One emission is 2 GETs + 2 PUTs per recipient** (dedup probe +
  idempotent Create, index CAS read + write) **plus 3 GETs + 2 PUTs
  fixed** (thread title/author read, watchers read, seq-reservation
  CAS, activity Create). No LIST on the emission path at any
  population. The fixed cost is the price of §2 resolution without
  event scans; the per-recipient cost is two CAS-arbitrated writes,
  parallel under the cap-8 semaphore.
- **The unread dedup makes replay WRITES flat (probes stay linear).** A
  second emission while every recipient still holds a live entry costs
  (3 + n) GETs + 2 PUTs — 4 + 2 at 1 recipient, 13 + 2 at 10 (3 fixed
  probes for thread/watchers/seq plus one index probe per recipient) —
  with zero notification objects and zero index rewrites at any
  population. Each emission mints a
  distinct activity seq by design: a crash loses the fan-out per P8
  (it never replays it), so retries mint no duplicate webhook
  delivery keys.
- **A delivery pass is 1 LIST (hooks) + ~3 GETs + 1 PUT per event**
  (event read, POST, ring CAS) **+ 1 cursor CAS**; the cursor advances
  only past 2xx deliveries (at-least-once, per-hook isolation — a
  refused sink holds its own cursor at 0 while others advance). The
  idle pass is 1 LIST + 3 GETs + 8 HEADs (the bounded honest-gap
  probe) and **zero writes**.
- **Overflow is the bound, not the cliff.** Past 100 recipients the
  request writes only the activity event (recipients in its payload as
  the durable queue) and returns; `notify-fanout` drains the set
  idempotently (deterministic ids + index dedup) under the same 5 s
  budget. A 1 000-watcher repo costs one request-side activity write,
  not 1 000 synchronous CAS rounds.

**Over the network (S3/GCS/filesystem).** Same shape with per-op RTT:
a 10-recipient comment is ~23 control-plane GETs + ~22 small-JSON
PUTs, all off the git hot path (collaboration rate, human-driven);
webhook POSTs are the only wide-area calls (10 s each, per-hook
sequential, hooks parallel). The lever if a deployment ever saw
pathological mention storms is the sync cap + budget, not a redesign.

**Verdict.** Fan-out amplification is linear with slope 2+2 per
recipient and a 5-op fixed cost; deduped replays write flat (2 PUTs at
any population, probes 3+n GETs); delivery is
one cursor CAS per pass with zero-write idles. No redesign required.
