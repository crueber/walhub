# 09 — Rollout: phasing, dispatch plan, and invariants

> Source: docs/features/README.md primitives + docs/go/14_extensibility.md seams · Status: LANDED 2026-09-04 (Wave E integration). The audit below is the as-built record: §§4–5 verified with file:line evidence, the push fence measured (EVIDENCE.md E10), the full chain green (`TestE2E_CollabFullChain`), `make ci` extended to `vet test race cover contract e2e`.

## 1. What exists vs what this layer adds

The git host is DONE and green (`make ci`): store, WAL, git, server, API, bundles, events, maintenance,
policy, config, CLI, web. This layer adds collaboration objects + handlers + UI on the frozen seams. No
package under `internal/{store,wal,git}` changes except for the three touch points listed in §4.

## 2. New packages (one per feature — the agent-dispatch unit)

| Package | Doc | Contents |
|---|---|---|
| `internal/identity` | 01 | orgs, teams, members, access.json, role resolution, invitations, visibility |
| `internal/issues` | 02 | numbering, thread/event store, labels, milestones, assignees, cross-refs |
| `internal/pulls` | 03 | PR threads, refs/pull heads, mergeability, merge task, forks |
| `internal/review` | 04 | reviews, line-anchored threads, review requests, rollups |
| `internal/checks` | 05 | statuses/check-runs store, CI token scope, required-checks evaluation |
| `internal/notify` | 06 | notifications, subscriptions, mentions, webhook delivery loop |
| `internal/releases` | 07 | releases, assets, stars, watches |
| `internal/web` (extensions) | 08 | new SPA pages + SDK submodules (web/sdk/src/) |

Each package is an `api.RouteProvider` + task kinds + (where relevant) a policy-actor source. Packages
depend ONLY on `internal/{store,config,git,wal,server}` frozen contracts + `internal/identity`.

## 3. Dispatch plan (mirrors the go-spec wave strategy)

```
Wave A (1 agent):  internal/identity  — everything binds to roles; land FIRST
Wave B (3 agents): internal/issues ∥ internal/checks ∥ internal/notify (core trays)
Wave C (2 agents): internal/pulls ∥ internal/review   (need issues numbering + identity)
Wave D (2 agents): internal/releases ∥ web/ UI+SDK for ALL features (shapes are frozen by then)
Wave E (1 agent):  integration — RouteProvider registration in internal/server, policy
                    team:/role actor resolution wiring, e2e scenarios, Makefile ci additions
                    → LANDED 2026-09-04 (this doc's as-built sections are the record).
```

Concurrency: same rules as the go-spec build — frozen primitives first (this README), disjoint file
ownership, one package per agent, `go test -cover ./internal/<pkg>/` per agent, `make ci` at the end.
Coverage gate (≥95% per package) applies to the new packages from day one.

## 4. The three touch points in frozen code (each is a spec amendment, listed here for review)

1. **Auth:** principals gain a `checks:write` capability field (05) and the server's `require_read`
   consults repo visibility (01) — both in `internal/server/auth` + `internal/config` token schema.
2. **Policy:** `policy.json` gains actor sources (`team:`) and the merge-time effects
   (`require_checks`, `min_approvals`) — evaluated by the merge task, not the push path (04/05 own the
   honesty boundary: push-time policy stays push-enforceable-only).
3. **Server:** `internal/server` registers the new RouteProviders (one block per package) and the
   per-user SSE stream mounts.

Everything else is additive: new packages, new bucket prefixes, new web/ modules.

### As-built amendments (Wave E audit — law 12: the plan said X, the code does Y, Y wins)

- **Touch 1, `checks:write` is NOT a `Principal` field and NOT a config token
  scope.** The as-built credential is the bucket-native `wct_` CI token
  (`internal/checks/auth.go` shape resolution → unprivileged `ci:<id>`
  principal; the secret is verified handler-side per repo with
  `checks:write` in scopes — `internal/checks/service.go:165-181`,
  `CITokenScope` at `internal/checks/model.go:134`). The server chain
  stays shape-only via the Seam 2 `ExtraCredential` hook
  (`internal/server/auth.go:39-54`, registered in
  `cmd/walhub/checks.go:chainChecks`); `internal/config` `StaticToken`
  (`internal/config/config.go:28-34`) is unchanged — CI tokens are bucket
  objects, not config. Rationale: capability is per-repo (the token's
  home object lives in the repo), so a global principal flag would be the
  wrong shape; the handler-side check is where the repo is known.
