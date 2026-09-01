# 04 — Code review: approvals, line-anchored threads, review requests

> Depends on: `02_issues.md` (thread pattern applied to issues), `03_pull_requests.md` (PR objects,
> merge task, PR page). Shared architecture: P1–P9 in `README.md`. Seams: `docs/go/14_extensibility.md`
> (route providers §14.3, policy effects §14.5). Concurrency patterns: `docs/go/13_concurrency.md`.
> Wire conventions: `docs/go/07_api.md` §2. UI: `docs/go/12_web_ui.md`.

## 1. Scope

Code review is a **sidecar object family** peer to the PR conversation: reviews, line threads, and
review requests never touch the WAL, never gate a push (except via the policy effect's one enforceable
half, §6), and are enforced where review state is actually observable — inside **03's merge task**,
not receive-pack. This is the policy docs' honesty rule (`14_extensibility.md` §14.5): receive-pack can
enforce *who pushes the final ref*; it can never enforce "the PR had two approvals."

Non-goals carried from P9: no code-ownership/CODEOWNERS routing, no merge queues, no review analytics.

## 2. Object family

All keys under the P1 collaboration prefix. `num` is the shared issue/PR number (P2); `tid` is a
zero-padded 8-hex thread id (`0000002a`); event seqs are zero-padded 12-hex (P3).

| Key | Kind | Content |
|---|---|---|
| `repos/<o>/<r>/pulls/<num>/reviews/<seq:012x>.json` | immutable (`Create`) | review events: `kind:"review"` or compensating `kind:"review_dismissed"` (§3) |
| `repos/<o>/<r>/pulls/<num>/threads/<tid>/thread.json` | CAS'd | thread header: anchor, resolution state, comment counter (§4) |
| `repos/<o>/<r>/pulls/<num>/threads/<tid>/events/<seq:012x>.json` | immutable (`Create`) | `kind:"review_thread_comment"` events (§4) |
| `repos/<o>/<r>/pulls/<num>/review-requests.json` | CAS'd | current requested reviewers (§5) |
| `repos/<o>/<r>/issues/<num>/thread.json` | CAS'd (03 owns) | PR header; 04 adds `next_review_seq`, `next_thread_num`, `review_summary` (§7) |

`pulls/<num>/` is the review subtree of the collaboration family (the 14.10.4 sketch's layout); the PR
conversation header stays at `issues/<num>/thread.json` because numbering is shared (P2). The CAS'd key
families `pulls/<num>/review-requests.json` and `pulls/<num>/threads/*/thread.json` join the frozen
overwritable list (14 §14.11 rule 2) via the `03`/`04` spec revision — an unlisted mutable key is a bug.

### Concurrency

Hazard: two writers racing on one CAS'd object (PR header, thread header, review-requests) lose an
update. Avoidance: every mutation is a `PutUpdate(version)` CAS loop with 412 → re-read → recompute →
retry (13_concurrency.md playbook; CAS **is** the lock — no cross-feature locks, no sidecar mutexes,
no `.lock` objects). Allocation counters (`next_review_seq`, `next_thread_num`) live on the PR header
so a review submit and a thread open arbitrate on ONE CAS. Crash between header CAS and event `Create`
skips a seq — gaps are allowed and harmless (P3).

## 3. Reviews

A review is one immutable event, `Create`-only:

```json
{ "kind": "review", "seq": 42, "at": "2026-09-01T12:00:00Z", "by": "alice",
  "state": "APPROVED" | "CHANGES_REQUESTED" | "COMMENTED",
  "commit_sha": "<full 40/64-hex head sha at submit time>",
  "body": "markdown-lite text, \"\" allowed" }
```

- `commit_sha` MUST equal the PR head at submission (server-checked; otherwise `409` plain text
  `reviewed commit is not the pull request head`). This pins every approval to a tip and makes
  staleness a pure function (§6) — no background dismisser ever runs.
- A review with `threads` in the request atomically opens line threads (§4) before the review event
  lands; the review event is the summary record.
- **Dismissal is a compensating event** (events are never rewritten, P3):
  `{ "kind": "review_dismissed", "seq": 43, "at": …, "by": "<maintain principal>",
     "dismisses": 42, "reason": "stale / wrong reviewer / …" }` — `maintain` role only.
- Sequences reserve via `next_review_seq` on the PR header CAS (two-step P3 discipline), then `Create`.
- Timeline: reviews appear on the PR page as cards driven by `review_summary` + `GET reviews`; the PR
  event log (03) stays untouched — no duplicate marker events.

## 4. Line-anchored threads

Each thread is a full P3 conversation: CAS'd header + immutable comment events, grouped by `tid` (the
directory IS the thread id; every comment in a thread shares it).

