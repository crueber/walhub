# 05 — Checks & statuses: the external-CI integration point

> Depends on: `01_identity_permissions.md` (principals, roles), `03_pull_requests.md` (merge task, open-PR index).
> Seams used: 1 (route providers), 2 (auth providers), 3 (policy effects), 5 (task kinds) — see
> `docs/go/14_extensibility.md`. Shared primitives: P1 (object family), P4 (CAS'd index + compaction),
> P5 (no LIST on hot paths), P6 (roles), P7 (tasks + SSE), P8 (synchronous fan-out). Wire conventions,
> cache classes, SSE envelope: `docs/go/07_api.md`. Concurrency playbook: `docs/go/13_concurrency.md`.
>
> **Stance:** walhub **stores** CI results and gates merges on them; it never runs CI (P9). Any CI
> system (Woodpecker, Jenkins, a cron job) reports per-commit statuses over the write API. The merge
> gate consults stored results only — receive-pack is never blocked by a status (the honest note in
> `14_extensibility.md` §14.5: process that happens elsewhere is not enforceable at push time).

## 1. Scope — statuses in v1; check runs deferred

v1 ships **commit statuses**: one mutable record per `(sha, context)` pair — the GitHub "status"
model. The richer **check-runs** model (actions/steps, annotations, per-check SHA-addressed artifacts)
and **check-suites** (grouping runs by ref/branch) are deferred. The seam note: both would be new key
families under `repos/<o>/<r>/checks/` registered by this package; the `require_checks` policy effect
and the combined-status API take a `[]string` of context names, which is the same shape a check-run
name would fill. Nothing in the v1 schema needs a migration to add them — `checks/<sha>/<context>.json`
gains siblings, never changes shape.

`internal/checks` is the feature package. It registers one `RouteProvider` (Seam 1), one
`auth.Provider` (Seam 2), zero new policy effects (§6 extends the existing `protect` effect), zero new
task kinds beyond index compaction (Seam 5), and depends only on `api.Env` seam interfaces.

## 2. Object family (P1)

All keys under `repos/<o>/<r>/checks/` — bucket-native, lowercase, `/`-delimited, sorted-scan friendly:

| Key | Kind | Write model | Purpose |
|---|---|---|---|
| `checks/<sha>/<context>.json` | status record | **Create** first report, **CAS Update** thereafter | one CI line item per commit |
| `checks/index.json` | CAS'd index (P4) | CAS Update only | checks table page, PR fast path |
| `meta/ci_tokens/<id>.json` | CI token record | Create on issue, CAS Update on revoke | `checks:write` credentials |

`<sha>` is the full 40/64-hex commit sha. `<context>` is a CI label (e.g. `ci/build`, `lint`,
`woodpecker/test`): charset `[A-Za-z0-9._/-]`, 1–100 chars, MUST NOT start/end with `/` or end with
`.json` (validated on write; contexts containing `/` simply extend the key — LIST by the
`checks/<sha>/` prefix still groups them). Status records are **Create-then-CAS**: the first report of
a context is `PutMode::Create` (a 412 means the context already reported for this sha — the caller is
re-reporting, so the handler retries as a CAS Update); every later report is a CAS `Update(version)`
overwrite, last-write-wins. Old results are **never deleted**; a re-run overwrites the same object.

Status object schema (`checks/<sha>/<context>.json`):

```json
{
  "sha": "cb38da1…",                       // full 40/64-hex
  "context": "ci/build",
  "state": "pending",                      // pending | success | failure | error
  "target_url": "https://ci.example/…",    // optional, ≤ 2 KiB, absolute http(s)
  "description": "Build #123",             // optional, ≤ 256 chars
  "started_at": "2026-09-01T10:00:00Z",    // optional, RFC 3339 UTC
  "completed_at": "2026-09-01T10:04:31Z",  // optional; cleared on a re-report of pending
  "creator": "ci:t3k9",                    // principal name of the reporter
  "created_at": "2026-09-01T09:58:00Z",    // first report time
  "updated_at": "2026-09-01T10:04:00Z"     // last CAS write
}
```

Checks index (`checks/index.json`, P4 discipline): CAS'd, `{"compacted_through":"", "shas":[{"sha",
"state","contexts":[{"name","state","updated_at"}],"updated_at"}]}` — newest-sha first, hot window
capped at 256 KiB. It is a *projection*: the table page and the PR head-sha fast path read it; the
combined view (§5) always reads the canonical per-context objects. Same size cap and refresh rules as
P4; compaction prunes shas whose newest update is older than the hot window (default: newest 500 shas)
and runs as task kind `checks-index-compact` (Seam 5 registration; `(repo, kind)` single-flight per §6.8).

Write discipline for one report (the mutating handler, one instance — P8):

1. Validate context charset, state enum, and that `sha` resolves to a commit (`RepoView.Commit`).
2. Try `Create` of `checks/<sha>/<context>.json`; on 412, read the existing record and CAS-update it.
3. CAS-update `checks/index.json` (upsert this sha's context row; new sha → prepend).
4. Broadcast the SSE `check` packet (§7) and, on failure/error, enqueue notifications (§8) — all
   after the CAS commits, synchronously in-request per P8.

### Concurrency

Hazard: two reports racing for the same `(sha, context)` CAS the same object. Avoidance: by
construction this is contention-free — the v1 contract is **one CI system per context** (a second
system reusing a context is a configuration error on their side, and CAS makes the outcome still
well-defined: last write wins, no corruption). The index CAS is the only multi-writer point; it is a
plain CAS retry loop (`13_concurrency.md` §3 pattern — the CAS *is* the lock; no new repo lock, no
cross-feature lock). A lost index update (writer died between step 2 and 3) costs one stale table row
until the next report — the timeline of per-context objects is the backfill truth, same contract as
P3/P8. No new locks: this family never touches `syncMu`/`packMu`/`rw` (13_concurrency.md §2).

## 3. CI tokens — the `checks:write` credential class

External CI authenticates with a **static CI token**: an operator-created secret scoped to one repo
and one capability. It is NOT a repo-write credential — a leaked CI token cannot push, merge, or edit
the repo.

Token record (`repos/<o>/<r>/meta/ci_tokens/<id>.json`, CAS'd):

```json
{
  "id": "t3k9",                          // short opaque id, 8 chars [a-z0-9]
  "name": "woodpecker",                  // display name, required
  "token_hash": "<hex sha-256 of the secret>",
  "scopes": ["checks:write"],            // v1: exactly ["checks:write"]; the field exists for growth
  "created_by": "jane",
  "created_at": "2026-09-01T09:00:00Z",
  "revoked_at": null
}
```

The secret is issued **once** in the POST response as `wct_<id>.<secret>` (`wct_` keeps the `wgt_`
HMAC-token prefix convention distinct — D-NAME-1 keeper family). Only `token_hash` is stored; a
revoked record is kept (revoked_at set) so old credentials fail with 403, not 401-if-absent ambiguity.
Namespaced to the repo by construction — the token grants nothing outside it.

**Auth path (Seam 2, 14_extensibility.md §14.4):** a compiled-in provider claims the `wct_` prefix
(startup validates no overlap). Because authentication is repo-scoped but the chain sees only the
credential, the provider verifies the token id format and resolves an *unprivileged* principal
`{name: "ci:<id>", write: false, admin: false}` — the frozen `Principal` is NOT extended. The scoped
capability is checked **in the handler**: `POST …/checks/statuses/{sha}` loads
`meta/ci_tokens/<id>.json` (one conditional GET), compares `token_hash`, requires `checks:write` in
`scopes` and `revoked_at == null`. Mismatch = 401; valid but revoked = 401 (git-style: make the
client erase it); valid but no `checks:write` = 403.

## 4. API surface

Repo-scoped, mounted on both lanes via `api.Lanes` (Seam 1); plain-text errors, `[]`-not-null,
RFC 3339, full SHAs per `07_api.md` §2. All checks GETs are **no-store** (statuses mutate in place, so
neither cache class of §9.2 applies — sha-addressed does NOT mean immutable here).

| Method + path | Auth (P6) | Request → response | Seam / notes |
|---|---|---|---|
| `POST /{o}/{r}/api/checks/statuses/{sha}` | CI token (`checks:write` on this repo) **or** repo `write` role | `{context, state, target_url?, description?, started_at?, completed_at?}` → `200` full status record | `internal/checks` RouteProvider; `400` bad context/state/URL; `404` unknown sha (not a commit); `409` state not in the enum |
| `GET /{o}/{r}/api/checks/statuses/{sha}` | read | `{sha, statuses: []}` (context-sorted) | no-store; LIST under `checks/<sha>/` + bounded parallel GETs |
| `GET /{o}/{r}/api/checks/{sha}` | read | combined view (§5): `{sha, state, total_counts, statuses: []}` | no-store |
| `GET /{o}/{r}/api/checks?after=&n=` | read | `{checks: [{sha, state, contexts: []}], more}` — index page, name-cursor `after`, `n` default 50 max 200 | P5 paginated index read |
| `POST /{o}/{r}/api/checks/tokens` | admin | `{name, scopes?}` → `201 {id, token, scopes}` (secret shown once) | admin-only per P6 |
| `GET /{o}/{r}/api/checks/tokens` | admin | `{tokens: [{id, name, scopes, created_by, created_at, revoked_at}]}` (no secrets) | no-store |
| `DELETE /{o}/{r}/api/checks/tokens/{id}` | admin | `204` (sets `revoked_at`) | — |

No SSE-attach endpoints here: check reports are instant CAS writes, never long work (P7) — the *PR
stream* consumes them, §7. `GET /api/v1` discovery lists these routes with `Name() == "checks"`.

### Concurrency

Hazard: a report storm (many contexts finishing at once) stampeding the index CAS and blocking reads.
Avoidance: handlers are two short bucket ops (status object + index) with bounded CAS retries (≤ 5,
then 503); reads are conditional GETs — no locks, no goroutines, no singleflight needed (a CAS retry
loop IS the single-flight here: losers re-read and re-apply). Reports are CI-rate, never git-hot-path;
per `13_concurrency.md` §2 rule 4 nothing here may run under a repo lock.

## 5. Combined status (GET per sha)

`GET /{o}/{r}/api/checks/{sha}` aggregates worst-of over the sha's contexts:

| Any context in… | Combined `state` |
|---|---|
| `error` | `error` |
| `failure` | `failure` |
| `pending` (or no statuses at all) | `pending` |
| all `success` | `success` |

Precedence: `error > failure > pending > success`. Zero contexts ⇒ `state: "pending"`,
`total_counts` all `0` — a caller cannot distinguish "not started" from "in flight", which is exactly
what a gate needs. Response: `{sha, state, total_counts:{pending,success,failure,error},
statuses:[{context,state,…}]}`.

## 6. Required checks — the `require_checks` policy extension + merge gate

`policy.json`'s existing `protect` effect (Seam 3 registry; the effect union stays open per
`14_extensibility.md` §14.5) gains one optional field:

```json
{ "name": "main-needs-ci", "match": { "refs": ["refs/heads/main"] },
  "effect": { "protect": { "require_checks": ["ci/build", "lint"] } } }
