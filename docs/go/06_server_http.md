# 06 — HTTP server (`internal/server`)
> Source: MASTER_RUST_SPEC.md §8 (8.1 middleware, 8.2 routing, 8.3 routes, 8.4 git endpoints, 8.5 static objects, 8.6 edge contract, 8.7 LFS, 8.8 auth, 8.9 setup/recipes/installer, 8.10 health/metrics/startup, 8.11 TLS notes), §20 items 2–5 · Status: normative for the walhub Go implementation.

Package: `internal/server` (module `git.packden.us/crueber/walhub`). Binary `walhub`. Dependencies allowed here: `github.com/go-chi/chi/v5` (core only), `github.com/BurntSushi/toml`, `golang.org/x/net` (`h2c`) — nothing else. The router is chi (§3); the git subprocess layer (`internal/git`), WAL engine (`internal/wal`), API handlers (`internal/api`), and the setup module (`internal/setup`, §3.4) are consumed through small interfaces defined in this doc (§2.4) so siblings can land independently. Cross-references: 04_git.md, 07_api.md, 11_config_cli.md, 13_concurrency.md.

## 1. What this server is

One process serves: git smart HTTP (v0/v2), the LFS basic transfer protocol, immutable static objects (bundles, LFS bytes), the JSON API with SSE, the SPA shell, the setup UI/API (§3.4), and instance health/metrics. There is **no dumb-HTTP mode** (§20.2: `GET /{o}/{r}/HEAD` and `/objects/info/packs` MUST NOT exist — always 404 via the wildcard catch-all) and **no `/services/git-*` routes** (§20.3: `/services/*` hosts only JSON-UI endpoints, `setup.json`, and `public/*`).

## 2. Server core

### 2.1 The `Server` struct and `AppState`

```go
type Server struct {
    cfg      *config.Server           // internal/config
    store    store.Store              // internal/store (02_storage_protobuf.md)
    wal      wal.Registry             // internal/wal
    git      *git.Service             // internal/git (04_git.md)
    bundler  *bundle.Builder          // internal/bundle
    auth     *auth.Service            // §8 of this doc
    metrics  *metrics.Registry        // §11 of this doc
    inflight Inflight                 // global in-flight gauge (advisory cap)
    semaphores *RepoSemaphores        // per-repo git concurrency (striped by repo id)
    draining DrainState               // two-phase drain (§12)
    version  string                   // build sha: build-time env → git short sha → "dev"
    instance string                   // name[/id] per MASTER_RUST_SPEC §3.4
}

type Inflight struct {
    mu   sync.Mutex
    n    int64
    high int64 // configured advisory cap: server.max_concurrent_requests
}
```

### 2.2 Handler composition

One `http.Handler` chain, built in `internal/server/middleware.go` as an **explicit ordered slice** of `func(http.Handler) http.Handler` factories applied through chi's `Use` — no reflection, no registration magic (chi requires all `Use` calls before any route registration, which the fixed order guarantees):

```go
func (s *Server) Handler() http.Handler {
    r := chi.NewRouter() // chi v5
    for _, name := range s.cfg.MiddlewareOrder { // outermost first; MUST precede route registration
        r.Use(middlewareByName[name](s)) // each factory returns func(http.Handler) http.Handler
    }
    s.mount(r) // §3: the chi route tree
}
```

