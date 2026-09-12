# 07 — JSON API, SSE envelope, tasks & caching

> Source: MASTER_RUST_SPEC.md §9 (§9.1–§9.6), §6.8, §8.2–§8.3, §14, §15.2, §20 item 4 · Status: normative for the walhub Go implementation.

## 1. Scope and package seams

`internal/api` implements every JSON endpoint, the SSE envelope, the task/ops surface, and the two-tier
render cache. It owns **wire shapes only** — repo state comes from `internal/wal` (sync levels, manifest),
git output from `internal/git` (render recipes run exact `git` argv), auth from `internal/server`
middleware, long work from `internal/maintain`.

Seams that keep future GitHub-like features (issues, PRs, review) additive:

- Handlers depend on two interfaces only: `RepoView` (refs sync + object access: `Resolve`, `Tree`,
  `Blob`, `Commits`, `Commit`) and `Tasks` (`List`, `Attach`, `Begin`) — both defined in this package and
  implemented by `internal/wal` / `internal/maintain`. New feature domains register their own lane routes;
  nothing in the core response shapes changes.
- Route registration is table-driven: `[]Route{Method, Pattern, Handler, Auth}` consumed by
  `internal/server`. Lanes (`/api` vs `/api-browser`) are resolved **before** dispatch: strip
  `/{owner}/{repo}[.git]/api` or `/api-browser` and mark the lane; handlers are lane-agnostic (§8.2).
- The repository prefix is the only routing key. **Chi** (divergence D1) routes the obvious cases
  (`r.Get("/{owner}/{repo}/api/refs", h)`); `.git`-suffix stripping and `/{owner}/{repo}/api` (no trailing
  slash) are handled by the shared fallback parser (06_server_http.md §3.2) — chi wildcards cannot express
  `[{o}/{r}[.git]]`. Parse order: exact
  mux → fallback (`/{o}/{r}[.git]/<sub>`; bad repo id → 404; `.git` accepted everywhere and stripped).

Binary/module identity: `walhub`, module `git.packden.us/crueber/walhub`.

## 2. Wire conventions (§9.1 — normative)

| Rule | Implementation |
|---|---|
| Success | `200` + JSON body + cache headers (§4 below) |
| Errors | non-2xx with a **plain-text body** (`text/plain; charset=utf-8`), shown verbatim in the UI. NO JSON error envelope. `404` for unknown owner/repo/ref/path/sha — git's "not a tree object", "unknown revision", "bad revision", "does not exist" all map to `404` |
| Null safety | every array field serializes as `[]` when empty, never `null`. In Go: initialize slices (`entries := []Entry{}`); never emit a nil slice |
| Timestamps | RFC 3339, UTC: `t.UTC().Format(time.RFC3339)` |
| SHAs | full 40-hex (or 64-hex for sha256 repos) everywhere; the UI abbreviates. Never truncate server-side |
| Sizes | bytes, integers |
| Path encoding | clients `encodeURIComponent` each segment separately; the server decodes **per segment** (`r.PathValue` gives one decoded segment — re-split on `/` only on the raw path before decoding). Never `url.PathUnescape` a joined multi-segment string |
| Consistency | reads are as fresh as a `git fetch` from the same host: after a push is acknowledged the next API call on ANY node reflects it (refs-level sync per request). Writes on this surface are admin-only; content moves over git/LFS, never JSON |

## 3. Endpoint reference (wire contract, verbatim from §9.5)

These shapes are frozen. Field names, ordering of *semantics* (not JSON key order), and status codes are
the contract; do not "improve" them.

```text
GET /api/v1                    → {version:1, base, browser_base:"/api/v1", sdk, auth, endpoints[]}
GET /api/v1/me                 → {principal, write, anonymous, admin?, avatar_url?} | 401 (no-store)
GET /api/v1/owners             → ["demo","jane"] (sorted; from the STORE, not disk)
GET /api/v1/owners/{o}/repos   → ["hello","walgit"] (short names; 200 [] for unknown owner)
GET /{o}/{r}/api               → {owner, name, full_name, head:{name,sha}|null, branches, tags,
                                  health:"empty"|"healthy"|"degraded", missing_total? (degraded only),
                                  clone_url, ssh_clone_url?, html_url, api_url}   (SWR + ETag "<head sha>" + "~degraded"
                                  suffix when degraded; "" when unborn — §9.1)
PUT/DELETE /{o}/{r}/api        → create (write) / delete (admin)
GET …/refs                     → {head:{name,sha}|null} — O(1), default branch only (SWR + ETag)
GET …/refs/{branches|tags}?prefix=&q=&after=&n=
      → {refs:[{name,sha}],more}   (name-sorted page; prefix under the namespace; q case-insensitive
        substring on the short name; after = name cursor strictly greater, byte order; n default 100
        max 1000; tag sha = peeled; SWR; SSE variant per §6 below)
GET …/resolve[/{rest}]         → {ref, sha, path, kind:"branch"|"tag"|"commit"} (SWR + ETag "<sha>")
GET …/tree/{rev}[/{path}]      → {ref, sha, path, entries:[{name,type:"blob"|"tree"|"commit",mode,size,sha}],
                                  commit?: Commit (newest touching path), readme?: {name,contents}}
GET …/blob/{rev}/{path}[?raw]  → {ref, sha, path, name, size, contents?|binary?:true|too_large?:true}
GET …/commits?ref=&path=&skip=&n= → {ref, sha, commits:[Commit], more}
Commit = {sha, parents[], author, author_email, author_date, committer, commit_date, subject,
          body (message minus trailer block, trimmed), trailers:[{key,value}]}
GET …/commit/{sha}             → {commit, stats:[{path,additions,deletions}], patch}
GET …/policy                   → policy JSON (missing = allow-all)
PUT/DELETE …/policy            → admin; PUT validates (400 with reasons; fail closed on the next push)
POST …/policy/validate         → {ok, errors[], rules, groups, protect}
POST …/policy/dry-run?last=N   → {pushes, allowed, denied,
                                  results:[{seq, at, principal, atomic, refs:[{name, ok, reason, force}]}]}
GET …/settings                 → {revision, author, updated_at, message, toml} (revision 0 = none)
PUT …/settings?message=        → body = TOML ≤ 16 KiB; validated; 200 {revision}; 400 + reason
DELETE …/settings              → publishes empty (back to host config)
GET …/settings/effective       → effective [bundles]/[maintenance]/[compaction]/[upstream] as TOML
                                  (application/toml; no host secrets, no token_env)
GET …/settings/history         → {min_seq, entries:[{seq, revision, author, message, at, toml}]}
GET …/settings/describe        → {settings, sections, strategies:[{name, kind, base, schedule,
                                  schedule_human, next, keep, backfill_max, min_commits, refs, chain,
                                  filter}], bundles, maintenance:{checkpoints, interval_secs,
                                  this_host:{name, serves, maintains, disk, max_pack_bytes,
                                  cache_budget_bytes, roles}}, compaction, upstream:{git, lfs,
                                  token_env(bool), follow, follow_interval_secs, last_round?}, fields:
                                  [{key, value, host_value, source:"host"|"setting"}], head_seq}
POST …/settings/validate       → same shape for the WOULD-BE effective config + {ok, errors[]}
GET …/overview                 → walhub-specific WAL health (no-store): {repo, clone_url, ssh_clone_url?, hostname,
                                  health:{status:"ok"|"degraded"|"error", issues[], deep,
                                  suggestions:[{op, params?, reason, auto?}]},
                                  fsck?:{missing_total, missing[] (bounded sample, [] never null),
                                  problems, repaired_seq, at?, host?, repair_stalled,
                                  upstream? ("" absent = none configured)} — §12.1,
                                  manifest:{version, next_seq, min_seq, segments[], tail_entries,
                                  entries, checkpoint?, packset?, advertised_bundle_uri?, last_push?},
                                  local:{version, next_seq, bootstrap, reconciled, size_bytes},
                                  packs:{live, live_bytes, pushes}, bundles:[{sha,size,at_seq,created,
                                  uri,strategy,kind,base_id,creation_token,filter,tips}],
                                  bundle_plan:{slots:[{strategy,kind,slot,status,detail,bundle_id}],
                                  upcoming[], maintainers[], orphaned}, compactions[], node{counters}}
GET …/ops                      → {available:[OpSpec], recent:[TaskRecord], bundle_strategies}
POST …/ops/{op}                → SSE attach (tasks, §10)
GET …/tasks                    → {hostname, running:[TaskRecord], recent:[TaskRecord]} (no-store)
GET …/tasks/{id}               → TaskRecord JSON, or SSE attach with `Accept: text/event-stream`
```

Lane note: every repo-scoped path above exists under both `/{o}/{r}/api/…` and
`/{o}/{r}/api-browser/…` (same handlers; browser lane sends `credentials: include` for cross-origin).
Non-repo endpoints have `/api/v1` and `/api-browser/v1` twins, plus `/services/api/…` twins for
`owners`/`instance` — with one deliberate carve-out: the self-service SSH-key surface
(`GET`/`POST`/`DELETE /api/v1/ssh-keys`, 17_ssh.md §3) is token-lane-only and has no
`/api-browser/v1` twin. Nothing consumes twins there — the `/keys` page fetches the `/api/v1`
routes directly — so twins would widen the browser-lane (cookie) surface for no consumer.

## 4. The three cache classes (§9.2 — the central design rule)