**Anchor spec (normative):**

```json
{ "path": "src/main.go", "side": "NEW",
  "old_start": 0,  "old_lines": 0,
  "new_start": 120, "new_lines": 3,
  "commit_sha": "<head sha the anchor was computed against>",
  "context_sha": "<hex sha-256 of the drift hash>" }
```

- `side` selects the file the lines index into: `NEW` (added/unchanged; `new_start/new_lines` > 0) or
  `OLD` (pure-deletion anchors; `old_start/old_lines` > 0). Renames anchor on the NEW display path
  (matches the server's `stats[]` convention, 12 §2.8).
- **Drift hash (normative):** `context_sha` = hex SHA-256 over `path + "\n"` followed by up to 3
  unchanged context lines *before* and up to 3 *after* the anchored range, verbatim except
  trailing-whitespace-trimmed, LF-joined. Computed from the hunk the comment was made on.
- Drift detection is **derived, never stored**: at view time the client recomputes the hash from the
  diff (§8) — mismatch ⇒ the thread renders as *outdated* (collapsed, original line shown); it is never
  silently relocated and never mutated. Same derivation for the merge gate, so no writer owns
  staleness.

Comment event: `{ "kind": "review_thread_comment", "seq": 7, "at": …, "by": …, "body": "…" }`. Thread
header:

```json
{ "tid": "0000002a", "num": 7, "kind": "review_thread",
  "anchor": { …as above… },
  "resolved": false, "resolved_by": "", "resolved_at": "",
  "comment_count": 3, "next_event_seq": 8, "created_at": …, "updated_at": … }
```

Opening: reserve `tid` from `next_thread_num` on the PR header CAS, `Create` the header with the first
comment as event seq `000000000001`. Resolution: CAS the thread header only (`resolve`/`unresolve`
records `resolved_by`/`resolved_at`); comments CAS the header two-step (reserve seq → `Create`).

### Concurrency

Hazards: (a) concurrent comments on one thread race the header CAS — avoided by the P3 two-step with
412-retry; the reserved-seq discipline makes `Create` unambiguous. (b) Thread open races a concurrent
review submit for `next_thread_num`/`next_review_seq` — one CAS on the PR header arbitrates; loser
re-reads and retries. (c) Resolve racing a comment: both CAS the same header; last writer wins on a
field-disjoint merge is NOT assumed — handlers re-read, recompute the full header, and retry; a
resolve that loses simply re-applies to the newer version.

## 5. Review requests

`review-requests.json` is the **current-state index**; the audit trail lives in the PR timeline as
`review_requested` / `review_request_removed` events emitted by 03's event log (03 owns PR event
kinds; 04 only emits through its handler per P8).

```json
{ "version": 1, "updated_at": "2026-09-01T12:00:00Z",
  "reviewers": [ { "principal": "bob", "by": "alice", "at": "…" } ] }
```

- `POST` adds entries (dedup by principal — re-request is a no-op); `DELETE` removes. Entries are
  removed implicitly when the principal submits a review or dismisses.
- Auth: the PR author or `triage`+; a requested principal may self-remove.
- `GET …/review-suggest?q=` merges, in order: `access.json` role bindings with role ≥ `read` (P6),
  org-team members of those bindings, then authors of commits in the PR's head branch (from
  `repo.commits` metadata); prefix-filtered by `q`, page size 20, no-store. LIST scope is bounded by
  the binding set — collaboration page, P5 fine.

### Concurrency

Hazard: `POST` and `DELETE` racing one `review-requests.json` (lost add/remove). Avoidance: single CAS
loop; the write is idempotent per entry (dedup by principal), so a 412 retry after re-read converges.
A review-submitted removal racing a re-request is last-writer-wins by CAS order — acceptable (both are
explicit human actions; the timeline shows both).

## 6. Review rollup + required reviews (the merge gate)