```

Unknown keys *elsewhere* in the effect are still parse errors (fail closed); `require_checks` itself
is a strict list of 1–32 validated context names. Combination rule (declared here per §14.5): across
multiple matching rules the contexts are **unioned**; the load-time cross-rule check is extended
accordingly (two `protect` rules on the same ref with disjoint bypass lists already fail load —
unchanged). Rolling-upgrade caveat from §14.5 applies: ship the binary before repos adopt the key.

**Evaluation point — the MERGE task (03), at merge time, only.** This is deliberately NOT a push
effect: statuses arrive asynchronously after a push, and receive-pack cannot see them (the §14.5
honest note). When the merge task runs for PR *N* onto a protected ref whose rule carries
`require_checks`, it:

1. resolves the PR head sha,
2. reads the combined view for that sha,
3. refuses unless **every** required context is present AND `state == "success"`.

Refusal is the task's normal failure: plain-text message listing each offender verbatim —
`merge refused: required checks not green for <sha>: ci/build (failure), lint (missing)` — surfaced
on the task stream and the PR page. No bypass list: bypassing the gate means editing `policy.json`
(admin, audited via settings history). Direct pushes to the protected ref are not gated by this field
(CI cannot have reported for a commit that was just pushed); teams wanting push-side control use the
existing protect rules.

### Concurrency

Hazard: the merge task reading statuses while a late CI report lands — a merge that passed a stale
`pending`. Avoidance: accepted by design and made harmless by ordering: the merge task takes its
*last* read of the combined view immediately before the manifest CAS commit, and a report arriving in
between either targets the pre-merge sha (irrelevant) or the head sha after a fast-forward (the ref
already moved; a subsequent push re-evaluates). The gate never takes a lock shared with the checks
family — it reads CAS'd objects only, so there is no cross-feature lock to order (13_concurrency.md
§2 lists remain untouched).

## 7. SSE — check reports on the collaboration stream (P7)

Every report (step 4 above) broadcasts one packet on the single repo collaboration SSE stream — the
same stream 03's `pull` events ride; there is NO checks-specific endpoint. Clients filter by event
name; the repo hub does not:
```
event: check
data: {"sha":"cb38da1…","context":"ci/build","state":"success","combined_state":"pending","updated_at":"…"}
```

The PR page subscribes to that stream and keeps `check` frames whose `sha` equals the PR's current
head sha. Publishing is the drop-oldest broadcast of the existing repo hub — a slow client never
blocks a CI report (07_api.md §6 backpressure rules).

## 8. Notifications on failure (06's contract, P8)

On a transition into `failure` or `error` whose sha is the head of an **open** PR (resolved via the
shared `issues/index.json` — cards carry `head_sha`, filter `kind: "pr"`; 03 §2/§8 — bounded lookup
at collaboration rate), the handler synchronously enqueues the 06 activity-log action
`check_reported` (06's §5.3 enum — emitted for transitions into `failure` or `error` ONLY), payload
`{sha, context, state, description?, target_url?, pr?}` with reason `subscribed`. 06's fan-out
computes the recipients (PR participants); this handler only enqueues the event, synchronously per
P8: after the CAS commits, in the same request, best-effort. A crash after CAS loses one
notification, not data. Success/pending transitions emit nothing (a green check is not an
interruption).

## 9. UI & SDK

Pages (vanilla ESM SPA, `12_web_ui.md` §2 patterns; SSE via `lib/sse.js` `mountStream`, never
`EventSource`):

| Surface | Behavior |
|---|---|
| `/:owner/:repo/checks` | checks table page: paged index (`GET …/checks`), state pill per sha, expandable per-context rows, filter by state/context; live rows via the `check` SSE event |
| Commit page + commits list | one **check pill** per commit (combined state, colored dot); click → per-context list with `target_url` links and descriptions |
| PR page | check pills on the head sha; the merge button is disabled with a tooltip listing missing/failing required contexts (fetched from the combined view + the repo's policy); pill updates live from the `check` stream — no polling |
| Repo settings → CI tokens | admin-only: create (secret shown once, copy button), list, revoke |

SDK additions — new submodule `web/sdk/src/checks.js` (esbuild-bundled per D-WEB-2), group `checks` on
the repo client: `combined(sha)`, `statuses(sha)`, `list({after, n})`, `report(sha, payload)` (used by
external CI with a `wct_` token), `tokens.{create, list, revoke}` (admin). Registered in `index.js`
re-exports; envelope/SSE parsing reuses `sdk/src/sse.js` — one parser (12_web_ui.md §1.4).

## Decisions

- **v1 is statuses-only; check runs/suites deferred** — the context model covers required-check gating; suites add grouping, not new gates; the key layout leaves room without a migration.
- **One mutable record per `(sha, context)`, CAS overwrite** — matches the assignment's contention-free-by-construction model; history-of-attempts is deliberately not kept.
- **CI tokens are per-repo records with a `scopes` field, resolved through an unprivileged principal + handler-side capability check** — keeps the frozen `Principal{write, admin}` untouched (a frozen contract) while scoping leaks to one repo and one capability.
- **Secret format `wct_<id>.<secret>`, only `sha256` hash stored; revoked records retained** — id-in-token enables repo-local lookup with one conditional GET; retention makes revocation unambiguous.
- **Combined = worst-of with `error > failure > pending > success`, zero contexts ⇒ `pending`** — matches the aggregate-intuition contract; the merge gate separately distinguishes "missing" from "failing".
- **`require_checks` lives inside the existing `protect` effect, evaluated by the merge task, union across matching rules** — statuses cannot exist at receive-pack time (honest note, 14.5), so the gate belongs where the merge decides.
- **Combined/status GETs are no-store, not sha-addressed-immutable** — sha-addressed content here mutates by design; misclassifying it as immutable would cache stale reds/greens forever.
- **No new task kind except `checks-index-compact`** — reports are short writes; only index compaction outgrows a request, and it follows P4's compaction rule.
- **Failure notifications only for head shas of open PRs** — every context failure for every sha would spam; the PR is the review surface that cares.

## Explicitly out of scope

- **Running CI** — walhub never executes builds (P9); it is a results store and a gate.
- **Check runs, actions, annotations, suites** — deferred behind the §1 seam note; v1 schema does not pre-announce them.
- **Status history / audit log per context** — last write wins; CI systems keep their own logs.
- **Per-repo required-check defaults outside `policy.json`** — the protect effect is the only gate language; no settings.json parallel.
- **CI token scopes beyond `checks:write`** — the `scopes` field exists, but no second capability is defined; granting repo-write or admin via a CI token is explicitly rejected.
- **Merge queues / batching** — the gate is per-merge-task; a queue is 09-rollout territory and deferred.