- **Touch 1, `require_read`↔visibility LANDED as specified:** the gate
  itself (`internal/identity/gate.go:31-49` — authenticated role ≥ read,
  anonymous only on public + `anonymous_read`, host flags always allow),
  the server seam (`internal/server/server.go:50-51,95-97`,
  `internal/server/bind_api.go:57-63,99-107` — nil gate = legacy), wired
  in composition (`cmd/walhub/collab.go:buildCollab`,
  `cmd/walhub/serve.go:readGateOf`) and consulted on the git read path
  (`internal/server/smart.go`), LFS reads (`internal/server/lfs.go:53`),
  and every repo-scoped read endpoint (each package's `CheckRead` seam).
- **Touch 2 LANDED as specified:** `team:`/`role:` expansion at policy
  load (`internal/policy/expansion.go:9-46`,
  `internal/identity/gate.go:94-192`, expander bound at
  `cmd/walhub/collab.go:buildCollab` via `apiEnv.GroupExpander`);
  `require_checks` strict inside `protect`
  (`internal/policy/effect_protect.go:15,101-118`) and `required-reviews`
  (`internal/policy/effect_required_reviews.go`) evaluated by the merge
  task through the `ChecksGate`/`ReviewGate` seams
  (`internal/pulls/checks.go:18-38`, `internal/pulls/review.go`,
  call site `internal/pulls/merge.go:159`); push-time policy stays
  push-enforceable-only — `ProtectEffect.Evaluate` never consults
  `RequireChecks` (`internal/policy/effect_protect.go:154-157`), and the
  push fence (`cmd/walhub/push_budget_test.go`) fails on any collab key
  touched by a push.
- **Touch 3 LANDED as specified, in composition:** one block per package
  (`cmd/walhub/collab.go:chainCollab` — identity, issues, pulls, review,
  checks, releases, social, notify; `social` is the `internal/social`
  package of doc 07 §§4–6) plus the per-user SSE mounts that ride the
  notify handler (`internal/notify/http.go:tray/stream`,
  `internal/notify/collab.go:collabStream`,
  `internal/notify/stream.go` buses). The assembly itself was extracted
  from `serveHTTP` into `buildCollab` so the measured composition IS the
  shipped composition — pure move, no behavior change.
- **Fork manifest-sharing DEFERRED (03 §7 honest boundary):** the
  pull-fork task records fork objects only (`ForkExecutor` nil —
  `internal/pulls/merge.go:runFork`); the pack-sharing manifest copy and
  its GC enforcement land with the maintain unit. The e2e chain
  (`internal/e2e/collab_test.go` step 12) seeds the fork head by direct
  push and proves the cross-fork *open* path (numbering, routing,
  metadata), not pack sharing.

## 5. Invariants this layer must not break (from AGENTS.md laws)

1. **The bucket is the only database.** Every feature doc's object families are the schema of record;
   a feature that wants "a table" designs a CAS'd index + compaction instead (P4).
2. **The WAL stays git-only.** No collaboration mutation may write a WAL entry or touch the manifest.
   PR *refs* ride the WAL because they ARE git refs.
3. **Pushes are never gated by collaboration state** except through explicit policy effects evaluated
   in the merge/receive paths by design (required checks, protected refs) — the push fast path itself
   gains zero bucket round trips.
4. **Immutable event objects, CAS'd headers** (P3) — append-only timelines, compensating events for
   corrections, gaps allowed.
5. **No LIST on git hot paths** (P5); collaboration pages index-first.
6. **Everything long is a task** (P7); everything live is SSE.
7. **Coverage ≥ 95% per new package, -race clean** — same gate as the core.

### Wave E verification (measured, not asserted)

- **Bucket-only:** every collab family lives under `orgs/`, `users/`, or
  `repos/<o>/<r>/` (`access.json`, `meta/`, `issues/`, `pulls/`,
  `checks/`, `releases/`, `collab-events/`, `webhooks/`, `fork.json`) —
  the push fence classifies exactly these and fails on any push-path
  touch. Disk/memory hold caches only (git materializations, LRUs).
- **WAL git-only:** no collab package imports `internal/wal`
  (composition owns the registry handle); PR heads publish through the
  normal WAL funnel as ordinary refs (`cmd/walhub/pulls.go`
  `pullsPublisher`); `refs/pull/**` is server-managed under a built-in
  policy default (03 Decisions).
- **Push +0 round trips:** measured — cold push 8 ops, warm push 9 ops,
  0 collab-family keys on either
  (`cmd/walhub/push_budget_test.go`, EVIDENCE.md E10). The one by-design
  exception is priced: a `policy.json` carrying `team:`/`role:` pays
  bounded expansion reads (E2); the fence counts them as collab so the
  day a push pays them, the test names the key.
- **P3/P4 discipline:** thread pattern everywhere (immutable
  `events/<seq>.json` + CAS'd `thread.json` with `next_event_seq`;
  gaps allowed); CAS'd indexes with `compacted_through` + ~256 KiB
  inline compaction (issues index, checks index, notification tray).
- **No LIST on git hot paths:** pulls mergeability/merge/sink are 0-LIST
  (E4); review gate/recompute LIST only inside one PR's prefix (E5);
  checks gate LISTs one sha prefix under a deadline (E6). Verified by
  evidence, not by grep.
- **Tasks + SSE:** merge, fork, fan-out overflow, webhook delivery,
  retention are task kinds with progress/SSE-attach; per-user tray SSE +
  repo collab stream carry live updates (E7/E9); the chain test observes
  a task `error` (blocked merge) and `ok` (green merge) through the
  merge/task poll.
- **One behavior fix shipped with its amendment:** the audit's `-race`
  run caught a pre-existing data race (`taskTable.end` stamped `Finished`
  under the table mutex while polls snapshot under the record mutex);
  fixed in `internal/pulls/tasks.go`, pinned by
  `TestCoverTaskEndSnapshotRace`, amendment in 03 Decisions. Same-shape
  latent notes (not fixed here): `internal/notify` returns the live
  webhook record from `StartWebhooks` but discards it at every production
  call site (`TaskStatus` copies under the table mutex — consistent
  today); `internal/wal` uses its own broadcast protocol, unexamined.

## 6. API versioning

All collaboration endpoints mount under the EXISTING lanes (`/{o}/{r}/api…`, `/api/v1/…`) as additive
routes via RouteProviders. Breaking changes get a new lane segment per D15/D20 (no infrastructure
work). Wire shapes follow 07_api.md conventions exactly: `[]`-not-null, RFC3339, full SHAs, plain-text
errors, `ETag`/SWR classes (sha-addressed ↔ immutable only applies to git objects; collaboration
responses are `no-store` or short-SWR keyed by the thread header version).

### Wave E confirmation: additive-only HELD

The integration added no route, no field, no bucket family, no task
kind, and no policy effect. The complete Wave E surface is: one e2e
scenario, one `cmd/`-side budget fence, the `buildCollab`/`chainCollab`
extraction (pure move), `make ci` + GH workflow additions, and doc
entries. Three endpoints arrived additively in their own waves with
decisions recorded where they landed (03: `POST …/pulls/{num}/comments`,
`GET …/pulls/{num}/merge/task`; the per-wave docs own those notes) —
09 itself ships zero product surface.

## 7. Risks & mitigations (the honest list)

| Risk | Mitigation |
|---|---|
| CAS'd index objects are a contention point on very active repos | Human-rate mutations; index CAS is one retry loop; P4 compaction keeps objects small |
| Thread headers grow (comment counts, rollups) | Header holds bounded state; unbounded history lives in immutable events |
| Fork object sharing vs retention GC | Fork manifests reference packs that retention MUST NOT collect — the GC rule gains "referenced by any live manifest" (maintenance amendment, Wave C) |
| Notification fan-out write amplification (big teams) | Dedup by deterministic id (P8); fan-out is a task; per-user index is the tray |
| Search without a database | Deferred (P9) — the event logs and index objects are the future corpus; a bucket-native index format is its own planning doc |

### Wave E re-check (measured outcomes)

- **Index contention:** no contention observed at human rate anywhere in
  the chain (every index CAS in the 2.2 s run succeeded first try); the
  retry loops are unit-pinned in each package. Verdict: mitigation holds,
  no change.
- **Thread headers:** bounded by construction (counters + rollups only);
  the chain's threads stayed single-KiB. Verdict: holds.
- **Fork GC:** DOWNGRADED to a specified-but-unenforced rule — manifest
  sharing is deferred (ForkExecutor nil), so there is no cross-manifest
  pack sharing for GC to consult yet. The rule text in 03 §7 stands as
  the contract the maintain unit will enforce when sharing lands; until
  then retention can only see single-manifest references, which is safe
  (nothing shared yet). No action in 09 beyond recording this honestly.
- **Fan-out amplification:** measured E7 (2+2 per recipient, 100-sync cap
  with task fallback, deduped replays write-flat). The chain's own
  emissions delivered to tray + webhook in ~2 ms. Verdict: holds.
- **Search:** still deferred (P9); the chain adds no new corpus format.
  Verdict: unchanged.
