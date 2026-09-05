# 02 — Issues: threads, comments, labels, milestones, assignees, close/reopen

> Depends on: [`01_identity_permissions.md`](01_identity_permissions.md) (roles, access.json). Consumes primitives P1–P9 from
> [`README.md`](README.md); wire conventions from `docs/go/07_api.md`; lock/race rules from `docs/go/13_concurrency.md` (CAS loops
> are the primary tool — no cross-feature locks); seams from `docs/go/14_extensibility.md`; UI patterns from `docs/go/12_web_ui.md`.
> Package: `internal/issues` (one route provider + one task kind). A reader must implement issues end to end from this file alone.

## 1. Object family

Bucket keys (all under the repo prefix; lowercase, `/`-delimited per P1):

| Key | Kind of object | Write mode |
|---|---|---|
| `repos/<o>/<r>/meta/next_num` | shared counter `{"next": N}` (P2) | CAS'd (`Update(version)`) |
| `repos/<o>/<r>/issues/<num:06x>/thread.json` | thread header (P3) | CAS'd |
| `repos/<o>/<r>/issues/<num:06x>/events/<seq:012x>.json` | immutable event (P3) | `Create` only |
| `repos/<o>/<r>/issues/index.json` | list index (P4) | CAS'd; **in the frozen overwritable list** (D-EXT-2) |
| `repos/<o>/<r>/meta/labels.json` | repo label set | CAS'd |
| `repos/<o>/<r>/meta/milestones/<id:06x>.json` | milestone | `Create` + CAS'd update |
| `repos/<o>/<r>/meta/milestones/index.json` | milestone id allocator `{"next": N}` | CAS'd |

`<num:06x>`/`<id:06x>` are lowercase hex, zero-padded to 6 digits, so LIST scans return numeric order (same idiom as P3's 12-hex
seqs). Threads for **both** kinds (`"issue"`/`"pr"`) live in this one family (P2 shared numbering); PR state lives in 03's
`pulls/<num>/pr.json` sidecar — no PR fields leak into `thread.json`. This doc's schemas cover the `kind: "issue"` variant and
the events a PR thread also uses (comments, references, reactions).

### 1.1 thread.json (issue variant — CAS'd header, P3)

```json
{
  "num": 7,
  "kind": "issue",
  "title": "Publish fails on empty tree",
  "state": "open",
  "state_reason": null,
  "author": "jane",
  "created_at": "2026-09-01T10:00:00Z",
  "updated_at": "2026-09-01T12:30:00Z",
  "labels": ["bug", "storage"],
  "assignees": ["alice"],
  "milestone": "000003",
  "participants": ["jane", "alice", "bob"],
  "next_event_seq": 14,
  "comment_count": 9,
  "reaction_summary": {"000003": {"+1": 3, "heart": 1}},
  "version": 11
}
```

| Field | Type | Notes |
|---|---|---|
| `num` | int | allocated from P2; equals the `<num:06x>` key value |
| `kind` | `"issue"` | `"pr"` threads share this schema; 03 owns the PR sidecar |
| `title` | string | 1–256 chars after trimming; never empty |
| `state` | `"open" \| "closed"` | |
| `state_reason` | `"completed" \| "not_planned" \| null` | `null` while open; cleared on reopen |
| `labels` | `[]string` | sorted, unique; names must exist in `meta/labels.json` at write time |
| `assignees` | `[]string` | sorted, unique, ≤ 10; each must resolve ≥ triage per P6 |
| `milestone` | string \| null | milestone id (`<id:06x>`) or `null` |
| `participants` | `[]string` | denormalized: author + every commenter + every assignee (06 fans out from this — do not scan events) |
| `next_event_seq` | int | the reservation counter of the P3 two-step |
| `comment_count` | int | denormalized counter of `commented` events |
| `reaction_summary` | map | key = target event seq, value = emoji name → count (see §8) |
| `version` | int | opaque CAS version; server-managed, echo never required |

Immutability (P3): `num`, `author`, `created_at` never change after `Create`; everything else is CAS-mutable through the two-step.

### 1.2 Event objects (immutable, `PutMode::Create`, P3)

Common envelope on every event object; `seq` matches the key digits:

