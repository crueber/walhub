# 06 — Notifications, subscriptions, mentions, and webhook fan-out

> Depends on: 01 (identity, teams), 02 (issues), 03 (PRs), 04 (review). Shared primitives P1–P9 are defined in
> [`README.md`](README.md); wire conventions in `docs/go/07_api.md`; lock/race rules in `docs/go/13_concurrency.md`;
> seams in `docs/go/14_extensibility.md`. Status: normative for walhub.

Notifications are the **fan-out layer** of the collaboration family: every mutating collaboration handler
(comment, assign, review request, check report, release publish) computes recipients and writes
notification objects **synchronously in the same request** (P8). Delivery surfaces: **in-UI** (v1: per-user
SSE stream + tray), **webhooks** (v1: repo-level configs + per-hook delivery loop), **email = OUT** (§5.2
seam; no SMTP client in v1). The WAL stays git-only: everything here is a parallel object family under
`users/` and `repos/<o>/<r>/`, its own CAS discipline, no WAL entries, no manifest touches (P1).

## 1. Object families

### 1.1 Notifications — `users/<principal>/notifications/<id>.json`

One object per (recipient, triggering event). **`<id>` is deterministic** so emission is idempotent:

```
id = hex(sha256("notification\x00" + principal + "\x00" + <owner>/<repo> + "\x00"
                + num + "\x00" + reason + "\x00" + event_seq))[0:32]     // 32 lowercase hex
```

A retried fan-out (crash between CAS commit and fan-out, or a replayed backfill) re-derives the same key;
the `Create` 412s and the retry treats "already exists" as success. Ids sort by the ULID-derived event
ordering, so LIST scans are newest-first without an extra sort field.

| Field | Type | Notes |
|---|---|---|
| `id` | string | 32-hex, as above |
| `repo` | string | `"owner/repo"` |
| `num` | int | shared issue/PR number (P2); 0 for repo-level events (e.g. release published) |
| `kind` | string | `"issue"\|"pull"\|"release"\|"repo"` |
| `reason` | enum | `mentioned\|assigned\|review_requested\|subscribed\|author\|team_mention` |
| `title` | string | thread title at emission time (denormalized; never rewritten) |
| `actor` | string | principal who caused the event; `""` for system |
| `state` | enum | `unread \| read` |
| `created_at` | string | RFC 3339 UTC (07 §2) |

Ops: **Create** on emission; **CAS'd** (`Update(version)`) for the ONLY mutable field, `state`
(`read`/`unread`); `Delete` on retention (§9). `id`/`num`/`reason` are immutable — a "read" marker is a
state flip, never a rewrite of history.

### 1.2 Per-user unread index — `users/<principal>/notifications/index.json`

P4 pattern. CAS'd, carried in the same two-step as the notification Creates:

| Field | Type |
|---|---|
| `unread_count` | int |
| `entries` | `[{id, repo, num, kind, reason, title, state, at}]` — hot window only, newest-first |
| `compacted_through` | string — oldest `id` still represented in `entries` |

The index is the tray's O(1) default read; older pages LIST `…/notifications/<id>.json` (P4/P5 —
paginated, page size 50). Compaction is the retention task (§9).

### 1.3 Watches

F7 (07_releases_stars) owns the records: `users/<principal>/watching/<o>/<r>.json` (CAS Create/Delete,
`{"repo","watched_at"}`) and `repos/<o>/<r>/meta/social.json`. 06 owns **nothing** here: fan-out reads the
`watching/` records as one subscription source (§2) and NEVER writes `social.json`. The SDK/UI watch
toggle belongs to 07/08; this doc only consumes it.

### 1.4 Webhook configs — `repos/<o>/<r>/webhooks/<id>.json`

`<id>` = 24-char lowercase ULID (time-ordered; lexical sort = creation order). CAS'd after Create (only
`url`, `events`, `active`, `insecure_tls`, `updated_at` are mutable); Delete removes it.

| Field | Type | Notes |
|---|---|---|
| `id` | string | 24-char ULID |
| `url` | string | https only (`http` allowed on loopback for dev) |
| `secret` | string | write-only: set via POST/PATCH body, NEVER returned; response carries `secret_set: bool` |
| `events` | array | filter list of activity `action`s, `[]` = all; see §5.3 |
| `active` | bool | `false` = config kept, delivery skipped |
| `insecure_tls` | bool | default `false` |
| `created_by`, `created_at`, `updated_at` | | RFC 3339 |

