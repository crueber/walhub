# 14 — Extensibility & modularity: seams for GitHub-like growth

> Source: MASTER_RUST_SPEC.md §1.3 (feature surface), §3.1 (roles), §8 (HTTP/auth), §9 (API), §12 (events), §14 (policy) · Status: normative for the walhub Go implementation. This document is NEW — the Rust system has no extensibility layer; it is derived from the frozen contracts the Rust spec already defines.

## 14.1 Philosophy: a small core that stays small

The Rust spec closes §1.3 with "explicitly not in scope: code review, merge queues, CI, issues, fork networks, PRs." That scope decision is what makes walgit fast and auditable. The Go rewrite keeps the same core, but is REQUIRED to make the boundary explicit so a future GitHub-like capability lands on a **named seam** instead of a core edit. The test for every proposal:

1. Does it change a frozen contract (§14.2)? If yes, it is a core revision and needs a spec change of this document family — not a feature commit.
2. Does it fit an extensible seam (§14.3–§14.9)? If yes, it is a registration plus a package, and core does not learn its name.
3. Does it add durable state anywhere but the bucket? If yes, it violates Principle 1 and is rejected regardless of seam fit.

The package tree is fixed (module `git.packden.us/crueber/walhub`):

```
cmd/walhub            # binary entry: subcommand dispatch (Seam 7)
internal/store        # bucket contract + backends (Seam 6)
internal/wal          # WAL engine, manifest CAS, log, checkpoints
internal/git          # smart HTTP, receive/upload-pack, stock git subprocess
internal/bundle       # bundle-uri subsystem
internal/policy       # policy.json parse + eval (Seam 3)
internal/server       # http server, middleware, auth (Seam 2), lanes
internal/api          # JSON API + SSE + route registry (Seam 1)
internal/events       # WAL tailing + sinks (Seam 4)
internal/maintain     # maintainer loop, task kinds (Seam 5), fsck
internal/config       # walhub.toml + env overrides
web/                  # vanilla ES-module SPA + SDK, zero npm deps (doc 12)
```

Extensions live in their own packages (`internal/issues`, `internal/pulls`, …) and depend **only on seam interfaces plus frozen types** — never on each other's internals.

## 14.2 The core/extensible boundary

### Frozen contracts (CORE — changing one is a spec revision, never a patch)