```json
{"seq": 14, "type": "commented", "actor": "bob", "at": "2026-09-01T12:30:00Z"}
```

Exact payload per type (extra fields only; unknown fields on read are ignored, on write rejected 400 —
fail closed, same rule as policy effects):

| `type` | Payload fields | Semantics |
|---|---|---|
| `opened` | `body: string` (≤ 64 KiB) | first event of every thread; never after the fact |
| `commented` | `body: string` (≤ 64 KiB) | linear discussion; no replies/edits — corrections are new comments |
| `title_changed` | `from, to: string` | |
| `labels_changed` | `added: []string, removed: []string` | delta; `thread.labels` is authoritative |
| `assignees_changed` | `added: []string, removed: []string` | delta |
| `state_changed` | `from, to: "open"\|"closed"`, `reason: "completed"\|"not_planned"\|null` | close/reopen; `to:"closed"` with reason `not_planned` is the "won't fix" path |
| `milestone_changed` | `from, to: string\|null` | milestone ids |
| `referenced` | `source: {kind, …}` | same-repo reference INTO this thread (§6) |
| `cross_referenced` | `source: {repo, kind, num}` | reference from a DIFFERENT repo (§6) |
| `reaction_changed` | `target_event_seq: int`, `content: string`, `op: "add"\|"remove"` | reactions (§8) |
| `closed_by_pr` | `pr_num: int`, `keyword: string` | written by the merge task via `ApplyClosingReferences` (§5) |

`source.kind` ∈ `"commit"` (`sha`), `"comment"` (`event_seq`), `"thread"` (`num`). Every payload carries `actor` in the envelope;
`at` is RFC 3339 UTC; bodies are raw text — markdown-lite rendering + sanitization are client concerns per `12_web_ui.md`.

### Concurrency

The P3 two-step is THE write discipline: CAS the header (`Update(version)` reserves `next_event_seq`), then `Create` the event.
A `412` on the Create is a bug signal — retry the loop, never leak a reserved seq; seq gaps from a crash between the two steps
are allowed and harmless. CAS loops are the only coordination tool (`13_concurrency.md` §3/§5) — **no cross-feature or
cross-thread locks exist**. A reference write into ANOTHER thread's log (§6) runs the same two-step on that thread; no ordering
between two threads' CAS targets is promised anywhere.

## 2. The index (P4)

`repos/<o>/<r>/issues/index.json` — CAS'd, in the frozen overwritable list (D-EXT-2):

```json
{
  "version": 42,
  "compacted_through": "00003a",
  "open": [ {card} ],
  "closed_recent": [ {card} ]
}
```

```json
{"num": 7, "kind": "issue", "title": "Publish fails on empty tree", "state": "open",
 "state_reason": null, "labels": ["bug"], "assignees": ["alice"], "milestone": null,
 "author": "jane", "created_at": "…", "updated_at": "2026-09-01T12:30:00Z",
 "comment_count": 9}
```

- Cards are the P3 header minus `version`/`next_event_seq`/`reaction_summary` — a projection, never an authority; the header wins.
- `open` = every `kind: "issue"` thread with `state: "open"`, newest-activity-first; `closed_recent` = closed threads newer than
  `compacted_through`. **Compaction rule:** when the object exceeds ~256 KiB, the `issue-index-compact` task (§9) evicts the oldest
  `closed_recent` entries and advances `compacted_through` in the same CAS; compacted threads are served by paginated LIST over
  `…/<num>/thread.json` (acceptable per P5).
- The `after=` cursor is `<num:06x>` byte-order over the index page; exhausted cursors fall through to the LIST scan (`n` default 50, max 200).
- The index is updated in the same handler after the header CAS commits, by its own CAS loop (P4). A lost update is repaired by
  the next thread mutation (handlers re-read, diff, repair) and by compaction — LIST fallback makes staleness a performance gap, never a correctness gap.

### Concurrency

Hazard: two handlers CAS the index concurrently → one bounded CAS-retry loop (10 attempts, then proceed without index update —
the repair path covers it). No lock spans the header CAS and the index CAS; they are separate objects, ordered header-then-index,
and a failure between them is tolerated by repair. The index is performance scaffolding, never authoritative.

## 3. Labels and milestones

