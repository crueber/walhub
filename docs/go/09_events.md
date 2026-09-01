# 09 — Events bridge (internal/events)

> Source: MASTER_RUST_SPEC.md §12 (events bridge), with §3.4 (roles), §5.1/5.4 (keys & seq semantics), §8.3 (notify route), §15.1 (config) · Status: normative for the walhub Go implementation.

## 1. The principle

Events are produced **FROM THE WAL** by one small service (role `events`) — never by the push path.
Every writer (serving host, broker, CLI, import) is covered because the bridge reads the log; a
webhook failure adds lag (a metric), never a push failure. In Go this is package `internal/events`
(module `git.packden.us/crueber/walhub`), a single long-lived goroutine plus one HTTP handler — no
part of `internal/git`, `internal/wal` publish, or `internal/api` emits events.

The bridge is instantiated only when **both** hold: the instance's `server.roles` includes `events`
(empty = all roles) and `events.webhook_url` is set. Otherwise `POST /_events/notify` answers 404
and no bridge goroutine is started.

```
publishers (any role) ──▶ WAL log segments (bucket, immutable)
                                   │
                     events role ──│── one catch_up goroutine ──▶ sink(s) ──▶ cursor CAS
                                   ├── POST /_events/notify  (wake-ups)
                                   └── sweep ticker (events.sweep_interval, default 5m)
```

## 2. Where the data comes from

The bridge talks to the WAL engine (`internal/wal`), never to raw store keys except for its own
cursor object. Per repo it needs:

- a **fresh refs sync** (revalidate the manifest; `wal.freshness_ttl` does not apply — the bridge
  always revalidates) giving `head_seq`, `min_seq` (entries below are folded into the checkpoint),
  and the object format (sha1 → 40-hex OIDs, sha256 → 64-hex);
- a **log read** `read_log(from, to)` over the framed `log/<first_seq:016x>.pb` segments
  (`uvarint(len) || LogEntry` frames; decoding stops at the first incomplete trailing frame — that
  is normal for a growing appendable segment, not an error).

Only `ENTRY_KIND_PUSH` and `ENTRY_KIND_REF_UPDATE` entries emit. `COMPACT`, `CHECKPOINT`,
`SETTINGS`, and symbolic HEAD retargets (`RefUpdate.new_symbolic_target` set) emit **nothing**.
A PUSH entry without a `txn` emits nothing either.

## 3. The `ref` event (wire shape, normative)

One event per ref update; emitted in entry seq order, and within one transaction in the recorded
update order. Exact Go type (note `_walgit.seq` is a JSON **string** — the uint64-as-string
convention; the field name keeps the Rust wire name verbatim for bucket-format compatibility):

```go
// internal/events/event.go
type RefEvent struct {
    Action        string `json:"action"`          // "create" | "update" | "delete" (force is NOT a wire action)
    RefType       string `json:"ref_type"`        // "branch" | "tag" | ""
    RefName       string `json:"ref_name"`        // full ref, e.g. "refs/heads/main"
    Old           string `json:"old"`             // hex OID; zero OID on create
    New           string `json:"new"`             // hex OID; zero OID on delete
    Pusher        string `json:"pusher"`          // log entry meta "principal"; "" when absent
    CorrelationID string `json:"correlation_id"`  // user-visible request id (meta "request_id")
    Repo          string `json:"repo"`            // "owner/name"
    Walgit        Walgit `json:"_walgit"`
}

type Walgit struct {
    SchemaVersion int    `json:"schema_version"` // 1
    Seq           string `json:"seq"`            // STRING, decimal uint64 — never a JSON number
    EntryKind     string `json:"entry_kind"`     // "push" | "ref_update"
    RequestID     string `json:"request_id"`     // same value as CorrelationID
}
```

Classification rules (normative, from the `RefUpdate` fields in the WAL entry):

| Condition on `(old_oid, new_oid)` | `action` |
|---|---|
| `old` all-zero, `new` non-zero | `create` |
| `old` non-zero, `new` all-zero | `delete` |
| both non-zero and different | `update` |
| `old == new` (incl. 0→0) | **no event** (no-op suppression) |

- `ref_type`: `refs/heads/` prefix → `branch`; `refs/tags/` → `tag`; anything else → `""`.
- Zero OIDs are the **full zero OID of the other side's length** (40 hex chars for sha1, 64 for
  sha256) — never `""`. Length comes from the repo's object format in the manifest.