| Class | Headers (exact values) |
|---|---|
| **sha-addressed** (full 40/64-hex in the `{sha}`/`{rev}` position): `tree/{sha}/…`, `blob/{sha}/…`, `commits?ref={sha}`, `commit/{sha}` | `Cache-Control: private, max-age=31536000, immutable` |
| **ref-dependent**: `owners*`, `refs*`, `resolve`, and any tree/blob/commits/commit addressed by a NAME | `Cache-Control: private, max-age=0, stale-while-revalidate=60` + `ETag: "<resolved sha>"` + `If-None-Match` → `304` |
| **mutable collab** (issue #280) + the repo summary (Forgejo #381) + the repos/detailed listing (Forgejo #384) + the owner profile (Forgejo #385): any GET whose resource can change via a direct user action — issue/PR threads (`ETag: "v<version>"`), social counters, single/latest/list releases, the pull view, identity profiles/orgs/teams/invites/access docs, the repo summary (visibility, open counts, description, mirror state all mutate with no ref movement), the repos/detailed rows (visibility, mirror + mirror_upstream mutate with no ref movement; the ETag is a content hash over the rendered rows), and the owner profile (display name, location, timezone, bio all PUT-editable with no ref movement; the ETag is a content hash over the served doc) | `Cache-Control: private, no-cache` + the existing version ETag + `If-None-Match` → `304` |

- Mutability, not addressability, decides the class: SWR's stale-serve window is for content whose
  staleness is bounded by ref movement (refs move rarely; seconds-old is fine). User-mutable state
  revalidates on every read instead — `no-cache` still caches in the browser, and the version ETag
  still makes unchanged responses 304 with zero body, so only the stale-serve window is lost (which
  is exactly the #259/#280 flip-flop: refresh 1 painting pre-mutation state while revalidation
  lands, refresh 2 painting post-mutation state).

- Ref-dependent ETags: the value is the **quoted resolved sha** (`ETag: "cb38da1…"`), matching a bare double-quoted hex
  string in the header; compare `If-None-Match` by stripping quotes and weak prefixes.
  (Mutable-collab ETags are version tokens — `"v<version>"`, store versions, folded view stamps —
  compared the same way.)
- SWR is honored server-side too: an expired-but-cached render MAY be served immediately (it is already
  within the 60 s stale window semantics) while revalidation happens in the background.
- **Navigation flow (drives every handler):** one ref-dependent call (`resolve` — SWR paints instantly,
  revalidates), then one sha-addressed call (immutable; browser cache hit on revisits). `refs` (head-only)
  is fetched once per repo visit.
- Implementer complexity rules: `resolve` O(path segments), `refs` O(1), ref lists O(page) — **never**
  "load all refs then filter". Keep an in-process LRU of resolved ref→sha and of rendered immutable JSON
  keyed by the repo's ref-state version (the manifest revision, §5).

### Concurrency

The render cache MUST NOT stampede: N concurrent misses for one key run the render once.
See §5.1 — a per-key single-flight layer (hand-rolled, no third-party `singleflight` per the dependency
policy; the pattern is the canonical one in `13_concurrency.md`).

## 5. Caching implementation in Go

Two in-process LRU caches per instance (hand-rolled weighted LRU — ~80 lines, dependency policy forbids
`golang-lru`/`ristretto`):

1. **Ref→sha LRU**: key `(owner, repo, refname)` → sha. Validated against the manifest revision: entries
   carry the revision they were resolved at; a sync returning a newer revision invalidates lazily (entry
   revision != current revision → re-resolve). Sized by entries (recommend 4 096).
2. **Rendered-immutable LRU**: key `(owner, repo, request-key)` → rendered JSON bytes, where `request-key`
   is the canonical request path + query (e.g. `tree/0123…abc/src` or `commit/0123…abc`). Entries carry
   the manifest revision; a revision change means re-render. Weights = bytes of the JSON, total budget
   `cache.render_cache_bytes` (default 256 MiB, host config).

**Shared bucket render cache** (when the repo is served remotely — pack set not local — and
`cache.shared_render_cache` is true, default): rendered immutable JSON is mirrored into the object store
at `cache/api/v1/<sha1-of-key>.json`. `<sha1-of-key>` = hex SHA-1 of the canonical request key. The file
is an envelope so stale generations are harmless:

```json
{"revision": 118, "body": <raw JSON bytes>}
```

Read path on a remote-served repo: check local LRU → conditional GET of the bucket object (version = the
revision the manifest gives us; `Unchanged` → use) → else render, then `put_file_parallel` the envelope
(Create-if-absent semantics; a lost race is fine — same key, same body as long as revisions match; on a
revision mismatch discard). Bucket writes happen on a worker goroutine and NEVER delay the response.

```toml
# walgit.toml — only cache-relevant keys live in host config
[cache]
shared_render_cache = true   # mirror immutable API JSON into the bucket
```

### 5.1 ### Concurrency — render-cache single-flight

Hazard: a cold popular key (e.g. the immutable tree of `main`'s head after a push) receives N concurrent
requests; N parallel `git` renders burn the per-repo git semaphore (§8, `max_concurrent_per_repo`) and the
timeout budget. Avoidance — per-key single-flight, never holding a lock across I/O:

```go
type RenderCache struct {
    mu       sync.Mutex
    lru      *lru                   // key -> *entry{revision, body, etag}
    inflight map[string]*renderCall // key -> call
}
type renderCall struct {
    done chan struct{} // closed exactly once by the renderer
    body []byte; etag string; err error
}

func (c *RenderCache) Get(key string, rev uint64, render func() ([]byte, string, error)) ([]byte, string, error) {
    c.mu.Lock()
    if e := c.lru.Get(key); e != nil && e.revision == rev { // revision-stamped
        c.mu.Unlock()
        return e.body, e.etag, nil
    }
    if call := c.inflight[key]; call != nil {
        c.mu.Unlock()
        select { // wait for the OTHER goroutine's render; bounded join:
        case <-call.done:
            return call.body, call.etag, call.err
        case <-time.After(30 * time.Second): // then render ourselves (fall through)
        }
    }
    call := &renderCall{done: make(chan struct{})}
    c.inflight[key] = call
    c.mu.Unlock() // NEVER render under the lock

    call.body, call.etag, call.err = render()
    c.mu.Lock()
    if call.err == nil { c.lru.Put(key, &entry{revision: rev, body: call.body, etag: call.etag}) }
    delete(c.inflight, key) // only the leader deletes its own entry
    c.mu.Unlock()
    close(call.done)
    return call.body, call.etag, call.err
}
```

Rules (canonical playbook in `13_concurrency.md`): lock order is `mu` → nothing (leaf lock); the render
function runs lock-free; the bounded join prevents a crashed/hung leader from wedging followers (worst
case: one extra render); every path closes `done` exactly once; the leader is the only remover of its own
inflight entry (no lost-wakeup).

## 6. The SSE envelope (§9.3)

Sent when the request's `Accept` contains `text/event-stream` AND the answer needs long work (the repo's
packs are not ready / remote-served). Otherwise plain JSON. Format:

1. Headers first: `200`, `Content-Type: text/event-stream; charset=utf-8`, `Cache-Control: no-store`,
   `X-Accel-Buffering: no`.
2. Opener comment: `: walgit\n\n` (flushed immediately).
3. Packets: `event: <name>\ndata: <json>\n\n`. Data JSON is produced by `encoding/json` and contains no
   raw newlines, so one `data:` line per packet always suffices.
4. `: keepalive` comment every 10 s while idle.
5. Terminal: **exactly one** of:

| event | data |
|---|---|
| `notice` | `{"text": "…"}` — what is happening now |
| `progress` | `{"label","done","total"?,"unit","percent"?}` — latest bar per label wins |
| `task` | `{TaskRecord}` — a background task this request depends on |
| `result` | exactly the JSON the plain endpoint returns (terminal) |
| `error` | `{"status": 503, "message": "…"}` (terminal) |

- Work continues after client disconnect: the render runs to completion and lands in the render cache;
  the next request for the same sha gets plain JSON. Cancellation stops only the *writing*, never the work.
- Streamed answers are not HTTP-cacheable but ARE kept in the render cache (§5).

Go writer sketch (stdlib only):

```go
type SSE struct {
    w http.ResponseWriter; fl http.Flusher
    rc *http.ResponseController // Go 1.20+: per-request write deadlines
    ctx context.Context; ka *time.Ticker
    mu sync.Mutex // serializes packet writes vs keepalive (no tearing)
    ended bool    // terminal-once
}

func NewSSE(w http.ResponseWriter, r *http.Request) (*SSE, bool) {
    fl, ok := w.(http.Flusher)
    if !ok { return nil, false }
    h := w.Header()
    h.Set("Content-Type", "text/event-stream; charset=utf-8")
    h.Set("Cache-Control", "no-store")
    h.Set("X-Accel-Buffering", "no")
    w.WriteHeader(http.StatusOK)
    io.WriteString(w, ": walgit\n\n"); fl.Flush()
    s := &SSE{w: w, fl: fl, rc: http.NewResponseController(w), ctx: r.Context()}
    s.ka = time.NewTicker(10 * time.Second)
    go func() { for range s.ka.C { if !s.comment(": keepalive") { s.ka.Stop(); return } } }()
    return s, true
}

// Event returns false when the client is gone or a terminal packet was already sent.
func (s *SSE) Event(name, dataJSON string) bool {
    s.mu.Lock(); defer s.mu.Unlock()
    if s.ended { return false }
    if s.write("event: "+name+"\ndata: "+dataJSON+"\n\n") != nil { s.ka.Stop(); return false }
    if name == "result" || name == "error" { s.ended = true; s.ka.Stop() }
    return true
}

func (s *SSE) comment(c string) bool {
    s.mu.Lock(); defer s.mu.Unlock()
    if s.ended { return false }
    select { case <-s.ctx.Done(): return false; default: }
    return s.write(c+"\n\n") == nil
}

func (s *SSE) write(p string) error {
    s.rc.SetWriteDeadline(time.Now().Add(15 * time.Second)) // a stuck client must not pin the goroutine
    _, err := io.WriteString(s.w, p)
    if err == nil { s.fl.Flush() }
    return err
}

func (s *SSE) Close() { s.ka.Stop() } // caller: defer
```

### Concurrency — per-request subscription and backpressure

- **Hazard 1: subscription leak.** A task/progress subscription left registered after the request ends
  holds a channel forever (goroutine + memory leak, and dead subscribers accumulate per repo).
  Avoidance: subscribe returns a cancel func; the handler runs `sub, cancel := broker.Subscribe(); defer
  cancel()`. The broker's publish loop treats a closed/cancelled subscriber as gone (remove under the
  broker mutex, never channel-close from the reader side — the *subscriber* closes nothing; the broker
  drops the channel from its map and lets GC collect it).
- **Hazard 2: slow client blocks the task.** Publishing is broadcast to many SSE clients; one stalled TCP
  window must not stall the maintenance task writing packets. Avoidance: lag-tolerant broadcast — each
  subscriber has a **bounded channel (cap 64)**; publish is `select { case ch <- p: default: drop-oldest }`
  (drain one, append the new packet) under the broker's short mutex. Tasks publish regardless of
  listeners; drops are invisible because `progress` semantics are "latest bar per label wins" and the
  task record carries the authoritative state. Never an unbounded buffer, never a blocking send.
- **Hazard 3: keepalive goroutine outlives the request.** `defer SSE.Close()` stops the ticker; the
  write deadline (15 s) bounds any single blocked write; context cancellation (`r.Context()`) is checked
  before every packet.
- Replay (tasks attach): the per-task replay buffer (200 packets, bars deduped by label, §10) is copied
  into the subscriber BEFORE live delivery, under the task's lock, so replay and live packets cannot
  interleave out of order.

## 7. Ref-list SSE dialect (the older dialect — preserved verbatim)

`GET …/refs/{branches|tags}` with `Accept: text/event-stream` streams matches as they are found:

- `event: ref` / `data: {"name":"refs/heads/main","sha":"<peeled 40-hex>"}` per match;
- terminal `event: done` / `data: {"more":<bool>}`.

Written **unbuffered** (flush after every packet), `X-Accel-Buffering: no`, **never compressed** (the
compression middleware must skip `text/event-stream` entirely — set no `Content-Encoding`, and if a
wrapping compressor exists, exclude this route). No `: walgit` opener, no keepalives, no `notice`/`progress`
packets — this dialect predates the §9.3 envelope and stays byte-compatible.

## 8. Discovery document, instance, owners (§9.6 — with the §20.4 fix)

`GET /api/v1` (public-informational; `Cache-Control: no-cache`). Divergence addition: the setup surface
(`GET|POST|PUT /api/v1/setup*`) is specified in `06_server_http.md` (Bootstrap & Setup) — it is owned
there because its behavior is bound to the boot lifecycle, not to this API's cache/SSE conventions; this
doc's discovery `endpoints[]` list MUST include `/api/v1/setup` once it exists. Everything below follows
the Rust spec.

The discovery document:

```json
{
  "version": 1,
  "base": "/api/v1",
  "browser_base": "/api/v1",
  "sdk": "/repos.js",
  "auth": {"bearer": true, "setup": "/services/setup.json", "browser": "/api-browser/v1",
            "authenticate": "/api/v1/authenticate",
            "browser_login": true, "login_url": "/_auth/login", "mode": "oidc"},
  "endpoints": [
    "/api/v1/me",
    "/api/v1/owners",
    "/api/v1/owners/{owner}/repos",
    "/api/v1/owners/{owner}/repos/detailed",
    "/api/v1/owners/{owner}/profile",
    "/{owner}/{repo}/api",
    "/{owner}/{repo}/api/refs",
    "/{owner}/{repo}/api/refs/branches",
    "/{owner}/{repo}/api/refs/tags",
    "/{owner}/{repo}/api/resolve/{ref}",
    "/{owner}/{repo}/api/tree/{rev}",
    "/{owner}/{repo}/api/blob/{rev}/{path}",
    "/{owner}/{repo}/api/commits",
    "/{owner}/{repo}/api/commit/{sha}",
    "/{owner}/{repo}/api/policy",
    "/{owner}/{repo}/api/settings",
    "/{owner}/{repo}/api/overview",
    "/{owner}/{repo}/api/ops",
    "/{owner}/{repo}/api/tasks"
  ]
}
```