### 3.1 Labels — `repos/<o>/<r>/meta/labels.json` (CAS'd)

```json
{"version": 3, "labels": [
  {"name": "bug", "color": "d73a4a", "description": "Something isn't working"}
]}
```

- `name` 1–64 chars, unique case-insensitively; `color` = 6-hex RGB without `#`; `description` ≤ 200 chars; validation rejects
  duplicates and unknown colors (400). All edits CAS the whole object.
- **Label names are immutable identities — no rename endpoint.** Rename = delete + create: headers keep the old name string, a
  compensating `labels_changed` event is emitted per affected open thread in the index hot window (older threads self-heal when
  next touched — a nonexistent label renders as the bare string). Deleting a referenced label is allowed; the response reports
  `{"threads_affected": N}` (index count, best-effort).

### 3.2 Milestones — `repos/<o>/<r>/meta/milestones/<id:06x>.json` (Create + CAS'd)

```json
{"id": "000001", "title": "v1.1", "description": "", "due_on": "2026-10-01T00:00:00Z",
 "state": "open", "open_issues": 4, "closed_issues": 11,
 "created_by": "jane", "created_at": "…", "updated_at": "…"}
```

- `id` allocated from `meta/milestones/index.json` `{"next": N}` (the P2 CAS-counter pattern).
- `due_on` RFC 3339 or `null`. `open_issues`/`closed_issues` are **denormalized counters** updated by the issue-mutation
  handlers that add/remove/close/reopen an issue on the milestone; progress (percent complete) is DERIVED on read, never stored.
  Closing a milestone is metadata-only; issues keep their own state.
- DELETE requires `open_issues = 0` (else 409) and clears `milestone` from affected open threads via compensating `milestone_changed` events.

### Concurrency

Both families are plain CAS loops (P2 reasoning applies: human-rate mutations). Milestone counters add
one CAS per affected milestone inside the issue mutation — a lost counter update is repaired by the
next issue event touching that milestone; the counter is display state, thread headers are the truth.

## 4. Assignees

- An assignee MUST resolve to role ≥ **triage** on the repo per the P6 order (access.json → org ownership → principal flags);
  unresolvable/below-triage names are rejected 400 (the plain-text error names the offending principal). The ACTOR changing
  assignees also needs ≥ triage (self-assignment included). Stored as sorted principal names, ≤ 10; adding emits
  `assignees_changed` + an `assigned` notification (§10).

## 5. State: close/reopen and closing keywords

- `PATCH {state: "closed", state_reason: "completed"|"not_planned"}` → `state_changed` event; reopen clears `state_reason` to
  `null`. Author may close/reopen own thread; others' need triage (P6).
- **Closing keywords (`fixes #N`) — decided: parsed at PR MERGE time, never at push.** The receive-pack path is git-only (README
  law; a push may contain commits that never merge), the merge task is the single place where "landed on the default branch" is a
  fact, and P8 wants exactly one mutating handler per mutation. The push path stays untouched.