- `old`/`new` carry the literal `old_oid`/`new_oid` from the WAL; peel information
  (`new_peeled`) is not put on the wire — consumers resolve via git if they care.
- `pusher` = the log entry's `meta["principal"]` (set from `X-Walgit-Principal` on forwarded
  pushes); `correlation_id` and `_walgit.request_id` = `meta["request_id"]` (the middleware honors
  an inbound `x-request-id`); empty string when the metadata is absent.
- `action = "force"` does not exist; consumers derive forcedness from their remembered old value.

Example (this exact shape is the compatibility contract):

```json
{
  "action": "update",
  "ref_type": "branch",
  "ref_name": "refs/heads/main",
  "old": "48a0637…", "new": "cb38da1…",
  "pusher": "alice@example.com",
  "correlation_id": "d1f916f7-…",
  "repo": "acme/monorepo",
  "_walgit": {"schema_version": 1, "seq": "42", "entry_kind": "push", "request_id": "d1f916f7-…"}
}
```

## 4. Delivery (the sink)

### 4.1 Wire contract

Each catch-up POSTs **one JSON array** — the whole batch `(cursor, head]` — to
`events.webhook_url`:

- `Content-Type: application/json`
- `X-Walgit-Delivery: <sha1 hex of the exact request body>` (lowercase hex, `crypto/sha1`)
- `X-Walgit-Signature: sha256=<hex HMAC-SHA256(body, events.webhook_secret)>` — header present only
  when `webhook_secret` is configured (`crypto/hmac` + `crypto/sha256`; stdlib only).

**2xx acks.** Anything else, or a **10 s timeout**, leaves the cursor untouched and replays the
same range at the next wake-up. Delivery is at-least-once.

**Dedup key (normative): `(repo, _walgit.seq, ref_name)`** (or `X-Walgit-Delivery` per batch).
Ordering guarantee: by seq within a repo; **nothing across repos**.

### 4.2 Go client

`net/http` with one client reused per process (default transport is fine; the batch is small). The
entire POST — connect through body read — runs under `context.WithTimeout(ctx, 10*time.Second)`.
`http.Client.Timeout` is NOT set (it would also cap the connection reuse); the timeout lives on the
request context.

### 4.3 Sink abstraction

```go
type Sink interface {
    Name() string                          // metric label, e.g. "webhook"
    Deliver(ctx context.Context, repo string, batch []RefEvent) error
}
```

Exactly one built-in sink ships: `webhookSink` above. The interface exists so GitHub-like
integrations (issues/PR bots) can register additional sinks later without touching the loop.
Sinks are called **sequentially, one POST per sink, for the whole batch**; if any sink fails the
catch-up aborts there with the cursor untouched (at-least-once, no gaps) and the failure is logged
at WARN with the sink name.

#### Concurrency

- **Hazard:** fan-out per event would multiply webhook load and complicate "cursor untouched on
  failure". **Avoidance:** one goroutine (the bridge, §5) performs sink POSTs synchronously, one
  POST per sink per catch-up; volume is tiny, so no per-event goroutines. The 10 s context timeout
  bounds the worst case per wake-up; a slow webhook delays the sweep/notify queue (bounded, §5.2)
  rather than growing memory. See 13_concurrency.md.

## 5. The catch-up loop

### 5.1 Algorithm — `catchUp(ctx, repo)` (normative order)

Serialized process-wide (volume is tiny). Exactly, in order:

1. **Refs sync** (fresh manifest). `head = head_seq`.
2. `readable_from = min_seq − 1` (entries below are folded into the checkpoint). A **cold cursor**
   (object absent) defaults to `readable_from` — everything still in the log window is published
   once; operators pre-seed the cursor to skip history (§5.3). A cursor **below** `readable_from`
   is a **gap**: increment `events_bridge_gap_total{repo}` + WARN — never silently repaired; the
   effective read start is then `min_seq` (folded entries are unreadable).
3. **Lag gauge** `events_bridge_lag_entries{repo} = head − cursor` (always recorded, even when the
   subsequent publish fails).
4. If `head > cursor`: `read_log(max(cursor+1, min_seq), head)` → ref events (§3) → **publish to
   every sink; a sink failure aborts here, cursor untouched**.
5. **CAS `events/cursor.json` to head** — `{"published_seq": N, "updated_at": RFC3339}` — via the
   store's generic CAS helper (read version → conditional PUT; 412 = contention). **Lost CAS
   (another bridge advanced it) is treated as success**: our emission was a duplicate and the dedup
   key holds.