Delivery records: `repos/<o>/<r>/webhooks/<id>/deliveries/recent.json` — one CAS'd last-25 ring
`{updated_at, entries:[{seq, event, status, at, error?, duration_ms}]}` (debugging surface only, not
a durability mechanism).

### Concurrency

Hazard: a fan-out touching N notifications races concurrent emissions on the same thread. Avoidance:
**CAS loops are the only tool** — no cross-feature locks, no new repo locks (13_concurrency.md §2; the
three repo locks are closed). Each notification Create is independent and idempotent (§1.1); the unread
index is a single CAS retry loop; a lost index race is retried, a lost Create is a no-op (412). The
webhook delivery loop and cursor are CAS'd per hook (§5.3). A crash mid-fan-out loses at most one
notification, never data — the thread timeline is the backfill source (P8 contract).

## 2. Subscription model — who gets notified

For an event `E` on thread `T` of repo `R` by actor `A` (the actor NEVER receives a self-notification):

| Source | Resolution | Reason emitted |
|---|---|---|
| Thread author | `thread.json.author` | `author` |
| Commenters / participants | `thread.json.participants[]` — confirmed by 02/03: denormalized principal array maintained by the SAME header CAS that reserves event seqs; 06 reads `thread.json` alone, no event scans | `subscribed` |
| Assignees | `thread.json.assignees[]` on `assigned` events | `assigned` |
| Requested reviewers | review-request event payload (04) | `review_requested` |
| Repo watchers | `repos/<o>/<r>/meta/social.json` `watchers[]` (07-owned; see below) | `subscribed` |
| Check failure on an open PR's head (05) | PR participants from `thread.json.participants[]`; only on `failure\|error` state transitions | `subscribed` |
| `@principal` in the new event body | §3 | `mentioned` |
| `@org/team` in the new event body | F1 team membership | `team_mention` |

Watcher enumeration is the one non-trivial resolution: watching records live user-side and there is no
reverse index that avoids a users LIST. DECISION: `social.json` (07) MUST carry `watchers: []` (cap
1 000, `truncated: bool`); fan-out reads that array — 07 owns it, 06 only reads. Beyond the cap, watch
subscriptions notify no one (`walhub_watchers_truncated_total{repo}`). Watches notify `subscribed`.

Dedup precedence: one notification per (user, thread, reason). If an **unread** notification with the
same (user, thread, reason) already exists, the emitter MUST skip the Create (checked via the unread
index) — one live notification per thread per user. The id still includes `event_seq`, so the skip is
an optimization; the idempotent Create is the backstop. A **read** notification does not block a new one.

## 3. Mention parsing

Runs **once, at event write time**, inside the mutating handler (P8) on the new event's text body:

- Token grammar: `@<principal>` and `@<org>/<team>`, matched only at word boundaries
  (`(?:^|[^a-z0-9-])@(…)`), case-insensitive match, canonical lowercase keys.
- Validation is a bucket probe, per 01's contract: a plain GET of `users/<principal>/profile.json`
  (404/NotFound = no such principal — mention invalid); teams via `orgs/<org>/teams/<slug>.json`,
  whose `members` is an array of principal strings read directly (no API hop). An unresolvable
  mention is silently ignored — never a 400, never an error; the text is stored verbatim regardless.
- Bounded: at most 50 mention tokens per event; beyond that, ignored (counter
  `walhub_mentions_dropped_total{repo}`).
- `@org/team` fans out to team members with reason `team_mention`; if the team has > 100 members the
  emitter caps the batch at 100 recipients (sorted, stable order) — a mention of a 10 000-member team
  is the operator's misconfiguration, not a fan-out DoS vector.
- Mention parsing happens on EVERY comment/review/issue-body event, including the issue/PR body itself
  (`opened` events).

## 4. Emission contract (P8 — synchronous in the mutating handler)

The mutating handler that owns the CAS commit (comment posted, assignee changed, review requested, check
reported, thread closed with a comment, release published) performs, **in the same request, after the CAS
commits**, exactly this sequence:

1. Compute the recipient set: mentions (§3) ∪ participants ∪ assignees/reviewers per event kind ∪
   repo watchers (§2), minus the actor, deduped.