- Keyword grammar (owned HERE, triggered by 03's merge task): case-insensitive
  `(close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved)\s+#(\d+)`, matched against the PR body and every merged commit
  message, outside code spans/fences (same skipper as §6); one issue matches at most once per merge.
- The merge task calls the 02-provided seam `ApplyClosingReferences(repo, pr_num, merged_sha, texts…)`: 02 parses `texts` (PR body
  + commit messages), and per matched num writes a `referenced` event (`source: {kind: "thread", num: pr_num}`) and a
  `state_changed` close (reason `completed`) through the normal P3 two-step. Already-closed issues are skipped.

## 6. Cross-references (`#N` mentions) and the ReferenceEvent

**Decided: `#N` mentions are parsed at WRITE TIME in the commenting handler** (`opened`/`commented` bodies) — not at render time,
not by background scan: the event log is the backfill source of truth (P8), so references must be durable events. Parser (raw
body, pre-markdown):

1. Skip fenced code blocks (``` / ~~~) and inline code spans — same tokenizer stance as `12_web_ui.md` markdown-lite.
2. Match `#(\d{1,7})` at word boundaries; self-references ARE recorded; cap 100 refs per body, over-cap stops parsing silently.
3. Dedup per (source event seq, target num); each surviving ref produces one event on the TARGET thread: `referenced` (same repo)
   or `cross_referenced` (different repo), `source: {kind: "comment"|"thread", event_seq?/num, repo: "<o>/<r>"}`.
4. The reference write is a P3 two-step on the TARGET thread inside the commenting request (P8: one handler). The commenting actor
   needs **read** on the target repo (mentioning is not moderating); `actor` is the commenting principal. A missing target
   (unknown num / 404 repo) is silently skipped — mentions are best-effort, never a 4xx for the commenter.
5. References do NOT subscribe the source author (subscription is 06's; only `@mention` does, §10).

### Concurrency

Hazard: one comment fans out to K target threads — K independent CAS two-steps in one request. Avoidance: each is the standard P3
two-step against its own header; partial failure loses some references, which is acceptable (the comment body is the durable
record). No ordering between targets is promised; a request retry may duplicate a reference — dedup at read is (source event seq,
target num), making replays idempotent. Cross-repo targets MUST be bounded by `context.WithTimeout` (10 s).

## 7. API endpoints

Registered by `internal/issues` as an `api.RouteProvider` (Seam 1) via `api.Lanes` — every route exists on BOTH `/{o}/{r}/api/…`
and `/{o}/{r}/api-browser/…`. Wire conventions per `07_api.md`: JSON success, **plain-text errors**, arrays `[]` never `null`,
RFC 3339, cache class per route. `num` matches `[1-9][0-9]{0,8}` (decimal on the wire; 06x is storage-only).

| METHOD + path | Auth (P6) | Request → Response |
|---|---|---|
| `GET …/api/issues?state=&labels=&assignee=&milestone=&since=&after=&n=` | read | → `{issues:[card], more:bool}`; index-first, LIST fallback; `labels` comma-list AND, `assignee` login or `*none`, `milestone` id or `none`, `since` RFC3339 on `updated_at`, `after` num cursor, `n` ≤ 100 default 50. no-store |
| `POST …/api/issues` | read (authenticated) | `{title, body?}` → `201 {thread, events:[opened]}`; allocates num via P2 |
| `GET …/api/issues/{num}` | read | → `{thread, events:[last 50], events_more:bool}`; `?after_seq=&n=` windows the log; `ETag: "v<version>"`, 304 on If-None-Match |
| `GET …/api/issues/{num}/events?after_seq=&n=` | read | → `{events:[], more:bool}`; seq-window pagination, newest-last |
| `PATCH …/api/issues/{num}` | title/state: author or triage; labels/assignees/milestone keys: triage | `{title?, state?, state_reason?, labels?, assignees?, milestone?}` → `{thread}`; unknown keys 400; no-op fields omitted |
| `POST …/api/issues/{num}/comments` | read (authenticated) | `{body}` → `201 {event}` |
| `POST …/api/issues/{num}/reactions` | read (authenticated) | `{target_event_seq, content}` → `{event}` |
| `DELETE …/api/issues/{num}/reactions/{target_event_seq}/{content}` | own reaction only | → `204` |
| `GET …/api/labels` | read | → `{labels:[]}` |
| `POST …/api/labels` | triage | `{name, color, description?}` → `201 {label}` |
| `PATCH …/api/labels/{name}` | triage | `{color?, description?}` → `{label}` |
| `DELETE …/api/labels/{name}` | triage | → `204`; see §3.1 rename/delete semantics |
| `GET …/api/milestones?state=` | read | → `{milestones:[]}` with derived progress |
| `POST …/api/milestones` | triage | `{title, description?, due_on?}` → `201 {milestone}` |
| `GET …/api/milestones/{id}` | read | → `{milestone}` |
| `PATCH …/api/milestones/{id}` | triage | `{title?, description?, due_on?, state?}` → `{milestone}` |
| `DELETE …/api/milestones/{id}` | triage | → `204`; 409 while issues reference it |

Auth per P6: `read` = role ≥ read (or `anonymous_read`); `authenticated` = any principal; `triage` = role ≥ triage. Errors are
plain text (`404` "unknown issue", `409` "milestone has open issues"); every LIST-backed route states its page size (P5).

### Concurrency

One header read + one header CAS + one event Create inline — sub-second bucket ops, the Seam 1 rule; nothing here is task-worthy
except compaction (§9). GET-by-num is ref-class SWR keyed on the header version; list endpoints are no-store.

## 8. Reactions

**Decided: reactions are EVENTS with `target_event_seq`** — `reaction_changed {target_event_seq, content, op}` appended to the
thread log — **plus a denormalized `reaction_summary` in the CAS'd header** (P3's denormalized view counters): the event log stays
the backfill truth (a buggy header can be recomputed from events); the summary gives O(1) counts without scanning the log.

- `content` ∈ GitHub emoji names (`+1`, `-1`, `laugh`, `hooray`, `confused`, `heart`, `rocket`, `eyes`); unknown → 400. One
  reaction (principal, target_event_seq, content) is UNIQUE — a duplicate add is a no-op returning the summary, not an event.
- Targets: `commented` and `opened` events only (400 otherwise); DELETE removes only the actor's own reaction (§7).
- Summary maintenance: the handler adjusts `reaction_summary` ± 1 in the SAME header CAS that reserves the event seq (one CAS, no
  extra round trip); a lost CAS retries from the fresh header.

### Concurrency

Hazard: a hot thread takes many concurrent reactions — every one CASes the same header. Accepted per the P2 reasoning
(human-rate); losers retry via the standard CAS loop, and the append is idempotent per (actor, target, content).

## 9. Tasks

| Kind | Trigger | Work |
|---|---|---|
| `issue-index-compact` | `issues/index.json` > ~256 KiB, checked opportunistically by handlers (1-in-N sampling) and by the maintainer pass | CAS-compaction per §2; `(repo, kind)` single-flight per 13_concurrency §3; SSE-attachable. No other task kinds: issue mutations are synchronous (P8) |

## 10. Notifications (contract with 06)

Per P8, emitted **synchronously in the mutating handler after the CAS commits** (a crash after CAS
loses one notification, never data). 06 owns the subscription model; 02 emits:

| Trigger (event) | Notification class | Recipients resolved from |
|---|---|---|
| `assignees_changed` (added) | `assigned` | the added principal |
| `@name` mention parsed in `opened`/`commented` bodies (same parser rules as §6, `@`-form) | `mentioned` | the named principal if resolvable |
| `opened` / `commented` / state or label changes by a non-participant | `subscribed` (participation) | new `participants[]` members (thread.json, §1.1 — maintained by the header CAS) |

Fan-out resolves targets from `participants[]` + parsed mentions — 06 MUST NOT scan the event log; `referenced` events do not
subscribe the source author.

## 11. UI and SDK

Pages (SolidJS SPA per `12_web_ui.md`, D-WEB-6; `.jsx` route components, `useData`):

| Route | Page | Live behavior |
|---|---|---|
| `/:o/:r/issues` | list: filter bar (state/labels/assignee/milestone/since), paged cards from the index | SSE `issue` upserts/patches cards in place |
| `/:o/:r/issues/new` | create form (title, markdown-lite body, preview toggle) | — |
| `/:o/:r/issues/:num` | thread: header (`#N` state badge — the single source of state), timeline (seq-window, older on demand), comment composer, one sidebar metadata card (labels + `+` dropdown / assignees / milestone in divided sections) | `issue_event` appends timeline frames; `issue` updates the header |
| `/:o/:r/labels` | label CRUD (triage-gated UI) | on save, refetch |
| `/:o/:r/milestones` | milestone CRUD + progress bars | on save, refetch |

SDK additions (`web/sdk/src/issues.js`, esbuild-bundled into `repos.js` per D-WEB-2; full shapes in 08):
`repo.issues.{list,get,create,patch,comment,events,reactions.add,reactions.remove}`, `repo.labels.{list,create,update,delete}`,
`repo.milestones.{list,get,create,update,delete}` — thin fetch wrappers over §7 with the SDK's lane/401 rules; thread pages use
`mountStream` (`12_web_ui.md` §2.5) to follow `issue`/`issue_event` on the repo's existing SSE stream.

## Decisions

- **Frontend idiom is the SolidJS SPA (D-WEB-6; docs fix for issue #76).** The §11 page sketches read in the shipped idiom: `.jsx` route components, `useData` on Solid primitives, SSE refetch over the SDK readers. Routes, live behavior, and wire shapes are unchanged.

- Issue nums are `<num:06x>` hex keys (decimal on the wire) — numeric order from byte-order LIST scans, same idiom as P3's `012x` seqs.
- `issues/index.json` carries cards for BOTH kinds; issue endpoints filter `kind: "issue"` — one index for one numbering space (03 lists PRs from the same object).
- Label names are immutable; rename = delete + create (no header rewriting, compensating events only).
- Closing keywords parse at PR-merge time (03's merge task calls `ApplyClosingReferences(repo, pr_num, merged_sha, texts…)`), never at push — the push path must stay git-only.
- `#N` references parse at write time and write immutable events into the TARGET thread (best-effort, deduped by (source seq, target num)).
- Reactions are events + a denormalized `reaction_summary` in the header — log stays the truth, header gives O(1) reads (P3).
- `participants[]` lives in the header, maintained by the same CAS as the event seq — one owner (06 depends on this).
- Milestone progress is denormalized counters, derived-for-display, never authoritative.
- Auth gates per P6: label/milestone CRUD and moderation at triage; issue create/comment at read.
- **Unified issue sidebar + labels dropdown (issue #107).** The thread page
  sidebar is ONE metadata card (labels / assignees / milestone in divided
  sections, each value directly under its header); the old state card is
  gone — the header `#N Open/Closed` badge is the single source of state.
  Labels keep their applied chips plus a triage-gated `+` button opening a
  dropdown with ALL repo label options (menuitemcheckbox rows, one PATCH
  per toggle; Esc/outside-click close, focus returns to the trigger).
- Compaction task kind `issue-index-compact`, trigger sampled — no timer goroutine invented.
- **List render is number-descending (issue #48, 2026-09-04):** every list
  read (`GET …/api/issues` under any state filter) renders newest-first by
  issue number descending — the merged `open` + `closed_recent` pool is
  re-sorted by num at read time, so activity recency never reorders the
  combined list. The index object itself is UNCHANGED (still
  newest-activity-first per §2); only the render sorts by num, and the SPA
  re-sorts at display (`web/src/lib/sort.js`) so stale cache windows and
  SSE refetches agree. The `after=` cursor stays positional in this
  number-desc display order.

### Wave B implementation notes (2026-09-04, `internal/issues` landed)

- **Notify seam is a synchronous emitter, nil until 06 lands.** `Service.Notify`
  (`NotifyEvent{repo, class, actor, issue_num, recipients, at}`) fires in the
  mutating handler after the CAS commits (P8); with no `internal/notify` wired
  it is a documented no-op. All durable inputs 06 needs (`participants[]`,
  mention bodies) are in the bucket regardless, so the timeline stays the
  backfill truth. Same for `Service.Stream` (`issue`/`issue_event` names are
  reserved for the repo SSE bus).
- **Seam 1 in code is the `server.ExtraRoutes` chain**, not `api.Lanes` — the
  package fronts the core mux via `Handle(w, r) bool` on both lanes, exactly
  like `internal/identity` (see the Wave A amendment in 14_extensibility.md
  Decisions). Route-for-route the §7 table is implemented verbatim.
- **`DELETE …/labels/{name}` answers 200 `{"threads_affected": N}`, not 204.**
  §3.1 mandates the report and a 204 cannot carry it; the table's 204 is
  superseded by the report requirement.
- **Cursor semantics pinned:** `after=` is positional in display order
  (newest-first; unknown cursor pages from the top); `after_seq=` is an
  exclusive upper bound (seqs strictly below, newest-last — older on demand).
  `n` clamps at 100 (list) / 200 (events); over-max list `n` is 400, over-max
  event `n` clamps (list bounds the merge work, events bound a single thread).
- **Completeness is checked, not hoped for.** `ListIssues` reads the index
  plus the P2 counter (2 GETs, O(1)) and serves index-only when every
  allocated number has a card; otherwise it LIST-merges with the header
  winning. A lost index update therefore degrades to LIST instead of hiding
  an issue (E3 in `docs/EVIDENCE.md` measures 2/0 at every population).
- **Compaction trigger is size-checked, not sampled.** Handlers hold the
  written bytes, so `updateIndex` compacts inline past ~256 KiB —
  observationally identical to 1-in-N sampling with no timer and no extra
  read. `compacted_through` advances monotonically (max evicted num).
- **`reaction_summary` keys are `%06x` event seqs** (minimum width 6, growing
  naturally) — matches the §1.1 example for small seqs, unambiguous, sortable.
- **Opened-body refs source from the thread** (`source.kind: "thread"`); only
  comment bodies source `kind: "comment"` with `event_seq`. Cross-repo sources
  always carry `repo` + `num`.
- **Milestone DELETE clears every referencing thread**, not just open ones
  (a superset of §3.2: a dangling id on a closed header helps nothing, and
  the clear is one CAS per thread).
- **PR-kind threads are invisible to issue endpoints** (GET/PATCH/comment/
  react → 404 `unknown issue`): 03 owns those timelines; the shared family
  never leaks across kinds at the API. The index still carries both kinds'
  cards (one numbering space) and issue reads filter `kind: "issue"`.
- **Close without a reason defaults to `completed`** (reopen still clears to
  `null`); duplicate reaction adds answer 200 with the summary (not an
  event); milestone `due_on: ""` clears.
- **Milestone ids ride the wire as stored** (`<id:06x>` hex) — no decimal
  twin, unlike issue nums.
- **Default close-reason + `*none`/`none` filter spellings** (`assignee=*none`,
  `milestone=none`) are implemented exactly as §7 rows state.
- **Event windows return arrays newest-first** (most recent event at index
  0; `after_seq=` pages strictly below toward older). The §2/P3 phrase
  "newest-last" names the pagination direction (older on demand), not the
  array order — code, tests, and the thread page all agree on newest-first.
- **Duplicate-reaction dedup is best-effort under true concurrency.** The
  (actor, target, content) check reads the log before the reserving CAS, so
  sequential double-submits (the real double-click case) are no-ops while
  two truly-simultaneous duplicate adds could both commit and double-count
  the summary; the event log stays the truth (a remove still resolves the
  live set). Fully CAS-atomic dedup would need the live set in the header
  schema — deferred as human-rate over-engineering.
- **Reaction buttons render unicode emoji, never the wire words** (issue
  #31: 👍 👎 😄 🎉 😕 ❤️ 🚀 👀 with the content spelling kept in
  `aria-label`/`title`; no emoji library per the frontend budget). Each
  commented/opened event carries its own emoji+count summary row sourced
  from `reaction_summary`; summary chips toggle (remove-404 falls back to
  add, GitHub semantics; any other remove failure only reports — issues
  #36/#42). `reaction_changed` events fold into the summary entirely
  (GitHub-style): they never render as timeline rows. Clicks paint
  optimistically (immutable `adjustSummary` step committed via the
  generation-safe `patchCached` path, which retires pre-mutation in-flight
  bodies under the #41 ordering guard) and the guarded refetch reconciles
  the guess; one in-flight mutation per (seq, content) disables its
  buttons, so double-clicks never double-fire.
- **Reactions add through a "+" menu, never an always-visible picker**
  (issue #113): each commented/opened event shows one reaction row where
  its chips appear — summary chips plus a tiny "+" trigger opening a
  dropdown with exactly the addable set (`addableReactions`: REACTIONS
  minus whatever the summary already shows for that seq, in wire order;
  the optimistic add bump drops the content from the menu). Contents
  already on the comment toggle off via their chips only. The menu is a
  native-button `menu` sibling of the LabelPicker "+" idiom (Esc/outside
  click close, ArrowUp/Down/Home/End move, dark+light via the shared
  chip/card classes); the thread timeline keeps its generic `actionsFor`
  slot for other surfaces.

## Explicitly out of scope

- Conversation locking/pinning, issue templates, saved replies, issue transfer between repos.
- Sub-issues, tasklists, projects/boards (P9), Discussions (P9).
- Cross-repo closing keywords (`fixes owner/repo#N`) — the parser records them as `cross_referenced` only.
- Full-text issue search (P9: index-format decision deferred); label rename propagation beyond the index hot window.
- Editing/deleting comments after write (corrections are compensating events, P3); reactions on labels/milestones.
