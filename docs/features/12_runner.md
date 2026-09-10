# 12 — GitHub-style runners: researched design plan (NO implementation until reviewed)

> Forgejo [`crueber/walhub#288`](https://git.packden.us/crueber/walhub/issues/288) · Status: **DRAFT PLAN — awaiting review, do not implement.**
> Reviewer: **Chris (crueber)**. This document is normative on conflict once reviewed (the Feature 11
> precedent: plan revision + plan review win). Until then it is a proposal and **no code may land**
> (`internal/`, `web/`, config, CLI — all frozen pending review).

walhub today **stores** CI results and gates merges on them; it never runs CI (features README P9,
doc 05). This plan designs the other half: a GitHub-Actions-shaped **runner system** — builds and
jobs that execute as part of walhub and report into the existing checks/statuses surface
(`internal/checks`). The coordinator state is bucket-native (AGENTS.md law 4); execution happens
outside the server's trust boundary except for the explicitly trusted R1 slice (§8).

Doc conventions: this is the feature spec (`docs/features/NN_name.md`, next number 12 — this IS the
feature doc, not `docs/go/` architecture; no new `docs/go/` number is claimed). Numbered decisions
**D1–D8** below are the reviewable units (the 11_mirror.md R1 (a)/(b) style). Rollout slices
**R1/R2/R3** (§8) are independently shippable. Dependency analysis (§10) works within law 1 with
zero amendments; the one reserve amendment is drafted, not proposed.

## 0. What exists that this builds on (no reinvention)

- **`internal/checks`** (doc 05): `checks/<sha>/<context>.json` Create-then-CAS status records,
  `checks/index.json` CAS'd P4 projection, `wct_<id>.<secret>` CI tokens resolved to unprivileged
  `ci:<id>` principals with handler-side capability checks (frozen `Principal` untouched), combined
  worst-of view, `require_checks` merge gate, `check` SSE packet shape (stream wiring still open).
- **Task system** (`internal/wal/tasks.go`, 14 Seam 5): narrated long work, `(repo,kind)`
  single-flight join semantics, progress packets, SSE attach, drain interruption → 503.
- **Scheduler precedent** (`internal/mirror`, 11_mirror.md; `internal/maintain` loop goroutine,
  `follow.go` shape): bucket lease (`leases/<kind>-<repo>.pb`, CAS+TTL) for cross-instance exclusion,
  probe-don't-list enumeration, backoff counters, narrate-via-task.
- **Egress posture** (`internal/egress`): SSRF gate (`BlockedIP`, `DialContext`, `RefuseRedirect`)
  for all server-initiated outbound traffic — reused for any coordinator fetch (§7 D8).
- **Secret hygiene precedents**: import/mirror never-stored-token rule (11 §2), `scrubURL`/`scrubError`
  redaction, `secret_set` presence-only task params.
- **Caps precedent**: `[import]` bounds section (max bytes, timeouts) — the pattern for §7 abuse caps.
- **Lived experience**: this repo itself runs on Forgejo + Woodpecker CI — the plan cites that
  operator experience where marked (LIVED).

## 1. Prior-art research (borrowed vs invented)

### 1.1 GitHub Actions self-hosted runners (protocol level, from knowledge)

Borrowed shape (adapted, not cloned — the message names below are descriptive, not wire-compatible):

1. **Registration**: operator mints a short-lived registration token
   (`POST /repos/{o}/{r}/actions/runners/registration-token`). The runner exchanges it once for a
   long-lived runner credential (OAuth-style bearer) plus a persistent runner identity (id, name,
   labels). Token is single-use, minutes-TTL. — **Borrowed** (D4 mirrors this two-step exactly).
2. **Connection**: runner opens a long-poll against the message queue
   (`GET /_apis/distributedtask/pools/{pool}/messages?sessionId=…`, ~60 s long-poll). The server
   never dials the runner — runners live behind NAT/firewalls. — **Borrowed** (D3: poll-only is
   non-negotiable for the same reason).
3. **Job message**: an `AgentJobRequest` carrying the full job plan (steps, variables, **mask hints**
   for secret redaction, endpoints + one-time tokens for reporting back). The runner needs no further
   server reads to execute. — **Borrowed** (D2 claim response is self-contained for the same reason:
   every runner read is a store round trip we budget).
4. **Session renewal / heartbeat**: `PUT` session keepalive; missed renewals return the job to the
   queue. — **Borrowed** (D3 lease heartbeats).
5. **Reporting**: timeline records (`PATCH` per step: pending → in_progress → completed with
   conclusion/outcome), chunked log upload to blob endpoints (server-issued upload URLs, idempotent
   chunk append), artifacts to a parallel blob API. Completion is a final timeline PATCH, not a
   separate call. — **Borrowed in structure** (D6/D7), re-expressed as bucket objects.
6. **Auth scoping**: the `GITHUB_TOKEN` is per-job, least-privilege, short-lived. — **Borrowed**
   (D4 `wjt_` job tokens).

What is NOT borrowed: the workflow expression language (`${{ }}` contexts, JS actions, OIDC-to-cloud,
reusable-workflow supply chain). v1 defines a deliberately small job schema (D5).

### 1.2 Gitea / Forgejo `act_runner` (from knowledge + LIVED operator experience)

`act_runner` (the daemon behind Forgejo Actions): register with a token minted in site/repo settings
(`act_runner register --token … --labels …`), then long-poll the Forgejo server for tasks matching
its labels, execute each job as containers (one container per step, Docker or Podman), stream logs
back over the same connection, report completion. Key observations:

- Label matching is the whole scheduler (`runs-on` ↔ runner labels) — no priorities, no bin-packing.
  v1 copies this (D2): label match only, FIFO within a repo queue.
- The runner is a **separate binary with a container runtime dependency** — the Forgejo server itself
  never runs containers. This is the precedent for §10: execution dependencies live outside the
  server's dependency budget.
- LIVED pain adopted as design input: label-less runners silently starving queues (→ D2 queue
  visibility endpoint + `stuck` surfacing), and Docker-socket access making every runner effectively
  root on its host (→ D8 states the isolation boundary honestly instead of promising sandboxing the
  coordinator cannot verify).

### 1.3 Woodpecker CI (from knowledge + LIVED operator experience — this repo's CI)

Server/agent split over **gRPC**: agents `Poll` (filter by platform/labels), run pipelines in
configurable backends (docker, local, ssh), stream logs via RPC, server holds all secrets and
injects them as (masked) env. Observations adopted:

- Secrets never touch the agent's disk: server-side store, env-only injection, masked in logs.
  → D6 copies this exactly (env-only + mask hints + server-side redaction as backstop).
- The server is a pure coordinator: queue + dispatch + state + UI. Agents are disposable and
  untrusted with respect to each other's jobs. → D1 trust split.
- LIVED pain adopted: secret-in-log leaks via `set -x` echo (→ D6 double redaction: runner masks
  with hints AND coordinator scrubs stored chunks on read-back), and stuck agents holding jobs
  forever (→ D3 lease expiry + steal, never infinite claim).

### 1.4 What walhub invents (nothing else fits the bucket)

The queue, leases, logs, artifacts, and run headers as **bucket objects with CAS discipline**
(§3–§6). No prior art does exactly this because every prior art has a database; walhub doesn't
(law 4). The CAS-claim protocol (D3) and chunked-immutable log layout (D7) are the two invented
pieces, and both are compositions of already-shipped walhub patterns (mirror lease, P3 immutable
events + CAS'd header).

## 2. Runner model — D1 (pull-based external runners; server coordinates, never dials)

**D1: the coordinator/runner split.** walhub ships a **coordinator** (queue, claims, heartbeats,
log/artifact intake, checks reporting — all bucket state + HTTP handlers in a new
`internal/actions` package). **Execution** happens in runner processes that are NOT walhub code in
v1: an out-of-tree runner agent (documented protocol, §4) plus, for R1 only, operator-triggered
server-side runs of trusted code (§8). The server never opens a connection to a runner (NAT,
disposability — §1.1 finding 2). Runners poll; the coordinator answers.

- Trust boundary: runners are **untrusted with respect to each other's jobs and with respect to
  the coordinator's other tenants**. A runner sees only: its claimed job plan, its job token
  (§4 D4), its own logs. It never sees other runs, other repos' secrets, or any principal
  credential. The coordinator never executes job steps in-process (R1 exception: §8 — trusted,
  operator-gated, documented as not-a-sandbox).
- Multi-instance: any instance serves any runner request; all arbitration is bucket CAS (D3). No
  sticky sessions, no in-memory queue (law 4 — "if every instance is wiped, what is lost?" must
  answer "warmth": pending/claimed/completed run state all survives).
- Scheduler placement: the queue-sweeper (requeue expired leases, fire scheduled triggers) runs on
  maintain-role hosts as a `RunLoop` goroutine at 1-minute cadence — the mirror `RunLoop` /
  `follow.go` shape (13_concurrency.md §1 row 6), never a maintenance unit, never blocking
  maintenance. Cross-instance exclusion via `leases/actions-sweep-<o>-<r>.pb` (CAS+TTL 10 m),
  held → skip + narrate (never wait).

## 3. Job queue — D2 (bucket-native; CAS is the lock)

**D2: queue and run state as a new object family** under `repos/<o>/<r>/actions/` (P1 peer family;
frozen-overwritable-list amendment drafted in §10 — same 14 §14.11 rule-2 procedure as 05/11):

```
repos/<o>/<r>/actions/
  runners/<id>.json                  # CAS'd runner record {id,name,labels[],status,last_seen,version}
  reg_tokens/<id>.json               # one-time registration tokens (Create; delete-on-use)
  queue.json                         # CAS'd pending list [{run_id,job_n,labels[],enqueued_at}] (P4 hot window)
  runs/<run_id>/
    run.json                         # CAS'd run header {id,trigger,head_sha,workflow,author,status,...}
    jobs/<n:06x>.json                # CAS'd job record {state,labels,claimed_by,lease_expires_at,timeline[]}
    logs/<job:06x>/<step:02x>/<chunk:06x>.log   # immutable chunks, Create-only
    artifacts/<name>/manifest.json  # CAS'd manifest + blobs artifacts/<name>/blobs/<chunk>
  secrets/<name>.json                # CAS'd sealed secret envelopes (D6)
```

- **Enqueue** (trigger handler, one instance — P8 pattern): CAS `run.json` (Create) + CAS `jobs/*`
  (Create, `state: queued`) + CAS-append to `queue.json` (retry loop, ≤ 5 then 503 — the 05 §4
  report-storm rule). Queue holds the hot window only (newest ~500 entries, 256 KiB cap per P4);
  history lives in `runs/` (LIST by prefix — collaboration-rate pages, P5-legal).
- **Claim** (§4 D3 details the protocol): runner long-polls `…/actions/jobs/poll`; coordinator
  matches labels → CASes the job record `queued → claimed` (`claimed_by`, `lease_expires_at =
  now + 60 s`) → removes the entry from `queue.json` in the same CAS loop (two CAS writes, loser
  retries — the CAS *is* the lock, 05 §2 concurrency note). Claim response is self-contained
  (§1.1 finding 3): job plan + mask hints + `wjt_` job token + log/artifact upload tickets.
- **Round-trip budget** (law 6): enqueue ≤ 4 store ops (run Create + job Create + queue
  read-modify-write = 2 + index probe); poll-hit ≤ 3 (queue probe + job CAS read-write + runner
  heartbeat CAS piggybacked); poll-miss = 1 probe (long-poll holds the request, not the store —
  re-probe on wake via the repo collab stream trigger, 60 s cap). The sim pins these (15_testing.md
  pattern); a correct change adding a sequential op to poll-miss is a regression.
- **Queue visibility** (the act_runner starvation lesson, §1.2): `GET …/actions/queue` (read role)
  shows pending entries with age; entries older than 10 min without a matching runner label set
  surface `stuck: true` with the missing-labels hint. No silent starvation.

### Concurrency

Hazard: two runners claiming the same job; sweep requeue racing a live heartbeat. Avoidance: claim
is one CAS on the job record (loser gets 409 → tries the next entry); sweep requeues only jobs
whose `lease_expires_at` is past AND whose CAS still shows the expired lease (re-read inside the
CAS loop — the ambiguous-CAS re-read rule); heartbeats are CAS with version check, never blind
overwrite. No new locks; this family never touches `syncMu`/`packMu`/`rw` (13 §2).

## 4. Assignment + auth — D3 (long-poll claim, leases) and D4 (token families)

**D3: poll, never dispatch.** Runner protocol (descriptive names; exact paths frozen at
implementation review):

```
POST …/actions/runners/register   {reg_token, name, labels[]} → 201 {runner_id, wrt_secret} (once)
GET  …/actions/jobs/poll?runner=<id>&labels=a,b&timeout=60s    → 200 {job,…} | 204 (timeout)
POST …/actions/jobs/{run}/{n}/heartbeat                        → 200 {lease_expires_at}
PATCH …/actions/jobs/{run}/{n}/timeline                        → 200 (step transitions)
POST …/actions/jobs/{run}/{n}/logs?step=k                      → 200 {chunk} (immutable append)
POST …/actions/jobs/{run}/{n}/artifacts/{name}                 → upload tickets / finalize
POST …/actions/jobs/{run}/{n}/complete {conclusion}            → 200 (writes checks status, §6)
```

- Long-poll holds the HTTP request ≤ 60 s; the store is probed on arrival and on wake (new enqueue
  pokes the repo collab stream; the poll handler subscribes — SSE-envelope pattern, 07 §9.3 —
  otherwise re-probes every 5 s). Poll-miss costs ~1 probe per 5 s window, never a hot spin.
- **Exactly-once-ish execution** (honest scope): at-least-once claim with at-most-once *effect* —
  lease 60 s, heartbeat every 30 s; missed heartbeats past `lease_expires_at + 30 s` grace →
  sweep requeues (`claimed → queued`, `attempts++`, `claimed_by` cleared). Step/log/artifact
  writes are idempotent by key (chunk seq, artifact blob hash); a double-executed job produces
  duplicate chunks that the viewer dedupes by seq, and checks reporting is last-write-wins per
  (sha, context) (05 §2 — already well-defined). `attempts > 3` → job `failure`, run continues
  per `fail-fast` (D5). No distributed transaction is promised; idempotent effects are.
- Lease steal is forbidden (only the sweeper requeues, only past grace) — a slow runner with live
  heartbeats is never preempted, even if a faster runner is idle (liveness over utilization; the
  Woodpecker stuck-agent lesson, §1.3).

**D4: three token shapes, zero Principal changes** (05 §3 precedent — frozen `Principal` untouched,
handler-side capability checks, startup prefix-overlap assertion):

| Token | Shape | Minted by | Scope | Lifetime |
|---|---|---|---|---|
| Registration token | `art_<id>.<secret>` (one-time) | repo admin, settings UI | one repo, `register` only | 15 min, single-use (delete-on-use) |
| Runner credential | `wrt_<id>.<secret>` | coordinator at register | one repo, `poll:jobs` + own-job writes | long-lived, revocable (record retained → 401, the `wct_` rule) |
| Job token | `wjt_<run>.<secret>` | coordinator at claim | one job: read repo (clone/fetch) + report own timeline/logs/artifacts + write own check context | job lease + 5 min grace, then 401 |

- Storage: `runners/<id>.json` holds `{token_hash (sha-256 hex), labels, revoked_at}` — secret
  shown once at register (copy button, the 05 CI-token UX). Only hashes in the bucket.
- **Machine users rejected** (explicitly considered): a persistent user-shaped credential with
  `write` would conflate "may push" with "may run" and leak across the frozen role lattice (P6);
  the three narrow shapes above scope leaks to one repo / one job respectively.
- Clone auth: the runner clones/fetches over HTTP(S) with the `wjt_` token as Basic password
  (same transport as CI today); the token resolves to an unprivileged `job:<run>` principal with
  repo-read + own-job-write checked handler-side. SSH clone with job tokens is a non-goal (R2).

## 5. Job definition — D5 (in-repo workflows, small schema, label scheduling)

**D5: workflows live in-repo at `.walhub/workflows/*.yml`** (versioned with the code under test,
audited via git history, no store-side workflow config in v1 — the trigger's `head_sha` pins the
exact file revision; re-runs use the pinned sha, never floating HEAD).

Minimal v1 schema (strict parse, fail closed — unknown keys are errors, the policy-parse rule):

```yaml
name: ci
on: [push, pull_request]          # v1 triggers: push, pull_request, workflow_dispatch. NOT schedule (R3).
jobs:
  build:
    runs-on: [linux, x64]         # label match vs runner labels; subset match (runner ⊇ job labels)
    steps:
      - name: checkout            # reserved: coordinator-supplied clone (no script)
        uses: checkout@v1         # v1: ONLY checkout (+ setup-* toolchain shims TBD at review). Arbitrary uses: NON-GOAL (§9).
      - name: build
        run: make build           # shell script, runner's default shell; multiline | allowed
      - name: test
        run: make test
        env: { FOO: bar }         # step env; secrets via ${{ secrets.NAME }} ONLY (§6 D6)
```

- **Triggers**: `push` (any push to a matched branch filter — default all branches), `pull_request`
  (head sha of the PR — the gate in 05 §6 reads the same sha, so runs and gates agree by
  construction), `workflow_dispatch` (manual POST, write role, R1's entry point). Enqueue fans out
  from the existing event sinks (Seam 4): the push/PR-merge handlers emit run-requests the same way
  06 notifications are enqueued (P8 — synchronously post-CAS, best-effort, timeline-of-runs is the
  backfill truth). `schedule` (cron presets à la mirror §1 — hourly/8h/daily/weekly/monthly, never
  freeform) is R3 (§8) and reuses the mirror `RunLoop` + `bundle.Cron.Next` anchoring verbatim.
- **Matrix/strategy**: non-goal for R1/R2; R3 adds `strategy.matrix` (bounded: ≤ 16 combinations,
  fail-fast flag) as pure enqueue-time fan-out (one job per combination — no coordinator change
  beyond the expander, which is unit-pinned).
- **Branch filters / paths filters**: `on.push.branches` + `paths-ignore` in R2 (cheap: evaluated
  from the push event's ref + changed-file list, zero extra store reads). Skipped runs record a
  `skipped` run header (visible, not silent — law 7).

## 6. Execution, secrets, and reporting — D6 (env-only secrets, double redaction) and D7 (checks/logs/artifacts)

**D6: execution environment + secrets.** The coordinator is backend-agnostic: the job plan says
*what* (steps, env, workdir); the runner decides *how* (bare subprocess minimum viable; containers
its own choice — §10: no runtime dependency on either side of the walhub repo). The reference
runner (out-of-tree, documented alongside the protocol) supports bare-subprocess + Docker backends.

- `checkout` is coordinator-defined: clone URL + `wjt_` token + pinned sha, shallow default
  (`fetch-depth: 1`, full clone opt-in). Runner MUST check out the pinned sha and attest it in the
  first timeline PATCH (coordinator verifies equality — a runner reporting a different sha fails
  the job; cheap integrity anchor).
- **Secrets** (the object-store-only design problem, not hand-waved): repo secrets are sealed
  envelopes `actions/secrets/<name>.json` = `{name, sealed (secretbox, nonce-prefixed, base64),
  created_by, updated_at, version}`. Sealing key: 32-byte server key from **env
  `WALHUB_ACTIONS_SECRETS_KEY` or `[actions] secrets_key_file`** (operator-managed, never in the
  bucket, never logged); crypto is `golang.org/x/crypto/nacl/secretbox` — **already inside the law-1
  budget** (SSH amendment), so no amendment needed. No key configured → secrets API answers 503
  with "secrets not configured" (fail closed; the mirror public-upstreams-only posture generalized:
  no key = no stored secrets, jobs requesting them fail at enqueue with a clear message).
  - Access: `${{ secrets.NAME }}` interpolation happens **coordinator-side at claim** (runner never
    lists secrets); values travel in the claim response (TLS assumed — same assumption as `wct_`
    issuance), injected as process env only, never written to disk by the reference runner.
  - **Double redaction**: claim carries `mask[]` hints (GitHub shape); the runner masks log chunks
    before upload AND the coordinator re-scrubs stored chunks on read (`scrubError` precedent —
    server-side backstop for `set -x` echoes and custom runners). Secret values never appear in
    task params, SSE payloads, or error strings (`secret_set` presence-only rule).
  - Rotation = CAS-update envelope (version++); in-flight jobs keep old values (pinned at claim —
    documented, not a bug); audit = who/when CAS'd (header fields, no separate log in v1).

**D7: reporting into `internal/checks` + logs + artifacts.**

- **Checks**: each job maps to one status context `actions/<workflow>/<job>` (charset-legal per 05
  §2). Lifecycle: enqueue → coordinator writes `pending` (creator `job:<run>`); runner step events →
  still `pending` (description = current step); `complete` → coordinator writes terminal
  (`success`/`failure`/`error`) with `target_url` → the run detail page. `ReportInput` is
  **extended additively** (optional `run_id`/`job`/`attempt` fields ride the existing struct —
  14 §14.12 field rule), NOT versioned: the wire shape is unchanged, the merge gate (05 §6) and
  combined view work unmodified. Richer check-runs (annotations, per-step conclusions) are a
  documented follow-up that adds sibling objects under `checks/` (05 §1 deferred seam note covers
  this) — v1 maps conclusion→state and stores the step table on the job record.
- **Logs**: immutable chunk objects `logs/<job>/<step>/<chunk:06x>.log` (`PutMode::Create`,
  64 KiB/chunk cap, `step` count ≤ 100, total per job ≤ 64 MiB default — `[actions]` caps, §7).
  Viewer reads head + tail windows (first 32 KiB + last 256 KiB + seq-range pages — bounded,
  P5-paginated). Live tail reuses the repo collab SSE stream (`action-log` event with
  run/job/step/seq cursor; drop-oldest broadcast — 13 §6; terminal state refetched from the
  record so lagged clients never miss outcomes).
- **Artifacts**: `artifacts/<name>/manifest.json` (CAS'd: `{files:[{path,sha256,size}], finalized}`)
  + content blobs (immutable, content-addressed by sha256 — dedup across attempts free).
  Defaults: per-file ≤ 1 GiB, per-run ≤ 2 GiB (the `releases.max_asset_bytes` precedent shape),
  retention 30 days (sweeper task `actions-retention`, same pass as the queue sweep). Artifact
  bytes are served with the static/bytes contract (no compression — the 07 byte-family precedent),
  `job:<run>`-or-read-role gated.
- UI (08-pattern SPA): `/:owner/:repo/actions` (runs list — status pill per run, trigger filter,
  live rows via SSE), run detail (jobs × steps matrix, log viewer with tail follow, artifact
  download links), commit/PR check pills reuse the 05 surfaces (job contexts render as ordinary
  statuses — zero new pill machinery), settings → Actions (runner list + register-token mint +
  revoke, secrets CRUD, caps display). SDK: `web/sdk/src/actions.js` group (`runs.list/get`,
  `runners.{register,list,revoke}`, `secrets.{set,list,remove}`, `artifacts.download`).

### Concurrency (D6/D7)

Hazard: log-chunk Create races from a retried upload; artifact finalize racing a late chunk;
checks index CAS storm on mass completion. Avoidance: chunk seqs allocated coordinator-side in the
claim/heartbeat responses (runner never invents seqs — retries resend the same seq → 412 = already
stored, idempotent); finalize CAS requires `chunks_complete` count match; checks writes reuse the
05 ≤ 5-retry rule with projection-repair-on-next-write (05 implementation note: index exhaustion
still answers 200). Nothing here takes a repo lock.

## 7. Security posture — D8 (honest boundaries, abuse caps, egress)

**D8: the coordinator enforces what it can observe; everything else is stated, not promised.**

- **Isolation**: the coordinator provides NO sandbox. R1 server-side runs execute trusted code only
  (see §8 — operator's own workflow, manually triggered, clearly labeled "runs on the server as the
  server user"). External runners are untrusted hosts by definition: the reference runner documents
  the Docker-backend + no-privileged + per-job-user minimum, but the coordinator cannot verify it
  (a runner attests its backend in `register`; the UI shows the attestation *as an attestation*,
  never as a guarantee — the 14 §14.5 honest-note applied to CI).
- **Egress**: coordinator-side fetches (R3 schedule evaluation needs none; `uses:` resolution needs
  some — but arbitrary `uses:` is a non-goal, so v1 coordinator egress ≈ 0). Runner-side egress is
  the runner operator's firewall, documented with a default-deny + allowlist recipe. Any future
  coordinator fetch goes through `internal/egress` (SSRF gate already shipped).
- **Abuse caps** (`[actions]` config section, additive per 14 §14.12; the `[import]` bounds pattern):
  `max_concurrent_jobs_per_repo` (default 4), `max_runtime` per job (default 30 m, hard ceiling
  6 h), `max_log_bytes_per_job` (default 64 MiB), `max_artifact_bytes_per_run` (default 2 GiB),
  `max_runs_retained` (default 1000/run-headers window; logs/artifacts age out at 30 d regardless),
  `max_workflows_per_repo` (default 32 — parse cost bound). Exceeding a cap fails the job/run with
  a plain-text reason (fail closed), never silently drops.
- **Fork/PR safety**: `pull_request` runs from forks (03 fork networks) get NO secrets (empty
  `${{ secrets.* }}`, explicit `secrets_denied: true` in the claim) and read-only `wjt_` tokens
  until an explicit "approve run" (write role) flips the run to `approved` — the crypto-miner
  abuse shape, closed by default.
- **Multi-instance**: all of the above is bucket state, so placement is irrelevant by construction;
  the only cross-instance primitive is the sweep lease (§2 D1) plus job CAS. A partitioned
  instance's heartbeats fail → its jobs requeue elsewhere after grace (liveness without fencing;
  idempotent effects make the overlap harmless — D3).

## 8. Rollout slices — R1/R2/R3 (each independently shippable, each with acceptance criteria)

**R1 — Manual single-job runs, server-side, trusted code only.** `workflow_dispatch` trigger only;
one job per run; execution as a coordinator-spawned subprocess on the server host (`exec.CommandContext`
under the task ctx — drain kills it, 13 §8), running as the server UID in a fresh temp dir, env from
claim (secrets only if key configured), no network beyond clone (egress deny by default —
`internal/egress.DialContext` refusal wired into the child's env? No: child uses a restrictive
`http.Proxy` env + documentation; honest statement, §7). Surfaced as task kind `actions-run`
(Seam 5: `(repo, kind)` single-flight, progress packets per step, SSE attach). This slice proves:
enqueue → claim(self) → execute → timeline → logs → artifacts → checks status → merge gate green —
end to end with zero new trust assumptions (the operator runs their own code on their own host).

- R1 acceptance: dispatch a 2-step fixture workflow via API → task narrates steps → logs stream →
  `actions/ci/build` context `success` on the head sha → PR merge gate passes; caps enforced
  (timeout kill at `max_runtime` verified with `sleep`); `git status` clean of everything but the
  slice's files; ≥ 95% coverage on `internal/actions`.
- R1 does NOT need: runner protocol (§4 poll/register — stubbed by self-claim), secrets-at-scale,
  schedules, matrices.

**R2 — Pull-based external runners.** Register/poll/heartbeat/timeline/logs/artifacts protocol
(§4) + reference runner skeleton (out-of-tree directory `runners/reference/` — Go, stdlib-only,
documented as the protocol's executable spec; NOT built into the server binary, NOT in the law-1
budget). R1's self-claim becomes one client among many. Label matching + `push`/`pull_request`
triggers + branch/paths filters + fork-PR approval gate land here.

- R2 acceptance: reference runner on a second host claims a queued job (server never dialed it) →
  full report-back; lease-expiry requeue proven by killing a runner mid-job (sweep requeues after
  grace, second runner completes, chunks dedup by seq); revoked `wrt_` → 401; `stuck` queue
  surfacing covered; fake-runner e2e (in-process poller) green under `-race -count=50`.
- R2 does NOT need: schedules, matrices, annotations.

**R3 — Schedules + matrices + retention hardening.** `schedule` triggers (mirror preset shapes,
`bundle.Cron.Next` anchoring), `strategy.matrix` fan-out (≤ 16), `actions-retention` sweeper
(logs/artifacts age-out + run-header window), artifact GC accounting, UI polish (tail-follow,
matrix grid). Each sub-item shippable alone.

- R3 acceptance: daily-preset fixture fires within one sweeper cadence of anchor; matrix fixture
  fans out to N jobs with one red cell failing only that cell (fail-fast on/off both pinned);
  31-day-old artifacts gone, headers kept; EVIDENCE.md entries for queue budgets (law 6).

## 9. Explicit non-goals for v1 + open questions for Chris

Non-goals (will be proposed again only with new issues): arbitrary `uses:` actions + action
marketplace (supply-chain review burden); reusable workflows / composite actions; OIDC-to-cloud
(`GITHUB_TOKEN → AWS` style federation); self-hosted runner *pools shared across repos/orgs*
(per-repo runners only in v1 — org runners are question Q3); GPU/special-hardware scheduling;
build caching (cache API); check-run annotations (deferred sibling objects, §6 D7); deployment
environments + manual approvals beyond fork-PR run approval; per-branch runner routing; Windows /
macOS runner support in the reference agent (protocol is platform-neutral; the agent is Linux-first).

Open questions (need Chris's call before implementation planning):

- **Q1.** R1 server-side execution: acceptable even trusted-only, or should R1 be "manual dispatch
  to a locally-attached reference runner" (same UX, zero in-server exec)? The latter removes the
  only in-server arbitrary-code path entirely.
- **Q2.** Secrets key management: env/config-file key (proposed, D6) vs OS keyring vs "no stored
  secrets until a dedicated KMS issue". If the latter, D6 degrades to mirror-style memory-only
  secrets passed at dispatch time (works for `workflow_dispatch`, nothing else).
- **Q3.** Org-shared runners: per-repo-only v1 forces runner-per-repo sprawl for orgs; is the
  added token-scope complexity (`wrt_` bound to org + repo allowlist) worth pulling into R2?
- **Q4.** Workflow file location: `.walhub/workflows/` (proposed — avoids colliding with a future
  GitHub-compat import story for `.github/workflows/`) vs `.github/workflows/` (compat bait).
- **Q5.** Log/artifacts retention + caps defaults (§6–§7 numbers are proposals, all `[actions]`
  overridable): are 30 d / 64 MiB / 2 GiB sane for packden's own hosting bill?
- **Q6.** Workflow file format: YAML-subset hand-roll vs TOML workflows — decided in §10
  dependency analysis; Chris's call before implementation planning.

## 10. Testing strategy (per 15_testing.md tiers) + dependency analysis

Tests (law 11 — `internal/actions` ≥ 95%, `-race` mandatory, table-driven httptest per handler):
queue CAS contention (N runners × 1 job → exactly one 200, rest advance), lease expiry + sweeper
requeue + no-steal-while-heartbeating, claim-response self-containment (runner with nothing but the
claim completes), chunk idempotency (resend seq → 412 → dedup), double-redaction (secret in
`set -x` echo masked in both stored chunk and read-back), fork-PR secret denial, cap kills
(runtime/log/artifact), budget assertions (enqueue ≤ 4, poll-hit ≤ 3, poll-miss = 1 probe),
stress `-count=100` on claim + heartbeat, deadlock canary on sweep-vs-heartbeat, fake-runner e2e
(real git fixture → dispatch → run → green gate), `node --test` for the Actions lib + SDK group,
EVIDENCE.md entries for queue budgets at both population sizes.

**Dependency analysis (law 1 — no amendment proposed):** coordinator needs YAML parsing for
workflows — hand-rolled minimal parser (the workflow schema is fixed-shape; `BurntSushi/toml`
precedent shows the house style is vendored-format-parsing, and YAML-subset parsing of a strict
schema is smaller than a vendored YAML library) or, if review prefers, workflows in TOML
(`.walhub/workflows/*.toml` — zero parser code, `BurntSushi/toml` already in budget; Q6 for
Chris — **Q6.** YAML-subset hand-roll vs TOML workflows?). Secretbox is `golang.org/x/crypto`
(already amended in). Cron math reuses `internal/bundle` (mirror). Reference runner is out-of-tree
and may depend on whatever its own README justifies — it is not the server and never enters the
budget. **Reserve amendment (drafted, NOT proposed):** if a future slice wants server-side
containerized execution, propose `+ container runtime client (HTTP against Docker/Podman socket,
stdlib net/http — no new module)` as a law-1 amendment with rationale at that time; R1–R3 as
specified here never trigger it.

## Decisions & deviations from the Rust design

- **(D1, 2026-09-10, #288 plan).** Pull-based external runners; coordinator never dials. Rationale:
  runners live behind NAT on disposable hosts (walhub instances are disposable too); the GitHub +
  act_runner + Woodpecker protocols all independently converged on poll-only — three existence
  proofs beat one theory.
- **(D2, 2026-09-10, #288 plan).** Bucket-native queue + run headers with CAS discipline (new
  `actions/` family; frozen-overwritable-list amendment lands WITH the implementation change per
  14 §14.11 rule 2, same change, law 12). Rationale: law 4 — no external queue survives
  "wipe every instance"; CAS-claim is the 05/mirror arbitration pattern reused.
- **(D3, 2026-09-10, #288 plan).** Long-poll claim + 60 s leases + sweeper requeue past grace;
  idempotent effects instead of exactly-once execution. Rationale: exactly-once distributed
  execution is not on offer anywhere at this budget; leases + idempotent chunk seqs + last-write-wins
  checks make at-least-once harmless (Woodpecker stuck-agent + act_runner starvation lessons).
- **(D4, 2026-09-10, #288 plan).** `art_`/`wrt_`/`wjt_` token shapes; machine users rejected;
  frozen Principal untouched (05 precedent). Rationale: narrowest blast radius per credential class;
  role-lattice conflation is a worse bug than a third token prefix.
- **(D5, 2026-09-10, #288 plan).** In-repo `.walhub/workflows/*.yml`, strict small schema,
  label-subset scheduling; arbitrary `uses:`, matrices, schedules deferred per §8/§9. Rationale:
  the trigger sha pins the workflow revision (auditability); the scheduler stays FIFO+labels
  (act_runner shape) until load proves otherwise.
- **(D6, 2026-09-10, #288 plan).** Coordinator-agnostic execution; secretbox-sealed repo secrets
  with operator-held key (x/crypto already in budget); coordinator-side interpolation + double
  redaction. Rationale: the bucket-only secret problem is solved with stored-ciphertext +
  env-key (no new dep); masking at both ends because `set -x` defeats either end alone (LIVED).
- **(D7, 2026-09-10, #288 plan).** Additive `ReportInput` extension (no API version bump);
  immutable log chunks + content-addressed artifact blobs; SSE live-tail on the collab stream.
  Rationale: 14 §14.12 field rule covers the checks extension (gate/combined view unmodified);
  P3 immutable-events shape covers logs; the 07 byte contract covers artifacts.
- **(D8, 2026-09-10, #288 plan).** No coordinator sandbox promised; `[actions]` abuse caps
  ([import]-bounds pattern); fork-PR runs secretless until approved; runner egress is the runner
  operator's firewall. Rationale: the 14 §14.5 honest note — enforce only the observable; state
  the rest (Docker-socket=root lesson from §1.2 written down, not wished away).
- **(R1/R2/R3, 2026-09-10, #288 plan).** Trusted server-side manual runs → external pull runners →
  schedules/matrices. Rationale: each slice proves the reporting spine before widening the trust
  perimeter; R1 needs no protocol, R2 needs no scheduler beyond labels, R3 needs neither new
  tokens nor new trust.
- **(No law-1 amendment, 2026-09-10, #288 plan).** YAML-subset hand-roll or TOML workflows (Q6),
  secretbox from x/crypto, cron from internal/bundle, reference runner out-of-tree. Rationale:
  every execution-side dependency lives outside the server repo by construction (the Forgejo
  server/act_runner split is the precedent); the reserve container-socket amendment is drafted in
  §10 so a future slice cannot smuggle it in silently.

---

**Status: awaiting review — do not implement. Reviewer: Chris (crueber).**
The R1/R2/R3 slices above are proposals; implementation tickets, `internal/actions` code, workflow
parsing, token minting, UI, and the frozen-overwritable-list amendment all wait for plan approval
on issue #288.