2. For each recipient, `Create` `users/<p>/notifications/<id>.json` (deterministic id, §1.1). A 412
   means "already emitted" (retry/backfill) and is success.
3. CAS the user's `notifications/index.json` (unread_count, entries) — retry loop, own version.
4. Append the immutable activity event for webhooks (§5.3).
5. Publish one SSE `notification` frame to the recipient's stream (§5.1) — non-blocking (07 §6:
   never a blocking send; drop-oldest).

Order 2→3→4→5 is normative (notifications before index before stream): a crash leaves at most a missing
tray entry, never a phantom index entry. Crash before step 2 loses one notification — the thread
timeline is the backfill source of truth (same contract as the events bridge, 09_events §2).

### Concurrency

Hazard: fan-out is N sequential CAS rounds on the request path (a 100-recipient team mention would stall
the commenter's request). Avoidance: the Creates and index CAS are bounded (`errgroup.SetLimit(8)` —
13_concurrency.md §4, no raw `go func()` over store I/O) and the whole fan-out carries a 5 s budget;
overflow recipients (> 100 per event) fall back to a `notify-fanout` task (Seam 5) instead of extending
the request. No cross-feature locks: dedup is CAS-Create arbitration, not a lock. Hazard: two instances
emitting the same event concurrently (retry after ambiguous failure). Avoidance: deterministic id +
Create semantics — exactly one copy survives; both losers read "exists".

## 5. Delivery surfaces

### 5.1 In-UI: per-user SSE stream + tray

New top-level route `GET /api/v1/notifications/stream` (Seam 1, `api.RouteProvider`, both `/api/v1` and
`/api-browser/v1` twins), SSE envelope per 07 §6 verbatim (opener `: walgit`, 10 s keepalive, `no-store`,
15 s packet deadline). Frames:

| event | data |
|---|---|
| `notification` | one notification object (§1.1 shape) on emission |
| `result` | terminal, unused for streams — the stream runs until client cancel |
| `error` | `{"status": 503, "message": "…"}` |

The tray page (§7) opens this stream; a `notification` frame prepends to the tray signal and bumps the
chrome badge. The per-user stream is the only per-user SSE; repo streams stay repo-keyed and unchanged.

### 5.2 Email — explicitly out (the seam note)

No SMTP client, no email address storage in v1. The seam: the §5.3 activity log is an ordinary event
stream; an email sink is a future `Sink`/delivery-loop registration (Seam 4/5 pattern) that reads the
same cursor family. A future agent needs: a hand-rolled SMTP client, a user email-verified flag (01),
and per-user email preference keys — nothing in this doc's objects precludes that; nothing implements it.

### 5.3 Repo webhooks — v1 decision

The question "extend the 09_events WAL bridge with collaboration events?" is decided: **v1 webhooks are
repo-level configs delivered from a collaboration activity log, NOT from the WAL bridge.** The WAL bridge
stays git-only (P1 law); the events-bridge loop is frozen and gains no collaboration awareness. Instead:

- **Activity log:** every fanned-out mutation appends one immutable event at
  `repos/<o>/<r>/collab-events/<seq:012x>.json` (`Create`; seq reserved by CAS on
  `repos/<o>/<r>/meta/collab_state.json` `{"next_seq": N}` — the P3 two-step). Body:
  `{seq, repo, action, num?, kind, actor, title, at, payload}`; `action` ∈
  `commented\|opened\|closed\|reopened\|assigned\|review_requested\|review_posted\|check_reported\|release_published\|mentioned\|ping`.
  Also the notification backfill source (§4). `check_reported` (05's emission point, `state: failure\|error`
  only) carries `{sha, context, state, description?, pr?, target_url?}` → PR participants get `subscribed`.
- **Config objects:** §1.4. `events` filter matches on `action` (with `*` wildcard).
- **Delivery loop:** new maintain task kind `webhooks` (Seam 5, `internal/notifications` package),
  single-flight `(repo, "webhooks")` per §6.8. One loop pass per repo: for each active hook, scan
  `collab-events/` from the hook's cursor, POST each matching event, CAS-advance the cursor.
- **Per-hook cursor:** `repos/<o>/<r>/webhooks/cursors/<hook-id>.json`,
  `{"published_seq": N, "updated_at": RFC3339}` — CAS'd, a direct analog of the per-sink
  `events/cursors/<sink>.json` family (14 §14.6); proposed for the frozen overwritable list (the D-EXT-2
  amendment class).