**Rollup (denormalized on the PR header, 03's `thread.json`):**

```json
"review_summary": {
  "decision": "APPROVED" | "CHANGES_REQUESTED" | "REVIEW_REQUIRED",
  "latest": { "alice": { "state": "APPROVED", "seq": 42, "commit_sha": "…", "at": "…" },
              "bob":   { "state": "DISMISSED", "seq": 39, "commit_sha": "…", "at": "…" } },
  "approvals": 1, "requested": ["carol"],
  "threads_total": 12, "threads_unresolved": 2 }
```

- Rule: per reviewer, the **latest** review event in seq order wins; a `review_dismissed` targeting
  seq N demotes that reviewer's latest to `DISMISSED`. `decision` = `CHANGES_REQUESTED` if any
  surviving latest is `CHANGES_REQUESTED`; else `APPROVED` if ≥ 1 surviving `APPROVED`; else
  `REVIEW_REQUIRED`. `review_summary` is policy-independent; policy minima apply only at the gate.
- Staleness is derived: `latest[x].commit_sha != current head` ⇒ stale (UI badge; gate rule below).
- The review handler recomputes the summary **inside its CAS loop** by scanning the immutable review
  events (LIST over `pulls/<num>/reviews/` — low-volume collaboration subtree, P5-fine). The summary is
  a pure function of the immutable event set, so any racing writer computes the same value: 412-retry
  converges without coordination.

**Required reviews — policy effect `required-reviews`** (registered via Seam 3, `policy.RegisterEffect`):

```json
{ "name": "pr-gate", "match": { "refs": ["refs/heads/main"] },
  "effect": { "required-reviews": { "min_approvals": 2, "dismiss_stale": true,
                                    "bypass": ["group:admins", "svc:merge-queue"] } } }
```

- **Push-time half (enforceable, honest):** at receive-pack the effect behaves exactly like the
  `review-required` sketch of 14 §14.5 — deny direct (non-bypass) pushes to matched refs. Pure, local
  evaluation per the Seam-3 concurrency rule.
- **Merge-time half (the review gate):** 03's merge task, before publishing the merge ref, resolves
  every `required-reviews` rule matching the PR's **base** ref and requires: surviving approvals ≥
  `min_approvals` (most restrictive across matching rules), no surviving `CHANGES_REQUESTED`, and —
  when `dismiss_stale` — only approvals whose `commit_sha` equals the current head count. Failed gate ⇒
  task narration states the shortfall (law 7) and the merge ref is not published.
- Envelope rules are frozen: unknown keys inside the effect are parse errors, fail closed (400 on
  `PUT …/policy`); load-time compatibility check: overlapping `required-reviews` rules on the same ref
  combine most-restrictively, never fail the load (unlike disjoint-bypass `protect`).

### Concurrency

Hazard: the gate reading `review_summary` while a review lands (stale cache deciding a merge).
Avoidance: the gate NEVER trusts the denormalized summary — it re-derives the verdict by scanning the
review events at merge time (authoritative scan; the summary is a render cache). The scan is inside the
merge task's context with its own deadline; no locks are taken (13_concurrency.md — CAS-derived state
needs none). `svc:merge-queue` exclusivity comes from 03's `leases/merge-pull-<n>.pb`, not from 04.

## 7. API endpoints

Registered by `internal/review` as a `RouteProvider` (Seam 1); repo-scoped routes mounted on BOTH lanes
via `api.Lanes` (14 §14.12 two-lane rule). All responses `no-store` (ref-independent); errors are
plain text (07 §2); arrays serialize `[]`; timestamps RFC 3339 UTC; SHAs full-hex.

| Method + path | Auth (P6) | Request → response |
|---|---|---|
| `GET /{o}/{r}/api/pulls/{num}/reviews` | read | → `{reviews: [review event], more}` (paged, `n` default 50) |
| `POST /{o}/{r}/api/pulls/{num}/reviews` | read | `{state, body?, commit_sha, threads?: [{anchor, body}]}` → `{review, threads[]}`; author self-approve/request-changes → `422` |
| `GET /{o}/{r}/api/pulls/{num}/reviews/{seq}` | read | → review event; `404` unknown |
| `POST /{o}/{r}/api/pulls/{num}/reviews/{seq}/dismiss` | maintain | `{reason}` → `{review: DISMISSED …}` |
| `GET /{o}/{r}/api/pulls/{num}/threads` | read | → `{threads: [thread header], more}` (`?resolved=` filter) |
| `POST /{o}/{r}/api/pulls/{num}/threads` | read | `{anchor, body}` → `{thread}`; drift-hash verifiable server-side |
| `GET /{o}/{r}/api/pulls/{num}/threads/{tid}` | read | → `{thread, comments: [events]}` (seq window) |
| `POST /{o}/{r}/api/pulls/{num}/threads/{tid}/comments` | read | `{body}` → `{comment}` |
| `POST /{o}/{r}/api/pulls/{num}/threads/{tid}/resolve` | read (opener, review participants) or triage+ | → `{thread}` |
| `POST /{o}/{r}/api/pulls/{num}/threads/{tid}/unresolve` | same as resolve | → `{thread}` |
| `GET /{o}/{r}/api/pulls/{num}/review-requests` | read | → `{reviewers: [...]}` |
| `POST /{o}/{r}/api/pulls/{num}/review-requests` | author or triage+ | `{reviewers: [principal]}` → `{reviewers}` |
| `DELETE /{o}/{r}/api/pulls/{num}/review-requests` | author/triage+ or self | `{reviewers: [principal]}` → `{reviewers}` |
| `GET /{o}/{r}/api/pulls/{num}/review-suggest?q=` | read | → `{suggestions: [principal]}` (20/page) |

