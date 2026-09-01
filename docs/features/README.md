# walhub — GitHub-Grade Features: Planning Documentation

This directory plans the **collaboration layer** on top of the walhub git host: the features that turn
"a very good git server" into a place where teams build software together. Everything here is
**bucket-native** — the object store defined in `docs/go/02_storage_protobuf.md` is the ONLY database.
No SQL, no Redis, no search engine, no queue. Every feature is a new family of objects under the bucket
prefix, accessed through the same `store.ObjectStore` contract, the same CAS discipline, and the same
seams (`14_extensibility.md`: route providers, auth providers, policy effects, event sinks, task kinds,
CLI subcommands).

**Relationship to the git core (the one architectural law of this layer):** the WAL stays git-only.
Issues, PRs, reviews, checks, notifications are a **parallel object family with its own CAS discipline** —
they never produce WAL entries, never touch the manifest, and never gate a push (except where a policy
effect explicitly consults them, e.g. required checks). Conversely, git data stays where it is: a PR's
commits are ordinary git objects; a PR's *head* is an ordinary ref published through the normal push path.

## Reading order

1. This file — scope, the shared object-store primitives every feature reuses, the permission model,
   and the rollout phasing.
2. Your feature doc (below).
3. `docs/go/14_extensibility.md` (the seams), `docs/go/13_concurrency.md` (the lock/race rules),
   `DEVIATIONS.md` (what walhub already decided), `AGENTS.md` (the laws).

## The documents

| Doc | Feature | Depends on |
|---|---|---|
| [`01_identity_permissions.md`](01_identity_permissions.md) | Users, orgs, teams, membership, repo roles, access.json, invitations | — |
| [`02_issues.md`](02_issues.md) | Issues: threads, comments, labels, milestones, assignees, close/reopen | 01 |
| [`03_pull_requests.md`](03_pull_requests.md) | Pull requests: opens, refs/pull/N, mergeability, merge tasks, forks | 01, 02 (shared numbering) |
| [`04_code_review.md`](04_code_review.md) | Reviews: approve/request-changes/comment, line-anchored threads, review requests | 02, 03 |
| [`05_checks_statuses.md`](05_checks_statuses.md) | Commit statuses + check runs (external CI posts results), required-checks gate | 01, 03 |
| [`06_notifications.md`](06_notifications.md) | Notifications, subscriptions, mentions (in-UI + webhook fan-out) | 02, 03, 04 |
| [`07_releases_stars.md`](07_releases_stars.md) | Releases + assets, stars, watches | 01 |
| [`08_ui_sdk.md`](08_ui_sdk.md) | Frontend surfaces + SDK surface for ALL of the above | 01–07 (shapes only) |
| [`09_rollout.md`](09_rollout.md) | Implementation phasing, agent dispatch plan, API versioning, invariants | all |

## Shared primitives (normative for every feature doc)

These are decided HERE once; feature docs reference them by name and must not reinvent them.

### P1 — The collaboration object family

All new objects live under **new top-level prefixes**, peer to `repos/`:

```
orgs/<org>/                          # org profile, members, teams (01)
users/<principal>/                   # per-user profile, prefs, notifications, stars (01, 06, 07)
repos/<o>/<r>/meta/                  # repo-collab metadata that is NOT git/WAL state
repos/<o>/<r>/issues/                # issues AND pull requests (shared numbering; 02/03)
repos/<o>/<r>/checks/                # commit statuses + check runs (05)
repos/<o>/<r>/releases/              # releases + asset metadata (07)
repos/<o>/<r>/access.json            # repo role bindings (01) — CAS'd
```

`meta/` holds collaboration state that is deliberately NOT WAL state; the WAL keeps being the source of
truth for git only. Key layout rules are the same as the store layer: lowercase, `/`-delimited, sorted
scans cheap, no LIST on a hot path (P5).

### P2 — Numbering: issues and PRs share one space (GitHub semantics)

One counter per repo: `repos/<o>/<r>/meta/next_num` — a CAS'd JSON `{"next": N}`. Allocation is a
`PutUpdate` CAS loop (freshness ~1/creation; issue/PR creation is human-rate, so CAS contention is a
non-issue — document this reasoning). Issue #7 and PR #7 cannot both exist; `thread.json.kind`
distinguishes them. PRs additionally get `refs/pull/<num>/head` published as an ordinary WAL ref.