- **Wire shape (per 09_events §12.2, keepers intact):** one POST per event, JSON body = the activity event,
  headers `Content-Type: application/json`, `X-Walgit-Delivery: <hex(sha256(event body+seq))>`,
  `X-Walgit-Signature: sha256=<hex HMAC-SHA256(body, secret)>` (omitted if no secret),
  `X-Walgit-Event: <action>`. 10 s timeout; 2xx = delivered; anything else = not advanced (at-least-once;
  consumers dedup on `X-Walgit-Delivery`).
- **Ping:** `POST …/webhooks/{id}/ping` synthesizes activity event `action: "ping"` (num 0) — delivered
  through the same loop, so ping success proves the URL + secret end to end.

### Concurrency

Hazard: the delivery loop re-scanning `collab-events/` while new events land, or two instances delivering
the same hook. Avoidance: the task table's `(repo, kind)` single-flight guarantees one `webhooks` worker
per repo per process (§6.8 join semantics); the per-hook cursor CAS arbitrates multi-instance races (a
lost cursor CAS = redelivery = at-least-once, never a skip — the cursor advances only after a successful
POST batch). Per hook, delivery is sequential; hooks on one repo run in parallel under
`errgroup.SetLimit(8)`. A slow webhook holds back only its own cursor — exactly the per-sink isolation
of 14 §14.6. Cursor below a compacted/gap window: count `walhub_webhook_gap_total{repo}` and continue
from the oldest readable event (09 §12.3's honest-gap semantics). The loop body never runs under any
repo lock (13 §2 rule 4); it is a maintainer-pass unit with a 10 s-per-POST context budget.

## 6. API endpoints

Auth levels per P6. User notification routes are top-level (`/api/v1/…` + `/api-browser/v1` twins); webhook
and watch routes are repo-scoped via `api.Lanes`. Wire conventions per 07 §2: plain-text errors, `[]`
never null, RFC 3339, no-store on user-private reads. All registered by the `internal/notifications`
RouteProvider (Seam 1).

| METHOD + path | Auth | Request → response | Notes |
|---|---|---|---|
| `GET /api/v1/notifications?state=&after=&n=` | authenticated (self only) | → `{notifications: [Notification], more}` (n default 50, max 200) | no-store; index-first (P4), LIST overflow |
| `GET /api/v1/notifications/unread_count` | authenticated (self only) | → `{count}` | no-store; O(1) from index |
| `POST /api/v1/notifications/{id}/read` | authenticated (self only) | → `Notification` | CAS state flip; 404 if not owner's |
| `POST /api/v1/notifications/{id}/unread` | authenticated (self only) | → `Notification` | |
| `POST /api/v1/notifications/read_all` | authenticated (self only) | → `{updated: N}` | one index CAS + per-object state writes bounded to the index window |
| `GET /api/v1/notifications/stream` | authenticated (self only) | SSE (§5.1) | no-store |
| `PUT /{o}/{r}/api/watch` | read | → `{watching: true, watchers}` | 07 owns the record; this is its API |
| `DELETE /{o}/{r}/api/watch` | read (self only) | → `{watching: false, watchers}` | |
| `GET /{o}/{r}/api/watch` | authenticated (self only) | → `{watching: bool}` | no-store |
| `GET /{o}/{r}/api/webhooks` | admin | → `{webhooks: [Hook (no secret)]}` | |
| `POST /{o}/{r}/api/webhooks` | admin | `{url, events[], secret?, insecure_tls?}` → `Hook` | 400 on bad URL/events |
| `GET /{o}/{r}/api/webhooks/{id}` | admin | → `Hook` (`secret_set`, never `secret`) | |
| `PATCH /{o}/{r}/api/webhooks/{id}` | admin | partial → `Hook` | CAS'd |
| `DELETE /{o}/{r}/api/webhooks/{id}` | admin | → 204 | also deletes cursor + deliveries |
| `POST /{o}/{r}/api/webhooks/{id}/ping` | admin | → `{delivery: true}` | enqueues `ping` activity event |
| `GET /{o}/{r}/api/webhooks/{id}/deliveries` | admin | → `{updated_at, entries: [...]}` | no-store |

`Notification` (wire) = the §1.1 object; `Hook` = the §1.4 object minus `secret` plus `secret_set`. Auth
detail: `{id}` routes resolve only for the owning principal — a foreign `id` is `404` (never `403`,
which would leak existence). Webhook secrets are never logged.

## 7. UI and SDK

UI surfaces (full SPA patterns in 08/12_web_ui; these are the notification-specific pages):

| Page / component | Route | Behavior |
|---|---|---|
| Notification tray | `/notifications` | Paged tray from `GET /api/v1/notifications`; state filter tabs (all/unread); row click navigates to `/{repo}/issues/{num}` (or PR view) and marks read; mounts the per-user SSE stream to prepend live items |
| Chrome badge | every page | Unread count from `unread_count`, refreshed on stream frames; badge links to the tray |
| Watch toggle | repo header | `PUT/DELETE /{o}/{r}/api/watch`; optimistic flip, reconcile on error |
| Webhooks settings | `/{o}/{r}/settings/webhooks` (admin only) | CRUD table + `ping` button + recent deliveries expander; secret shown once at creation |
| Mention autocomplete | comment composer | suggests `@principal` from F1 user search and `@org/team` from F1 teams; purely advisory — the server re-parses |

SDK additions (`web/sdk/src/notifications.js`, bundled by esbuild into `repos.js` per 12 §1.0):

```js
client.notifications.list({state, after, n})             // GET tray
client.notifications.unreadCount()                       // GET unread_count
client.notifications.markRead(id) / markUnread(id) / markAllRead()
client.notifications.stream(onNotification)              // fetch-based SSE reader, cancel fn returned
client.watch.get(o, r) / client.watch.set(o, r, on)      // PUT/DELETE watch
client.webhooks.list(o, r) / create(o, r, spec) / update(o, r, id, patch) / remove(o, r, id) / ping(o, r, id) / deliveries(o, r, id)
```

Streaming uses the SDK's fetch-based reader (never `EventSource`; 12 §2.5 lane/auth rules apply — the
per-user stream is a browser-lane, credentials-included stream).