The store key is repo-relative `events/cursor.json`, bucket-format compatible with the Rust
implementation (same JSON fields, same CAS semantics).

### 5.2 Process shape

```go
// internal/events/bridge.go
type Bridge struct {
    sinks   []Sink
    work    chan string        // repo keys "owner/name"; capacity 64
    pending map[string]bool    // coalescing set, guarded by mu (only owner + enqueue paths touch it)
    mu      sync.Mutex
    // deps: wal registry handle, store, metrics, logger; ctx cancels everything
}
```

One goroutine owns `catchUp`. Wake-ups (notify handler, sweep ticker) call `Bridge.Wake(repo)`,
which coalesces (`pending` set) and performs a **non-blocking send** on `work`. The bridge drains
`work`, moving each repo into `pending` before running `catchUp` and removing it after — a repo
woken during its own catch-up runs again once, which is harmless (at-least-once).

Startup: role `events` + `webhook_url` present → `go bridge.Run(ctx)`. Shutdown: `ctx` cancel →
the goroutine finishes the in-flight catch-up (cursor is always consistent) and exits; the sweep
ticker stops; the HTTP server's drain (§3.4 of the spec) does not wait for it beyond the normal
process exit.

#### Concurrency

- **Hazard:** concurrent `catchUp` for the same repo (notify storm + sweep) → duplicate POSTs and
  CAS fights. **Avoidance:** exactly one bridge goroutine; all wake-ups funnel through the bounded
  `work` channel — serialization is fine because volume is tiny (a handful of repos, batch sizes in
  the tens).
- **Cross-instance:** several instances may run the `events` role. The **cursor CAS is the lock**:
  the loser's publish was a duplicate and its lost CAS is treated as success (step 5). No lease is
  needed (leases, §4.9 of the spec, are deliberately NOT used here).
- **Hazard:** the notify handler blocking on a full `work` channel couples webhook lag to notifier
  HTTP latency (and Pub/Sub redelivery). **Avoidance:** `Wake` never blocks — non-blocking send +
  coalescing set; when the channel is full or the repo is already pending, the wake is a no-op and
  the **sweep is the backstop**. The handler reports `dropped` (§6.3) so operators can see it.
- Every goroutine (bridge, sweep ticker) exits on `ctx` cancel; the ticker's `time.Ticker` is
  stopped with `defer ticker.Stop()`. See 13_concurrency.md.

## 6. Wake-ups

Both wake-up kinds are idempotent and only ever call `catchUp`.

### 6.1 `POST /_events/notify`

Route in `internal/server` (chi route `r.Post("/_events/notify", …)`, divergence D1), auth: **read**
(`require_read` in the handler); **404 when this instance has no bridge**. Accepted bodies —
parsed with `encoding/json` into explicit structs, tolerant of extra fields:

| Body shape | Recognition | Extracted key |
|---|---|---|
| GCS Pub/Sub push envelope | `message.attributes.eventType == "OBJECT_FINALIZE"` | `message.attributes.objectId` (also check `objectId` under `message`) |
| S3 event notification | any `Records[].eventName` with prefix `ObjectCreated:` | `Records[].s3.object.key` |
| Glue | `{"key": "…"}` | `key` |
| Glue | `{"repo": "o/r"}` | repo directly |

Trigger rule: an object **key ending `repos/<owner>/<repo>/manifest.pb`** (the commit point
itself, as the notification) schedules `catchUp(owner/name)`. The `{"repo":"o/r"}` glue form
schedules directly. Everything else — other keys, other event types, unparseable-but-valid JSON —
is **acked and ignored**.

Responses:

- `200` + a JSON **array of reports** on ack:
  `[{"repo":"acme/monorepo","status":"queued"|"dropped"|"ignored"}]` (one entry per recognized
  trigger; `dropped` = coalesced or channel full — the sweep will cover it).
- `503` **on any sink failure**: the notifier redelivers. (The handler runs the failing catch-up
  synchronously only when it must report the sink failure — i.e. the request that triggered the
  failing batch waits for the batch outcome, 10 s timeout.)
- `404` when no bridge (no `events` role or no `webhook_url`).

Example (notify is how the bucket fires the bridge; GCS Pub/Sub push and S3 notifications point
here):