Discovery: all routes land in `/api/v1` `endpoints[]` with provenance `review` (07 §9.6). Mutations
fan out notifications synchronously after the CAS commits (P8) via 06's seam, and publish SSE packets
on the repo stream: event names **`review`** (posted/dismissed; payload carries the new
`review_summary`) and **`thread`** (comment/resolution; payload carries `tid`), using the 07 §6
envelope.

### Concurrency

Hazard: a handler blocking a request goroutine on multi-object writes (review + N threads + summary
rescan) starving the control plane (14 §14.3). Avoidance: a review submit performs at most one LIST of
the low-volume review subtree plus per-thread two-steps; no git subprocesses, no bulk reads, no tasks —
everything is control-plane-sized. The `(repo, kind)` task table is untouched: **04 registers no task
kinds**; the only long work in its dependency graph is 03's `merge` task, which owns the gate.

## 8. UI (per `12_web_ui.md`) and SDK

No new top-level routes: review lives on 03's PR page (`/:owner/:repo/pull/:num`) as components.

- **Review summary bar:** decision badge + reviewer chips (`review_summary.latest`), stale badge derived
  client-side from `commit_sha != head`, requested-reviewer chips, unresolved-thread counter.
- **Diff review surface:** reuses `lib/diff.js` `parsePatchFiles` (12 §2.8). A line's comment affordance
  builds the anchor from the parsed hunk: `side`/`old_*`/`new_*` from hunk counters, `path` from the
  file header, `commit_sha` from the rendered head, `context_sha` computed by one shared
  `anchorContextSha(hunk, range)` helper in `lib/diff.js` — the ONLY implementation of the §4 hash.
- **Thread cards:** rendered inline at the anchor's start line; unresolved-first ordering; resolve
  toggle calls the thread endpoint; outdated threads (hash mismatch) collapse with the original lines.
- **Finish-review modal:** collects pending single line comments (each opened a thread immediately) +
  a top-level body + verdict (`COMMENT`/`APPROVE`/`REQUEST_CHANGES`); one `POST reviews`.
- **Reviewers panel:** picker fed by `review-suggest` (150 ms debounce, abort-on-keystroke per the
  §2.6 ref-picker pattern); Dismiss button visible to `maintain`.
- **SSE:** `mountStream` (12 §2.5) subscribes the repo stream; `review`/`thread` frames patch the
  summary/thread signals — no polling loops (P7).

**SDK additions** (new submodule `web/sdk/src/pulls.js`, re-exported by `index.js`; 08 owns the full
inventory): `repo.pulls.reviews.{list,get,submit,dismiss}`,
`repo.pulls.threads.{list,get,create,comment,resolve,unresolve}`,
`repo.pulls.requests.{list,add,remove}`, `repo.pulls.suggest(q)`. All plain-fetch JSON with the §1.4
envelope handling; JSDoc `@typedef Review/ThreadAnchor/ThreadHeader` in `types.js`.

## Decisions

- **Reviews and threads are sidecar families under `pulls/<num>/`, PR header stays in `issues/<num>/`** —
  shared numbering (P2) plus the 14.10.4 sketch layout; review state never rewrites the PR timeline.
- **Sequences for reviews and thread ids both allocate from the PR header CAS** — one arbitration point,
  gap-tolerant per P3.
- **Dismissal is a compensating event, not a mutation** — events are immutable (P3); rollup demotes.
- **Staleness is always derived (`commit_sha != head`), never written** — pure function of immutable
  state, so the UI and the merge gate agree with zero coordination and no background dismisser.
- **`review_summary` is a pure-function render cache** — recomputed inside the CAS loop from immutable
  events; racing writers converge, and the merge gate never trusts it (re-derives by scan).
- **`required-reviews` is one policy effect with two honest halves** — push-time denial (enforceable at
  receive-pack) + merge-gate evaluation (where approvals are observable), per 14 §14.5.
- **No new task kinds; no author self-approval, enforced server-side; maintain-only dismissal.**

## Explicitly out of scope

- CODEOWNERS / code-ownership routing of review requests (needs an ownership index — P9-adjacent; the
  `review-suggest` seam is where a future provider would register).
- Merge queues, review-analytics dashboards, approval-weighting, review templates/checklists.
- Editing or deleting posted reviews (compensating events only), private review comments.
- Cross-PR / cross-repo review aggregation; email-based review submission (the API + UI are the only
  writers — the CLI is never a second writer implementation, 14 §14.9).
