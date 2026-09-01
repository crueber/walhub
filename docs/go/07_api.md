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
GET /api/v1/me                 → {principal, write, anonymous} | 401 (no-store)
GET /api/v1/owners             → ["demo","jane"] (sorted; from the STORE, not disk)
GET /api/v1/owners/{o}/repos   → ["hello","walgit"] (short names; 200 [] for unknown owner)
GET /{o}/{r}/api               → {owner, name, full_name, head:{name,sha}|null, branches, tags,
                                  clone_url, html_url, api_url}   (SWR + ETag "<head sha>")
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
GET …/overview                 → walhub-specific WAL health (no-store): {repo, clone_url, hostname,
                                  health:{status:"ok"|"degraded"|"error", issues[], deep,
                                  suggestions:[{op, params?, reason, auto?}]},
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
`owners`/`instance`.

## 4. The two cache classes (§9.2 — the central design rule)

| Class | Headers (exact values) |
|---|---|
| **sha-addressed** (full 40/64-hex in the `{sha}`/`{rev}` position): `tree/{sha}/…`, `blob/{sha}/…`, `commits?ref={sha}`, `commit/{sha}` | `Cache-Control: private, max-age=31536000, immutable` |
| **ref-dependent**: `owners*`, `refs*`, `resolve`, and any tree/blob/commits/commit addressed by a NAME | `Cache-Control: private, max-age=0, stale-while-revalidate=60` + `ETag: "<resolved sha>"` + `If-None-Match` → `304` |

- The ETag value is the **quoted resolved sha** (`ETag: "cb38da1…"`), matching a bare double-quoted hex
  string in the header; compare `If-None-Match` by stripping quotes and weak prefixes.
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
            "authenticate": "/api/v1/authenticate"},
  "endpoints": [
    "/api/v1/me",
    "/api/v1/owners",
    "/api/v1/owners/{owner}/repos",
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
list above; keep it mechanically derived from the route table so it cannot drift). Merge-queue data
arrives as commit trailers; there is no merge-queue endpoint. `endpoints` entries are path templates;
adding a route without updating this list is a bug. The doc never lists admin-only writes either —
`endpoints` is a *capability hint*, not an ACL.

- `GET /api/v1/me` → `{principal, write, anonymous}`, `no-store`; `401` (plain text) when unauthenticated
  in a mode that requires auth.
- `GET /api/v1/owners` → sorted owner names **from the STORE** (object-store listing / registry), never
  from a local disk directory; SWR class. `GET /api/v1/owners/{o}/repos` → short repo names, `200 []` for
  an unknown owner (never 404).
- `GET /services/api/instance` → `{kind, name, revision, instance, version, roles[], disk, shape, cpus,
  memory_bytes}` (`no-store`) — "this machine" for UI footers: hostname, declared roles, disk mode, CPU
  count, `runtime.NumCPU()` / total memory.

## 9. Reads: summary, refs, resolve

### 9.1 Repo summary — `GET /{o}/{r}/api` (and `{lane}` root)

After a refs-level sync: `{owner, name, full_name, head:{name,sha}|null, branches, tags, clone_url,
html_url, api_url}`. `head` = default branch (`null` → JSON `null` — the one sanctioned null, it is not an
array). `branches`/`tags` are **counts** (integers). `clone_url` from `server.public_url` (or request
Host); `api_url` = the `/api` lane URL; SWR + `ETag: "<head sha>"`. `PUT` here creates (require_write,
`?object_format=sha1|sha256`, `201`/`409` exists); `DELETE` (require_admin) → `204`.

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
   On success `kind = "commit"`, `ref = ""` echoed as the revision input, remaining segments = `path`.
4. Still unresolved → `404` (plain text).
5. Tags resolve to the **peeled commit**; the response echoes `ref` (full ref name), `sha`, and `path`
   (the segments after the matched prefix, `/`-joined, NOT pre-encoded).

A tags-name tie example: rest `v1/src` with tag `v1` and branch `v1` both existing → branch wins, `path = "src"`.

### 9.4 Tree — `GET …/tree/{rev}[/{path}]`

Resolve `{rev}` first (§9.3); then `git ls-tree -l -z <tree-sha> -- <path>` is wrong when `path` is a
prefix walk — normative recipe: resolve the tree object of `path` first (`rev`'s commit → tree, descend
by `path` segments via `git ls-tree -z <tree> <seg>` per segment or a single
`git ls-tree -z -l <commit-ish> -- <path>` on the directory), then:

```text
git ls-tree -l -z <tree-sha>            # entries of THAT directory
git log -1 --format=<FMT_COMMIT> --no-color <commit-sha> -- <path>   # commit? newest touching path
```

- `-z` output lines: `<mode> SP <type> SP <sha> SP <size> TAB <name>` NUL-terminated. `type` ∈
  `blob|tree|commit` (submodule); `size` is `-` for trees/submodules → emit `-1`.
- Sort in Go: directories first, then byte order by name (NOT git's order).
- `readme`: the first blob named `readme` with optional extension `.md|.markdown|.txt|.rst`,
  case-insensitive, in the sorted order above; contents fetched via `git cat-file blob <sha>`, emitted
  only when valid UTF-8 (`readme: {name, contents}` omitted otherwise).
- Response: `{ref, sha, path, entries:[{name,type,mode,size,sha}], commit?, readme?}`; `commit` present
  only when `path` is non-empty; full-sha `rev` → immutable class; `404` if the target is not a tree.
- `mode` is the 6-char git mode string verbatim (`100644`, `040000` for trees as git prints, `160000`).

### 9.5 Blob — `GET …/blob/{rev}/{path}[?raw]`

1. Resolve `rev`, then walk `path` to the blob sha (must be `100644|100755|120000`; else `404`).
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

## 14. Decisions & deviations from the Rust design

- **Hand-rolled LRU + single-flight** instead of moka/`golang-lru`/`singleflight` crates — the dependency
  policy allows only `x/net` and `BurntSushi/toml`; the single-flight sketch (§5.1) is the whole pattern.
- **Render-cache entries are revision-stamped** and the bucket envelope carries `revision` — the Rust spec
  keys renders by "ref-state version"; stamping makes lazy invalidation explicit and stale bucket
  generations self-identifying.
- **New config key `cache.render_cache_bytes`** (default 256 MiB) — the Rust spec fixes cache sizes per
  use (`cache.*_entries`); one weighted-bytes budget is simpler and the bucket/LRU split is preserved.
- **Discovery document lists only real routes** and is derived from the route table — fixes §20.4 (the
  phantom `…/commit/{sha}/merge-queue` advertisement must not be copied).
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