## 8. Tasks

| Kind | Key | Startable | Work |
|---|---|---|---|
| `webhooks` | `(repo, "webhooks")` | via sweep + wake-up after each fanned-out event | delivery loop, §5.3 |
| `notify-fanout` | `(repo, "notify-fanout")` | internal (overflow fallback, §4) + redrain sweep (§8.1) | bulk notification Create burst for > 100 recipients; per-seq `collab-fanout/` completion record on drain |
| `notify-retention` | global (maintainer pass unit, `Ops: nil`) | no | §9 retention + index compaction |

## 8.1 Fan-out redrain (restart safety, issue #77)

The §4 overflow contract ("the activity payload is the only queue that survives a restart") was
aspirational: the `(repo, kind)` task table is in-memory, so a `notify-fanout` task in flight at
process death left its seqs attached to a table that no longer exists. The redrain closes it with
two durable pieces and one sweep — no new task kind, no new global LIST:

- **Pending flag:** overflow/shortfall emissions set `fanout_pending: true` in the activity payload.
  Sync-complete emissions omit it (`omitempty`, zero byte change on the common path and on their
  webhook bodies).
- **Completion records:** `repos/<o>/<r>/collab-fanout/<seq:012x>.json` (Create-only `{"seq","at"}`,
  412-tolerant), written by the drain after `fanoutOne` completes a seq. Retention (§9) deletes the
  record alongside its activity event.
- **Sweep:** `Run` executes one redrain pass at startup (before serving wake-ups), then every minute
  beside the webhooks sweep. Discovery reuses the webhooks repo enumeration (no new LIST dimension;
  hookless repos are covered — pending fan-out exists independently of webhooks). Per repo the sweep
  reads `collab_state` (quiet repos cost exactly one GET per pass) and probes only the unprobed
  window `(seen, next_seq]` capped at 32 seqs/pass (deeper backlogs converge across passes; gaps are
  skipped with honest-gap semantics, but a transient store failure stops the window instead of
  skipping — the high-water never advances past an unprobed seq, so the next pass retries it). An in-memory high-water per repo bounds repeat work; it is
  rebuilt every restart, which is exactly what makes the restart pass complete. Pending seqs without
  a completion record re-enqueue onto the existing `notify-fanout` single-flight.

### Concurrency

Hazard: the sweep racing a live drain on the same seq (double drain), or two instances sweeping
concurrently. Avoidance: no coordination at all — both paths are idempotent (deterministic ids +
Create-412 + index dedup) and the completion Create arbitrates; a lost write re-drains on the next
pass, never skipping (at-least-once, the webhooks cursor discipline). The high-water lock guards
only the map and is never held across a store or network call.