| Contract | Source | Why frozen |
|---|---|---|
| Bucket key layout `repos/<o>/<r>/…` and bucket-root keys | §3.1, §5 | Rust/walhub buckets must interoperate |
| `manifest.pb` protobuf + CAS semantics (the only commit point) | §3.2, P2 | Every correctness proof stands on it |
| WAL `LogEntry` kinds: PUSH, COMPACT, REF_UPDATE, CHECKPOINT, SETTINGS; append-only proto, field numbers frozen | §21, P2/P10 | Replay, checkpoints, burn, events all parse it |
| Overwritable-object list: `manifest.pb`, `bundles/list.pb`, `leases/*`, `maintain/<host>.pb`, `events/cursor(s)/…`, `policy.json`, `fsck.pb`, render cache, the identity families (`users/*/profile.json`, `users/*/invitations/index.json`, `orgs/*/org.json`, `orgs/*/members.json`, `orgs/*/teams/*.json`, `repos/<o>/<r>/access.json` — Wave A amendment, see Decisions), the pulls families (`repos/<o>/<r>/pulls/<num>/pr.json`, `repos/<o>/<r>/pulls/<num>/mergeable.json`, `repos/<o>/<r>/meta/forks.json` — Wave C1 amendment, see Decisions; `repos/<o>/<r>/fork.json` is Create-once-then-CAS'd provenance in the same family), the checks families (`repos/<o>/<r>/checks/<sha>/<context>.json` — Create-then-CAS status records, `repos/<o>/<r>/checks/index.json` — CAS'd P4 projection, `repos/<o>/<r>/meta/ci_tokens/<id>.json` — CAS'd token records, revoked retained — Wave 05 amendment, see Decisions) | P2 | Everything else is `Create`-only, forever |
| Git wire protocol v0/v2, receive-pack behavior, 401 semantics | §7, §8.4 | git is the client; we do not fork it |
| Principal `{name, write, admin, anonymous}`; admin independent of write; auth modes `none`/`token`/`oidc` decision tree | §8.8 | Every route's authz reasoning assumes it |
| API conventions: plain-text errors, arrays `[]` never null, RFC 3339, two lanes `/api` + `/api-browser` (same handlers), SSE envelope, cache classes | §9 | SDK and edge depend on them |
| `policy.json` envelope: `version`, `groups`, ordered `rules`, effect-tagged-union, fail-closed, unknown keys *inside* a rule/effect are parse errors | §14 | A typo must never become an allow |
| Event schema (`action`, `ref_type`, `ref_name`, `old`, `new`, `pusher`, `correlation_id`, `_walgit`) and dedup key `(repo, _walgit.seq, ref_name)` | §12.1–12.2 | Consumers already key on it |
| TaskRecord shape, `(repo, kind)` join semantics, packet stream, drain behavior | §6.8 | Narration is a product feature (P9) |
| Dependency budget: stdlib + `golang.org/x/net` + `github.com/BurntSushi/toml` only | context | The whole point of the rewrite |

### Extensible seams (EXTENSIBLE — register, do not modify)

| # | Seam | Package | Extends by |
|---|---|---|---|
| 1 | Route providers | `internal/api` | new HTTP endpoints, both lanes |
| 2 | Auth providers | `internal/server/auth` | new principal sources (OAuth apps, LDAP, SSO) |
| 3 | Policy effects | `internal/policy` | new `effect` kinds in `policy.json` |
| 4 | Event sinks | `internal/events` | new consumers of WAL events, per-sink cursor |
| 5 | Task kinds / ops | `internal/maintain` | new long-running narrated units |
| 6 | Store backends | `internal/store` | new bucket providers |
| 7 | CLI subcommands | `cmd/walhub` | new operator verbs |

A seam change ships with the core (compiled-in registries, not plugins). There is no dynamic plugin loading, no cgo, no embedded interpreters — a registry entry is a Go value added in one `Register*` call.

## 14.3 Seam 1 — HTTP route registration (`api.RouteProvider`)

The server is one chi router (divergence D1; `06_server_http.md` §3.1) with the shared fallback that
hand-parses `/{owner}/{repo}[.git]/<sub>`. Core registers its routes; extensions register through the same
mechanism, so an issue page and `refs` are peers at the router. Chi core is the router; middleware stays
hand-rolled (dependency policy).

```go
// internal/api/env.go — the shared state every handler gets; constructed once at startup (§8.10 order).
type Env struct {
    Store   store.Backend        // bucket access
    Repos   *store.RepoRegistry  // owner/repo listing + handles
    Auth    *auth.Chain          // Seam 2
    Policy  *policy.Loader       // per-repo policy.json, fail-closed
    Tasks   *maintain.Table      // Seam 5: running/recent/by-id, SSE attach
    Events  *events.Hub          // Seam 4: sinks + notify endpoint
    Cfg     *config.Config
    Version string
}

// internal/api/router.go
type RouteProvider interface {
    // Name is used in startup logs, the /api/v1 discovery `endpoints[]` provenance, and metrics.
    Name() string
    // Register receives the chi router and shared env. It MUST register on BOTH lanes where the
    // route is repo-scoped: use api.Lanes(r, env, pattern, handler) which mounts
    // "/{owner}/{repo}/api/..." and "/{owner}/{repo}/api-browser/..." to the same handler.
    Register(r chi.Router, env *Env) error
}

// Core + extensions, in a fixed order:
func Build(env *Env, extras ...RouteProvider) (chi.Router, error) {
    r := chi.NewRouter()
    for _, p := range append(coreProviders(), extras...) {
        if err := p.Register(r, env); err != nil { return nil, err }
    }
    return r, nil
}
```

Rules for route providers:

- Registration order is deterministic; duplicate route registrations are a startup failure (chi panics on
  double-mount of an identical method+pattern), never a runtime overwrite.
- Handlers authenticate **in the handler** per §8.1 (only the gated sub-router uses `require_auth` middleware); a provider uses `env.Auth.RequireRead/Write/Admin(r)` — the 401/403/503 mapping is frozen.
- Repo-scoped extensions MUST go under `/{owner}/{repo}/(api|api-browser)/…` and get the two-lane mount, SWR/ETag or no-store cache classification per §9.2, and SSE via the envelope helpers (§9.3) when work can be long.
- New top-level path families (outside `/{o}/{r}` and `/api*`) require a spec note; the repository prefix is the only routing key.

```go
// Extension example (internal/issues/routes.go)
func (P) Register(r chi.Router, env *api.Env) error {
    h := &handlers{env: env}
    return api.Lanes(r, env, "/{owner}/{repo}/issues", h.list) /* also mounts /{o}/{r}/issues/{id}/... */
}
```

### Concurrency

Hazard: a provider's handler blocking a request goroutine on bulk work (bucket fetches, merges) starves the control plane (§8.1/§19 incident rule). Avoidance: handlers do one manifest conditional GET + short bucket ops inline; anything long is a task (Seam 5) with SSE attach; git subprocesses run under the bounded pool, never on the serve path. Ownership: `Build` owns the router for the process lifetime; no provider may mutate routes after startup (shutdown is server-wide, `http.Server.Shutdown` under context).

## 14.4 Seam 2 — authentication providers (`auth.Provider`)

The frozen Principal and the §8.8 decision tree stay. A provider is a **credential-resolution step** slotted into that tree, not a replacement for it:

```go
// internal/server/auth/provider.go
type Credential struct {
    Kind  CredentialKind // Bearer | Basic | SessionCookie | ForwardedPrincipal
    Token string         // bearer token, or Basic password (= the token)
    User  string         // Basic username (informational)
}

type Provider interface {
    // Claim is a cheap prefix/shape test; e.g. "wgt_", "ldap_", "gho_".
    // Two providers MUST NOT claim overlapping prefixes — startup validates this.
    Claim(c Credential) bool
    // Authenticate resolves a claimed credential.
    //   (principal, nil)            — authenticated
    //   (nil, ErrInvalid)           — bad credential → real 401 (git erases it; §8.4)
    //   (nil, ErrForbidden)         — valid but not allowed → 403
    //   (nil, ErrUnavailable)       — upstream down → 503 + Retry-After: 15
    Authenticate(ctx context.Context, c Credential) (*Principal, error)
    Routes() api.RouteProvider // optional: login/callback pages (e.g. /_auth/* twins)
}
```

Resolution order (normative, preserves §8.8): edge-forwarded credential resolution → static tokens (`token_env` aware) → `wgt_` HMAC tokens → session cookies → **registered providers in registration order** → anonymous. The provider never sees credentials claimed by an earlier step. Every 401 still carries `WWW-Authenticate: Bearer realm="walgit"`; providers return errors, they do not write responses.

Registration (in `internal/server/auth/registry.go`):

```go
func Register(p Provider)            // compiled-in; panics on duplicate Claim at startup
func ChainFromConfig(cfg *config.Config) *Chain
```

Worked examples:

- **OAuth app** (e.g. a Git forge): provider claims `gho_`-style tokens, exchanges/verifies them against the issuer, maps identity to `{name: email, write, admin}` via config lists (`allowed_domains`, `admin_emails` — the §8.8 email-policy shape generalizes).
- **LDAP**: provider claims `ldap_` tokens (operator-issued static tokens that defer membership to the directory), resolves group membership with a 5 s context timeout and a 60 s in-process cache; directory down = `ErrUnavailable` (503), never a silent allow.
- **SSO/SAML or another OIDC issuer**: the built-in `oidc` mode already covers any OpenID Connect issuer (§8.8); a new provider is only needed for non-OIDC flows.

Operator configuration keeps TOML keys stable and namespaced:

```toml
[auth]
mode = "token"                      # frozen modes; provider additions extend, never rename
[auth.providers.ldap]
domain = "ldaps://ldap.corp.internal"
bind_dn = "cn=walhub,ou=svc,dc=corp,dc=internal"
token_env = "WALHUB_LDAP_BIND_PW"
write_groups = ["git-users"]
admin_groups = ["git-admins"]
```

### Concurrency

Hazard: a slow upstream (LDAP/JWKS refresh) serializing all authentications. Avoidance: providers cache verdicts per credential (bounded LRU, TTL ≤ 300 s, mirroring the edge's `/_auth/check` caching, §8.8) and MUST bound upstream calls with `context.WithTimeout` (10 s); JWKS-style key caches refresh stale-while-valid with single-flight (one refresh goroutine per key set, others join). Who owns what: the `Chain` owns provider lifetime and shutdown (context cancel at drain); a provider owns only its own caches.

## 14.5 Seam 3 — policy effects (`policy.Effect`)

`policy.json` stays the frozen envelope (§14.1–14.2). Effects are the tagged union inside a rule; the union is open for extension by registration:

```go
// internal/policy/effect.go
type Effect interface {
    Kind() string                       // "protect", "history", "size", … unique key
    Parse(raw json.RawMessage) error    // STRICT: unknown keys inside the effect = parse error (§14.1)
}

type Update struct {                    // the eval input, per update, at receive-pack step 5 (§14.4)
    Principal string
    Ref       string
    Op        string // create | update | delete | force-push
    // force-push is NOT an OID shape: server runs `merge-base --is-ancestor` after ingest (§14.3)
}

type Verdict struct{ Allow bool; Rule string } // deny names the rule on the wire: `rejected by rule '<name>'`

type PushEffect interface {             // effects that participate in push evaluation
    Effect
    Evaluate(u Update, g Groups) Verdict
}
```

Registry and load rules:

- `policy.RegisterEffect(e PushEffect)`; duplicate kinds are a startup panic. Unknown effect kinds in a loaded file are **parse errors** (fail closed: 400 on PUT, REJECT on next push — §14.4). Rolling-upgrade rule: deploy the new binary fleet-wide *before* any repo adopts the new effect, because an old binary fails closed (rejects) on files it cannot parse. This is the cost of open effects; it is accepted by design.
- Combination is decided by the EFFECT TYPE, never declared in the file (§14.1). A new effect documents its own combination rule in this doc family at registration time.
- The load-time check (two `protect` rules, same (ref, op), disjoint non-empty bypass → load fails) extends per-effect: each effect type declares an analogous compatibility check or states "no cross-rule interaction".

**The honest note (from the Rust policy design, §14.4):** receive-pack evaluates `(principal, ref, op, force?)` after ingest and can only enforce what is decidable there. It can enforce *where changes may land* and *who may land them*; it cannot enforce *process that happens elsewhere*. Concretely:

- A `review-required` effect can deny direct pushes to `refs/heads/main` for everyone except a listed bypass (the merge-queue bot). That IS enforceable: the bot lands the change as a normal push that passes the same rule.
- A `ci-required` effect can deny pushes whose commits lack a trailer/status the queue writes; it cannot see an external CI system. Enforce only the observable artifact.
- What is genuinely NOT enforceable at receive-pack (never promise it): "the PR had two approvals", "CI is green on the tip". Those live in the review objects (§14.10.4) and are enforced by *who is allowed to push the final ref*, not by policy magic.

Suggested additive effect (illustrative shape, same JSON style):

```json
{ "name": "no-direct-main", "match": { "refs": ["refs/heads/main"] },
  "effect": { "review-required": { "bypass": ["group:admins", "svc:merge-queue"] } } }
```

### Concurrency

Hazard: policy eval is on the push path; a network-backed effect (e.g. one that calls out to fetch review state) would add a blocking I/O to every push. Avoidance: push-effect `Evaluate` MUST be pure and local — it receives already-loaded data and returns in memory; anything external is resolved before eval (cached, refreshed by a background goroutine) or it is not a push effect. Load/parse happens under the per-repo policy cache's single-flight (one parse per manifest revision; concurrent pushes share the verdict set).

## 14.6 Seam 4 — event sinks (generalizing the webhook)

§12 is a one-sink bridge. walhub generalizes the *sink*, keeping the bridge loop frozen: events are produced FROM THE WAL (P3), catch-up is serialized per repo, delivery is at-least-once with the dedup key `(repo, _walgit.seq, ref_name)`, and the backfill contract (read the log from your last seq) always remains the correctness path.

```go
// internal/events/sink.go
type Event struct {
    Action, RefType, RefName string
    Old, New                 string // full zero OID of the other side's length; never ""
    Pusher, CorrelationID    string
    Repo                     string
    Walgit                   WalMeta // schema_version, seq (string), entry_kind, request_id
}

type Sink interface {
    Name() string                                   // stable; names the cursor object and metrics label
    Publish(ctx context.Context, batch []Event) error // one batch POST-equivalent; 10 s timeout expected
}

func Register(s Sink) // compiled-in
```

Per-sink durable cursor (additive generalization of `events/cursor.json`, §12.3 step 5):

- Key: `repos/<o>/<r>/events/cursors/<sink>.json`, same body `{"published_seq": N, "updated_at": RFC3339}`, CAS'd, in the frozen overwritable family (see §14.11 decision).
- Each sink catches up independently: a slow/failing webhook no longer holds back a second consumer, and one sink's failure aborts only its own catch-up before its own cursor CAS (at-least-once preserved per sink).
- Gap semantics per §12.3 are per sink: cursor below `readable_from` counts `events_bridge_gap_total{repo,sink}` and warns; it is never silently repaired.

Configuration stays declarative (TOML keys extend the `[events]` section; names preserved):

```toml
[events]
sweep_interval = "5m"
[[events.sinks]]
name    = "ci-webhook"
kind    = "webhook"                    # the §12.2 POST shape, unchanged
url     = "https://ci.internal/hook"
secret_env = "WALHUB_HOOK_SECRET"
[[events.sinks]]
name    = "audit-log"
kind    = "jsonl"                      # append Event JSON lines to a bucket object family
prefix  = "audit/events"               # objects audit/events/<repo>/<yyyymmdd>.jsonl
```

Built-in sink kinds at launch: `webhook` (identical to §12.2 wire behavior, including `X-Walgit-Delivery` and HMAC `X-Walgit-Signature`) and `jsonl` (bucket-side append-only audit trail). Everything else is registered by extension packages.

### Concurrency

Hazards and avoidance (canonical playbook: `13_concurrency.md`):

- One catch-up goroutine per repo (single-flight keyed by repo — concurrent wake-ups join, never duplicate work; §12.3's "serialized process-wide" becomes "serialized per repo," which is safe because events carry seq and the cursor CAS arbitrates).
- Sinks publish sequentially within one catch-up batch, each with its own `context.WithTimeout` (10 s); a sink error stops only that sink's advancement. **The publisher (catch-up loop) owns and closes the batch slice; sinks MUST NOT retain it** beyond the call (copy if async).
- Wake-ups (`POST /_events/notify`, sweep) are idempotent and only ever call catch-up; notify handling never blocks on publish completion beyond one batch (503 on sink failure so the notifier redelivers, §12.3).
- No unbounded buffering: batches are materialized from `read_log` windows, not queued in memory; the sweep interval bounds worst-case lag.

## 14.7 Seam 5 — task kinds and ops registry

Every long thing is a narrated task (P9, §6.8). The TaskRecord, packet stream, `(repo, kind)` join, replay buffer, and drain hooks are frozen. Extension adds kinds:

```go
// internal/maintain/task.go
type Kind struct {
    Name string // e.g. "merge", "issue-reindex"; unique across core + extensions
    // Run executes the unit. ctx is cancelled by drain phase 1 (record failure 503 "interrupted…").
    // params come from POST …/ops/{op}?params (or internal callers).
    Run func(ctx context.Context, t *Task, env *api.Env, params url.Values) (any, error)
    // Ops marks it user-startable via POST {lane}/ops/{op}; OpSpec metadata for GET …/ops.
    Ops *OpSpec
}

func RegisterKind(k Kind) // panics on duplicate name
```

The maintainer loop picks units from the registry (core units per §13.1 first, then extension kinds under the same bounded one-unit-per-repo-per-pass discipline). `GET {lane}/ops` lists available ops from the registry; `POST {lane}/ops/{op}` starts/joins by `(repo, kind)` exactly as §9.4. Extension tasks get instance-memory records + leases for cross-instance exclusivity, like all tasks.

### Concurrency

Hazard: an extension kind hogging the maintainer pass or colliding with a core unit on the same repo. Avoidance: the loop runs at most one unit per repo per pass (core before extension); a kind that needs cross-instance exclusion takes a `leases/<kind>-<repo>.pb` lease (CAS+TTL) instead of inventing its own mutex; a kind that spawns helpers MUST derive all goroutines from its task `ctx` (drain cancels everything; no detached goroutines). The task table owns the `(repo,kind)` join; kinds never implement their own dedup.

## 14.8 Seam 6 — storage backends registry

The store contract (§4.1) is the seam; S3-compatible, GCS (JSON API over plain HTTPS — no gRPC client, per the dependency policy), and in-memory (tests) ship in core. One contract suite runs against every backend (§1.3).

```go
// internal/store/backend.go
type Backend interface { /* §4.1: Get/Put(Create|Update(version))/Delete/Compose/List(off-hot-path)/… */ }

func Register(name string, open func(ctx context.Context, cfg config.Store) (Backend, error)) // "s3","gcs","memory",…
```

The bulk/control-plane transport split (§4.2) is part of the contract: a backend that cannot split transports emulates it internally (separate connection pools, separate bounded channels), because the queue-depth metrics and the no-LIST-on-hot-path rule assume it. New backends MUST pass the contract suite including CAS precondition tests and the ambiguous-CAS re-read rule (§3.2).

### Concurrency

Hazard: unbounded range-read fan-out against one backend connection limit. Avoidance: the bulk pool is bounded (dedicated worker pool + permits, §19 mapping); backends advertise their concurrency ceiling in config and the pool respects it; no request goroutine ever issues a bulk read directly.

## 14.9 Seam 7 — CLI subcommands

`clap`-equivalent without dependencies: a hand-rolled dispatcher (stdlib `flag` per subcommand is sufficient; keep exit codes 0/1/2/3 and `--config` semantics, §19).

```go
// cmd/walhub/sub.go
type Subcommand struct {
    Name string              // "serve", "repo", "issues", …
    Summary string
    Run func(ctx context.Context, args []string, cfg *config.Config) error
}
func Register(s Subcommand)  // sorted help output; "walhub help" prints the registry
```

Core registers the §1.3 CLI surface (renamed to `walhub …`); extensions add verbs (e.g. `walhub issues import`). Subcommands that produce writes go through the same publish/CAS path as the server — the CLI is never a second writer implementation.

## 14.10 Roadmap: GitHub-like features mapped to the seams

### 14.10.1 Multi-user repositories and per-repo permissions — no core change

Principals (Seam 2) and `groups` rosters (§14.1) already exist. Per-repo ACL = **policy.json groups + rules only**:

```json
{ "version": 1,
  "groups": [
    { "name": "acme-monorepo-write",  "members": ["alice@example.com", "@okta:platform", "group:bots"] },
    { "name": "acme-monorepo-robots", "members": ["svc:ci"] }
  ],
  "rules": [
    { "name": "monorepo-writers-only",
      "match": { "refs": ["refs/heads/**", "refs/tags/**"],
                 "principals": ["^group:acme-monorepo-write"] },
      "effect": { "protect": { "restricts": ["create", "update", "delete", "force-push"] } } }
  ] }
```

`^`-exclusion on principals is legal here because `protect` is most-restrictive (§14.2). What changes for the product: a repo-generation convention (owner-scoped policy templates applied by `walhub repo create --policy-from <file>`, Seam 7) — no engine change. Read-side visibility (private repos) stays the global `anonymous_read` lever; per-repo read ACLs would need a `require_read` hook and are explicitly deferred, not smuggled into policy (policy evaluates pushes only — §14.4).

### 14.10.2 Issues — sidecar objects, never the WAL

Principle: the WAL is git ref history. Issues are a **separate durable state family** with their own CAS index, so a broken issues subsystem can never corrupt git history (and vice versa). They follow P1 (bucket only), P2 (CAS commit point for the index; immutable Create for everything else), P4 (reads revalidate), P7 (probe, don't list).

Data model (bucket keys under `repos/<o>/<r>/`):

| Key | Kind | Content |
|---|---|---|
| `issues/index.json` | overwritable, CAS'd (joins the frozen overwritable family, §14.11) | `{"version":1,"next_id":42,"next_seq":1007,"issues":{"1":{"title","state":"open|closed","created_at","updated_at","labels":[]}}}` — small; full issue state projection lives here |
| `issues/<id>/thread/<seq>.json` | immutable, Create-only | one thread event: `{"seq","at","by","kind":"comment|close|reopen|label|title","body?","labels?"}` — append-only thread |
| `issues/index.json.lock`-equivalent | none | none — CAS *is* the lock |

Write path (one comment): (1) render event JSON; (2) PUT `issues/<id>/thread/<n+1>.json` **Create** (if it exists, we already lost a race or retried — treat as done, idempotent); (3) CAS `index.json` bumping `next_seq` (and `next_id` for new issues) with retry-on-412 re-read. Crash between (2) and (3) leaves an orphan thread object — harmless and swept by the issues reindex unit (Seam 5), exactly the orphan/burn philosophy of §6.4. Issue IDs are integers from `next_id`; issue creation and comment write are two CAS round trips — fine at human volume (P7 honored: index GET is a probe, thread listing walks only that issue's keys by prefix-free exact-name construction... note: enumerating a repo's issues uses `index.json` itself, so no LIST on hot paths).

API (Seam 1, both lanes; caching: `no-store` for index/thread lists — they are ref-independent so §9.2's classes don't apply; ETag on index via CAS version):

```
GET    /{o}/{r}/api/issues?state=&n=&after=      → {issues:[…], more}
POST   /{o}/{r}/api/issues                        → create (write)  {id, …}
GET    /{o}/{r}/api/issues/{id}                   → {issue, thread:[…]}
POST   /{o}/{r}/api/issues/{id}/comments          → append thread event {seq}
POST   /{o}/{r}/api/issues/{id}/{close|reopen}    → append thread event
GET    /{o}/{r}/api/issues/{id}/stream            → SSE: `issue` packets on change + keepalive
```

SSE for comments reuses the §9.3 envelope with event name `issue`; the stream is fed by the per-repo issues watcher (a goroutine with the manifest-revision-style freshness check on `index.json`'s CAS version, keepalive every 10 s, terminal `error` packet on loss). Events (Seam 4) MAY publish `issue` actions to sinks, but they are produced by the issues writer *after* its own CAS — mirroring P3's shape without touching WAL kinds.

### 14.10.3 Pull requests — DESIGN SKETCH, not a contract

PRs are **convention over machinery**: they live in the ordinary ref namespace, so bundles, events, policy, fsck, and replication work on them for free.

- Refs: `refs/heads/pull/<n>/head` (the contributor's tip) and `refs/heads/pull/<n>/merge` (the queue's computed merge result). Creating a PR = pushing a `pull/<n>/head` ref through normal receive-pack (policy applies!).
- Merge computation = a maintainer unit (Seam 5, kind `merge`): lease `leases/merge-pull-<n>.pb`, materialize, `git merge`, connectivity check, then publish `refs/heads/pull/<n>/merge` as a normal PUSH log entry (WAL-covered, event-emitting).
- Landing = the merge-queue principal (a `svc:` bot from Seam 2) pushes `refs/heads/main` to the computed merge sha; the `review-required` effect (Seam 3) denies everyone else. The queue's failure mode is a stalled bot, never a corrupted repo.
- Fork networks, merge queues as products, review state machines: NOT designed here. This paragraph exists so nobody "temporarily" adds a PR kind to the WAL.

### 14.10.4 Review/approvals — DESIGN SKETCH, not a contract

Approvals are issues-style objects (§14.10.2 model) attached to a PR: `repos/<o>/<r>/pulls/<n>/reviews/<seq>.json` (immutable events: approve/request-changes/comment) with a CAS'd per-PR index. Enforcement stays honest (§14.5): policy checks *who may push the final ref*; the review objects are the record the queue consults before it pushes. No core involvement beyond the seams.

## 14.11 What a feature MUST NOT do

Any violation is a bug even if tests pass:

1. **Never write WAL entries of a new kind without a schema revision.** WAL kinds are a closed enum in append-only protobuf; adding one changes the replay contract for every reader and requires a versioned schema change in core. Extensions NEVER add WAL kinds — that is what sidecar state families (§14.10.2) and events (Seam 4) are for.
2. **Never bypass the manifest CAS.** No feature may make a ref visible, mutate repo state, or publish settings by any route other than the frozen commit points (P2). A feature that needs an overwritable key MUST add its key family to the frozen overwritable list in a spec revision (as this document does for `events/cursors/<sink>.json` and `issues/index.json`), never write an unprotected mutable object.
3. **Never add dependencies beyond the budget** (`golang.org/x/net`, `github.com/BurntSushi/toml`). Hand-roll per the policy: SigV4, GCS JSON API, Prometheus text, SSE, JWT/JWKS, singleflight, LRU — each already spec'd in its own doc.
4. **Never put feature state on local disk.** Instance disk/memory are caches (P1). No local queues, flag files, sqlite, or env-encoded state. Durable = bucket, or it does not exist.
5. **Never block a request goroutine on bulk work; never leave a goroutine without a shutdown path.** Tasks, bounded pools, context deadlines — see `13_concurrency.md`.
6. **Never LIST on a hot path** and never share bulk bytes with the control-plane lane (P6/P7, §4.2).
7. **Never parse a policy/rule/effect leniently.** Unknown keys inside a rule/effect are parse errors; fail closed (§14.1/§14.4).
8. **Never fork the git wire protocol or the event schema.** Additive fields follow §14.12; semantic changes are new versions.

## 14.12 Versioning rules for additive API changes

- **Two-lane rule (D27):** every new repo-scoped endpoint is registered on BOTH `/api` and `/api-browser` lanes by `api.Lanes`; a lane-only endpoint is a bug. Non-repo endpoints get their `/api/v1` + `/api-browser/v1` twins the same way.
- **Lane segment rule (D15/D20):** additive endpoints extend the CURRENT version prefix (`/api/v1/…`) and MUST be added to the discovery document's `endpoints[]` (§9.6) so the SDK and UI can enumerate capabilities. Breaking changes (removing/renaming/retyping a field, changing a status mapping, changing an SSE event's terminal semantics) require a NEW prefix (`/api/v2` or a new lane segment) served alongside v1; v1 is never edited in place.
- **Field-level additive changes** to existing JSON responses are always allowed: new optional fields only; consumers ignore unknown fields; arrays stay `[]` when empty; uint64s travel as strings (§12.1 convention).
- **SSE:** new event names are additive; existing event payloads follow the field rule; the terminal contract (exactly one of `result`/`error`) is frozen.
- **Bucket formats:** protobuf append-only (field numbers frozen, new optional fields get new numbers); JSON objects gain optional fields only; `version` integers bump only when a reader must be able to refuse, and readers refuse unknown versions (§14.1 policy rule is the model).
- **Config:** new `[section]`/keys are additive; existing keys never change meaning; unknown keys are validated per-section (all-or-nothing replacement semantics preserved, §15).

## 14.13 Operator walkthrough: adding a capability end to end

Adding "issues" to a running fleet touches zero core files:

1. Ship the binary with `internal/issues` compiled in (its `RouteProvider`, `Sink`, task kinds registered via `init()`).
2. Enable per-sink events if desired:

```toml
[[events.sinks]]
name = "issues-to-chat"
kind = "webhook"
url = "https://chat.example/hook"
secret_env = "WALHUB_ISSUES_HOOK_SECRET"
```

3. Users create and watch issues:

```bash
curl -X POST -H "Authorization: Bearer $WALHUB_TOKEN" \
  -d '{"title":"flaky test in ci","body":"see run 1234"}' \
  https://walhub.example/acme/monorepo/api/issues
curl -N -H "Authorization: Bearer $WALHUB_TOKEN" \
  https://walhub.example/acme/monorepo/api-browser/issues/1/stream
```

4. Roll back by disabling routes (config flag on the extension) — the bucket objects remain, well-formed and inert.

## Decisions & deviations from the Rust design

- **New document, no Rust counterpart:** the Rust codebase hard-codes its webhook sink, three auth modes, and fixed ops; walhub makes the same frozen contracts but registers everything else. Rationale: the user's modularity requirement, without weakening a single frozen contract.
- **`events/cursor.json` generalizes to `events/cursors/<sink>.json`** (one per sink, same body shape). Rationale: a slow sink must not hold back others; at-least-once and dedup semantics are unchanged, and the key family is explicitly added to the frozen overwritable list here.
- **Bridge serialization weakens from process-wide to per-repo** (single-flight per repo). Rationale: seq-ordered events plus the CAS'd cursor make cross-repo ordering meaningless (§12.2 already says "nothing across repos"); per-repo parallelism is free correctness-preserving concurrency.
- **Auth is a provider chain instead of a closed three-mode switch.** Rationale: `none`/`token`/`oidc` semantics are frozen as the first chain members; LDAP/OAuth/SSO become registrations. Resolution order preserves the exact §8.8 decision tree and 401 semantics.
- **Policy effects are an open registry; `review-required`/`ci-required` are additive and honestly scoped.** Rationale per the Rust policy doc: receive-pack only enforces what it can observe (who/where/shape); the enforceable form is "deny direct pushes except listed bypass," never "external CI is green."
- **Issues/PRs/reviews use sidecar state families, never WAL kinds.** Rationale: WAL kinds are a closed schema (frozen contract #2); the orphan-retry pattern from §6.4 is reused for issues so a crash never corrupts either family.
- **PRs are a ref-namespace convention (`refs/heads/pull/<n>/{head,merge}`) computed by a maintainer unit, marked DESIGN SKETCH.** Rationale: refs get bundles/events/policy/fsck for free; anything richer would drag merge-queue machinery into core, which the Rust spec explicitly excludes.
- **Issues index/PR review indexes added to the frozen overwritable-key list** (`issues/index.json`, `pulls/<n>/index.json` if adopted). Rationale: P2 requires overwritable objects to be enumerated; an unlisted mutable key would violate the invariant silently.
- **GCS stays JSON-API-over-HTTPS (no gRPC client) even though the Rust implementation is a gRPC/JSON hybrid.** Rationale: dependency budget; the store contract and bulk/control split are preserved inside the backend.
- **Route/auth/task/CLI registries are compiled-in, not plugins.** Rationale: no dynamic loading, no cgo, no reflection-based DI — keeps the binary auditable and the dependency budget intact.
- **Per-repo private-read ACLs are explicitly deferred** (read gating stays the global `anonymous_read` lever). Rationale: policy evaluates pushes only (§14.4); bolting read checks onto the policy engine would silently change its contract. The seam for it (a `require_read` hook) is named but not spec'd.
- **Wave A identity amendment (2026-09-04, docs/features/01):** the frozen overwritable-key list gains the identity families — `users/<principal>/profile.json`, `users/<principal>/invitations/index.json`, `orgs/<org>/org.json`, `orgs/<org>/members.json`, `orgs/<org>/teams/<slug>.json`, `repos/<o>/<r>/access.json` (14 §14.11 rule 2, in the same revision that adopts the feature). Invitation objects (`orgs/<org>/invitations/<id>.json`, `repos/<o>/<r>/meta/invitations/<id>.json`) are Create-only immutable, delete-on-transition — not overwritable, no listing needed. The named-but-unspec'd `require_read` hook is now specified and implemented (01 §4.1): `env.Auth.RequireRead` consults the registered read gate after principal resolution; policy.json stays push-only. Two production notes the implementation records: (a) Seam 1 in code is the `server.RouteProvider` chain (`ChainAPI`), not a chi `Register` — feature surfaces implement `Handle(w,r) bool` and front the core mux on both lanes; (b) the api seam now authenticates + injects the principal before dispatch (previously mode-defaults), so token-carrying API calls resolve their configured rights. Rationale: read gating without principal resolution is decoration; the injection closes the gap with no change to anonymous behavior.
- **Wave C1 pulls amendment (2026-09-04, docs/features/03):** the frozen overwritable-key list gains the pulls families — `repos/<o>/<r>/pulls/<num>/pr.json` (CAS'd sidecar), `repos/<o>/<r>/pulls/<num>/mergeable.json` (Create-then-CAS derived cache), `repos/<o>/<r>/meta/forks.json` (CAS'd parent index) — plus fork-side `repos/<o2>/<r2>/fork.json` (Create-once-then-CAS'd provenance) in the same family (14 §14.11 rule 2, in the same revision that adopts the feature). The shared `issues/index.json` / `meta/next_num` / thread keys are NOT new families (02 owns them; 03 reads/writes the same objects). The `refs/pull/**` server-managed namespace is enforced in the core push pipeline (`internal/git.IsManagedRef` + the server's pushPipeline refusal, both transports) with NO feature import — a pure namespace predicate, the same shape as any future managed namespace. Rationale: receive-pack must refuse PR refs even when no feature package is consulted; the push funnel is the one place every client write traverses.
- **Wave C2 review amendment (2026-09-04, docs/features/04):** the frozen overwritable-key list gains the review families — `repos/<o>/<r>/pulls/<num>/review-requests.json` (CAS'd current-state index) and `repos/<o>/<r>/pulls/<num>/threads/<tid>/thread.json` (CAS'd per-thread headers) in the same revision that adopts the feature (14 §14.11 rule 2). Review events (`reviews/<seq>.json`) and thread comment events are Create-only immutable — not overwritable, no listing needed. The `next_review_seq` / `next_thread_num` / `review_summary` fields ride the EXISTING shared `issues/<num>/thread.json` header as additive optional fields (14 §14.12 field rule) — no new header family, and 03's header writes round-trip them opaquely (`pulls.Thread` carries them as preserved-but-uninterpreted state). The `required-reviews` effect registers via `policy.RegisterEffect` (Seam 3) with its type in `internal/policy` beside its siblings (push-path matching uses the package-private glob/actor law, so the type lives where the law lives); the merge-time half is 04's event scan, consulted by 03's merge task through pulls' `ReviewGate` seam — the merge logic is NOT forked, the gate is one check at the step-4 call site. 04 registers NO task kinds (the only long work in its graph is 03's `pull-merge`), NO event-sink cursor, and no discovery entries (the `/api/v1` `endpoints[]` derive from the core route table only — 01/02/03/C2 feature routes are absent there alike; the SDK enumerates them statically). Rationale: additive header fields keep one CAS arbitration point for review/thread allocation instead of inventing a second counter object; the effect type follows the existing effect code (strict parse, push-pure eval) while the unobservable half stays where the state is observable.
- **Feature 06 notifications amendment (2026-09-04, docs/features/06):** the frozen overwritable-key list gains the notify families — `users/<principal>/notifications/index.json` (CAS'd P4 unread hot window), `users/<principal>/notifications/<id>.json` (Create-only, CAS state-flip read/unread, delete-on-retention), `repos/<o>/<r>/meta/collab_state.json` (CAS'd activity seq allocator), `repos/<o>/<r>/webhooks/<id>.json` (CAS'd hook config), `repos/<o>/<r>/webhooks/cursors/<id>.json` (CAS'd per-hook cursor, the direct analog of the per-sink `events/cursors/<sink>.json` family proposed here for the frozen list: `{"published_seq": N, "updated_at": RFC3339}`, one slow webhook never stalls another), `repos/<o>/<r>/webhooks/<id>/deliveries/recent.json` (CAS'd last-25 ring) — in the same revision that adopts the feature (14 §14.11 rule 2). Activity events (`collab-events/<seq>.json`) and watch records (`users/<principal>/watching/<o>/<r>.json`) are Create-only immutable, delete-on-transition — not overwritable, no listing needed. `social.json` (`repos/<o>/<r>/meta/social.json`, CAS'd counters + `watcher_list`) is WRITTEN here but OWNED by 07: the 07 wave adopts the family and must preserve the `watcher_list`/`watchers_truncated` fields 06 consumes. 06 registers three task kinds (`webhooks` per-repo delivery loop via sweep + wake-up, `notify-fanout` overflow drain, `notify-retention` global pass) as in-process (repo,kind) single-flight + background sweeps started by composition (the events-bridge pattern), NOT in the wal task table or the maintainer registry — no core touch. Seam 1 in code is the `server.ExtraRoutes` chain (`Handle(w,r) bool`, both lanes, top-level twins + repo lanes). No discovery entries (same rule as 01/02/03/C2/05). Rationale: per-hook cursors are the same isolation shape as per-sink cursors (a failed/slow consumer holds back only itself); the activity log is the webhook unit AND the fan-out backfill source, so its seq allocator and cursors need the same CAS protection as the families they mirror.
- **Wave 05 checks amendment (2026-09-04, docs/features/05):** the frozen overwritable-key list gains the checks families — `repos/<o>/<r>/checks/<sha>/<context>.json` (Create-then-CAS status records, last-write-wins, never deleted), `repos/<o>/<r>/checks/index.json` (CAS'd P4 hot-window projection, newest 500 shas, inline compaction past 256 KiB), `repos/<o>/<r>/meta/ci_tokens/<id>.json` (CAS'd token records; hash-only, revoked retained) — in the same revision that adopts the feature (14 §14.11 rule 2). `require_checks` is NOT a new effect kind: it is an optional strict field inside the existing `protect` effect (1–32 validated context names, `policy.ValidCheckContext` — one grammar shared with the writer), parsed fail-closed but never evaluated on the push path (direct pushes are not gated; the merge task owns the verdict). Combination across matching rules is union over rules the merger does not bypass (03 §5 step 4: bypass lists apply unchanged; 05's "no bypass list" means the field carries no bypass of its own). Seam 2 in code is the `AuthService.ExtraCredential` hook (a func consulted after static tokens in `token`/`oidc` modes — the chain resolves the wct_ SHAPE to an unprivileged `ci:<id>` principal; the secret is verified handler-side per repo, so the chain never blocks on store I/O and `Evaluate` stays pure). Startup panics on prefix overlap (`checks.AssertPrefixDisjoint`, called from composition). The merge-time half is 05's stored-combined-view read, consulted by 03's merge task through pulls' `ChecksGate` seam — the merge logic is NOT forked, the gate is one check at the step-4 call site next to the 04 gate; nil backend fails closed only when a rule actually carries the gate. 05 registers one task kind (`checks-index-compact`, inline-triggered like 02's). No discovery entries (same rule as 01/02/03/C2: `/api/v1` `endpoints[]` derive from the core route table only). Rationale: the gate belongs where the merge decides (statuses cannot exist at receive-pack time — the 14.5 honest note); the hook keeps the frozen `Principal` untouched while scoping a leaked CI token to one repo and one capability.
