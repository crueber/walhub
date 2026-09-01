# 09 — Rollout: phasing, dispatch plan, and invariants

> Source: docs/features/README.md primitives + docs/go/14_extensibility.md seams · Status: plan of record for implementing the collaboration layer.

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

## 6. API versioning

All collaboration endpoints mount under the EXISTING lanes (`/{o}/{r}/api…`, `/api/v1/…`) as additive
routes via RouteProviders. Breaking changes get a new lane segment per D15/D20 (no infrastructure
work). Wire shapes follow 07_api.md conventions exactly: `[]`-not-null, RFC3339, full SHAs, plain-text
errors, `ETag`/SWR classes (sha-addressed ↔ immutable only applies to git objects; collaboration
responses are `no-store` or short-SWR keyed by the thread header version).

## 7. Risks & mitigations (the honest list)

| Risk | Mitigation |
|---|---|
| CAS'd index objects are a contention point on very active repos | Human-rate mutations; index CAS is one retry loop; P4 compaction keeps objects small |
| Thread headers grow (comment counts, rollups) | Header holds bounded state; unbounded history lives in immutable events |
| Fork object sharing vs retention GC | Fork manifests reference packs that retention MUST NOT collect — the GC rule gains "referenced by any live manifest" (maintenance amendment, Wave C) |
| Notification fan-out write amplification (big teams) | Dedup by deterministic id (P8); fan-out is a task; per-user index is the tray |
| Search without a database | Deferred (P9) — the event logs and index objects are the future corpus; a bucket-native index format is its own planning doc |