```bash
curl -s -X POST http://127.0.0.1:8080/_events/notify \
  -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
  -d '{"key":"repos/acme/monorepo/manifest.pb"}'
# → 200 [{"repo":"acme/monorepo","status":"queued"}]
```

### 6.2 Sweep

Every `events.sweep_interval` (default `"5m"`; `"0s"` = off): `catchUp` for every repo in the
registry. Not needed for correctness — it is the **backstop and the health check**: anything a
sweep publishes means notifications are not flowing → `events_bridge_sweep_found_total` increment
+ WARN log. The ticker lives in the bridge goroutine (`select` on `ticker.C` and `ctx.Done()`).

## 7. Backfill contract (normative, consumer-side)

On any gap, consumers read the WAL log from their last seq and treat each PUSH/REF_UPDATE ref
transaction as the missed events. The webhook is a **latency optimization over polling the log**;
correctness never depends on it. This is why the bridge may drop wake-ups, coalesce, and treat a
lost cursor CAS as success without a repair protocol.

## 8. Metrics and config

Prometheus exposition is the shared text renderer of `internal/server` (§8.10 metric inventory);
the bridge only registers/increments:

| Metric | Type | Meaning |
|---|---|---|
| `events_published_total{sink}` | counter | events successfully delivered per sink |
| `events_bridge_lag_entries{repo}` | gauge | `head_seq − cursor` at each catch-up |
| `events_bridge_gap_total{repo}` | counter | cursor below `readable_from` (never silently repaired) |
| `events_bridge_sweep_found_total` | counter | events published by a sweep (= notifications not flowing) |

Config (TOML keys keep their names; env override `WALGIT__EVENTS__WEBHOOK_URL` etc.):

```toml
[events]
webhook_url    = "https://hooks.example.com/walgit"  # required for the bridge to exist
webhook_secret = "rotate-me-regularly"               # optional; enables X-Walgit-Signature
sweep_interval = "5m"                                # "0s" disables the sweep
```

## 9. What is NOT an event

Push denials, auth failures (metrics + logs), LFS, compaction/checkpoints (already WAL entries),
repo/policy admin. Only PUSH and REF_UPDATE entries produce `ref` events.

## 10. Minimal test surface

The implementation must be provable against: event classification table (§3), zero-OID lengths for
both object formats, `_walgit.seq` string encoding, signature headers over the exact body bytes,
10 s timeout leaving the cursor untouched, lost-CAS-as-success, cold-cursor default, gap counting,
and the notify parser table (§6.1). Use the memory store backend; a deterministic fake sink that
fails N times exercises at-least-once replay.

## Decisions & deviations from the Rust design

- **No in-process delivery retry**: a failed/timeout POST leaves the cursor and replays at the next
  wake-up (notify redelivery + sweep) — matches §12.2 exactly and keeps the client dead simple; the
  10 s bound lives on the request context, not `http.Client.Timeout`, so connection pooling stays.
- **One bridge goroutine per process, sequential sinks, whole-batch single POST** (no per-event
  fan-out): volume is tiny and the publish-then-CAS-cursor ordering must be linear; the bounded
  work channel + coalescing set replaces the Rust async task scheduling.
- **Notify handler never blocks**: non-blocking wake send, bounded channel (64), `dropped` report,
  sweep as backstop — protects notifier HTTP latency from webhook lag.
- **Sink is an interface with one built-in webhook implementation**: keeps the loop open to future
  integrations without core changes; failure semantics ("any sink failure aborts, cursor untouched")
  preserved verbatim.
- **Report array field names for `/_events/notify` (`repo`, `status`) are a Go-side choice**: the
  Rust spec fixes only "200 + JSON array of reports"; the values `queued|dropped|ignored` make the
  drop-vs-ignore distinction observable.
- **Effective read start `max(cursor+1, min_seq)` when a gap exists**: implied by §12.3 step 2
  (folded entries are unreadable) and made explicit here so no implementation reads below `min_seq`.
- **`http.Client.Timeout` unset in favor of per-request context timeouts**: pure Go mechanics;
  behaviorally identical to the Rust 10 s delivery timeout.
- Everything else — event JSON shape (incl. `_walgit.seq` as string), zero-OID rule, sha1 delivery
  id / sha256 HMAC signature headers, dedup key, cursor key/JSON, CAS semantics, notify body
  parsers, sweep defaults, and metric names — is carried over verbatim for bucket- and wire-format
  compatibility with the Rust implementation.