**Normative fix (§20.4):** the Rust discovery document advertises `…/commit/{sha}/merge-queue`, but no
such route exists — the walhub Go discovery doc MUST list only routes the router actually serves (the
list above; keep it mechanically derived from the route table so it cannot drift). Feature-owned
routes served by `server.ExtraRoutes` append their templates via `api.RegisterExposed` from
composition in the same change (law 12 — Forgejo #272: EVERY ExtraRoutes surface registers, so
`endpoints[]` also carries the issues, pulls, releases, review, social, notify, identity, tags,
and mirror (top-level twin AND repo lanes) shapes alongside checks, the `/api/v1/repos` create
twin, and the import twins); a registered template for an unserved route is
the same bug as a missing one. Each surface pins the template↔route correspondence both ways in
its own `TestExposedCoversRoutes` (plus an exact-shape test), and composition pins the
registration in `cmd/walhub` (`TestCollabServicesRegisterDiscovery`) — removing a registration
or a template fails CI. Merge-queue data
arrives as commit trailers; there is no merge-queue endpoint. `endpoints` entries are path templates;
adding a route without updating this list is a bug. Core admin-only writes stay out of the
table-derived list (PUT/DELETE rows are never `Expose`); feature-registered capability entries
(checks token mint/revoke, the create twin) are listed anyway — `endpoints` is a *capability
hint*, not an ACL.