The ordered list, outermost first, is FIXED (the Rust spec's stack; the code-vs-docs note in §20.4 is honored — **no timeout layer and no body-limit layer exist**):

| # | Middleware | Go notes |
|---|---|---|
| 1 | `requestID` | honor inbound `X-Request-Id` else mint a random 16-byte hex UUID; echo on the response; start the `http.request` log record: request_id, method, path, user_agent (truncate to 200 chars), status, bytes_in/out, principal, repo, trace_id (parsed from `X-Cloud-Trace-Context` or `traceparent`). In-flight count increments here and decrements **only when the response body is fully written** (wrap `http.ResponseWriter` with a write-completion hook, not a pre-handler defer). |
| 2 | `canonicalBrowserHost` | only GET/HEAD that "look like browsers" (Accept contains `text/html`, OR `Sec-Fetch-Dest: document`, OR UA contains `Mozilla`) AND host is loopback (IP loopback or `localhost`) → 302 to `walgit.localhost[:port]`, same path+query, scheme `https` when TLS is on else `http`. Skipped for `/_auth/*`, `/healthz`, `/readyz`, `/services/public*`. Never applies to git/curl clients (they fail the browser test). |
| 3 | `hostFromAuthority` | HTTP/2 only: if `r.Host == ""`, copy `r.URL.Host` (populated from `:authority` by net/http) into `r.Host`. Purely cosmetic normalization for downstream host checks. |
| 4 | `serverHeaders` | every response — errors, SSE, static — carries `Server:` and `X-Walgit-Server:` = `walgit/<version> (<kind>; <name>[/<instance>])`, kind ∈ `serverless\|ssd\|dev`. Implemented as a ResponseWriter wrapper so late header writes (before WriteHeader) still land. |
| 5 | `recoverPanic` | `defer recover()` → log with request_id → 500 plain text `internal error` (request_id included). MUST NOT kill the process; MUST NOT swallow the inflight decrement (order matters: recovery inside the inflight-wrapped writer). |
| 6 | `cors` | path-scoped only. See §2.3. |
| 7 | `refreshSession` | sliding cookie: if a valid `walgit_session` cookie is older than `session_ttl/4` AND the principal still passes policy, re-issue via `Set-Cookie` on app responses (skip `/_auth/*`). Stateless: age comes from the token's `iat` (§8.5). |
| 8 | *(gated group only)* `requireAuth` | NOT in the main chain. Attached with `Use` inside the gated route group (SPA shell, `/_ui/*`, `/services/setup.json`, `/metrics`, `/{owner}` UI routes — a `chi.Router` group via `r.Route`/`r.Group`; §3.1). On failure: if the request is a browser-ish GET without an `Authorization` header and browser login is enabled → 307 `/_auth/login?next=<path?query>`; else emit the mapped status (401 with `WWW-Authenticate: Bearer realm="walgit"`, 403, 503+`Retry-After: 15`) with a plain-text body naming the setup command. All other endpoints authenticate **inside handlers**. |
| 9 | `compress` | attached with `Use` ONLY to the three web route groups (JSON API `/api*`+`/api-browser*`, repo JSON lanes, UI) — each is a chi sub-router/group so compression stays scoped. brotli+gzip at fastest level. **NEVER** on git smart HTTP, bundles, LFS bytes, or the token page's SSE-adjacent streams; SSE excluded (streamed answers are never compressed); `/_ui` assets arrive precompressed and pass through untouched. Rationale: packs are already compressed and Content-Length/Range must stay exact. |

### 2.3 CORS — exact rules

Scope: only paths matching `/api*`, `/api-browser*`, and `/{o}/{r}/api[-browser]/…`. No CORS headers anywhere else.

- Allowed origins: `server.cors_origins`; entries are exact origins or one leading `*.` label (the wildcard label must differ from the request's label — `*.example.com` does not match `example.com`).
- Preflight (`OPTIONS`): MUST answer 204 unauthenticated with `Access-Control-Allow-Origin` (or `*` never — see below), `Access-Control-Allow-Credentials: true`, `Access-Control-Allow-Methods: GET, HEAD, POST, PUT, DELETE, OPTIONS`, `Access-Control-Allow-Headers: Authorization, Content-Type, Accept, If-None-Match, X-Requested-With`, `Access-Control-Max-Age: 600`.
- Non-preflight from an allowed origin: `Access-Control-Allow-Origin`, `Access-Control-Allow-Credentials: true`, `Access-Control-Expose-Headers: ETag, Cache-Control, Content-Type, Location`, `Vary: Origin`.
- **A state-changing request (non-GET/HEAD/OPTIONS) from a foreign non-same-origin → 403 before any handler runs.** "Foreign" = not in cors_origins and not the canonical host itself.
- Credentials mode is always `include`, so the allow-origin value is never `*` — it is the concrete origin.
- The `cors` middleware stays **hand-rolled** — `chi/cors` is NOT used (chi core only; the header rules above, including the foreign-origin 403, are more specific than any library). It plugs into the chain as any other `func(http.Handler) http.Handler`.

### 2.4 Interfaces this package consumes (seams for 07/04/05 siblings)

```go
// internal/api handlers, mounted per §3
type APIHandlers interface {
    Repo(w, r, repo *wal.RepoRef, lane Lane) // Lane = LaneAPI | LaneAPIBrowser | LaneRoot
    Instance(w, r); Owners(w, r); Me(w, r); Authenticate(w, r)
    Tasks(w, r, repo *wal.RepoRef); TaskAttach(w, r, repo *wal.RepoRef, id string)
    Ops(w, r, repo *wal.RepoRef); Op(w, r, repo *wal.RepoRef, op string)
}

// static immutable objects: one code path (§5)
type ObjectSource interface {
    Head(ctx context.Context, key string) (Version string, Size int64, ContentType string, err error)
    Open(ctx context.Context, key string, rng string) (io.ReadCloser, error)
    PresignURL(ctx context.Context, key string, ttl time.Duration) (string, error) // accel offload
}

// internal/setup HTTP surface, mounted per §3.4
type SetupAPI interface {
    http.Handler // GET /api/v1/setup · POST /api/v1/setup/test · PUT /api/v1/setup (§3.4)
}
```

## 3. Routing

### 3.1 Router shape

One `chi.Router` (`github.com/go-chi/chi/v5`, core package only — `chi/cors`, `chi/middleware`, and every other chi subpackage are NOT used). The root router carries the §2.2 middleware chain via `Use`; everything below mounts onto it. chi's precedence (static > param > wildcard) serves the explicit tree first and sends everything unmatched to the trailing `/*` wildcard, which replaces the old ServeMux `"/"` fallback and parses `/{owner}/{repo}[.git]/<sub>` **by hand** (chi patterns cannot express an optional `.git` suffix). `.git` is accepted everywhere and stripped; `RepoId` parse failure (owner/repo charset per 04_git.md) → 404 plain text.

```go
r := chi.NewRouter()                     // §2.2: middlewares Use'd first
// health & SDK
r.Get("/healthz", s.healthz)
r.Get("/readyz", s.readyz)
r.Get("/repos.js", s.sdk("repos.js"))    // one plain-ESM SDK file (divergence D2)
r.Get("/services/public/install.sh", s.installSh)
r.Get("/services/public/ca.pem", s.caPem)
r.Get("/api/v1", api.Discovery)          // public-informational
r.Mount("/api/v1/setup", s.setupAPI)     // internal/setup HTTP surface (§3.4) — mounted BEFORE the /api/v1 catch
r.Mount("/api/v1", http.StripPrefix("/api/v1", api.NonRepo)) // me/authenticate/owners + /services/api twins
r.Mount("/_auth", s.authFlow)            // §8.6
r.Post("/_events/notify", s.eventsNotify) // handler-authenticated (09_events.md)
r.Mount("/_ui", gated(s.serveUIAssets))
r.Get("/services/setup.json", gated(s.setupJSON))
r.Get("/metrics", gated(s.metrics))
r.Mount("/setup", s.setupUI)             // setup UI shell + assets (§3.4)
r.Get("/", gated(s.spaHome))             // SPA shell; ?format=text → plain repo list
r.NotFound(notFound)                     // everything else: deliberate 404 — includes /services/public/* beyond install.sh/ca.pem
r.MethodNotAllowed(methodNotAllowed)     // 405 + Allow
r.HandleFunc("/*", s.repoDispatch)       // EVERYTHING repo-scoped, parsed by hand (§3.2)
```

chi mechanics (normative):

- **Method enforcement** is chi's on the routes above: a `GET`-only route hit with `POST` reaches `MethodNotAllowed` → 405 with `Allow`. Inside the `/*` wildcard the hand dispatch owns the §3.3 table and enforces methods itself: known repo path + unsupported method → 405 with `Allow` + plain-text body; unknown path → 404. Both 404 and 405 bodies are plain text, no HTML error pages (Rust contract).
- **Gated group** (`requireAuth`, §2.2 #8): `r.Group(func(g chi.Router) { g.Use(s.requireAuth); … })` holds the SPA shell `/`, `/_ui/*`, `/services/setup.json`, and `/metrics`; the `/{owner}` and `/{owner}/{repo}` UI page routes are reached through the wildcard, which applies the same check per §3.3.
- **Compress groups** (§2.2 #9): the JSON API (`/api*`, `/api-browser*`) and UI lanes are `r.Route`/`r.Group` subtrees with `Use(compress)`; git, bundles, and LFS traffic stays outside them.
- **Setup-only mode** (§3.4) is a branch at the top of `s.mount`: when active, only `/setup*`, `/api/v1/setup*`, `/healthz`, `/readyz`, and `/services/public/*` are registered (plus a `503` fallback for everything else); the full tree below is never mounted.

### 3.2 The `{owner}/{repo}` wildcard dispatch

The trailing `r.HandleFunc("/*", s.repoDispatch)` receives every path the explicit tree did not match:

```
segments = TrimPrefix(path, "/") split on "/"
owner, rest = segments[0], segments[1:]
if len(rest) == 0 → UI page route /{owner} (gated)
repoSeg, sub = rest[0], rest[1:]
if strings.HasSuffix(repoSeg, ".git") { repoSeg = trim; hadGit = true }
repo = wal.ParseRepoRef(owner, repoSeg) or 404
```

Then dispatch on `sub[0]` (after `.git`-strip and re-join with "/"): the table in §3.3. The repository prefix (`/{owner}/{repo}[.git]`) is the **only routing key** — everything after it is dispatched inside the wildcard handler, which is what makes `.git`-everywhere work with chi: no single chi pattern can express `/{owner}/{repo}[.git][/…]` with the optional suffix, and `repoDispatch` is also the last-resort 404 for non-repo-shaped junk (§1).

### 3.3 Complete route inventory (every row copied from Rust spec §8.3)

**Open (no auth):**

| Method | Path | Notes |
|---|---|---|
| GET | `/healthz` | `{status:"ok", version}` |
| GET | `/readyz` | §10.2 |
| GET | `/repos.js` | plain-ESM SDK (single file, divergence D2); no-cache + strong ETag + precompressed |
| GET | `/services/public/install.sh[?repo=]` | `text/x-shellscript`, `Cache-Control: public, max-age=300` |
| GET | `/services/public/ca.pem` | only when this host terminates TLS itself; else 404 |
| * | `/services/public/*` (other) | deliberate 404 |
| * | `/_auth/*` | self-authing flow (§8.6) |
| GET | `/api/v1` | discovery JSON, public-informational |
| POST | `/_events/notify` | handler-authenticated |
| OPTIONS | (preflights) | §2.3 |

**Setup (open per the §3.4 access rules; the API is `internal/setup`, the UI is `web/src/setup*`):**

| Method | Path | Notes |
|---|---|---|
| GET | `/setup` | setup UI shell (plain ESM page, D2); assets at `/setup/assets/*` |
| GET | `/api/v1/setup` | full config schema + effective values + file state + validation errors |
| POST | `/api/v1/setup/test` | validate a proposed config without saving |
| PUT | `/api/v1/setup` | validate + atomically write `<data-dir>/walhub.toml` |

**Self-authing (`require_read`/`require_write` in handler):**

| Method | Path | Notes |
|---|---|---|
| GET/HEAD | `/{o}/{r}[.git]/info/refs?service=` | `git-upload-pack` → require_read; `git-receive-pack` → require_write; unknown service → 400; `Git-Protocol` header selects v0/v2 |
| POST | `/{o}/{r}[.git]/git-upload-pack` | v0 or v2 fetch; gzip request body accepted |
| POST | `/{o}/{r}[.git]/git-receive-pack` | **requires the `.git` suffix**; placement+drain gates; optional broker forward (§4.3) |
| GET/HEAD | `/{o}/{r}[.git]/info/lfs/objects/{oid}` | static contract (§5); upstream read-through (`?size=` honored) |
| PUT | `/{o}/{r}[.git]/info/lfs/objects/{oid}` | require_write; size+sha256 verified; `lfs.max_object_bytes` cap |
| POST | `/{o}/{r}[.git]/info/lfs/objects/batch` | LFS batch API (`application/vnd.git-lfs+json`) |
| POST | `/{o}/{r}[.git]/info/lfs/verify` | require_write |
| GET | `/{o}/{r}[.git]/bundles/list[?filter=]` | git-config bundle list (fulls + chain); only `?filter=blob:none` accepted (else 400); records the principal for D17; `no-cache` |
| GET | `/{o}/{r}[.git]/bundles/catchup[?filter=]` | same list without the fulls |
| GET/HEAD | `/{o}/{r}[.git]/bundles/{strategy}/{name}` | the bundle object; full static contract (§5) |
| GET | `{lane}/refs`, `{lane}/refs/{branches\|tags}`, `{lane}/resolve[/{rest}]`, `{lane}/tree/{rev}[/{path}]`, `{lane}/blob/{rev}/{path}[?raw]`, `{lane}/commits`, `{lane}/commit/{sha}` | JSON API (07_api.md); `lane` = `/api` or `/api-browser` |
| GET | `{lane}` | repo summary (SWR + ETag head sha) |
| GET | `{lane}/policy`, `{lane}/settings`, `{lane}/settings/{effective\|history\|describe}`, POST `{lane}/policy/{validate\|dry-run}`, POST `{lane}/settings/validate` | read-level |
| GET | `{lane}/overview` | WAL dashboard JSON, no-store |
| GET | `{lane}/ops` | `{available:[OpSpec], recent:[TaskRecord], bundle_strategies}` no-store |
| GET | `{lane}/tasks`, `{lane}/tasks/{id}` | task list / attach (SSE or JSON) |
| GET | `/api/v1/me` (+ browser twin) | `{principal, write, anonymous}`, no-store |
| GET | `/api/v1/authenticate` (+ browser twin) | popup landing page (postMessage `repos:authenticated`) |
| GET | `/api/v1/owners`, `/api/v1/owners/{owner}/repos` (+ `/services/api/…` twins) | owner listing, SWR |
| GET | `/services/api/instance` | instance facts (no-store) |
| GET | `/_auth/me`, `/_auth/check` | §8.6 |

**Write/admin:**

| Method | Path | Auth | Notes |
|---|---|---|---|
| PUT | `/{o}/{r}` or `{lane}` (repo root) | require_write | create repo; `?object_format=sha1\|sha256`; 201/409 |
| DELETE | `/{o}/{r}` or `{lane}` | require_admin | 204 |
| PUT/DELETE | `{lane}/policy` | require_admin | policy document |
| PUT/DELETE | `{lane}/settings` | require_admin | ≤ 16 KiB else 413; validated; 200 `{revision}` |
| POST | `{lane}/ops/{op}?params` | require_write | start maintenance op; SSE attach; joins a running same-(repo,kind) task |
| GET/POST | `/_auth/tokens` | session | token page / mint (CSRF-guarded same-origin) |

**Gated (`require_auth` = read):** SPA shell + `/_ui/*` assets, `/services/setup.json`, `/metrics` (`text/plain; version=0.0.4`), `GET /` (SPA; `?format=text` or text Accept → plain one-per-line repo list), `/{owner}`, `/{owner}/{repo}`, `/{owner}/teams/{slug}` UI page routes (tree/blob/commits/commit/wal/settings/issues/labels/milestones/pulls/pull all return `index.html`, `no-cache`). The setup routes above are NOT in this group — their access rule is §3.4 (open exactly while no config file exists, the config is invalid, or auth mode is `none`).

**Both API lanes hit the same handlers.** `/{owner}/{repo}/api/…` (bearer/same-origin) and `/{owner}/{repo}/api-browser/…` (cross-origin browser, `credentials: include`) differ only in the `Lane` value passed to the handler (which changes cache headers and auth-redirect behavior per 07_api.md). Same for `/api/v1` vs `/api-browser/v1` in the non-repo lane.

#### Concurrency (routing)

Hazard: the wildcard dispatch runs on every unmatched request (scanners, probes). Avoidance: it is pure CPU on ≤ 4 segments — no allocation beyond the split slice, no locks; a hard `strings.HasPrefix` fast-path rejects paths that can't be repo-shaped (no `/a/b` prefix) before any parse. chi's trie adds a constant-time miss before the wildcard is reached.

### 3.4 Bootstrap & setup (zero-config first run)

`internal/setup` owns the schema description for the UI, validate-for-save, the atomic write of `<data-dir>/walhub.toml`, and bootstrap-mode detection; `internal/server` wires its HTTP surface (`s.setupAPI`, `s.setupUI`); `web/src/setup*` is the frontend page (plain ESM, D2).

**Boot flow decision tree (normative):**

```
locate data dir (--data-dir flag > WALHUB_DATA_DIR env; default ~/.local/share/walhub, containers /var/lib/walhub)
├─ no <data-dir>/walhub.toml → boot with the first-run defaults (below) + a LOUD setup banner in the logs on every start
├─ config file present, parses + validates → normal boot (§10.4)
└─ config file present but INVALID (parse or validation errors) → SETUP-ONLY MODE:
     only /setup, /setup/assets/*, /api/v1/setup, /api/v1/setup/test, PUT /api/v1/setup,
     /healthz, /readyz, and /services/public/* answer; everything else → 503 plain text
     with a pointer to /setup. The UI shows the exact errors; a saved fix takes effect
     after a restart (the process stays in setup-only mode until restarted).
```

**First-run defaults (built into the binary; no TOML needed to start):**

| Key | Default |
|---|---|
| `server.listen` | `"0.0.0.0:8080"` |
| `store.backend` | `"filesystem"`, rooted at `<data-dir>/store` (02_storage_protobuf.md) |
| `server.auth.mode` | `"none"` — **deliberately allowed on any bind** (see below) |
| `server.auto_create_on_push` | `true` |
| everything else | Rust-spec defaults (11_config_cli.md) |

**Data-dir layout:** `<data-dir>/store/` (object store), `<data-dir>/cache/` (prewarm, LFS spool, TLS), `<data-dir>/walhub.toml` (saved by setup Save; the only config file the server reads).

**Setup UI:** `GET /setup` serves the page; `GET /setup/assets/*` serves its ESM/CSS (embedded `fs.FS`, precompressed, like `/_ui`). The page groups ALL config keys by section with current effective values (merging defaults ← file ← env), validates client-side, and Saves via the API.

**Setup API:**

- `GET /api/v1/setup` → `{ "file_state": "absent"|"valid"|"invalid", "errors": [<validation error>], "requires_restart": [<key>…], "groups": [{ "section": "server", "keys": [{ "key": "server.listen", "value": <effective>, "default": <…>, "type": "string"|"int"|"bool"|"duration"|"list", "secret": bool, "doc": "…" }] }] }`. `no-cache`.
- `POST /api/v1/setup/test` — body: raw TOML (or `{overrides: {key: value}}`); runs the full validator WITHOUT saving → `200 {errors: []}` or `422 {errors: [{key, message}]}`.
- `PUT /api/v1/setup` — body: same as test. Validate; on success atomically write `<data-dir>/walhub.toml` (tmp + rename in the data dir), then respond `200 {saved: true, requires_restart: [<key>…], errors: []}`. The running process does NOT hot-reload — `requires_restart` lists every key whose effective value differs from the just-saved file (all of them, effectively; the process continues in its current boot mode).

**Access rules:** open while (no config file) OR (config invalid) OR (`server.auth.mode = "none"`); otherwise an admin principal is required (same `require_admin` mapping as §8.3). Optional `WALHUB_SETUP_TOKEN` env: when set, the setup routes require it (query `?token=` or `Authorization: Bearer <token>`) and skip the admin check — the escape hatch for exposed hosts.

**auth-none on any bind (supersedes the Rust fail-closed loopback rule):** the Rust rule (refuse auth `none` unless the listen address is loopback) is deliberately dropped for zero-config friendliness. With `mode = "none"` on a non-loopback bind, the server boots but: logs a persistent `WARN auth mode is "none" — anyone on the network has write+admin` at startup AND once per hour thereafter; `/setup` shows a red banner; `readyz` gains `"warnings":["auth_none"]`; and the setup UI pre-selects a real auth mode as the recommended fix. No request is ever refused because of it.

#### Concurrency (setup)

Hazard: concurrent Saves racing the file write. Avoidance: `internal/setup` serializes Saves with a process-wide mutex, writes `<data-dir>/walhub.toml.tmp` then `rename`s (same directory, atomic on POSIX); validation reads the in-memory effective config, never the file mid-write.

## 4. Git endpoint behaviors (§8.4)

### 4.1 Response headers for every git response

```
Cache-Control: no-cache, max-age=0, must-revalidate
Expires: Fri, 01 Jan 1980 00:00:00 GMT
Pragma: no-cache
Content-Type: application/x-git-upload-pack-advertisement
            | application/x-git-receive-pack-advertisement
            | application/x-git-upload-pack-result
            | application/x-git-receive-pack-result
```

### 4.2 The 401-vs-pkt-ERR rule (normative, all four conditions)

On an `info/refs` auth failure, reply **200 + pkt-line `ERR`** (git prints `fatal: remote error: …`) ONLY when ALL of:

1. the client is git-ish: UA starts with `git/` or `JGit/` or contains `git-lfs`;
2. `?service=` is present;
3. the request carried an `Authorization` header;
4. the auth error is **Forbidden** or **Unavailable** (retrying cannot help).

**Invalid/expired credentials MUST get a real 401** — that is what makes git erase the dead token and re-prompt. The pkt-ERR body is the setup help text (host, where tokens come from). Every 401 carries `WWW-Authenticate: Bearer realm="walgit"` (never Basic); every 503 carries `Retry-After: 15`.

### 4.3 Placement/drain gates and the push broker

Placement/drain gates run **before any sync work**:

- `not_served_here` → 503 + `Retry-After: 15` + (for git-ish clients per §4.2 conditions) pkt-line `ERR walgit: <repo> is served by <host>; retry shortly` — `<host>` from the maintainer heartbeats (10_maintenance.md).
- draining → the same 503 shape.
- `receive_pack` additionally refuses a non-`.git` URL with a pkt ERR.

**Push broker forward:** when `wal.push_broker_url` is set and this host maintains nothing, the receive-pack body is forwarded to the broker: method+path preserved, `X-Walgit-Forwarded: 1` and `X-Walgit-Principal: <name>` added, a request carrying `X-Walgit-Forwarded` is refused (loop guard). Broker down → local fallback: the body was buffered up to `wal.push_broker_buffer_bytes` (streamed passthrough above that cap cannot be replayed — fall back only when buffered).

#### Concurrency (git endpoints)

Hazard: per-repo git concurrency and body buffering. Avoidance (13_concurrency.md is the playbook): the per-repo semaphore (`server.max_concurrent_per_repo`) is taken **inside handlers** with `TryAcquire` → 503 `Retry-After: 15` when full, never a blocking wait from a request goroutine. The broker-forward buffer uses a bounded reader (≤ `wal.push_broker_buffer_bytes`) with a spill-to-deny above it. Upstream `git` subprocesses get `ctx`-bound lifetimes: client disconnect cancels the context, which kills the child (see 04_git.md for the process-owner pattern).

## 5. Static object serving (§8.5) — one code path

One code path for every immutable byte (bundles, LFS objects, anything immutable):

- Strong `ETag` = the store version (e.g. `"v123"`); compare `If-None-Match` → 304 (no body).
- `Range` + `If-Range` → 206 with the exact byte range, or 416 when unsatisfiable. `If-Range` mismatch (validator differs) → full 200.
- HEAD supported (headers only).
- `Cache-Control: public, max-age=31536000, immutable`; `Accept-Ranges: bytes`.
- `Content-Type` per kind: `application/x-git-bundle` for bundles, the stored/declared type for LFS objects.
- `X-Content-Type-Options: nosniff`; `Vary: Accept-Encoding` where encoding applies.

**Accel offload (edge contract, §8.6):** when `server.accel_redirect = true` AND the TCP peer (`r.RemoteAddr` host) is loopback AND the method is not HEAD AND the store can produce a URL, the static answer becomes `200` (empty body) + `X-Accel-Redirect: /_store/` + `X-Walgit-Store-Url` (S3: presigned; GCS: path URL) + `X-Walgit-Store-Authorization` (GCS only) + `X-Walgit-Store-Key` (percent-encoded store key = the edge cache key) + `X-Walgit-Etag` (the validator the edge must re-emit). When hit directly (no `X-Walgit-Capabilities`), walgit assumes nothing and streams bytes itself. The reference edge slices ranges (64 MiB), caches by key+range, strips bucket headers, and never forwards client conditionals to the bucket.

## 6. LFS (§8.7)

### 6.1 Batch semantics

`POST …/info/lfs/objects/batch`, media type `application/vnd.git-lfs+json`, `operation=upload|download`, `transfer=basic`. Per requested object:

| Store state | `operation=upload` | `operation=download` |
|---|---|---|
| present in our store (or resolvable upstream via read-through) | **no actions** (git-lfs treats it as present, so pushes of imported history proceed) | our href (static contract) |
| missing | `upload` action (href = our PUT URL) + `verify` action | missing+upstream → our href + `?size=N` (upstream batch demands exact size) |
| nowhere | per-object 404 entry | per-object 404 entry |

### 6.2 Transfer

- `GET|HEAD …/info/lfs/objects/<oid>`: the static contract (§5) with `Content-Type: application/octet-stream`.
- `PUT`: streamed; size + sha256 verified **before the store write**; cap `lfs.max_object_bytes` (default 16 GiB) → 413 when exceeded.
- `POST …/info/lfs/verify`: require_write; checks the recorded oid/size against what was stored.

### 6.3 Upstream read-through (`upstream.lfs` configured)

- One upstream batch per request, **only the missing oids**, 10 s timeout, HTTP Basic `x-access-token:<token>` where the token comes from the env var named by `upstream.token_env`.
- GET: stream the upstream bytes to the client while tee-ing into a spool file under `<cache.dir>/lfs-spool/`. After a complete sha256-verified read, persist the spool into our store. **Never** persist on short read or hash mismatch; **a disconnecting client does not stop the persist** (the tee continues to completion).
- Any upstream failure (non-200, timeout, bad auth) = treat as absent. **Never 5xx on the batch.**
- HEAD → 200 + `Content-Length` from the upstream batch (no byte fetch).
- `lfs.serve_via`: `proxy` (default; bytes through walgit/edge) or `signed_url` (presigned store URLs in the batch hrefs).

#### Concurrency (LFS)

Hazards: the spool tee and body streaming limits. Avoidance:
- **Spool tee lifecycle:** the handler owns one goroutine — `io.Copy` from upstream into a `MultiWriter(clientW, spoolFile)` — no channel, no fan-in. Client write error closes the client half only (`clientW.Close()` semantically: stop writing to the client, keep reading upstream) by switching the client writer to `io.Discard`; the upstream read continues to EOF, verify runs, persist runs. The persist is the goroutine's terminal step; handler returns after the persist OR after the client-abandon path completes, whichever is later. Owner writes; nothing else touches the spool file; it is removed on failure.
- **Bounded memory:** never buffer the object. `io.Copy` with a fixed 1 MiB buffer; the sha256 runs in the same copy loop (`io.TeeReader` into `sha256.New()`), no second pass.
- **PUT limits:** wrap `r.Body` in `io.LimitReader(n = max_object_bytes+1)`; reading one byte over the cap → 413 and drain-abandon. No global body-limit middleware exists (§2.1); limits are per-feature.
- **Upstream batch:** singleflight per (repo, oid) set is unnecessary — one batch per request by contract; the 10 s timeout is a `context.WithTimeout` passed to the HTTP client.

## 7. Gzip request decompression bounds (git POSTs)

`POST …/git-upload-pack` MAY arrive with `Content-Encoding: gzip` (§8.11). Decompress with `compress/gzip` reading **directly from `r.Body`** — never into a full buffer. Bound: `io.LimitReader` at the git protocol's own push/fetch size limits (per 04_git.md ingest caps; no undocumented middleware limit). A corrupt gzip stream → 400 plain text. No decompression anywhere else except the compress middleware's inbound side (JSON APIs accept gzip bodies too, same bounded-reader pattern).

## 8. Authentication (§8.8) — all three modes

### 8.1 Principal

```go
type Principal struct {
    Name      string
    Write     bool
    Admin     bool // independent of Write: delete repos, PUT/DELETE settings+policy
    Anonymous bool
}
var Anonymous = Principal{Name: "anonymous", Anonymous: true} // no write, no admin
```

`mode=none`: everyone is `anon` with write+admin. The Rust spec's fail-closed rule (refuse this mode unless the listen address is loopback) is **superseded** by the walhub divergence (§3.4): any bind is allowed, with loud warnings in the logs, the setup UI, and `readyz`.

### 8.2 Client credential resolution order

1. `X-Walgit-Authorization` present (non-empty) → **that** is the client credential.
2. Else if `X-Walgit-Capabilities` contains `client-authorization` → the client sent none (the `Authorization` header belongs to the edge hop and is never read as the client's).
3. Else the plain `Authorization` header.

Then parse: `Bearer <token>` (case-insensitive prefix) or `Basic base64(user:password)` (the password is the token; the user is ignored except for LFS upstream auth conventions).

### 8.3 Decision trees per mode

**`token` mode:**
```
resolved credential?
├─ none → anonymous (read gated by server.auth.anonymous_read)
└─ bearer/basic token
   ├─ exact match against resolved static tokens → principal with write/admin flags from config
   │   (token_env env var overrides the literal at startup; empty-resolved entries dropped)
   └─ miss → Invalid → 401
```

**`oidc` mode:**
```
bearer token?
├─ static token match → principal            (static tokens work in oidc mode)
├─ "wgt_" prefix → verify wgt_ token (§8.5) → principal from email
└─ else → verify as ID token via JWKS (§8.4) → principal
basic? → static token or wgt_ only (no ID tokens over Basic)
none? → session cookie walgit_session → verify (§8.5) → authenticated principal
     → else anonymous (or 401 when the endpoint requires auth)
```

**Checks:** `require_read`: anonymous && !anonymous_read → 401. `require_write`/`require_admin`: lacking → 403.

### 8.4 ID token verification (hand-rolled JWKS — no external JWT library)

Use `crypto/rsa`, `crypto/ecdsa`, `encoding/base64`, `encoding/json` only. Rules copied exactly:

1. `alg` MUST be RS256 or ES256; anything else → Invalid.
2. Key from JWKS by `kid`: RSA (`n`,`e`) or EC P-256 only (`crv` = P-256); the key's advertised alg must match the token alg.
3. Leeway 30 s on `exp`/`nbf`/`iat`.
4. `iss` must equal the configured issuer with trailing slash stripped, **or** its bare host.
5. `aud` must be in `server.auth.audiences ∪ {oauth_client_id}`; the browser flow pins exactly `oauth_client_id`.
6. Claims require `email` and `email_verified`.
7. Email policy: allowed iff domain ∈ `allowed_domains` or address ∈ `allowed_emails` (else 403). `write` = allowed, unless `write_domains` is set: write iff domain ∈ write_domains. `admin` via `admin_emails`/`admin_domains` (lowercased comparisons).

**JWKS cache:** discovery document cached once per process; keys cached honoring the JWKS response's `Cache-Control: max-age` (default 300 s); stale-while-refresh on expiry (serve stale keys while one background refresh runs); inline refresh on unknown `kid`; refresh failure → 503 Unavailable.

```go
type JWKS struct {
    mu      sync.RWMutex
    keys    map[string]*jwk  // kid → parsed key
    fetched time.Time
    ttl     time.Duration
    refresh flight.Singleflight // hand-rolled (13_concurrency.md): one refresh at a time
}
func (j *JWKS) Get(ctx context.Context, kid string) (*jwk, error) // stale-while-refresh + inline unknown-kid refresh
```

#### Concurrency (JWKS)

Hazard: many request goroutines triggering simultaneous refreshes on unknown kid (thundering herd against the issuer). Avoidance: singleflight (one leader refreshes; followers share the result), `RWMutex` where reads dominate; the leader's context has a 10 s timeout independent of the request context so one slow issuer does not hold requests open beyond their own deadline — followers that time out return 503, they never queue behind a slow issuer twice.

### 8.5 walgit-issued tokens & sessions (stateless)

Payload = `"{kind}\n{exp}\n{iat}\n{email}"` (kind `session` | `token`; exp/iat = unix seconds). MAC = HMAC-SHA256 over the payload with `server.auth.session_secret` (≥ 32 bytes; validated at config load). Wire format:

```
wgt_? + base64url_nopad(payload) + "." + base64url_nopad(mac)
```

Access tokens carry the `wgt_` prefix (kind `token`, TTL `access_token_ttl` = 90 d). Sessions (kind `session`, no prefix) live in the cookie. Rotating the secret revokes everything; nothing can be listed or revoked individually — say so in the token page text.

Session cookie `walgit_session`: `HttpOnly`; `SameSite=None; Secure` when CORS origins are configured, else `SameSite=Lax`; `Max-Age` = `session_ttl` (30 d); sliding re-issue at ttl/4 (§2.1 #7).

### 8.6 Browser OIDC flow (`/_auth/*`; enabled iff mode oidc + session_secret + client id + client secret)

- `GET /_auth/login?next=` → fetch discovery → HMAC-signed state `"{now+600}\n{nonce}\n{next}"` (`next` sanitized: must start with a single `/`) → 302 to the issuer's authorization endpoint with `response_type=code`, `scope=openid email`, `prompt=select_account`, `&hd=` = first allowed domain; **no PKCE**; the nonce is carried but not verified — the state HMAC is the anti-forgery.
- Redirect URI: `{public_url}/_auth/callback`. Loopback origins use `http(s)://localhost[:port]/_auth/callback` **plus** a `/_auth/claimed?ticket=` hop that sets the cookie on `walgit.localhost` (a 60 s signed ticket) — because `localhost` and `walgit.localhost` are different cookie hosts.
- `GET /_auth/callback?code&state`: verify state (600 s), exchange the code (one retry), verify the ID token (aud = **exactly** the client id, then domain policy), set the session cookie, redirect to `next`.
- `/_auth/logout` clears the cookie. `/_auth/me` → `{principal, write}`.
- `/_auth/check` (the edge's `auth_request`): on success 204 + `X-Walgit-Principal: <name>` + `X-Walgit-Write: 0|1` + `Cache-Control: private, max-age=300` (edges cache one verdict per credential ~5 min); 401/403/503 otherwise.
- `POST /_auth/tokens`: session required, same-origin CSRF guard (`Sec-Fetch-Site` header must be `same-origin`), returns `{token, principal, write, expires_at}` (no-store); GET renders the mint page.

**Identity forwarding (push broker):** `X-Walgit-Principal` is honored only when the authenticating caller (the hop) has write AND its principal name is in `server.auth.trusted_forwarders`; the forwarded name replaces the principal, keeps the caller's write flag, admin re-derived from policy. The rule applies **uniformly on every authenticated surface** — core git/API lanes AND every Seam 1 feature surface (collab issues, pulls, review, checks, notify, releases, social, identity, repoimport): all resolve through the single entry point `AuthService.AuthenticateForwarded` (authenticate, then forward; authentication errors return before any forwarding is considered). A feature handler that re-authenticates via bare `Authenticate` drops the forwarded identity and misattributes brokered actions to the broker — that is an auth bug, not a fallback.

**401/403/503 mapping:** Invalid/Unauthorized → 401 (+ `WWW-Authenticate: Bearer realm="walgit"`); Forbidden → 403; Unavailable → 503 (+ `Retry-After: 15`).

### 8.7 Operator example: end-to-end auth flow

```sh
# mint a token in the browser at https://walgit.example.com/_auth/tokens, then:
export WALGIT_TOKEN=wgt_...
git ls-remote https://walgit.example.com/myteam/myrepo.git HEAD
# token is dead? git gets a REAL 401 → the credential helper erases it → re-prompt.
curl -s https://walgit.example.com/api/v1/me -H "Authorization: Bearer $WALGIT_TOKEN"
```

```toml
# walgit.toml — the auth block the server consumes
[server.auth]
mode = "oidc"
anonymous_read = false
session_secret = "32-bytes-minimum-generated-once"
audiences = ["walhub"]
allowed_domains = ["example.com"]
write_domains = ["example.com"]
admin_emails = ["crueber@example.com"]

[server]
cors_origins = ["https://app.example.com", "*.localhost"]
max_concurrent_per_repo = 4
accel_redirect = false
```

## 9. Setup: recipes, installer, credential helper (§8.9)

### 9.1 `GET /services/setup.json[?repo=]` (read-authed, no-cache)

Fields (exact names): `base_url`, `host`, `token_url` (only oidc), `install` (the one-liner), `install_url`, `manual_clone`, `plain_clone`, `blobless_clone`, `bundle_list`, `setup_text`, `ca_url?`, `trust?`. Every UI surface renders recipes from here — never its own copy.

- `manual_clone` = `git -c http.extraHeader="Authorization: Bearer $WALGIT_TOKEN" -c transfer.bundleURI=true -c fetch.bundleURI={base}/{repo}.git/bundles/catchup clone {base}/{repo}.git` (no extraHeader when auth none).
- `plain_clone` = `git clone -c fetch.bundleURI=<catchup-url> <url>`.
- `blobless_clone` = `git clone --filter=blob:none --sparse --bundle-uri=<list-url>?filter=blob:none -c fetch.bundleURI=<catchup-url>?filter=blob:none <url>`.

### 9.2 `GET /services/public/install.sh[?repo=|tree=]` (open; idempotent POSIX sh)

The Go implementation serves the script from an embedded template (`embed`); its behavior spec:

1. Requires git ≥ 2.46 + curl; exit 1 otherwise.
2. Self-signed TLS: download `ca.pem`, `git config --global http.https://<host>/.sslCAInfo <file>`.
3. Token source order: `$WALGIT_TOKEN` → an already-stored token file → the terminal (`$WALGIT_INSTALL_TTY`); no terminal → **exit 2** printing the two things to do (browse to the token page / run the config command). Auth-none mode: sends `X-Walgit-Anonymous: 1`.
4. Writes `${XDG_CONFIG_HOME:-~/.config}/git/<host-slug>-token` (0600) and `<host-slug>-credential-helper` (0755); the CA at `<host-slug>-ca.pem` when self-signed. Slug = every non-`[a-z0-9]` char → `-` (host-derived, so two walgit hosts coexist).
5. Git config (exact keys): `credential.https://<host>.helper` reset to `""` then set to the helper path; unset stale `http.https://<host>/.extraHeader`; `transfer.bundleURI true`; **unset** global `fetch.bundleURI` (invalid globally; per-clone only); `fetch.uriProtocols https`.
6. Self-test: with `?repo=` → `git ls-remote <base>/<repo>.git HEAD`; else `curl …/api/v1/me` and extract the principal (exit 1 on refusal, message names where tokens come from).
7. With `?repo=` → print ready + exec the plain clone; with `$1` → set/add `origin`; else print the `git remote add origin … && git push -u origin HEAD` recipe.

### 9.3 Credential helper (`get`/`store`/`erase`)

- `get`: prints (git ≥ 2.46 authtype protocol):
  ```
  capability[]=authtype
  authtype=Bearer
  credential=<token>
  username=token
  password=<token>
  ```
  Token from `$WALGIT_TOKEN` else the token file (missing → stderr hint naming `/_auth/tokens`, exit 1).
- `store`: saves the password from stdin to the token file (0600, atomic write — temp file + rename).
- `erase`: skipped when `$WALGIT_TOKEN` is set; deletes the file and tells the user where a new one comes from. This is the dead-credential path, driven by the real 401 (§4.2).

The helper is generated with the host baked in and served by the installer; the Go source owns one `text/template` for it (single source of truth, tested by 15_testing.md golden fixtures).

## 10. Health, readiness, metrics, startup (§8.10)

### 10.1 `GET /healthz`

`200 {"status":"ok","version":"<build sha>"}` — version resolution: build-time env → git short sha → `"dev"`.

### 10.2 `GET /readyz`

- 200 `{"status":"ready","prewarm_pending":N,"instance":"<name>","placement":{"serve":bool,"serve_exclude":bool,"maintain":bool,"maintain_exclude":bool}}` when prewarm finished (or `cache.prewarm_ready_timeout` elapsed; 0 = don't gate). In defaults-banner mode (no config file, §3.4) this adds `"config":"defaults"`.
- 503 `{"status":"warming"|"draining","running":N}` before readiness or during phase-2 drain; the draining variant adds `Retry-After: 15`.
- 503 `{"status":"setup_required","errors":[<config validation error>…]}` in SETUP-ONLY MODE (§3.4) — the readiness gate IS the config; `/healthz` still answers 200 so orchestrators can tell "up but unconfigured" from "down".

### 10.3 Metrics — hand-rolled Prometheus text exposition

`GET /metrics`, content type `text/plain; version=0.0.4`. No client library: a `metrics.Registry` (map of named collectors + a writer that emits `# HELP`, `# TYPE`, and samples in lexicographic family order; labels escaped per the text format). Counters/gauges/histograms implemented in ~200 lines; see 13_concurrency.md for the atomic-based update pattern (`sync/atomic` for counters, per-family mutex only on scrape).

Notable series (the inventory; every one registered at startup):

`walgit_http_inflight`; `walgit_tasks_running`; `walgit_tasks_started_total{kind}`; `walgit_tasks_finished_total{kind,ok}`; `walgit_task_duration_seconds{kind,ok}`; `walgit_lock_wait_seconds{lock}`; `walgit_store_bulk_queue_seconds`; `walgit_store_bulk_inflight`; `walgit_remote_block_cache_{hits,misses}_total`; `walgit_remote_range_reads_total{repo}`; `walgit_remote_bytes_total{repo}`; `walgit_remote_delta_chain`; `walgit_remote_faulted_objects_total`; `walgit_sync_too_large_total`; `walgit_publish_local_apply_failed_total`; `walgit_checkpoint_seconds`; `walgit_checkpoints_total{outcome}`; `walgit_push_refused_total{reason}`; `walgit_not_served_here_total{service}`; `walgit_checkpoint_lag_entries`; `walgit_checkpoint_age_seconds`; `walgit_repo_missing_objects{repo}`; `walgit_maintain_pass_seconds{host}`; `walgit_maintain_units_total{host,kind,outcome}`; `walgit_maintain_unit_seconds{kind}`; `walgit_maintainer_heartbeat_timestamp{host}`; `walgit_bundle_plan_slots{repo,strategy,state}`; `walgit_cache_disk_used_fraction`; `walgit_api_immutable_hit{tier}`; `events_published_total{sink}`; `events_bridge_lag_entries{repo}`; `events_bridge_gap_total{repo}`; `events_bridge_sweep_found_total`; `walgit_follow_rounds_total{repo,outcome}`; `walgit_follow_refs_total{repo}`; `walgit_lfs_upstream_total{op,result}`; `walgit_repair_objects_total{repo}`.

```sh
curl -s localhost:8442/metrics | grep walgit_http_inflight
```

### 10.4 Startup order

1. TLS crypto provider init (§11).
2. **Bootstrap leg (§3.4, `internal/setup`):** resolve the data dir (`--data-dir` flag → `WALHUB_DATA_DIR` → default `~/.local/share/walhub`; containers `/var/lib/walhub`), ensure `store/` + `cache/`, load `<data-dir>/walhub.toml` if present. Missing → first-run defaults + loud setup banner; invalid → SETUP-ONLY MODE (log the exact errors, mount only the §3.4 subset, and skip steps 4–8); valid → continue.
3. Tracing init (filter from `RUST_LOG`-equivalent env else `telemetry.log_filter`; pretty or Cloud-Logging JSON).
4. Open store.
5. Build AppState (wal registry, bundler, auth service + JWKS, per-repo semaphores, metrics registry).
6. Spawn: prewarm (bounded parallelism `cache.prewarm_parallelism`, default 2), events bridge (if role+sink), maintainer loop (if role), follow loop (if role), watchdog (1 s tick; warns "runtime stalled" when a tick is > 2.5 s late).
7. Bind:
   - TLS off: plain TCP, `TCP_NODELAY` per connection, **h2c** (HTTP/2 prior knowledge) + HTTP/1.1 — via `golang.org/x/net/http2/h2c` wrapped around the chi router.
   - TLS on: `crypto/tls` in-process (lazy handshake on first I/O — net/http does this natively); ALPN `h2`, `http/1.1`.
   - Loopback binds also take the IPv6 twin (`::1`) so `*.localhost` works.
8. Auth `none` on a non-loopback bind is allowed (§3.4 divergence from the Rust fail-closed rule) — emit the loud warning instead of refusing.

Go shape: one `http.Server` with `BaseContext` carrying the app context; `Serve(l)` on a listener built per the above; h2c via `h2c.NewHandler(r, &http2.Server{})` where `r` is the chi router (§3.1).

## 11. TLS (§8.10–8.11)

- Self-signed generation: hand-rolled with `crypto/x509` + `crypto/ecdsa` (P-256) — replaces the Rust rcgen. Written once to `<cache.dir>/tls/{cert,key}.pem` plus `cert.sans` (the SAN list actually used). **Regenerated only when the SAN list changes**: read `cert.sans`, compare with the desired SAN set (default: `localhost`, `*.localhost`, `127.0.0.1`, `::1` + the `public_url` host + `server.tls.hostnames`), rewrite all three files only on mismatch. The cert is published at `/services/public/ca.pem`.
- `files` mode: cert + key from config paths.
- HTTP/2 both clear (h2c) and via ALPN; request and response bodies stream both ways; `TCP_NODELAY` on every connection (report-status stalls without it) — set via a wrapped listener's `Accept` loop (`net.TCPConn.SetNoDelay(true)`).

## 12. Graceful shutdown — two-phase drain (Rust spec §3.4)

Signal handler (`SIGTERM`/`SIGINT`) drives the `DrainState`:

- **Phase 1 (bounded 30 s):** stop starting maintenance units, interrupt the running unit at once; serving + `/readyz` stay up; maintenance ops endpoints return the drain 503 shape.
- **Phase 2:** `/readyz` → 503 `{"status":"draining"}` + `Retry-After: 15`; new fetch/push/LFS requests refused 503 (git-ish clients get the pkt-ERR shape per §4.2 conditions); in-flight requests capped at `server.drain_timeout` (each handler's context is cancelled at the deadline); then exit.

Go mechanism: the signal handler cancels a `drainCtx` (phase 1) and, after the phase boundary, cancels the root `appCtx` with a `context.WithTimeout(appCtx, server.drain_timeout)` governing the remaining requests; `http.Server.Shutdown` (stop accepting) runs at the phase-2 boundary, and the process exits when `Inflight` hits zero or the timeout fires. Channel ownership and the shutdown checklist are in 13_concurrency.md — every goroutine started in §10.4 takes its context from `appCtx` and returns on cancel.

#### Concurrency (drain)

Hazard: goroutines (SSE writers, spool persists, git children) outliving the listener. Avoidance: single ownership — the HTTP server is the only component that waits on in-flight handlers; background loops each select on their own ctx's `Done()`; the spool persist (§6) holds a reference to `appCtx` (NOT the request ctx) precisely so a client disconnect during drain cannot abort a persist mid-write.

## 13. SSE writer lifecycle (for handlers in `internal/api`, contract fixed here)

The SSE encoding itself (envelope, keepalives, terminal events) is specified in 07_api.md; the server-side contract every SSE handler follows:

- Writer acquires: `http.NewResponseController(w)` → `SetWriteDeadline` per event; `Flush()` after each event. Headers sent on first write (200, `text/event-stream; charset=utf-8`, `no-store`, `X-Accel-Buffering: no`).
- Keepalive: a `time.Ticker` (10 s) owned by the handler goroutine; `defer ticker.Stop()`.
- Terminal: exactly one of `result`/`error`, then return — the write completion hook (§2.1 #1) releases the inflight slot; a leaked SSE connection otherwise pins inflight and blocks drain.
- Work continues after client disconnect (per 07_api.md): writes check the error; on error the handler stops writing but the underlying task (if any) continues under `appCtx`.
- Never compressed (§2.1 #9); ref-list pages use their own dialect, written unbuffered.

#### Concurrency (SSE)

Hazard: keepalive ticker and event writer racing on the same `http.ResponseWriter`. Avoidance: ALL writes happen on the handler goroutine — the ticker's channel delivers a tick to the same loop that writes events (single `select` loop), never a second goroutine writing. Channel ownership: the task-update channel is created by the handler, closed by the task publisher; the handler never closes it.

## 14. Decisions & deviations from the Rust design

- ~~Router is Go 1.22+ `http.ServeMux` plus a hand-rolled `/{owner}/{repo}[.git]/<sub>` fallback instead of a path-matching framework~~ — superseded by divergence D1 (chi); the hand-rolled `/{owner}/{repo}[.git]/<sub>` parsing survives inside the trailing wildcard (§3.2).
- Middleware is an explicit ordered slice of `func(http.Handler) http.Handler` factories applied via chi `Use` (not tower layers) — the order is load-bearing (§2.2); making it data makes it reviewable and testable.
- Self-signed TLS uses `crypto/x509`/`crypto/ecdsa` instead of `rcgen` — the SAN-stable regeneration contract (§11) is preserved; rcgen is not needed.
- JWKS/JWT verification hand-rolled on `crypto/rsa`/`crypto/ecdsa` instead of a JWT library — per the dependency policy; the algorithm/claim rules are copied verbatim from the Rust spec so behavior is identical.
- Prometheus exposition hand-rolled (text format writer) instead of a client library — dependency policy; the metric inventory is normative so dashboards survive the rewrite.
- h2c via `golang.org/x/net/http2/h2c`, the one sanctioned third-party backend module; TLS-mode h2 comes from stdlib ALPN.
- SIGTERM handling implemented with `signal.NotifyContext` + the two-phase `DrainState` rather than any graceful-shutdown library — the phase semantics are app-specific.
- The install.sh and credential helper are served from `embed` templates rather than shipped as separate files — single-binary packaging (16_packaging.md) and one source of truth for the host-slug rules.
- Go adaptation of the request-id span: the Rust "open the span" becomes the structured log record + trace_id extraction, since Go tracing here is slog-based — same observability surface, no otel dependency.
- No timeout/body-limit middleware is implemented, matching §20.4's truth about the Rust code; limits stay per-feature (push ingest, settings ≤ 16 KiB, blob ≤ 2 MiB, LFS ≤ `lfs.max_object_bytes`).

- **NEW (2026-09-04) — `issues`/`labels`/`milestones` join the `/{owner}/{repo}` UI page routes** (Wave B, docs/features/02 §11): nested paths (`/issues/new`, `/issues/:num`) ride the `sub[0]` match and return the SPA shell; no wire change, the pages use the issues JSON API.
- **NEW (2026-09-04) — `pulls`/`pull` join the `/{owner}/{repo}` UI page routes** (Wave C1, docs/features/03 §9): nested paths (`/pulls`, `/pull/:num`) ride the `sub[0]` match and return the SPA shell; no wire change, the pages use the pulls JSON API.
- **NEW (2026-09-04) — `/{owner}/teams/{slug}` serves the SPA shell** (Feature 08 §1): the 3-segment
  shape reroutes to the gated shell (GET/HEAD only, 405 otherwise); a repo literally named `teams`
  keeps its 2-segment root and every deeper path. New nested repo pages (`/pulls/new`,
  `/pull/:num/commits`, `/pull/:num/files`, `/checks/:sha`) ride the existing `sub[0]` match —
  no router change for those.
- **NEW (2026-09-05) — broker forwarding is uniform across core + collab surfaces** (Forgejo #71): the §8.6 rule resolves at the single entry point `AuthService.AuthenticateForwarded`, and every Seam 1 feature surface (all nine `chain*` closures in `cmd/walhub`) resolves through it — bare `Authenticate` in a feature chain is an auth bug (it attributes brokered actions to the broker). Rationale: one forwarding decision point keeps core git/API lanes and collab handlers identical by construction; fail-closed ordering (errors before forwarding) is inherited, not re-implemented per surface.

**Divergence (2026-08-31):**

- **D1 — Router is chi.** `github.com/go-chi/chi/v5` (core package ONLY — no `chi/cors`, no `chi/middleware`) replaces Go 1.22 ServeMux patterns (§3.1). Route inventory and handler behavior are unchanged; only registration/matching mechanics moved to chi. CORS stays hand-rolled (§2.3). Backend dependency budget is now exactly: `chi/v5`, `BurntSushi/toml`, `golang.org/x/net` (h2c).
- **D2 — Frontend is standard ECMAScript.** No TypeScript, no framework, no bundler. The SDK is ONE plain-ESM `web/sdk/repos.js`; the `/repos.mjs` route and the esbuild twin are gone (§3.1, §3.3). `web/src/setup*` is a plain-ESM setup page. Vite/solid-js/marked/esbuild are not used anywhere in this server's surfaces.
- **D5 — Zero-config first run.** Missing config boots with built-in defaults (`0.0.0.0:8080`, filesystem store under `<data-dir>/store`, auth `none`, `auto_create_on_push`) instead of fatal exit 2 — the old step-2 "missing config file is a fatal exit 2" of the startup order is superseded by the bootstrap leg (§10.4, §3.4).
- **D6 — Setup UI + API first-class.** `/setup` + `/api/v1/setup{,/test}` with the open-while-unsecured access rule and the SETUP-ONLY MODE for invalid configs (§3.4, new `internal/setup` package).
- **Supersession (deliberate, fail-closed):** the Rust rule that auth `mode = "none"` is refused unless the listen address is loopback is REPLACED — auth-none is allowed on any bind with loud warnings (logs, setup UI, `readyz`) and zero refused requests (§3.4, §8.1, §10.4 step 8).