## 9. Retention and compaction

- The `notify-retention` maintenance unit (Seam 5, maintainer pass, not user-startable) runs once per pass:
  for each principal with a `notifications/index.json` older than its `swept_at`, compact — drop `read`
  entries older than `notifications.retention_days` (default 30), `Delete` their objects, advance
  `compacted_through`, reconcile `unread_count` against actual unreads (drift repair); unread never swept.
- Repo-deleted rows (issue #63): the same pass drops index entries naming a deleted repo in ANY state
  (their objects go too — a deleted repo never returns the same threads, so the rows are garbage) plus a
  bounded overflow sweep (prefix LIST, cap 1 000 scanned / shared 200-delete budget) for dead-repo
  objects past the hot window, which have no index row. Live-repo overflow is never touched.
- The webhook `deliveries/recent.json` ring self-trims to 25; no separate task.
- Repo activity events (`collab-events/`) are compacted by the same pass once the newest webhook cursor is
  ≥ 1 000 seqs ahead: events below the minimum webhook cursor AND older than 7 days are deleted; the
  cursor honesty rule (§5.3) covers the gaps. The minimum-cursor check spans hooks — still one sweep, no locks.

### Concurrency

Hazard: the sweep racing a live fan-out on the same user's index. Avoidance: the sweep's index CAS uses
the same version-stamped retry as any writer; a lost race defers that user to the next pass. Deletion of
a read notification while its tray page is open is harmless (404 → UI drops the row).

## Decisions

- **Webhooks v1 are repo-level configs delivered from a collaboration activity log, not from the WAL events bridge** — the WAL stays git-only (P1), and collaboration mutations are single-handler (P8) so no bridge changes are needed.
- **Per-hook cursor family `repos/<o>/<r>/webhooks/cursors/<id>.json`** mirrors 14 §14.6's per-sink cursors; one slow webhook cannot stall another.
- **Deterministic notification id = sha256(principal, thread, reason, event_seq)[0:32]** — CAS Create makes retries and backfill idempotent for free; no "sent" ledger object needed.
- **Repo-side `watchers[]` array in `social.json` (07-owned)** — fan-out must resolve watchers without listing users; the embedded list is the boring choice, capped and `truncated`-flagged.
- **Thread participants come from `thread.json.participants[]`** (02/03 confirmed: same header CAS as event seq), not from event scans at emission time — one CAS owner per header.
- **Mention validity = plain GET of `users/<p>/profile.json`**; teams read `orgs/<org>/teams/<slug>.json` `members[]` (array of principal strings) directly (01's contract).
- **Email is out; the activity log is the named seam** — a future email sink needs no schema change here.
- **`X-Walgit-*` header keepers on collaboration webhooks** — wire identifiers are contracts, not branding (D-NAME-1).
- **User notification SSE stream is a new top-level route**, not a repo-stream extension: notifications are user-scoped and must not require per-repo subscriptions.

## Decisions (Feature 06 implementation wave, 2026-09-04)

- **Package name is `internal/notify`** — 09 §2's dispatch table wins over this doc's `internal/notifications` text. Rationale: one package per feature, and the rollout table is the dispatch authority agents branch from.
- **Activity seq is reserved BEFORE the notification Creates** (the ids embed the seq), while the activity object itself is still written at step 4 — the normative 2→3→4→5 order governs the WRITES. Rationale: ids must be computable before step 2; a crash between reservation and step 4 leaves a gap, which the honest-gap rule already covers.
- **§1.1 sort sentence corrected**: sha256-hex ids do NOT sort by event time, so LIST overflow pages sort by `(created_at desc, id)` — the index (newest-first entries) is still the O(1) default read. Rationale: the formula is normative; the sentence described a different id scheme.
- **Watch endpoints live in `internal/notify` until 07 lands** (`PUT/DELETE/GET /{o}/{r}/api/watch` writing the 07 §5 record shape verbatim), and the watcher array field is **`watcher_list`** (capped, `watchers_truncated`), NOT `watchers` — 07 defines `watchers` as the COUNT. Rationale: 06 must resolve watchers without listing users, and the count/array names must not collide when 07 adopts `social.json`.
- **Thread author receives `author` INSTEAD of `subscribed`** on activity classes (never both). Rationale: the §2 table names three distinct reasons; doubling the author's tray for one event is noise, and the dedup key keeps both representable if a future dial wants both.
- **Repo StreamEvent frames land on notify's in-process repo bus** (drop-oldest, 64-frame ring, `SubscribeRepo` seam) with NO v1 HTTP reader — 08's collab stream subscribes there. Rationale: no repo broadcast bus exists yet; the normative live proof (notification object + `notification` frame) rides Emit, and inventing a second stream endpoint here would preempt 08's stream design.
- **Overflow (> 100 recipients) writes the activity event FIRST** (recipients in the payload as the durable queue) and returns; `notify-fanout` drains it. Dedup-skips never arm the task (a task racing a later read-flip could otherwise mint a duplicate live entry). Rationale: the request never extends past the 5 s budget; the activity payload is the only queue that survives a restart.
- **No config key**: retention window is a `RetentionDays` field defaulting to 30, no `notifications.*` TOML. Rationale: one scalar does not justify a config surface + validation + schema churn; composition can set it from env later without a wire change.
- **Issues `NotifyEvent` gains additive `Action`** (opened|commented|closed|reopened; "" = commented) so the activity log records the true action behind the coarse `subscribed` class; pulls/review/checks classes were already precise. `mentioned`-emission added to pulls (opened body, PR comments) and review (submitted reviews, thread comments) via the shared `identity.ParseMentions` §3 parser (email principals + `@org/team`, code-span stripping, 50-token cap); issues additionally passes team spellings through (`/` cannot appear in a principal, so the spelling is self-describing). Rationale: §3 mandates parsing on EVERY comment/review/body event, and the parser belongs to 01 (the principal authority) so all emitters share one grammar.
- **PR #17 review fixes (2026-09-04):** `insecure_tls` is honored per hook (dedicated insecure lane cloned from the default transport — the field was stored but never selected); the retention activity sweep is read-bounded (600 seqs/pass, converging across daily passes — the loop was O(minCursor) reads); the §6 watch rows now spell the key `watchers` (matching 07 §5 and the implementation); E7's replay row now reports the measured (3+n) GETs (writes were and are flat at 2 PUTs). Rationale: a stored-but-dead TLS flag fails closed against the wrong party (self-signed dev hooks never deliver); maintainer passes must stay bounded; wire tables must match the owned shape.
- **Repo-delete userspace hygiene (issue #63, 2026-09-04):** the tray skips entries naming a deleted
  repo (one manifest HEAD per merged entry; probe errors and malformed repos keep the entry) while
  `unread_count` stays O(1) off the index — the retention pass (§9) drops the dead rows and reconciles
  the count, so badge and tray reconverge daily. Watch writes fail closed on ghosts (`PUT` → 404;
  `DELETE` still removes the record but skips the counter CAS so no `social.json` is resurrected);
  `GET` reports not-watching. Emission is deliberately NOT gated: handlers reach fan-out only after a
  live commit (a delete racing the fan-out is a millisecond window), and the droppings are
  self-cleaning through the tray filter + retention. No users LIST anywhere (enumerating people stays
  a non-feature); no tombstones (per-repo state dies with the prefix).
- **Fan-out drain/end race closed (issue #72, 2026-09-05):** the `notify-fanout` leader saw an empty
  drain while a concurrent `enqueueFanout` still found the running entry, joined it, and attached —
  then the leader's `end()` removed the task while the joiner (already returned) started no worker,
  orphaning the seq with no worker left to drain it (silent notification loss; Run sweeps
  webhooks/retention only, never unprocessed fan-out). Close is two-sided, both in `tasks.go`:
  (a) the leader ends ONLY via `endIfQuiescent`, which re-checks the attachment and removes the task
  atomically — the table lock nests the entry lock, the ONLY place both are held (lock order is
  always table → entry; no lock is held across store/network calls); a refusal means "seqs pending,
  drain again". (b) a joiner re-checks `current()` after attaching and re-enqueues onto the live
  entry on a miss (the seq is re-attached there, so the orphaned copy on the detached entry is never
  drained twice — and any double drain is idempotent anyway via deterministic ids + Create-412).
  Rationale: neither half alone closes it (a re-checking end still loses an attach landing between the
   check and the removal; a re-verifying joiner still loses to a bare end that never re-checks).
- **SSE keepalive goroutine lifecycle (issue #73, 2026-09-05):** the per-user stream's keepalive
  goroutine ranged over `ka.C`, and `close()` only called `Ticker.Stop()` — which never closes the
  channel — so every disconnected stream leaked one goroutine. The writer now carries a sender-owned
  `done` channel (closed exactly once under the `ended` guard) plus the request context, and the
  goroutine selects on both instead of ranging the ticker (13 channel rule: sender owns and closes;
  every goroutine exits via context). Same latent shape fixed in the sibling `internal/api` SSE
  envelope (`Close()` now ends the stream, not just the ticker); the 10 s keepalive wire contract is
  unchanged. Rationale: Stop-without-close cannot terminate a range; teardown must signal the waiter.
- **Undrained fan-out is restart-safe (issue #77, 2026-09-05):** the §4 claim ("the activity payload
  is the only queue that survives a restart") is now true — previously nothing re-drained events
  whose `notify-fanout` task was in flight at process death. Overflow/shortfall emissions set
  `fanout_pending` in the payload; the drain writes a Create-only per-seq completion record
  (`repos/<o>/<r>/collab-fanout/<seq>.json`, deleted by retention alongside its event); `Run`
  redrains at startup and every minute via `sweepFanout`, which reuses the webhooks repo enumeration
  (no new global LIST) and probes at most 32 unprobed seqs per repo per pass (1 GET for quiet
  repos). Per-seq records instead of a per-repo cursor because sync emissions must not pay a cursor
  CAS on the hot path (law 6); sweep/worker races and multi-instance sweeps collapse in the
  idempotent Creates (at-least-once, the webhooks cursor discipline). Rationale: the durable queue
  was write-only without a reader — the sweep is that reader, scoped to recent activity so a restart
  never scans the bucket.
- **Tenant webhook egress hardening — no redirects, delivery-time IP screen (issue #78, 2026-09-05):**
  `validateHookURL` (https-only except loopback-http) was bypassable: both delivery clients followed
  up to 10 redirects across schemes/hosts, and Go forwards custom headers cross-host (only
  Authorization/Cookie are stripped), so a validated `https://…` hook could 302/307/308 the
  `X-Walgit-Signature` + body onto `http://169.254.169.254/` or any private IP. The fix hardens BOTH
  lanes (`hookClient` and `hookClientInsecure`): `CheckRedirect` refuses ALL redirects (a 3xx is a
  delivery failure, cursor untouched, replay next pass) and every dial resolves, screens, and dials
  only public survivors with check and connect pinned to one resolution (no TOCTOU; literal private
  IPs fail pre-SYN; unresolvable fails closed). Refuse-all rather than same-host-only because
  same-host still permits an https→http plaintext downgrade of secret+body, and "same host" is
  DNS-defined anyway. Trade-off, stated plainly: benign redirects fail too — a trailing-slash
  normalization hop or an http→https upgrade is a delivery error, not a silent follow, so hook URLs
  must be configured in canonical (final, https) form. The screen lives in the shared stdlib-only
  `internal/egress` package (one range table — RFC1918, link-local incl. 169.254.169.254, CGNAT,
  benchmark 198.18/15, TEST-NETs, ULA, multicast, unspecified, reserved, incl. mapped-v6 — reused
  verbatim by the events bridge, which keeps its existing names as thin wrappers), so the two sinks
  cannot drift apart. Loopback stays allowed on both validation and dial (tenant dev hooks target
  localhost; contract tests deliver to httptest servers). Screening runs at every delivery (DNS can
  change between config and POST), not just at config time. No new goroutines or locks, so no new
  concurrency hazard (the shared transports are goroutine-safe, dials are sequential).

## Explicitly out of scope

- Email delivery (§5.2 seam), push notifications, mobile.
- Per-thread mute/snooze and per-repo notification settings UI (GitHub's "participating vs all" dial) — v1 emits for participants + watchers; a preference object is the natural extension.
- Org-level webhook configs (the schema is repo-scoped; org hooks are a future family).
- Notification digest batching, threading/grouping rules beyond the (user, thread, reason) dedup.
- Webhook retry-with-backoff schedules (v1 is at-least-once via cursor; a failed delivery retries on the next pass), delivery payload customization/templates, and bot signatures.
- In-repo "notifications" for git events (pushes) — those stay on the WAL events bridge (09).