### P3 — The thread pattern: immutable event log + CAS'd header

Every conversation (issue, PR, review thread) is:

- `…/<num>/events/<seq:012x>.json` — **immutable** event objects, `PutMode::Create`, seq zero-padded
  12 digits. One event per mutation: opened, commented, labeled, state-changed, referenced, review
  posted, check reported…
- `…/<num>/thread.json` — the CAS'd **header**: `{num, kind, title, state, labels[], assignees[],
  author, created_at, updated_at, next_event_seq, …}` plus denormalized view counters.

**Write discipline (the two-step):** CAS the header (`Update(version)`; it carries `next_event_seq`)
to *reserve* seq N, then `Create` the event object. If the Create 412s (impossible unless a bug — seq
is reserved) the header CAS is retried. If the process dies between the two steps, the reserved seq is
skipped — gaps in the event log are allowed and harmless (timeline reads by seq order, not density).
Never rewrite an event object; corrections are compensating events.

**Read discipline:** header for lists/cards, header + last-K events for thread pages, full event scan
for history. Comment pagination is by seq window, newest-last.

### P4 — CAS'd indexes with compaction

List surfaces (issue lists, PR lists, notification trays) read a CAS'd **index object** (e.g.
`issues/index.json`) that the mutating handler updates in the same two-step as P3 (index CAS carries
its own version). Indexes hold at most the hot window (open items + recently closed); older pages are
served by LIST over `…/<num>/thread.json` headers (LIST is acceptable here — these are not the git hot
path — but index-first keeps the default UI at O(1) requests). Each index records `compacted_through`;
compaction is a task when the object exceeds ~256 KiB.

### P5 — No LIST on a git hot path; LIST is fine for collaboration pages

The no-LIST rule (AGENTS law 4) applies to the git/push/fetch path. Collaboration pages MAY LIST, but
SHOULD index-first per P4. Any LIST-backed endpoint MUST be paginated and MUST say its page size.

### P6 — Permissions model (one source of truth)

Roles are ordered: `read < triage < write < maintain < admin`. They resolve to the existing
`Principal{write, admin}` flags PLUS feature-level gates (triage: label/assign/close on others' issues;
maintain: merge, push to protected refs). Resolution order for a repo:

1. `repos/<o>/<r>/access.json` role bindings (`{principal: role}` / `{team: role}` where team is
   `orgs/<org>/teams/<slug>.json` membership);
2. else org ownership (principal is an owner of the owning org);
3. else the auth principal's write/admin flags (existing behavior);
4. else anonymous → read if `anonymous_read`, nothing otherwise.

`access.json` is admin-write via the same API class as policy/settings. The policy engine's
`group:`/actor resolution (14_extensibility Seam 3) gains `team:` and role sources — identity doc owns
the exact wire shape. Protected-refs and required-checks gating stay in `policy.json` (it is the push
path's rule language).

### P7 — Heavy work is a task; SSE everywhere

Merges, forks, release asset imports, notification fan-out bursts: all `Task` kinds via the existing
task table (SSE attach, `(repo,kind)` single-flight). Live UI updates (new comments, check results) via
the SSE envelope on the repo's existing stream — no polling loops.

### P8 — Feature events feed notifications and webhooks, synchronously in the mutating handler

Unlike git events (bridge reads the WAL), collaboration mutations happen in exactly one handler on one
instance — so the handler writes notifications (06) and enqueues webhook deliveries as part of the same
request, after the CAS commits. A crash after the CAS but before fan-out loses one notification, not
data; the timeline is the backfill source of truth (same contract as the events bridge, §12.2).

### P9 — What is explicitly OUT of scope for this layer

Code search (needs an index format decision — deferred), GitHub Actions runners (walhub stores check
results, it does not run CI), Discussions, Packages, Projects/boards, SAML/SCIM. Each gets a paragraph
in its nearest doc describing the seam it would plug into.

## Rollout phasing (summary — details in 09)

1. **Phase A:** identity + permissions (01) — everything else binds to it.
2. **Phase B:** issues + notifications (02, 06 core) — proves the thread pattern end to end.
3. **Phase C:** pull requests + code review + checks (03, 04, 05) — the merge task and required-checks gate.
4. **Phase D:** releases, stars/watches, forks (07), full UI surfaces (08).
5. **Phase E:** search and the deferred list — separate planning.