- `GET /api/v1/me` → `{principal, write, anonymous, admin, avatar_url?}`, `no-store`; `401` (plain text) when unauthenticated
  in a mode that requires auth. `admin` (Forgejo #371) gates the navbar's Setup menu entry;
  `avatar_url` (Forgejo #376 — the stable user-avatar URL, `?v=` cache-busted, omitted when
  the user has none or the identity surface is unwired) feeds the navbar identity control;
  the discovery auth block carries the instance `mode` (`none|token|oidc`) so the navbar knows
  whether Login/identity apply at all (never in `none` mode).
- `GET /api/v1/owners[?sort=activity&order=]` (Forgejo #283) → sorted owner names **from the STORE** (object-store listing / registry), never
  from a local disk directory; SWR class. Default (no query) is the legacy
  store order, byte-identical — no catalog read, zero added trips.
  `sort=activity` orders by the derived per-owner max-commit rollup (max
  `last_commit_time` over the owner's catalog rows — ONE catalog read
  regardless of owner count; `order=asc|desc`, default asc; unknowns always
  last in either direction; ties name-ascending, the `FilterSort` key order).
  Absent catalog degrades to name order (never 404/500 for the missing
  optional object); corrupt catalog is a 503. `GET /api/v1/owners/{o}/repos` → short repo names, `200 []` for
  an unknown owner (never 404).
- `GET /api/v1/owners/detailed[?sort=&order=]` (Forgejo #283 — NEW alongside v1, triple twins
  `/api/v1` + `/api-browser/v1` + `/services/api`, discovery-listed, SDK `owners.listDetailed`) →
  `{owners: [{name, is_org, repo_count, last_commit_sha|null, last_commit_time|null}]}` (`[]` never null;
  `is_org` (Forgejo #348) is the org-namespace marker — always present, never null (false for
  users and for instances without identity wired);
  `repo_count` (Forgejo #307) is the owner's manifest-gated live-repo count — always present,
  never null (membership implies ≥1 live repo), ghost-filtered exactly like `liveRepos`;
  the instance repo total is the sum over the uncapped payload;
  times are the per-owner max over the owner's repos — RFC 3339 UTC, null when the owner has no
  commits; never a fake epoch). Query: `sort=name|activity` (default name), `order=asc|desc`
  (default asc) — the same total order as the string list (unknowns always last, ties
  name-ascending). Membership is the registry owners list (the catalog never invents owners);
  activity rides the derived rollup in the same ONE catalog read. SWR class.
- `GET /api/v1/owners/{o}/repos/detailed` (Forgejo #248 — NEW alongside v1, triple twins
  `/api/v1` + `/api-browser/v1` + `/services/api`, discovery-listed, SDK `owners.detailed`) →
  `{repos: [{name, size_bytes|null, object_count?, head_seq?, updated_at?, last_commit_sha|null, last_commit_time|null, last_push_at?}]}` (`[]` never null;
  `size_bytes: null` = unknown/unbackfilled, `0` = verified-empty; activity nulls (Forgejo #247) =
  unknown/unbackfilled — HEAD-tip sha + commit date with commit-date semantics, `last_push_at` =
  push wall-clock). Query: `sort=name|size|activity`
  (default name), `order=asc|desc` (default asc), `min_bytes=`/`max_bytes=` (uint64; unknown
  rows never match a bound; `min>max` → 400). `sort=activity` orders by `last_commit_time`
  (unknowns always last in either direction; the explore page uses `sort=activity&order=desc`).
  Served from the aggregate catalog in ONE
  object read regardless of repo count; absent catalog degrades to null rows (never 404/500
   for a missing optional object). Ties break on `(owner, name)` (shared with #247 ordering).
   Mutable-collab class (`private, no-cache`, Forgejo #384 — the #381 pattern:
   SWR's stale-serve window painted pre-flip visibility/mirror badges on
   refresh; the ETag is a content hash over the rendered rows covering
   visibility, mirror + mirror_upstream, and the catalog fields, so
   unchanged listings still 304). Size semantic: stored-object size (packs+idx — see 02 §2.1; overview
  `LiveBytes` stays Σ PackSize-only and is documented as differing by IdxSize).
  Mirror flags (Forgejo #281): every row also carries `mirror` (always present,
  never null — true iff the pull-only mirror sidecar exists) plus
  `mirror_upstream` (the sidecar's canonical upstream URL, only when the
  sidecar parses) so listing rows render the mirror indicator with no
  per-row summary fetch. The flags come from bounded-parallel per-row
  sidecar probes (≤ 8 in flight — request count, no sequential depth;
  see Decisions); absent/corrupt sidecars degrade to `false` / `true`
  without upstream (fail closed), never an error.
- `GET /api/v1/owners/{o}/profile` (Forgejo #234 — NEW alongside v1, triple twins
  `/api/v1` + `/api-browser/v1` + `/services/api`, discovery-listed, SDK `owners.profile`) →
  `{owner, display_name, location, timezone, bio_markdown, updated_at?, can_edit?}` (all strings,
  `""` = unset; `can_edit` is request-scoped, never stored). AuthRead (public bio). Unknown owners
  read as an empty profile (`200`, the `ownerRepos` 200-[] convention — never 404); only a
  syntactically invalid slug (outside the repo-id owner charset) 404s.
  Mutable-collab class (`private, no-cache`, Forgejo #385 — the #381 pattern:
  every projection on the response is PUT-editable with no ref movement, so
  SWR's stale-serve window painted the pre-edit bio on refresh; the ETag is
  a content hash over the served doc covering display_name, location,
  timezone, bio_markdown, updated_at, and can_edit, so unchanged profiles
  still 304). There is no avatar projection on this route: avatars ride
  `GET /api/v1/me`'s `avatar_url` (Forgejo #376).
- `PUT /api/v1/owners/{o}/profile` (Forgejo #234 — same triple twins, SDK `owners.updateProfile`) —
  idempotent full-document replace `{display_name, location, timezone, bio_markdown}` (body-carried
  `owner`/`updated_at`/`can_edit` ignored; the key names the owner, the server stamps `updated_at`);
  unknown fields, over-budget values (names/locations 200 chars, timezone 64 bytes IANA shape,
  bio 64 KiB), and malformed JSON are 400. AuthWrite gate first (anonymous → 401/403), then the
  owner rule: host admin, name-matched principal (case-insensitive), or org-owner role via the
  `OwnerEditor` seam (identity implements it — law 8; probe failure fails closed with 503).
  Written to `owners/<o>/profile.json` through the bounded CAS loop (the sidecar commit point —
  no WAL at owner scope); 5 consecutive races → 409. `200` answers the stored doc.
- `GET /services/api/instance` → `{kind, name, revision, instance, version, roles[], disk, shape, cpus,
  memory_bytes}` (`no-store`) — "this machine" for UI footers: hostname, declared roles, disk mode, CPU
  count, `runtime.NumCPU()` / total memory.

- **Visibility filtering (Forgejo #345 — spec amendment, see Decisions):** every listing above
  omits repos the caller cannot read (anonymous ⇒ public only; authenticated ⇒ public + their
  own/org repos; host admin/write ⇒ everything, +0 probes). Owners with no readable repo are
  absent (never zero-valued); per-owner counts cover visible repos only; the `sort=activity`
  rollup folds the aggregate catalog AFTER filtering to visible repos, so a private repo's
  commit time never lifts its owner's row; the repos/detailed rows carry the same `visibility`
  spelling as the summary (per-row LRU-backed probes, ≤ 8 in flight, like the mirror flags).
  No private name leaks in any name, count, or aggregate. Nil `Access` (identity unwired) →
  legacy unfiltered behavior, byte-identical.

## 9. Reads: summary, refs, resolve

### 9.1 Repo summary — `GET /{o}/{r}/api` (and `{lane}` root)

After a refs-level sync: `{owner, name, full_name, description, head:{name,sha}|null, branches, tags,
health, missing_total?, clone_url, ssh_clone_url?, html_url, api_url}`. `head` = default branch (`null` → JSON `null` — the one sanctioned null, it is not an
array). `description` is the per-repo short display string (issue #235, `""` when unset — always
present; old clients ignore it per 14 §14.12): sourced from the `description` key of the
WAL-published settings TOML, folded in `walView.Summary` from the manifest-inline copy the refs
sync already holds (**zero new store round trips** — the same in-memory guarantee as the `empty`
predicate below; unparseable docs fail open to `""`). `ETag` covers the description alongside
health: `"<head sha>"` (`""` when unborn, as before) suffixed `~d<fnv1a32hex>` when set — without
the suffix a description-only change (same head sha) would 304 and keep showing the stale text;
clearing the description drops the suffix, which busts the cache too. `branches`/`tags` are **counts** (integers). `clone_url` from `server.public_url` (or request
Host); `ssh_clone_url` (17_ssh.md §3 — the SSH transport advertisement, `external_port` else the
listen port on the same public host, `:22` omitted; absent while SSH is disabled with no external
override); `api_url` = the `/api` lane URL; mutable-collab class (`private, no-cache`,
Forgejo #381 — SWR's stale-serve window defeated the suffix-covered ETag, see below)
+ `ETag: "<head sha>"` with the `~degraded`/`~d`/`~m`/`~c`/`~v` suffixes. `PUT` here creates (require_write,
`?object_format=sha1|sha256`, `201`/`409` exists); `DELETE` (require_admin) → `204`.

`health` is the **repo-state vocabulary** (issue #209 — scoped: this field describes the repo,
while `overview.health.status ∈ ok|degraded|error` below describes the WAL dashboard; the two
names do not mix): `empty` (unborn — no resolvable head, zero branches/tags; derived from the
manifest + ref counts the summary already holds, **zero new store round trips**), `healthy`, or
`degraded` (refs present AND (the cached `fsck.pb` report lists missing objects
OR the serve-health sidecar `meta/serve-health.json` is present — issue #320:
the serve path's own sticky failure record, §5.2.1 of 05)). `missing_total`
rides `degraded` only (the authoritative count from the fsck report; absent
otherwise — a serve-health degrade carries no count). The degraded
overrides cost **exact-key probes** on non-empty summaries only —
empty repos skip both probes (branch on data in hand): the `fsck.pb` probe
first (a hit short-circuits), then serve-health — directly (one GET) for
non-mirrors, or via the mirror hook's `degraded_reason` verdict for mirrors
(+0 extra GETs there: the hook already probed beside its `mirror.json`
load). Non-empty summaries therefore pay at most +2 GETs; the law-6 budgeted
paths (push ≤ 5, warm refs 1, checkpoint 4) never call here, so their sim
budgets hold unchanged. `ETag` covers the
health field: `"<head sha>"` (`""` when unborn, as before), suffixed `~degraded` when degraded —
without the suffix a revalidating client would 304 on the same head sha and keep showing
`healthy` after damage lands.

`placeholder` is the **explicit-create projection** (issue #210, R1 B1 — additive
`{created_by, created_at, expires_at} | null`, omitted when null): probed from
`repos/<o>/<r>/meta/placeholder.json` by exact key (never LIST) **only when the in-hand data
already says empty** (`HeadSeq == 0 && refs == 0`) — real repos pay **+0 round trips**; the
empty path pays at most +1 GET. UI affordances key on `refs == 0 && marker`, never marker
alone (a stale marker on a real repo — crash between push CAS and marker delete — renders as
real and carries no projection).

`PUT` here creates (require_write, `?object_format=sha1|sha256`, `201`/`409` exists); with
`?placeholder=true` it selects placeholder create-semantics on the same PUT (the manifest is
created as today, plus the `meta/placeholder.json` sidecar and the eager `access.json` default
— see §9.1.1); without the flag the path is byte-identical to today. `DELETE` (require_admin)
→ `204`.

`open_issues`/`open_pulls` are the tab-badge numerators (issue #319 — additive integers, always
present, 0 = none; old clients ignore them per 14 §14.12): open kind:`"issue"` / kind:`"pr"`
cards from the shared `issues/index.json`, read index-first behind the `Env.CollabCounts` hook
(one exact-key GET — probe, don't list, law 4; absent index → zeros with the byte-identical
ETag, so pre-collab repos are untouched). Riding the summary costs zero new client requests
(the tab bar already holds the shared summary signal; the two-count-endpoints alternative
costs two extra requests per repo view) at +1 server-side probe per summary — off the law-6
budgeted paths (push/sync/checkpoint never call here, so their sim budgets hold unchanged).
`ETag` covers the counts with the `~c<index-version>` suffix: the shared index version bumps
on every card upsert by either collab writer, so a close/reopen with no ref move still busts
a revalidation (same trap as `~degraded`/`~d`). The class is the §4 mutable-collab class
(`private, no-cache`, Forgejo #381 — this supersedes the earlier "class stays SWR,
stream-invalidation closes the window" coordination: the suffixes make revalidation
correct, but only no-cache removes the stale-serve window that painted pre-mutation
bodies on refresh; unchanged summaries still 304).

`visibility` is the badge source (Forgejo #345 — additive `"public"|"private"`,
Forgejo #374 adds `"authenticated"`: always present, `""` when the identity
surface is unwired; old clients ignore it per 14 §14.12): read behind the `Env.RepoVisibility` hook (one LRU-backed conditional `access.json`
GET — usually a version hit, no body; missing/empty/invalid `access.json` resolves `public`,
the §10 legacy default, so pre-existing repos badge public). `ETag` covers the field with
the `~v<visibility>` suffix — a visibility flip moves no ref, so without it a revalidating
client would 304 and keep showing the stale badge (same trap as `~degraded`/`~d`/`~m`/`~c`).
The suffix makes revalidation correct; the §4 mutable-collab class (Forgejo #381) removes
the stale-serve window the suffix alone cannot close.

### 9.1.1 Explicit create — `POST /api/v1/repos` (+ `/api-browser/v1` twin; issue #210)

The discoverable create action (the `PUT` lane root is undiscoverable — no UI, no SDK method
on `repo.js`): `POST /api/v1/repos` with JSON body `{owner, name, object_format? (sha1|sha256,
default sha1), placeholder? (default true), visibility? (public|authenticated|private — Forgejo
#374)}` (require_write; in
`none` mode anonymous inherits the existing write). Naming validation is `git.ParseRepoId`
(two segments, `[A-Za-z0-9._-]{1,100}`, no leading `.`, not `..`, `.git` suffix stripped —
`400` plain-text on violation; case preserved verbatim, `Acme/X` and `acme/X` are distinct
prefixes). Thin wrapper over the one writer implementation (Seam 7: no second writer — the
`PUT`-flag and `POST`-twin paths share it). Served via `server.ExtraRoutes` +
`api.RegisterExposed` (both-lane twins, discovery lists `/api/v1/repos`; Feature 10
precedent — no core-table edit, law 8).

Responses (all added fields enumerated here — additive per 14 §14.12): `201
{owner, name, full_name, placeholder:true, clone_url, ssh_clone_url?, html_url}` (+ `Location:` the repo URL,
+ non-blocking `warning:"owner name collides with a UI route"` when the owner hits a reserved
single-segment UI name — creation allowed, only the `/:owner` page misroutes); idempotent
`200 {…, placeholder:true, already:true}` for a same-principal re-create of a still-unborn
placeholder (no state change — double-click/retry-safe); `409` **plain text** carrying the
winner URL (`repository already exists: <html_url>` — the frozen 409 convention stays
`writePlain`, never a JSON envelope, so the UI links the squatter); `PUT` without the flag on
an existing placeholder keeps the legacy `409 "repository already exists"`. Creation
additionally requires the #346 owner admission (owner == self, member org, or host admin —
one exact-key `members.json` GET on the non-self path; foreign owner → `403` naming the
allowed owners, probe errors → `503`; see
`docs/features/01_identity_permissions.md`). First push adopts (never 409s — the push path
has no conflict surface by construction); same-principal re-create after the first push (now
real) is `409`. No expiry sweep in this change (`expires_at` always null; TTL off by default).

### 9.2 Refs

- `GET …/refs` → `{"head":{"name":"refs/heads/main","sha":"…"}}` or `{"head":null}` — O(1): the default
  branch from the ref snapshot; never scan. SWR + ETag.
- `GET …/refs/{branches|tags}?prefix=&q=&after=&n=` → `{refs:[{name,sha}],more}`. Name-sorted page under
  the namespace: `prefix` filters by full-name prefix (`refs/heads/` + prefix); `q` = case-insensitive
  substring on the short name; `after` = name cursor, strictly greater in byte order; `n` default 100,
  max 1000; `more` computed by asking for n+1 internally. Tag shas are the **peeled** commit (peel
  annotated tags at sync time; the ref snapshot stores peeled tag shas). Single pass over the sorted
  namespace, O(page).

### 9.3 Resolve — `GET …/resolve[/{rest}]`

Response `{ref, sha, path, kind:"branch"|"tag"|"commit"}`; SWR + `ETag: "<sha>"`. Algorithm (per §9.5):

1. `rest` = the remaining path segments after `/resolve` (empty → default branch: `ref = "refs/heads/<default>"`,
   `path = ""`).
2. Candidates: for k = `len(segments(rest))`, the k prefixes of rest interpreted as branch names
   (`refs/heads/<p>`) AND tag names (`refs/tags/<p>`) → **2k exact lookups** in the ref snapshot's map —
   never a scan. Longest match wins; **branch beats tag on ties**.
3. No prefix matched → take the FIRST segment as a revision: local repo — `git rev-parse --verify
   <seg>^{commit}` (§10 recipes argv); remote-served repo — unique-prefix lookup in the oid index + peel.
   Cold serving copy (issue #203): rev-parse needs the packs materialized, but the refs-level sync above
   never fetches them — so a rev-parse miss on a FULL-length hex sha (40/64 lowercase) serve-syncs once
   (the failure-path materialize) and retries before 404ing. Branch/tag lookups never need packs and
   skip the retry; short shas keep the single attempt.
   On success `kind = "commit"`, `ref = ""` echoed as the revision input, remaining segments = `path`.
4. Still unresolved → `404` (plain text).
5. Tags resolve to the **peeled commit**; the response echoes `ref` (full ref name), `sha`, and `path`
   (the segments after the matched prefix, `/`-joined, NOT pre-encoded).

A tags-name tie example: rest `v1/src` with tag `v1` and branch `v1` both existing → branch wins, `path = "src"`.

### 9.4 Tree — `GET …/tree/{rest}` (rev may contain slashes)

The route is greedy: the handler passes the whole tail to `Resolve` and the
§9.3 longest-prefix match splits ref from path (issue #251 — a slashed
branch like `feat/identity` resolves exactly as `…/resolve/feat/identity`
does). Then `git ls-tree -l -z <tree-sha>` is wrong when `path` is a
prefix walk — normative recipe: resolve the tree object of `path` first (`rev`'s commit → tree, descend
by `path` segments via `git ls-tree -z <tree> <seg>` per segment or a single
`git ls-tree -z -l <commit-ish> -- <path>` on the directory), then:

```text
git ls-tree -l -z <tree-sha>            # entries of THAT directory
git log -1 --format=<FMT_COMMIT> --no-color <commit-sha> -- <path>   # commit? newest touching path
git log <commit-sha> --format=WALHUBTREE\ %H\ %cI --name-status --no-renames --no-color --first-parent -z --max-count=<server.max_tree_log> [-- <dir>]   # per-entry dates (issue #301)
```

- `-z` output lines: `<mode> SP <type> SP <sha> SP <size> TAB <name>` NUL-terminated. `type` ∈
  `blob|tree|commit` (submodule); `size` is `-` for trees/submodules → emit `-1`.
- Sort in Go: directories first, then byte order by name (NOT git's order).
- Per-entry dates (issue #301): ONE batched walk per listing (never one
  subprocess per row — law 6), newest-first, first-parent (a merge
  attributes its whole merged diff), `--no-renames` (a rename lands as
  delete+add, so the new path still attributes with no rename-pair
  parsing), `-z` for exact path bytes. Each direct child of the listing
  gets the newest commit touching its path (`commit_sha` + `commit_time`
  verbatim `%cI`, both `omitempty`); nested paths attribute their direct
  child; submodule entries (`type: commit`) stay dateless; entries
  untouched within the capped walk stay dateless (neutral UI fallback, not
  the HEAD stamp). A failed walk leaves every entry undated and never
  fails the tree; an empty or all-submodule listing skips the walk. The
  dates derive from the same resolved sha, so the payload stays sha-pure
  (ETag/SWR contract unchanged) and the walk adds zero store round trips.
  Cap: `server.max_tree_log` (default 200, `>= 1`, validated fail-closed).
- `readme`: the first blob named `readme` with optional extension `.md|.markdown|.txt|.rst`,
  case-insensitive, in the sorted order above; contents fetched via `git cat-file blob <sha>`, emitted
  only when valid UTF-8 (`readme: {name, contents}` omitted otherwise).
- Response: `{ref, sha, path, entries:[{name,type,mode,size,sha,commit_sha?,commit_time?}], commit?, readme?}`; `commit` present
  only when `path` is non-empty; a full-sha addressed rev (the rev portion of the tail) → immutable class; `404` if the target is not a tree.
- `mode` is the 6-char git mode string verbatim (`100644`, `040000` for trees as git prints, `160000`).

### 9.5 Blob — `GET …/blob/{rest}[?raw]` (rev may contain slashes)

1. Resolve the whole tail (§9.3 longest-prefix split — issue #251); a
   slashed branch resolves exactly as the resolve endpoint splits it. Empty
   resolved path → `404` "blob requires a path". Then walk `path` to the blob sha (must be `100644|100755|120000`; else `404`).
2. `size` via `git cat-file -s <sha>`; if `size > 2 MiB` → `{"ref","sha","path","name","size","too_large":true}` (no contents).
3. Else `git cat-file blob <sha>` capped at 2 MiB+1 read; NUL or invalid UTF-8 → `"binary":true`; else `"contents":"<utf-8 text>"`.
4. `?raw` bypasses JSON: `200 text/plain; charset=utf-8` full raw bytes (the cap is a JSON-shape rule). Same caching rules as the JSON form.

### 9.6 Commits — `GET …/commits?ref=&path=&skip=&n=`

- `ref` default `HEAD` (resolve first); full sha in `ref` → immutable class; `n` default 35, cap 200.
- The server asks for `n+1` to compute `more` (`more = len(raw) > n`, then truncate to `n`).
- `skip` = offset; `path` limits to commits touching that path.

```text
git log --format=%H%x00%P%x00%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI%x00%s%x00%b%x1e --max-count=<n+1> --skip=<skip> --no-color <rev> -- <path>
```

The format string is literal (note: `%x00`/`%x1e` are ASCII text in argv — argv can never contain a NUL
byte; git expands them). Fields NUL-separated, records `\x1e`-separated; `parents` split on SP; `%aI`/`%cI`
are RFC 3339 already. `body` = `%b` trimmed; trailers parsed per §9.7.

### 9.7 Trailers (hand-rolled `git interpret-trailers --parse` semantics)

Parse the commit message `%B` (use `%B` for this, not `%b` — the trailer block sits at the message's
tail; `%b` equals `%B` minus subject, fine too):

1. Split into lines. Find the **last paragraph**: the maximal run of non-empty lines after the final
   blank line (trailing blank lines ignored). If no blank line separates a candidate block from the
   subject, the whole message after the first line is still the candidate (git treats the subject line
   specially: the trailer block must follow a blank line OR be the entire body).
2. The block qualifies as a trailer block **iff every line** is either a trailer line
   (`^[A-Za-z0-9-]+:` — token = alphanumerics and `-`, followed by `:`) or a continuation (starts with
   SP or TAB). If any line fails, there are **no trailers** and `body` = the whole message trimmed.
3. Trailer line → `{key: <token>, value: <everything after the colon, left-trimmed>}`.
   Continuation line → append `"\n" + line-with-leading-whitespace-stripped` to the previous trailer's
   value (folded continuation, preserved in order; the value keeps embedded newlines).
4. `trailers` is emitted in file order. `body` = the message minus the trailer block, right-trimmed.

Edge rules: an empty value is legal (`Key:` → value `""`); a `Key:` line inside a *non-final* paragraph is
NOT a trailer; `Signed-off-by:` is not special-cased here (ordering is file order, not git's
`--where` placement logic — this is parse-only).

### 9.8 Commit — `GET …/commit/{sha}`

`{commit, stats:[{path,additions,deletions}], patch}`.

```text
# Commit object (same Commit shape as §9.6):
git show -s --format=%H%x00%P%x00%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI%x00%s%x00%b%x00%B <sha>

# Stats + patch in one pass, parsed apart:
git show --format= --no-color -M --diff-merges=first-parent --root --numstat -z <sha>
```

- `stats` = the `--numstat` records in output order: NUL-separated `additions, deletions, path` (with
  `-z`, rename records carry `src` then `dst` as separate NUL fields — emit `dst` as the path, the rename
  appears **once**); binary → `-`/`-` → emit `-1`/`-1`.
- `patch` = the remainder of that output (the unified diff): **first parent** for merges
  (`--diff-merges=first-parent`), `--root` so a root commit shows its full diff, `--no-color`, `-M`
  rename detection. No header re-formatting: the diff body is passed through verbatim (minus the empty
  line `--format=` leaves before it).
- Full sha → immutable; else SWR + `ETag: "<full sha>"` (a short sha resolving here renders under the
  full-sha key).

### 9.9 curl examples (main reads)

```bash
H=https://git.example.com
curl -s $H/api/v1 | jq .endpoints                       # discovery
curl -s -H "Authorization: Bearer wgt_…" $H/api/v1/me   # who am I
curl -s $H/api/v1/owners; curl -s $H/api/v1/owners/acme/repos
# resolve-then-sha navigation (SWR, then immutable)
curl -s $H/acme/monorepo/api/resolve/main/src | jq .
curl -s $H/acme/monorepo/api/tree/cb38da1…/src | jq .
# refs pages (JSON and SSE dialect)
curl -s "$H/acme/monorepo/api/refs/branches?prefix=release/&n=100"
curl -sN -H "Accept: text/event-stream" "$H/acme/monorepo/api/refs/branches"
curl -s "$H/acme/monorepo/api/commits?ref=main&n=35" | jq .
curl -s $H/acme/monorepo/api/commit/cb38da1… | jq '{commit: .commit.sha, stats: .stats}'
curl -sI $H/acme/monorepo/api/tree/cb38da1…/src | grep -i cache-control   # private, max-age=31536000, immutable
```

### 9.10 Empty-repo reads — the `empty repository:` 404 marker (issue #209)

`resolve` / `tree` / `blob` / `commits` / `commit` on an **unborn** repo stay `404` (frozen wire
behavior — API clients see the same status as before), but the plain-text body carries the stable
machine-readable prefix `empty repository: ` (e.g. `not found: empty repository: unborn HEAD`),
so the UI can guide instead of traying without parsing prose.

- **Predicate (exact, review S2):** manifest `HeadSeq == 0` with no resolvable head and zero
  branches/tags, evaluated at handler time from the in-hand ref snapshot + the open handle's
  manifest snapshot (**zero new hot-path round trips** on the resolve path). The serve-level
  recipes (tree/blob/commits/commit) take one refs snapshot, but only on the failure path (law 6:
  verification goes on the failure path).
- **Fail-closed:** any manifest/snapshot error → no prefix. Damage-404s (refs present, objects
  missing) **never** carry the prefix — a degraded repo can never misclassify as empty.
- **Sites (all 9, `internal/api/bind_wal.go`):** resolve unborn-`HEAD` × 2, resolve rev-parse miss,
  tree, blob size, blob body, commits history, commit show, commit parse. Non-empty failures keep
  their exact frozen messages byte-for-byte.
- **No new endpoints:** the on-demand audit is the already-addressable `POST …/ops/fsck`
  (`require_write`, `(repo, kind)` single-flight join — §12.2); the `repair-check` option was
  considered and closed (Decisions).

## 10. Policy endpoints (§14 semantics)

- `GET …/policy` → the policy JSON document (§14.1 envelope); missing file = allow-all: emit
  `{"version":1,"groups":[],"rules":[]}`.
- `PUT …/policy` (require_admin) → body = policy JSON; validate at load (§14.4 rules: unknown keys inside
  rule/match/effect are parse errors; `^` exclusions refused on union families; disjoint-bypass protect
  load check). Invalid → `400` with a plain-text reason list. Valid → CAS-write `repos/<o>/<r>/policy.json`.
- `DELETE …/policy` (require_admin) → back to allow-all (`204`).
- `POST …/policy/validate` → parse the body (or the stored policy when body empty) →
  `{ok, errors[], rules, groups, protect}` where `protect` = the compiled protect-rule summary.
- `POST …/policy/dry-run?last=N` → evaluate the given body (else the stored policy) against the **last N
  PUSH entries of the live WAL log** → `{pushes, allowed, denied, results:[{seq, at, principal, atomic,
  refs:[{name, ok, reason, force}]}]}`. `force` is derived (`merge-base --is-ancestor` semantics — the
  wire triple cannot express it). No mutation; the dry-run never enforces.

## 11. Settings endpoints (D24, §15.2)

- `GET …/settings` → `{revision, author, updated_at, message, toml}` (revision `0` = none ever published).
  `toml` is the raw per-repo TOML body as published.
- `PUT …/settings?message=…` (require_admin): body = TOML, **≤ 16 KiB else `413`** (plain text). Validate
  against THIS serving host's build: only sections `[bundles]`, `[maintenance]`, `[compaction]`,
  `[upstream]` allowed (`[integrations]` accepted and ignored, forward compat); `upstream.token_env` and
  everything under auth/store/server/wal/cache is host-only and refused. Invalid → `400` + reason, **nothing
  published**. Valid → publish through the WAL (SETTINGS entry + manifest inline) → `200 {"revision":N}`.
- `DELETE …/settings` (require_admin) → publishes empty (back to host config), new revision.
- `GET …/settings/effective` → `application/toml` of the effective `[bundles]`/`[maintenance]`/
  `[compaction]`/`[upstream]` (host config ⊕ repo settings). No host secrets, no `token_env` values.
- `GET …/settings/history` → `{min_seq, entries:[{seq, revision, author, message, at, toml}]}` from the
  WAL's SETTINGS entries; `min_seq` = oldest readable log seq (entries below are folded).
- `GET …/settings/describe` → the shape in §3 verbatim: current `settings`, allowed `sections`,
  resolved bundle `strategies` (`schedule_human` = the cron rendered readably; `next` = next fire time),
  `bundles`, `maintenance` (incl. `this_host` facts), `compaction`, `upstream` (incl. `last_round?` from
  the follow loop, §13.4 of the master spec), `fields:[{key, value, host_value,
  source:"host"|"setting"}]` (every overridden key with its origin), `head_seq`.
- `POST …/settings/validate` → the SAME `describe` shape but computed for the **would-be** effective
  config (body applied), plus `ok` and `errors[]`. Never publishes.

## 12. Overview, ops, tasks

### 12.1 Overview — `GET …/overview` (no-store)

Shape verbatim in §3: WAL health dashboard data. Sources: manifest (version, next_seq, min_seq, segments,
tail_entries, entries, checkpoint?, packset?, advertised_bundle_uri?, last_push?), the local cache state
(`local`), live packs (`packs`), bundle objects and the planned slot table (`bundles`, `bundle_plan` —
including `upcoming` from maintainer heartbeats and `maintainers` from placement), `compactions[]` (recent
COMPACT entries), `node{counters}` (this instance's counters). `health.status` ∈ `ok|degraded|error` with
`issues[]` (plain strings) and `suggestions:[{op, params?, reason, auto?}]` — the ops the UI can offer to
run (e.g. `{op:"compact", params:{force:1}, reason:"16 tier-0 packs", auto:false}`).

`fsck?` is the read-only `fsck.pb` projection (issue #209 — the admin's machine interface for object
health): `missing_total` (authoritative; falls back to the sample length), `missing[]` (the report's
bounded sample, `[]` never null), `problems`, `repaired_seq` (nonzero = repair landed and disarmed),
`at?` (last audit, RFC 3339), `host?`, `upstream?` (effective repair source — per-repo `[upstream]`
settings over host config; absent = none configured, and the UI renders the "set `upstream.git` to
enable repair" guidance), and `repair_stalled` — derived read-side (R1 B2): upstream configured +
`repaired_seq == 0` + missing signal + the report older than one full `maintenance.fsck_interval`.
Absent when never audited. Cost: **+1 conditional GET** per overview call (R1 B1: stated, not zero —
no-store admin page, off the law-6 hot paths; the fsck unit itself never runs inline on a request).

### 12.2 Ops — `GET …/ops` and `POST …/ops/{op}`

- `GET …/ops` (no-store) → `{available:[OpSpec], recent:[TaskRecord], bundle_strategies}`. `OpSpec` =
  `{op, params:[{name, values?}]}`; `bundle_strategies` = the configured strategy names/kinds.
- `POST …/ops/{op}?params` (require_write). Ops and params:

| op | params |
|---|---|
| `fsck` | `connectivity=1` |
| `repair` | — |
| `follow` | — |
| `rev-index` | `pack=<checksum>` |
| `compact` | `force=1`, `base=1` |
| `bundle` | `strategy=<name>`, `slot=<n>` |
| `checkpoint` | `trigger=<reason>` |
| `sync` | — |
| `rematerialize` | — |

- Response: the **SSE attach stream** for the started task (§10.3). Unknown op → `404`; missing/invalid
  param → `400` plain text.
- **Join semantics (normative, §6.8):** the task table is keyed by `(repo, kind)`; a second start of the
  same `(repo, kind)` **joins** the running task (`Begin::AlreadyRunning`): attach to its stream, await
  completion up to a bounded wait, then reuse its outcome. The joiner's SSE stream replays the buffered
  packets (§10.3) and then follows live. Cross-instance exclusivity is the lease, not the table; a task
  running on ANOTHER host is not joinable here — the op returns its `task` record with that `hostname` and
  a terminal `error {status:409, message:"task runs on <host>"}`.

### 12.3 Task endpoints

- `GET …/tasks` → `{hostname, running:[TaskRecord], recent:[TaskRecord]}` (`no-store`). `running` = tasks
  on the ANSWERING instance only; finished-detection is instance-aware: a task vanishing from `running`
  counts as finished only when the same instance answers (or `recent` shows it with a result).
- `TaskRecord` (§6.8, frozen): `{id (uuid), kind, repo, hostname, started, finished?, elapsed_ms, ok?
  (null = running), summary, progress? {label, done, total?, unit, percent?}, log_tail (last 60 notices),
  params}`.
- `GET …/tasks/{id}`: with `Accept: text/event-stream` → **attach**: one `task` packet (the TaskRecord),
  then replay, then live packets, terminal `result {"task": <TaskRecord>, "value": …}` or
  `error {"status":…, "message":…}`. Without the header → the TaskRecord JSON. `404` if unknown **on this
  instance** (records are instance-memory only).
- Kinds (frozen list): `materialize, remote-index, history-pack, compact, bundle, checkpoint, fsck,
  repair, follow, rev-index, sync, rematerialize, prewarm`.

### 12.4 ### Concurrency — task attach and replay

- Replay buffer: per task, a ring of 200 packets with bars deduped by label (a new `progress` with the
  same label replaces the buffered one). Attachers receive buffer-then-live under the task's packet lock
  (§6 hazard 3 note) so ordering is stable.
- The task owns the broadcast; subscribers are passive. Publishing never blocks on a subscriber
  (drop-oldest, §6). When the task finishes it emits exactly one terminal packet, then closes the
  broadcast: subscribers see the terminal packet, flush, and end. A subscriber arriving after the terminal
  packet gets the buffered terminal immediately.
- Drain (SIGTERM): in-flight tasks are interrupted with terminal
  `error {"status":503,"message":"interrupted: instance shut down; will be retried by the next pass"}`
  (§6.8) — attached SSE clients get that packet; the record persists in `recent`.

## 13. Instance/auth facts re-used by handlers

`/api/v1/me` and every write gate read the request principal injected by `internal/server` middleware (`none` mode → `principal:"anonymous"`). Admin gating for `PUT/DELETE policy|settings`, `DELETE` repo: `require_admin`; `POST ops/{op}`: `require_write`; all reads: `require_read` (self-authing). Every `401` carries `WWW-Authenticate: Bearer realm="walgit"` (never Basic); `503` carries `Retry-After: 15`.

**Spec amendment (Forgejo #345): visibility is the read authority for repo-scoped reads.**
When the identity surface is wired (`Env.Access`), a repo-scoped `AuthRead` route consults
`CheckRead` BEFORE the `anonymous_read` flag gate: `public` admits the caller (including
anonymous) even with `anonymous_read=false`; `private` denies (401 anonymous + Bearer, 403
authenticated-without-read). The flag keeps its meaning for NON-repo surfaces only (owners
listings, profiles, `/explore`, setup.json, non-repo shells): anonymous there still needs
`anonymous_read=true`. Denials stay 401/403 rather than 404 by deliberate choice (see
Decisions): a 404 would break git's credential-erase on dead tokens (law 9, 06 §8.4) and the
#344 login-page mapping; existence protection for private repos comes from the filtered
listings (§8), never from the status code. Nil `Access` → legacy flag-only gating.

## 14. Decisions & deviations from the Rust design

- **Navbar identity signals (Forgejo #371).** `GET /api/v1/me` gains `admin`
  (the navbar gates its Setup menu entry on it — `setupAccess` admits host
  admins outside none mode) and the discovery auth block gains the instance
  `mode` (`none|token|oidc`), so the SPA knows whether Login/identity apply
  at all (never in `none` mode — nothing to log in to — and the Login
  button only when `browser_login` is true per #344). Both ride
  already-fetched payloads (the `me` cache key pages share; one AuthOpen
  discovery GET per app load) — zero new round trips on any hot path
  (law 6). `/_auth/me` keeps its `{principal, write}` shape (edge-consumed,
  unchanged).

- **Public/private visibility enforced across listings, git, and the UI (Forgejo #345).**
  Spec amendment, stated explicitly: visibility (`access.json`, public-by-default, missing →
  public) is the read authority for every repo-scoped read surface (JSON API lanes,
  smart-HTTP upload-pack, LFS reads, bundle lists, SSH fetch, repo SPA pages); `anonymous_read`
  keeps only its non-repo meaning (owners/profile/org listings, `/explore`, setup.json,
  non-repo shells). Rationale: the global flag made every public repo invisible to signed-out
  users on OIDC instances (`anonymous_read=false`) — per-repo visibility is the only switch
  with the right granularity. Consequences, all in the same change: (a) listings filter by
  the caller's read role behind the access LRU (admins/writers bypass with +0 probes; the
  activity/size aggregates fold the catalog after filtering, so no private name, count, or
  timestamp leaks); (b) `summaryBody` and the repos/detailed rows carry `visibility`, covered
  by the `~v` ETag suffix (same trap class as `~degraded`/`~d`/`~m`/`~c`); (c) the UI badges
  the header + rows and wires create (already sent it), General settings (admin PUT preserving
  bindings), and the existing Access tab; fork/import-created repos default public (a fork
  child writes no `access.json`, so reads synthesize public; import preserves an existing
  doc's visibility and materializes public otherwise). Private-denied reads stay 401
  (anonymous + Bearer) / 403 (authenticated) rather than 404: git must see a real 401 to
  erase dead credentials (law 9, 06 §8.4), the #344 browser login page keys on the 401, and
  the filtered listings — not the status code — are what hide private existence. Permission
  matrix (surface × visibility × principal): repo reads (API/smart/LFS/bundles/SSH/shell) —
  public: everyone incl. anonymous; private: bindings (user:/team:), org owners, host
  write/admin; writes/admin ops — unchanged (authenticated + flags/bindings; anonymous
  never). Out of scope (explicit): fine-grained org permissions, per-branch/path rules,
  private-discovery UX.

- **Greedy blob/tree tails (Forgejo #251).** `tree/{rev}[/{path}]` and
  `blob/{rev}/{path}` were single-segment rev routes, so a slashed branch
  (`feat/identity`) was amputated to `feat` before Resolve saw it (404)
  while `resolve/{rest...}` answered fine. Both routes are now greedy
  (`tree/{rest...}`, `blob/{rest...}`) and the handlers pass the whole tail
  to Resolve's longest-prefix split, using `res.Path` as the file path;
  the immutable cache class tests the rev portion of the tail
  (`refPartOf`), and render/?raw cache keys still sit on the resolved sha.
  Discovery templates keep the `{rev}`/`{path}` spellings, so `endpoints[]`
  is unchanged. Rationale: one split rule (§9.3) for every ref-addressed
  read instead of two disagreeing ones.
- **Hand-rolled LRU + single-flight** instead of moka/`golang-lru`/`singleflight` crates — the dependency
  policy allows only `x/net` and `BurntSushi/toml`; the single-flight sketch (§5.1) is the whole pattern.
- **Render-cache entries are revision-stamped** and the bucket envelope carries `revision` — the Rust spec
  keys renders by "ref-state version"; stamping makes lazy invalidation explicit and stale bucket
  generations self-identifying.
- **New config key `cache.render_cache_bytes`** (default 256 MiB) — the Rust spec fixes cache sizes per
  use (`cache.*_entries`); one weighted-bytes budget is simpler and the bucket/LRU split is preserved.
- **Discovery document lists only real routes** and is derived from the route table — fixes §20.4 (the
  phantom `…/commit/{sha}/merge-queue` advertisement must not be copied).
- **Checks API surfaced in discovery + `/api` docs (Forgejo #271).** The checks surface was
  dynamically reportable but undiscoverable: `endpoints[]` now also carries the five checks
  templates (`checks.ExposedTemplates`, registered from composition — the Feature 10 precedent),
  the `/api` page documents the routes, auth, request/response shapes, and a worked CI example
  (`#checks-ci`, linked from `/checks`), and `TestExposedCoversRoutes` pins the template↔route
  correspondence both ways. No wire or behavior change.
- **All feature surfaces surfaced in discovery + `/api` docs (Forgejo #272).** #271 proved the
  pattern on checks; #272 applies it everywhere the issue's audit found gaps: issues, pulls,
  releases, review, social, notify, identity, and tags each gained `ExposedTemplates` (one entry
  per distinct path shape, both lanes collapsing to one template) registered from composition in
  the same change, plus `TestExposedTemplatesExact` + `TestExposedCoversRoutes` per package and a
  composition registration test (`TestCollabServicesRegisterDiscovery`). Mirror's repo lanes
  (`/{owner}/{repo}/api/mirror`, `…/mirror/sync`) joined discovery too — they are real JSON API
  routes, so the old top-level-only exception is gone. The `/api` page renders the live discovery
  document beside a static route table that is now derived from the same registry (every row
  matches a template; drift fails CI), with per-surface auth/shape notes and worked examples for
  the release-asset upload and the notification SSE flow. Byte routes outside the api lanes
  (release asset bytes, attachment bytes) stay out of `endpoints[]` — the static contract, not
  the JSON API. Each documented route's auth was spot-checked against its `Handle`/service gate;
  the SDK already wrapped every surface (`issues`, `pulls`, `reviews`, `releases`, `social`,
  `notifications`, `users`, `orgs`, `invites`, `tags`, `collab`) — no method/route mismatch
  found. No wire or behavior change (docs + discovery wiring only).
- **`%x00` field separators in `--format` argv** (§9.6/§9.8) — argv strings cannot contain NUL bytes;
  git expands the literal `%x00`, so the format text is ASCII while the output is NUL-delimited.
- **Commit render splits into two `git show` invocations** (header via `show -s`, patch+numstat via
  `show --format=`) instead of parsing one mixed output — trivial parsing, one extra fork per commit view,
  and the numstat/patch order guarantee is preserved exactly.
- **Trailer folding joins with `"\n" + de-indented line`** — git's `--parse` prints folded trailers with
  their continuation lines; the JSON value keeps embedded newlines (the spec fixes "folded continuation
  lines, in order" but not the join character; this matches git's in-memory representation).
- **`?raw` blob responses are not size-capped** — the 2 MiB cap is a JSON-shape rule (`too_large`); raw
  is the download path. The Rust spec caps "the JSON"; raw is an intentional clarification, not a change.
- **`POST ops/{op}` returns `409` (SSE `error`) for a same-(repo,kind) task running on another host** —
  §6.8 makes records instance-local; the Rust behavior of awaiting is only defined for the local table,
  and a cross-host wait has no attachable stream. Local joins keep the bounded-wait + reuse semantics.
- **SSE writer uses `http.ResponseController` write deadlines** (15 s per packet), and keepalive + packet writes share one mutex per stream — a stalled client must not pin a goroutine and interleaved `: keepalive` comments and packets must not tear (tokio got both for free).
- **FIXED (issue #203) — sha-addressed resolve retries past a cold serving copy:** `Resolve` on a
  full-sha miss serve-syncs once and retries `rev-parse` before 404ing (`not found: <sha>`). The UI's
  resolve → sha flow never issues a ref-named request, so without the retry the first sha-addressed
  visit after a restart/recreate toasted on every tree/blob/commit fetch even though refs resolved and
  the objects were in the bucket — and nothing ever healed the copy (only ref-named requests reached
  the serve sync). Hot path unchanged (retry runs only on rev-parse miss); genuinely-missing shas keep
  the exact `not found: <seg>` 404 shape. Proven live: `GET …/api/tree/<head-sha>` 404'd from network
  (fresh browser profile, no `Cache-Control` on the 404, no proxy cache headers) while `…/tree/main`
  200'd and healed the copy — ruling out the poisoned-immutable-cache suspect.
- **FIXED (issue #200) — owners/repos listings are manifest-gated:** `GET /api/v1/owners` drops owners with no manifest-backed repo, and `GET /api/v1/owners/{o}/repos` drops prefixes without `manifest.pb` — deleted-repo litter (the filesystem CAS `.lock` sidecars persist by design, invisible to `List` yet keeping the directory behind `ListPrefixes`) and unborn fork-provisioned prefixes (#150, still tolerated row-side as a race). Manifest `Head`s fan out in parallel (limit 8, mirroring `wal.refreshList`); a missing/unreadable manifest drops the name (fail-closed, same as `refreshList`). `Exists`/create/delete were already manifest-gated, so re-create/re-import after a delete sees a clean name (no 409, no sweep change).
- **Self-heal API surface (issue #209, R1 + review normative):** additive `health` on summary
  (`empty|healthy|degraded` — repo-state vocabulary, scoped apart from `overview.health.status ∈
  ok|degraded|error`, the dashboard vocabulary) with `missing_total?` on degraded; `ETag` covers
  health (`"<head sha>"`, `""` when unborn as before, `~degraded` suffix so the flip busts SWR);
  `empty repository: ` 404-marker prefix on unborn-repo resolve/tree/blob/commits/commit 404s
  (exact `HeadSeq == 0` + no-refs predicate, fail-closed, damage-404s never prefixed, frozen wire
  otherwise); read-only `fsck?` projection on overview (+1 conditional GET, stated). `POST
  …/ops/fsck` was verified already addressable, so no new endpoint was added and the `repair-check`
  option is closed. Rationale: detection + guidance is the gap (repair already exists); the UI must
  distinguish "empty, guide me" from "broken, toast me" without parsing prose.
- **Repo description (issue #235):** additive `description` on the summary wire (`""` when
  unset), folded from the manifest-inline settings TOML at zero new store round trips; `ETag`
  gains the `~d<fnv1a32hex>` suffix so description-only changes bust SWR (same trap as
  `~degraded`). The General settings tab + header rendering are a 12_web_ui.md concern; the
  owner-list rows explicitly do NOT carry descriptions (names-only listing — per-row summary
  fetches would be N round trips).
- **Explicit create-repo placeholder (issue #210, R1 + review normative):**
  - *Sidecar classification:* `repos/<o>/<r>/meta/placeholder.json` is Create-once
    (`PutCreate`; 412 = already a placeholder — idempotent) + Delete-on-transition (cleared
    post-push-CAS, post-response), Delete-then-Create ONLY, never Update — the invitation class
    (01 §7: Create-only, delete-on-terminal, NOT overwritable), so **no §14.11 frozen-list change**
    was required; the adopting change states the classification here. No new manifest state, no
    new WAL kind, no new bucket family for the repo itself ("non-real" is a UI designation).
  - *Owner admission (01 §5.2, Forgejo #346 — supersedes the #210 org-only gate):*
    the owner must equal the principal's username or be a member org (host admins
    bypass) — 403 naming the allowed owners, probe errors → 503 (never 403-as-404).
    Enforced by `(*Service).CheckCreateOwner` BEFORE any namespace write via the
    `CreateOwnerGate` seam (explicit create), `RoleService.CheckCreateOwner` (import),
    and the mirror create-from-URL hook. Rationale: without it placeholder
    creation becomes name-squatting inside someone else's org — or any prefix at all.
  - *`?placeholder=true` justified AS the shape* (pre-1.0 rule): it selects create-semantics on
    the existing PUT — not an alias/shim/deprecated flag; the frozen 409-with-`html_url` stays
    plain text (`writePlain`), and every added response field is enumerated in §9.1.1.
  - *Discovery via `RegisterExposed`* (Feature 10 precedent): `POST /api/v1/repos` (+
    `/api-browser/v1` twin) rides `server.ExtraRoutes` — no core-table edit (law 8). At the time
    the `01/02/03/C2/05/06` routes stayed out of discovery; Forgejo #272 superseded that —
    registration is now the norm for every ExtraRoutes surface (see Decisions).
  - *SDK:* `repo.create(opts)` stays flag-less (frozen PUT); the flag rides new
    `repo.createPlaceholder({object_format})`, and the top-level twin is `client.repos.create()`
    (new `create.js` submodule + naming validation mirroring `ParseRepoId`; `S1`
    reuse-or-justify — one server writer, two entry shapes mirroring the two routes).
  - *Adoption is guarantee + hint, not a push branch:* the push path never branches on
    placeholder-ness (Open wins, no `ErrExists`, no 409 surface by construction — verified, not
    added); the marker clear is a same-process hint-gated (`api.PlaceholderHints`: create Adds,
    push Consumes) post-CAS post-response fire-and-forget Delete on the control-plane transport
    (server orchestrates, wal/git untouched). Push budgets unchanged (the push-budget test passes
    unmodified — unhinted pushes issue zero marker ops); stale markers are harmless (readers key
    on `refs==0 && marker`) and a maintainer sweep is future work.
  - *Auth-none (B5):* no eager `user:anonymous` binding (fails subject validation — subjects are
    emails); none-mode materializes a visibility-only doc or relies on synthesis (existing
    flag-driven grants); the access-bootstrap race is Create-wins, adopt-don't-overwrite.
  - *Expiry:* `expires_at` always null in this change (TTL off by default, no sweep, no
    `placeholders_per_principal` counter — rate-limit + docs only, per S5/`§8` cuts).
- **Mirror surface (Forgejo #240).** Repo lanes `/{o}/{r}/api/mirror`
  (GET open read, PUT/DELETE admin, `POST …/mirror/sync` admin → 202 with a
  service-level async id, `GET …/mirror/sync[?id=]`) plus the top-level
  create-from-URL twin `POST /api/v1/repos/mirrors` (write-gated, import
  dangerous-authority rule for off-allowlist hosts). Discovery lists ONLY the
  top-level twin (`api.RegisterExposed`, the import precedent). The summary
  gains `mirror: {upstream_url, schedule, next_sync_at, last_synced_at,
  last_result, consecutive_failures, due}` behind an `api.Env.MirrorSummary`
  hook (the ReadGate/CreateOwnerGate shape — this package never imports the feature);
  the ETag covers it (`~m` suffix, the #235 `~d` precedent) so outcome-only
  changes never 304. Strict JSON (unknown fields 400); tokens ride the POST
  bodies memory-only (`secret_set` presence-only in task params). Issue #320
  adds `degraded_reason` (omitempty, old clients ignore): the hook's
  serve-health verdict, so the projection agrees with the summary `health`
  field instead of advertising a stale `ok` — covered by `~m` too, and the
  summary derives `health: degraded` from it with +0 extra round trips.
- **Size-detailed listing (Forgejo #248, R1 B2 — new alongside v1).**
  `GET /api/v1/owners/{owner}/repos/detailed` (+ `/api-browser/v1` +
  `/services/api` twins, discovery `endpoints[]`, SDK `owners.detailed`) is
  the object-row surface the v1 string list can never become in place
  (14 §14.12: row-shape change forces a new endpoint; query params alone are
  additive). String lists stay byte-identical. Rationale: queryable
  size state (sort/filter) without per-repo manifest scans at query time —
  one catalog read per query, null-vs-0 preserved so unbackfilled repos hide
  instead of lying.
- **Activity rows extend the detailed surface (Forgejo #247 — additive, no
  new endpoint).** `sort=activity` (+ `last_commit_sha/time`, `last_push_at`
  row fields) extends `/detailed` under 14 §14.12's field rule (new OPTIONAL
  fields on existing JSON objects; a new sort value changes no existing
  query) — R1 B2's "new endpoint" is the `/detailed` surface itself, which
  #248 already landed; a second endpoint for the same rows would fork the
  surface for no isolation gain. v1 string lists stay untouched. Rationale:
  the explore page orders by most recent commit from the same ONE catalog
  read, and stamps render from the rows with zero per-row fetches.
- **Third cache class for mutable collab state (Forgejo #280 — amends the §4
  design rule).** §4 said "two cache classes", both for git content; the
  collaboration specs then borrowed the SWR class for version-keyed GETs
  (threads, social, releases, pull views, identity docs), licensing the
  refresh flip-flop #259 fixed scoped for threads and #280 fixes everywhere:
  `private, no-cache` + the existing version ETag. The rule's intent is
  preserved and narrowed — SWR stays exactly where staleness is bounded by
  ref movement (git-content routes keep byte-identical headers); mutability,
  not addressability, decides the class. Per-package `ccMutable` constants
  follow the established per-package `writeCached`/`matchETag` duplication
  (the #259 `ccThread` precedent); no shared import was added. The pull-view
  ETag folds head/base live shas + thread/pr versions + the mergeable stamp
  (a HeadLive-only token would 304 a thread whose comments never moved the
  head). Client invalidation helpers are unchanged (out of scope — the
  header contract covers the reload case).
- **Listing mirror flags ride the detailed endpoint (Forgejo #281).**
  `GET /api/v1/owners/{owner}/repos/detailed` rows gain `mirror` (always
  present, never null) + `mirror_upstream?` (only when the sidecar parses)
  so listing rows render the mirror indicator with no per-row summary
  fetch (the N-summary-GETs alternative is rejected — same additive shape,
  consumers-ignore-unknown-fields rule as #247/#248). The flags come from
  per-row sidecar probes (`repos/<o>/<r>/meta/mirror.json` — probe, don't
  list, law 4) issued in bounded-parallel (≤ 8 in flight: request count,
  no sequential depth — the catalog read stays the single sequential
  object read) with disjoint per-index writes (no locks; see the
  `### Concurrency` note on `fillMirrorFlags`). Present-but-corrupt counts
  as a mirror without upstream (the `IsMirror` fail-closed parity); the
   probe reads the body inline (no `internal/mirror` import from core —
   law 8) and any store error degrades to non-mirror, never a 500.
   Rationale: the catalog carries no mirror state and the listing (not a
   push/refs hot path) is where the indicator must live; parallelism keeps
   the round-trip budget honest.
- **Owner activity rollup (Forgejo #283 follow-up — server-side ordering).**
  `GET /api/v1/owners?sort=activity&order=` returns the FROZEN string list
  activity-ordered (no shape change — sort alone is additive), and
  `GET /api/v1/owners/detailed` (+ `/api-browser/v1` + `/services/api`
  twins, discovery `endpoints[]`, SDK `owners.listDetailed`) is the
  object-row surface the v1 string list can never become in place
  (14 §14.12: row-shape change forces a new endpoint — the #248 precedent).
  The per-owner max is DERIVED at request time (`sizecatalog.OwnerRollups`
  over the in-memory catalog — one comparison per repo, zero new bucket
  keys, zero new store trips beyond the ONE catalog GET, no proto/codec/
  fixture change): storing it would duplicate per-repo state needing
  backfill/monotonicity for no trip saving on a non-hot path. Incremental
  maintenance is structural — the #247 per-repo activity is already
  incremental (blind push-path write + sweep heal), so a new push moves its
  owner's max without a rescan. Rationale: the client re-rank cannot see
  past the MAX_OWNERS cap (unmounted sections never report) and first paint
  lies; the server ranks over ALL owners before the slice while the client
  rank stays as the fallback for missing/stale values (12_web_ui.md).
- **FIXED (issue #301) — per-entry last-commit dates on the tree payload.**
  Every tree row used to show the repo HEAD date (the UI stamped one
  `commits?n=1` value onto all rows). `TreeEntry` gains `commit_sha` +
  `commit_time` (both `omitempty` — additive JSON per 14 §14.12, SDK
  `TreeEntry` typedef extended, no route change: the tree keeps its single
  template on both lanes), computed by ONE batched walk per listing
  (`treeLogArgv`, §9.4 — never N per-row subprocesses, the rejected
  alternative). The walk is `--max-count` capped by the new
  `server.max_tree_log` key (default 200, `>= 1`, fail-closed — the
  [import] bounds pattern); past the cap and for submodules the fields
  stay absent and the UI renders the #133 `DateTime` with an em-dash
  fallback, never the HEAD date. The dates derive from the resolved sha,
  so the payload stays sha-pure (ETag/SWR §§4/9.2 unchanged) and the walk
  adds zero store round trips (local git only). Fail-open: a failed walk
  never fails the tree. Rationale: GitHub-truth per-row dates without
  breaking the hot-path cost model — one bounded local invocation, parse
  stops at full coverage.
- **Instance repo total rides `owners/detailed` as `repo_count` (Forgejo #307).** Each row gains
  one always-present `repo_count` (14 §14.12 field rule — no new endpoint, no new discovery
  template, all three twins + SDK shape comment carry it for free). Source is the REGISTRY's
  `OwnerRepoCounts` (one manifest-gated walk — the same trip profile as the `Owners` call it
  replaces, so the endpoint costs zero added store trips), deliberately NOT the #283 catalog
  aggregate: the sweep only adds/updates catalog rows and never prunes deleted repos, so a
  catalog fold would resurrect ghosts, and unbackfilled repos would undercount — the registry
  `liveRepos` gate is the only ghost-exact source. The Go `RepoRegistry` interface grows one
  method (real registry + both test fakes updated in the same change; compiler-checked, no
  fallback branch). Rationale: the issue's preferred rail (counts nearly free) with the sound
  source (ghost parity with the #295 acceptance rule) — the page sums the field over the
  uncapped payload (12_web_ui.md), never a capped slice and never a per-owner listing walk.
- **Tab-badge open counts ride the summary (Forgejo #319).** `GET …/api` gains always-present
  `open_issues`/`open_pulls` (14 §14.12 field rule — no new endpoint, both lanes + SDK
  passthrough carry them for free; the two-count-endpoints alternative costs two extra requests
  per repo view for one badge, rejected). Source is the shared `issues/index.json` read
  index-first behind the `Env.CollabCounts` hook (the `MirrorSummary` shape — api never imports
  the feature, law 8; one exact-key GET serves both numerators since issues + pulls share the
  index object; absent index → zeros with the byte-identical ETag). Deliberately NOT a
  windowed-list count (`{issues, more}` caps at 100) and NOT a LIST scan (the summary is a
  per-page-view path — probe, don't list, law 4; open cards are never compacted, so the index
  read is exact under the same envelope the lists read under). `ETag` gains the
  `~c<index-version>` suffix (same trap as `~degraded`/`~d`: close/reopen moves no ref). The
  class stays SWR — coordinated with, not duplicating, the #280 no-cache migration: the
  summary itself remains ref-dependent git content, and the residual ≤60 s window closes
  client-side (08 §4 stream invalidation of the shared summary entry + mutation-site
  reconcile, the #318 pattern). Rationale: zero new client requests with a version-keyed
  ETag — the cheapest correct source, with the staleness story stated instead of silent.
- **Self-service ssh-keys are token-lane-only (Forgejo #339).** The §3 lane note claimed
  `/api-browser/v1` twins for every non-repo endpoint, but `GET`/`POST`/`DELETE
  `/api/v1/ssh-keys` (`internal/api/routes.go`) never had browser-lane twins — 17_ssh.md §3
  and 11_config_cli.md name only the `/api/v1` routes and the `/keys` page fetches them
  directly. Doc-clarity fix only: the lane note now carves ssh-keys out as token-lane-only
  instead of adding twins (no consumer; twins would widen the browser-lane surface for
  nothing). No wire or behavior change.
- **Org marker rides `owners/detailed` as `is_org` (Forgejo #348).** Each row gains one
  always-present `is_org` bool (14 §14.12 field rule — no new endpoint, no new discovery
  template, all three twins + SDK rows carry it for free; old clients ignore it). Source
  is the `Env.Orgs` `OrgLister` seam behind one `ListOrgs` call per listing regardless of
  owner count (law 6 — deliberately NOT N per-owner probes, and NOT the #283 catalog
  aggregate, which knows repos but not org namespaces; api never imports identity,
  law 8 — the `MirrorSummary`/`CollabCounts` shape). Nil seam → all false (instances
  without identity wired); a list error fails open to all-false (display metadata must
  never fail the listing — the `CollabCounts` precedent). Rationale: the explore page
  badges org rows and links them to the org profile with zero extra GETs (12_web_ui.md).
- **Serve-health degraded source + `mirror.degraded_reason` (issue #320, 2026-09-11).**
  The summary `health: degraded` now has two sources: the cached `fsck.pb` report (a hit
  short-circuits) and the serve-health sidecar `meta/serve-health.json` (05 §5.2.1) — direct
  probe for non-mirrors, the mirror hook's `degraded_reason` verdict for mirrors (+0 extra
   GETs there). Cost change, stated plainly: healthy non-mirror summaries pay +2 exact-key
   GETs instead of +1 (same R1-B1 cost class, off the law-6 budgeted paths — not a hot-path
   regression). `mirrorHash` covers `degraded_reason` so the flip busts the SWR cache.
   Rationale: an unhealthy repo reporting `healthy`/`ok` with no recovery is the contract
   failure of #320; there is no cheaper read path to the serve verdict.
- **Repo summary joins the mutable-collab class (Forgejo #381 — amends the §4 scope
  and supersedes the #319 "class stays SWR" coordination).** The summary was the
  surface #280 left on `ccSWR` because its ETag covers most mutable fields — but a
  version-keyed ETag gives correct *revalidation* while SWR's stale-serve window is a
  *correctness* concession git content can afford and user-mutable state cannot: a
  visibility flip changed the ETag (`~v`) yet the browser still painted the pre-flip
  body on the next refresh (alternating public/private across refreshes). Fix (a)
  from the issue: serve `private, no-cache` (new per-package `ccMutable`, the #280
  precedent — bare `ccNoCache` would drop the `private` directive the
  visibility-filtered reads need); `~d`/`~m`/`~c`/`~v` suffixes unchanged, so
  unchanged summaries still 304 with zero body (law 6: ETag/304 economics kept, and
  the path is off the push/sync/checkpoint budgets). The settings visibility select
  (authoritative `access.json` GET, already no-cache) now paints the PUT's echo on
  save, so badge and select never disagree on one screen (the #259 sibling-endpoint
  lesson); its 5 s prefill TTL is kept (ttl=0 would refetch-loop against the data
  layer's signal-subscribed effect) with the `access:{full}` invalidation keys
   verified to match. This closes the #280 systemic scope: the summary call site is
   explicitly listed as fixed here.
- **Repos/detailed listing joins the mutable-collab class (Forgejo #384 — the
  #381 pattern, amends the §4 scope and the §8 "SWR class" line).** The
  detailed route served visibility + mirror flags under `ccSWR` with no ETag
  at all — the same stale-serve trap class #381 fixed on the summary, worse
  (no revalidation story, so a flip stayed stale for the whole SWR window
  instead of one refresh). Fix (a) from the issue: serve `private, no-cache`
  (the existing per-package `ccMutable` — no new constant) with a content
  ETag (`d<fnv1a32hex>` over the rendered rows' JSON). No `~suffix`
  discipline: the summary hangs suffixes on its single head sha, but the
  listing has N rows and N tips, so the hash covers every mutable projection
  (visibility, mirror, mirror_upstream — enumerated in the `detailedETag`
  doc comment) plus the catalog-driven fields and the row order itself.
  Unchanged listings still 304 with zero body (law 6: ETag/304 economics
  kept, and the path is off the push/sync/checkpoint budgets — the probes
  behind the rows are unchanged). SWR was not kept-and-documented because
  the flags mutate with no ref movement by construction (sidecar
  create/delete, access.json PUT), exactly the mutability rule §4 states.
- **Owner profile joins the mutable-collab class (Forgejo #385 — the
  #381 pattern, amends the §4 scope and the §8 "SWR class" line).** The
  profile route served the PUT-editable bio (plus display name,
  location, timezone) under `ccSWR` with no ETag at all — the same
  stale-serve trap class #381 fixed on the summary, worse (no
  revalidation story, so an edit stayed stale for the whole SWR window
  instead of one refresh). Fix (a) from the issue: serve `private,
  no-cache` (the existing per-package `ccMutable` — no new constant)
  with a content ETag (`p<fnv1a32hex>` over the served doc's JSON, the
  #384 `detailedETag` shape — no `~suffix` discipline, since the profile
  has no single head sha to hang suffixes on). The hash covers every
  mutable projection (display_name, location, timezone, bio_markdown,
  updated_at — enumerated in the `profileETag` doc comment) plus the
  request-scoped `can_edit`, so a grant-only change busts the
  revalidation too instead of 304ing a stale Edit affordance.
  Unchanged profiles still 304 with zero body (law 6: ETag/304 economics
  kept, and the route is off the push/sync/checkpoint budgets — the
  single exact-key sidecar GET behind the read is unchanged). The
  issue's "avatar?" question is answered in the §8 profile line: there
  is no avatar projection on this route (avatars ride `me.avatar_url`,
  Forgejo #376), so SWR was not kept-and-documented — every field on
  the response mutates via PUT by construction, exactly the mutability
  rule §4 states. The SPA needs no change: it already invalidates its
  `profile:{owner}` client entry on save.
