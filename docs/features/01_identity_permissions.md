# 01 — Identity & Permissions: users, orgs, teams, repo roles, invitations

> Phase A feature (the one everything else binds to). Package: `internal/identity` — registers a
> RouteProvider (Seam 1), consults auth providers (Seam 2), amends policy group resolution (Seam 3),
> and registers one task kind (Seam 5). Depends on nothing in this directory; siblings reference it.
> Shared primitives (P1–P9), wire conventions (07), lock rules (13 §2/§5), and seams (14) are normative
> here by reference and are not restated.

## 1. Scope and seams

Identity and permissions is the substrate: every other feature doc resolves "who may do this" through
the objects defined here. It introduces **one new package** (`internal/identity`) and **no new core
package**. It registers:

| Seam | What registers |
|---|---|
| 1 — RouteProvider | every endpoint in §8, on both lanes (`api.Lanes` for repo-scoped) |
| 2 — none | principals stay minted by auth providers; this package never authenticates (§2) |
| 3 — policy effect/group sources | `team:`/`role:` group-member expansion (§6) |
| 5 — task kind | `access-bootstrap` migration (§10) |
| 7 — CLI | `walhub access get/put` (thin store client over the same CAS path) |

The overwritable-key families introduced here (`access.json`, profile/org/members/team objects,
invitation objects, the invitation inbox index) MUST be added to the frozen overwritable list in the
same spec revision that adopts this doc (14 §14.11 rule 2). Everything else in this doc is
`Create`-only or immutable.

## 2. Principals vs user profiles

A **principal** is what authenticates: the frozen `Principal{name, write, admin, anonymous}` (06 §8.1),
with `name` being an **email** minted by an auth provider (static-token mapping, `wgt_` token from
email, OIDC ID token — 06 §8.8). Principals are NOT objects; there is no bucket key for a principal
itself. Auth-provider territory — and therefore explicitly OUT of this doc's object families — is:
SSH keys, tokens (`wgt_` minting), sessions, JWKS. Those live with the auth provider (Seam 2) and the
`/_auth/*` surface; nothing here reads or writes them.

A **user profile** is a bucket object keyed by the principal name:

| Key | Kind | Schema |
|---|---|---|
| `users/<principal>/profile.json` | CAS'd (overwritable family) | `{"version":1,"principal":"jane@example.com","display_name":"Jane Doe","bio":"","created_at":"RFC3339","updated_at":"RFC3339"}` (+ `avatar_content_type`/`avatar_updated_at` when the user holds a generated avatar, `avatar_disabled` when opted out — Forgejo #376, append-only) |
| `users/<username>/avatar.svg` | overwritable (Forgejo #376) | generated avatar bytes (`image/svg+xml`); pointer on profile.json, same bytes-first/pointer-second discipline as the #359 org avatar |
| `users/<principal>/invitations/index.json` | CAS'd (overwritable family) | inbox index, §7 |

Rules: `<principal>` is the lowercased email, percent-encoded per segment for keys with `@` → `%40`
keeping the one-segment rule. `profile.json` is created lazily: first authenticated request or first
role granted. GET on a missing profile is the canonical "does this principal exist" probe (06
notifications contract); there is deliberately no users LIST — enumeration of people is not a
feature. A principal's existence check is O(1) GET; profile updates are self-or-admin (`PUT` with CAS
`version`, 409 on mismatch).

## 3. Orgs, members, teams

All under `orgs/<org>/` per P1. `<org>` is a slug (lowercase `[a-z0-9-]`, 1–39 chars, validated like a
repo owner name). Org objects:

| Key | Create or CAS | Schema |
|---|---|---|
| `orgs/<org>/org.json` | **CAS'd** (overwritable family) | `{"version":1,"org":"acme","display_name":"Acme Corp","description":"","location":"","timezone":"","bio_markdown":"","avatar_content_type":"","avatar_updated_at":"","created_at","updated_at"}` (location/timezone/bio_markdown + avatar pointer are append-only, issue #359 — old readers ignore them, old writers omit them) |
| `orgs/<org>/avatar` | **overwritable** raw bytes (issue #359) | magic-sniffed PNG/JPEG/GIF/WebP, ≤ 2 MiB; object ContentType is the sniffed type; `org.json`'s `avatar_content_type` mirrors it as the render gate |
| `orgs/<org>/members.json` | **CAS'd** (overwritable family) | `{"version":2,"members":[{"principal":"jane@example.com","role":"owner","joined_at"},{"principal":"sam@example.com","role":"member","joined_at"}],"updated_at"}` |
| `orgs/<org>/teams/<slug>.json` | **CAS'd** (overwritable family) | `{"version":1,"org":"acme","slug":"platform","name":"Platform","description":"","members":["jane@example.com"],"created_at","updated_at"}` |

Write discipline:

- **Create** (immutable `PutMode::Create`) applies only to the *namespace*: creating an org is
  `Create` of `org.json` followed by `Create` of `members.json` binding the creator as owner.
  Creation is atomic-or-recoverable (#75): a failed members seed rolls the `org.json` reservation
  back (version-guarded, best-effort), and a retry on an ownerless reservation (members missing or
  ownerless — the crash-between-writes residue) completes the owner binding via CAS instead of
  409ing; a re-create by the already-bound owner is idempotent success. A lost race against a
  genuinely owned org still loses the org name — 409 "org already exists" — and the loser writes
  nothing. Creating a team is `Create` of its `teams/<slug>.json`. Everything else — profile edits,
  roster changes, membership — is a **CAS'd update** (`Update(version)`, retry-on-412 re-read; the
  canonical CAS loop, 13_concurrency §3). CAS is the lock; there is no separate lock object anywhere
  in this feature.
- **members.json is one object, human-rate.** Rosters are small (dozens), writes are rare (someone
  joins/leaves a team), so whole-roster CAS beats per-member objects: one GET answers the hot P6
  question ("is this principal an owner of `<org>`?") with no LIST. Contention is a non-issue at
  human rate (same reasoning as P2).
- Team membership is a plain string array of principals. Team listing is LIST over
  `orgs/<org>/teams/*.json` — collaboration page, not a git hot path (P5), paginated `n` default 100.
- Deleting a team or an org: team delete removes the object and its `team:<org>/<slug>` bindings from
  any `access.json` that references it (same handler, sequential CAS per affected repo — bounded by
  repos the team is bound to, discovered from `access.json` itself, never a LIST sweep of all repos).
  Org delete is owner-only and REFUSES while any repo is owned by the org (409 with the count —
  the message names transfer, which §3.1 provides, so the refusal always names a real next step).

### Concurrency

Hazard: two owners editing `members.json` concurrently lose an update if writes blind-PUT. Avoidance:
every mutation is CAS `Update(version)` with retry-on-412 re-read (13 §3 discipline; no new locks —
13 §2's lock list is unchanged, these are store CAS objects, not in-process locks). Hazard: org delete
racing a concurrent member grant — the org delete takes a CAS on `members.json` last and re-checks
roster emptiness inside the loop; membership objects are the source of truth, never an in-process set.

## 4. `access.json` — role bindings and resolution

One CAS'd object per repo: `repos/<o>/<r>/access.json` (P1 names it; it joins the overwritable family).

```json
{ "version": 7,
  "visibility": "public",
  "role_bindings": [
    { "subject": "user:jane@example.com", "role": "admin" },
    { "subject": "team:acme/platform",    "role": "write" }
  ],
  "updated_at": "2026-09-01T00:00:00Z" }
```

| Field | Type | Rules |
|---|---|---|
| `version` | integer | CAS token; PUTs must carry the version they read (409 otherwise) |
| `visibility` | `"public"\|"authenticated"\|"private"` (Forgejo #374) | gates anonymous reads (§4.1) — default `"public"` |
| `role_bindings` | array | empty `[]` allowed; subjects `user:<email>` or `team:<org>/<slug>`; roles `read|triage|write|maintain|admin` |

- **One binding per subject.** A PUT that carries duplicates for one subject is a 400 (plain-text error,
  07 §2). The array is stored sorted by subject so renders and diffs are stable.
- Roles are the P6 ladder `read < triage < write < maintain < admin`. A binding is the whole story for
  repo-scoped capability; there are no per-ref bindings here (that is `policy.json`'s job, §6).
- **Write discipline:** full-document replace via `PUT` with CAS. Read the object, apply the edit,
  CAS-write with the version just read; on 412 re-read and re-apply against the fresh version —
  last-writer-wins is NOT acceptable, the loop is bounded (≤ 5 attempts, then 409 to the client).
- **Who may write:** role `admin` at this repo, or host admin flag (P6). Same auth class as
  `PUT …/policy` and `PUT …/settings`.

**Resolution order** is P6 verbatim, with the team expansion made exact and the
Forgejo #374 visibility split: a `team:org/slug` binding
matches a principal iff the principal is in `orgs/<org>/teams/<slug>.json.members`. Resolution is
max-role across: (1) `access.json` bindings (direct + team), (2) the owning org's roster —
owners resolve `admin`, members resolve `read` (one `members.json` GET covers both; a non-org
owner reads as absent), (3) `authenticated` visibility grants any authenticated principal `read`,
(4) the auth principal's `admin` flag (the host-wide `write` flag grants NOTHING — the #347
direction extended to reads: it authenticates, never authorizes), (5) anonymous → `read` iff
`visibility == "public"`, nothing otherwise. First match in that list that yields ANY role wins;
within step 1 the max of matching bindings applies. Resolution result is memoized per-request; no
lock is involved — it is two bucket GETs worst case (`access.json`, one `teams/<slug>.json` per
referenced team, bounded by the binding list length; the roster GET replaces the old owner-only
probe, so no round trip is added).

### Concurrency

Hazard: read-modify-write of `access.json` losing a concurrent binding (two admins editing at once).
Avoidance: the CAS loop above (13 §3 primary tool) — no cross-feature locks, no lock object, no
`.lock` sidecar. Hazard: role check racing a demotion on the push path (a just-revoked writer pushes).
Avoidance: receive-pack evaluates policy against the `Update.Principal` resolved at request start,
which reads `access.json` fresh (conditional GET, control-plane transport, sub-second — the same class
as the sanctioned `freshenManifest` GET, 13 §2.2); a push in flight when bindings change completes
under the bindings it started with. This is accepted: revocation latency is one in-flight push, and
`access.json` staleness is bounded by the request, not a TTL.

### 4.1 Visibility and anonymous reads — the `require_read` integration

`visibility` lives on `access.json` (decided: NOT a repo flag elsewhere) because the object that
already carries the repo's permission model is the single place a reader consults, and because
`visibility` is CAS'd with the bindings that make it meaningful.

- Server `auth.anonymous_read` (06 §8.8) remains the **host-wide** lever: false means anonymous gets
  nothing anywhere, regardless of `visibility`. True means anonymous gets `read` **only where
  `visibility == "public"`** (`authenticated` and `private` both refuse anonymous with a real
  401 — law 9).
- `authenticated` ("private, logged in only", Forgejo #374) grants any authenticated principal
  `read` at resolution step 3 — the mode for user-owned repos that should stay off the anonymous
  internet without naming every reader. `private` ("visible only by owner/org") is owner(s),
  org members (org-owned repos — membership alone suffices, no binding needed), explicit
  bindings, and host admin; on a user-owned repo that is the owner binding plus explicit
  bindings plus host admin (fail closed — no binding, no read, even for the owner).
- A host `write`-flag-only outsider reads `public` and `authenticated` repos through visibility
  like any authenticated principal, and NOTHING on `private` repos (Forgejo #374 decision: the
  #347 push rule extended to reads). Host `admin` still passes everywhere.
- Enforcement point: the `require_read` hook named-but-not-spec'd in 14 §14.10.1 / D-EXT-1 is specified
  HERE. `env.Auth.RequireRead(r)` consults a registered read gate (this package) **after** principal
  resolution and before any handler body:
  1. authenticated principal → resolve per §4; role ≥ `read` (or public) → allow; else 403.
  2. anonymous → `access.json.visibility == "public"` AND host `anonymous_read` → allow, else **401**
     (`WWW-Authenticate: Bearer realm="walgit"` — git must erase the credential, 06 §8.4).
- Enforcement points: the git read path (`/{o}/{r}[.git]/info/refs`, upload-pack GET/POST), LFS reads,
  and every repo-scoped read endpoint. The gate runs at the `require_read` boundary so no route can
  forget it. Per-repo private-read is therefore no longer "deferred": this object IS the hook's
  implementation, and policy.json stays push-only (the 14 §14.4 contract is untouched).
- Cost: one conditional GET of `access.json` per read request (control-plane, sub-second) with an
  in-process LRU stamped by the CAS version — a changed version invalidates lazily, exactly the
  ref→sha LRU pattern (07 §5). Anonymous hot clones of public repos therefore cost one extra
  sub-second GET per `info/refs`, never a LIST.

## 5. Role → capability matrix

Resolution maps a role to capability checks; `admin` binding ⇒ the capability set of
`Principal{write:true, admin:true}` **for repo-scoped ops**. Host-level admin (setup, instance facts,
cross-repo enumeration) stays flag-gated — an access.json admin is not a host admin.

| Capability | read | triage | write | maintain | admin |
|---|---|---|---|---|---|
| Clone/fetch (`info/refs`, upload-pack) | ✓ | ✓ | ✓ | ✓ | ✓ |
| Open issues, comment on any issue | ✓ | ✓ | ✓ | ✓ | ✓ |
| Label/assign/close **others'** issues; edit/delete others' comments | | ✓ | ✓ | ✓ | ✓ |
| Push refs (unprotected, per policy.json) | | | ✓ | ✓ | ✓ |
| Create/edit releases, upload assets (07) | | | ✓ | ✓ | ✓ |
| Merge PRs; push to protected refs (with the policy effect's rules) | | | | ✓ | ✓ |
| PUT/DELETE repo `access.json` (incl. `visibility`); invite collaborators | | | | | ✓ |
| PUT/DELETE `policy.json`, repo `settings` (existing core admin ops) | | | | | ✓ |
| Delete repo | | | | | ✓ (core: admin flag or role admin) |

Triage-without-write is the only genuinely new gate for 02; maintain's protected-ref gate composes with
`policy.json` effects (the rule engine still decides refs; the role decides *who may be evaluated as a
pusher at all* — a `read`-role principal is rejected at `require_write` before policy runs).

### 5.1 Delete repository semantics

`DELETE /{o}/{r}/api` (both lanes, core `api.summary.repoDelete`) deletes the repo's manifest first
(linearization: new opens fail immediately), then every remaining key under `repos/<o>/<r>/`, then the
local serving copy. It is admin-only per the matrix above (non-admin → 403/401 with a plain-text body)
and idempotent (a second DELETE → 204). No new backend code was needed for the settings Danger Zone
(issue #39) — the endpoint, its SDK mirror (`repo.delete()`), and its table-driven httptest coverage
predate it.

Fork/GC: the delete touches exactly one repo prefix. Fork children are separate prefixes with their own
manifests, so deleting a parent neither strands nor removes its forks (they keep serving), and deleting
a fork removes only the fork. A deleted fork can leave a stale entry in the parent's `meta/forks.json`
(03 §7); fork-network readers MUST treat a missing child manifest as absent (conditional-GET miss =
skip), never as an error — the GC liveness rule stays total over a partially-deleted network.

### 5.2 Creation owner admission + eager access default (issues #210, #346)

A logged-in principal may create or import a repo under `owner` iff ONE
holds (the #346 admission rule, enforced by `(*Service).CheckCreateOwner`
BEFORE any namespace write — a deny allocates no counter, writes no
manifest, leaves no partial state):

- the owner equals their own username (case-insensitive), or
- the owner names an org whose roster contains them (ANY roster role —
  v1 member-may-create), or
- they hold the host `admin` flag (global-admin bypass, documented;
  the auth-none `anon` principal carries it, so zero-config first-run
  creation is unaffected).

Anonymous → `401` (real 401, law 9); a foreign owner → `403` naming the
allowed owners (own username + member orgs, enumerated on the deny path
only — law 6); roster probe failures → `503`, never 403-as-404. The rule
costs one exact-key roster GET on the non-self, non-admin path
(human-rate create/import only, never hot — same cost class as the P6
team expansion probes). The seam is `CreateOwnerGate` (`internal/api`)
/ `RoleService.CheckCreateOwner` (`internal/repoimport`) plus the mirror
create-from-URL hook — law 8: core defines the seam, this package
implements it. The UI bounds the New/Import owner fields to a dropdown of
self + member orgs (server-authoritative 403 is the real gate; admins
needing a foreign namespace use the API). The #347 push guardrail reuses
the helper verbatim for auto-create-on-push (same 403 shape); the
SSH/HTTP repo-scoped push rule for existing repos lands there, not here.

This SUPERSEDES the #210 org-only gate (which left unclaimed prefixes
open and said nothing about self): creation under a foreign prefix that
is neither self nor a member org is now 403, not legacy-open.

At placeholder creation the §10 synthesized default is materialized eagerly
(`{visibility:"public", role_bindings:[{subject:"user:<creator>", role:"admin"}]}`), so the
placeholder page has a deterministic visibility + an admin for the Danger-Zone delete
(Create-with-synthesis writer shape per §10 Concurrency: 412 = someone raced us — adopt, don't
overwrite). Auth-none: no eager `user:anonymous` binding (user: subjects are emails — such a
binding fails subject validation); none-mode materializes a visibility-only doc or relies on
read-time synthesis with the existing flag-driven grants. Policy/templates are NOT evaluated at
create (policy gates pushes; owner-scoped templates are a documented future).

### 5.3 Repo-scoped push rule (issue #347)

The §5 matrix's "Push refs" row is enforced by `(*Service).CheckPush` — P6
resolution with the host `write` flag STRIPPED (Resolve step 3 would
otherwise re-grant the exact host-wide write this rule retires):

- host `admin` passes without touching the store (auth-none `anon`
  carries it — zero-config pushes and the push-budget fast path cost
  zero reads); anonymous → `401` (law 9);
- self-namespace (owner segment equals the principal name — the §5.2
  self rule, mirrored: slug namespaces carry no `user:<owner>` binding,
  so without this a slug user could create under their name yet never
  push to it) passes;
- otherwise the principal's resolved role over their NAME alone must
  reach `write` (org-owner role, team/explicit binding); anything else →
  `403` naming the repo and the required relationship.

`CheckPush` covers EXISTING repos only; pushes that would auto-create
gate the owner segment through `CheckCreateOwner` (§5.2, verbatim reuse —
same 401/403/503 shape) first. Both transports (smart HTTP, SSH)
enforce the identical rule at receive-pack dispatch through the server
`PushGate` seam (core defines the seam, this package implements it —
the `ReadGate` shape, law 8); nil seam → legacy host-flag gating.
`CheckRole` is unchanged (its flag-aware contract serves the API
surfaces); the push path never calls it with host flags set.

### 5.4 Repo transfer between owners (issue #358)

Direct, owner-initiated transfer (v1 — no accept step):
`POST /{owner}/{repo}/api/transfer` with `{owner, repo?}` moves every
object under `repos/<src>/<r>/` to `repos/<dst>/<r>/` (repo name defaults
to the source name, so transfer+rename passes both). Gates, in order:
repo admin on the source (`CheckRole` admin — binding, source-org
ownership, or host admin), then `CheckCreateOwner` admission verbatim on
the destination (self, member org, or host admin — §5.2, same 401/403/503
shape). The service is principal-free (like `DeleteOrg`); the handler
gates. Success is `201 {owner, repo}`.

The move is copy-then-delete over opaque bytes (law 4 — no source key is
deleted before its copy ACKs), streamed (never whole packs in memory).
Every destination write is `PutCreate`: a lost race aborts 409 with
best-effort cleanup and the source untouched. The delete pass removes
exactly the copied set, then re-lists the source prefix — leftovers mean
a push raced the move and report 409 with the destination complete (a
mistyped transfer is recoverable by transferring back; a raced transfer
leaves the named src residue to delete — the destination is already
complete, so a re-transfer would 409 on the occupied manifest). `access.json`
is the one interpreted object: visibility and all bindings survive except
the owner-subject — `user:<src>` bindings drop (the seller keeps no
admin) and a user destination without a binding gains
`user:<dst>`/admin (org destinations need none — org-owner resolution
covers them); `team:` subjects name teams that still exist and move
untouched. A missing source `access.json` materializes the destination's
synthesized default for user destinations only; a corrupt one moves
byte-identical (transfer never fails on it). Repo invitations under
`meta/` move with the prefix, so pending invites survive. Content types
restore the writer convention (`.json`/`.pb`, else octet-stream —
serving MIME comes from sidecars, never object metadata). This is what
makes the org-delete 409 true: an org that owns repos is deletable once
each repo is transferred (or deleted) through a real capability.

## 6. Policy engine integration (Seam 3 amendment)

`policy.json` stays the frozen envelope; effects are untouched. The amendment is to **group member
resolution** (14 §14.10.1's `groups` rosters): a `groups[].members` entry MAY be

- `"team:<org>/<slug>"` → expands to the union of `orgs/<org>/teams/<slug>.json` `members`;
- `"role:<owner>/<repo>:<role>"` → expands to principals holding ≥ `<role>` on that repo per §4
  (org owners included).

Expansion happens at policy load under the existing per-repo policy cache's single-flight (13 §3:
`"policy:" + repo` join); expanded sets are cached with the policy generation and re-expanded on the
next manifest-revision-style reload — a team edit is visible to pushes within one policy reload, never
mid-evaluation. Expansion is a bounded read set (the teams/objects the file references — probed by
exact key, never LIST). A `team:`/`role:` reference that fails to resolve is a **parse-time warning and
evaluates to the empty set** (fail-closed for `protect` semantics: an empty allow-set denies). This
consumes identity state read-only; the policy engine never writes identity objects.

### Concurrency

Hazard: team expansion putting a blocking bucket read on the push path. Avoidance: expansion is
prefetched under the policy load's single-flight (one expansion per manifest revision, joiners share —
14 §14.5's cache pattern); `Evaluate` stays pure and local exactly as Seam 3 requires. No locks: the
CAS'd team object is read fresh per expansion; a concurrent team edit either lands before (seen) or
after (next reload) — never torn, because objects are read whole.

## 7. Invitations

Invitation kinds: `org` (join an org with role `owner|member`) and `repo` (collaborator with role
`read|triage|write|maintain|admin`). Objects:

| Key | Create or CAS | Schema |
|---|---|---|
| `orgs/<org>/invitations/<id>.json` | **Create**-only, immutable | `{"version":1,"id":"<16-byte hex>","token":"<32-byte hex>","kind":"org","org":"acme","role":"member","subject":"pat@example.com","invited_by":"jane@example.com","state":"pending","created_at","expires_at"}` |
| `repos/<o>/<r>/meta/invitations/<id>.json` | **Create**-only, immutable | same shape, `kind:"repo"`, plus `"role"` (repo role) |
| `users/<principal>/invitations/index.json` | **CAS'd** (overwritable family) | `{"version":1,"entries":[{"id","org"|"repo","role","invited_by","created_at","expires_at"}],"updated_at"}` — P4-style hot window of pending invites (`expires_at` rides the row since Forgejo #362; pre-#362 rows omit it) |

- `state` is carried by compensating **replacement objects? No — invitations are Create-only**;
  state transitions (`accepted`, `cancelled`, `expired`) are recorded by writing the invitee's inbox
  entry removal + a tombstone-free rule: the issuer-side object is DELETED on terminal state, and the
  inbox index entry is dropped in the same handler (P8 synchronous fan-out). Acceptance evidence is the
  resulting binding (org member, or access.json binding), not the invite object — the invite is a
  capability token, not a record.
- `id` and `token` are independent random values; `id` is the key, `token` is the bearer secret for
  emailed links. Email delivery is out of scope (06 owns webhooks; an operator's SMTP glue is theirs) —
  the API returns the accept URL: `/api/v1/invitations/{id}?token=<token>`.
- **Accept** (the only mutation): `POST /api/v1/invitations/{id}/accept` — requires an authenticated
  principal whose email equals the invite `subject`, OR a browser GET of the signed link (token
  matches) which renders the invite summary and the UI then POSTs accept (the link's token authorizes
  the *preview*; the binding write always follows the authed POST). Accepting writes the binding
  (member add / access.json CAS) and deletes the invite.
- Repo collaborator invites put the binding in `access.json` (P6 source 1); org invites write
  `orgs/<org>/members.json` (CAS).
- Deleted repos (issue #63): the prefix sweep removes the issuer-side object but cannot enumerate
  inboxes, so `MyInvites` skips repo-kind rows whose manifest is gone AND rows whose issuer object is
  gone (the inbox is a cache of issuer truth — this also keeps swept rows hidden after a recreate, and
  expiry still lists, failing closed on accept, unchanged). Creating a repo invite for a ghost is 404;
  accepting one is 409 "no longer pending" (no binding is written — the synthesized default must never
  gain ghost bindings). Probe errors keep the row (fail open).

### Concurrency

Hazard: accepting an invite concurrently from two tabs, or accepting a just-cancelled invite. Avoidance:
accept is a CAS sequence — binding write first (org `members.json` CAS / `access.json` CAS loop), then
invite-object DELETE, idempotent for the loser (a second accept sees 412/absent → done, like the P3
event path). A cancelled-vs-accept race is decided by which CAS lands first; the loser gets 409
"invitation no longer pending" and writes nothing. Inbox index updates happen after the commit in the
same handler (P8 shape); a crash drops one inbox entry — the issuer-side list is the truth.

## 8. API endpoints

Wire conventions per 07 §2: plain-text errors, `[]` not `null`, RFC 3339 UTC, per-segment decoding,
mutable-collab (`private, no-cache` + version ETag where a token exists) or `no-store` cache classes
(issue #280 — never SWR on mutable state), both lanes everywhere. All registered by the `identity`
RouteProvider (Seam 1).

### Top-level (`/api/v1` + `/api-browser/v1` twins)

| Method + path | Auth (P6) | Request → response |
|---|---|---|
| `GET /api/v1/users/{principal}` | any (public read) | → `{profile}` (carries `avatar_content_type`/`avatar_updated_at` when the user holds an avatar, `avatar_disabled` when opted out); 404 unknown |
| `PUT /api/v1/users/{principal}` | self or admin | body = profile → 200 profile; 400 invalid |
| `GET /api/v1/users/{principal}/avatar` | any (public read) | generated SVG (`image/svg+xml`, immutable max-age + version ETag, `?v=` busting); 404 when none |
| `POST /api/v1/users/{principal}/avatar` | self or admin | regenerate (clears opt-out, installs fresh deterministic render) → 200 profile; 404 without a verified email |
| `DELETE /api/v1/users/{principal}/avatar` | self or admin | remove + opt out of auto-generation → 200 profile; 404 unknown |
| `GET /api/v1/users/{principal}/orgs` | any (mutable-collab, content ETag) | → sorted `["acme", …]` over `MemberOrgsFor` (any roster role, #370 alias matching); `[]` when none, 200 for unknown principals (never 404); GET-only |
| `GET /api/v1/orgs` | any (mutable-collab, no version token) | → sorted `["acme", …]` |
| `POST /api/v1/orgs` | write | `{org, display_name}` → 201 `{org}`; 409 taken; creator becomes owner |
| `GET/PUT/DELETE /api/v1/orgs/{org}` | read / owner / owner | profile CRUD (PUT body = `{display_name, description, location, timezone, bio_markdown}`, full-document replace, owner-profile limits mirrored; description unbudgeted); 409 on delete with repos; DELETE also removes the avatar object |
| `GET/PUT/DELETE /api/v1/orgs/{org}/avatar` | read / owner / owner | raw avatar bytes (GET → sniffed Content-Type, immutable max-age; PUT raw bytes → 200 org doc; DELETE clears pointer + bytes, idempotent); 413 over 2 MiB; 415 outside PNG/JPEG/GIF/WebP |
| `GET/PUT/DELETE …/members/{principal}` | read / owner / owner | roster ops; last owner removal → 409 |
| `GET/POST …/teams`, `GET/PUT/DELETE …/teams/{slug}` | read / owner | team CRUD |
| `PUT/DELETE …/teams/{slug}/members/{principal}` | owner | membership edit |
| `POST /api/v1/orgs/{org}/invitations` | owner | `{email, role}` → 201 `{id, accept_url}` |
| `GET /api/v1/invitations` | any authed (no-store) | → my pending invites `[]` |
| `GET /api/v1/invitations/{id}?token=` | token OR subject match | invite summary (signed-link preview) |
| `POST /api/v1/invitations/{id}/accept` | authed, subject match | → 200 `{bound: "org"\|"repo"}`; 409 not pending |
| `DELETE /api/v1/invitations/{id}` | invitee (decline) or issuer (cancel) | → 204 |

### 8.1 Owner/repo listing (core, reused — no endpoint added here)

`GET /api/v1/owners` → sorted owner names from the STORE (never disk) and
`GET /api/v1/owners/{owner}/repos` → short repo names (`200 []` for an unknown
owner, never 404) are **core** endpoints (specified in `docs/go/07_api.md` §8,
registered by the core mux, not by this package's RouteProvider). They are the
read surface the owners (`/`) page renders per-owner repo sections from
(issue #117): `owners.list()` then one `owners.repos(owner)` per shown owner.
No pagination, no creation timestamps on the listing path — the endpoints return
sorted plain name lists (per-repo `first_state_at`/first-entry `created_at` proxies exist
deeper in the bucket but cost a manifest/log GET per repo), so caps and
newest-first ordering live client-side (`web/src/lib/owners.js`: `MAX_OWNERS`
50, `MAX_REPOS_PER_OWNER` 10, reverse of server order as the newest-first
proxy; 08 §6 owns the cache keys, 12 §2.3.1 the page contract).

### Repo-scoped `/{o}/{r}/api` (+ `/api-browser` twin via `api.Lanes`)

| Method + path | Auth | Request → response |
|---|---|---|
| `GET …/access` | triage | → `{version, visibility, role_bindings[]}` |
| `PUT …/access` | admin | full doc incl. `version` → 200 `{version}`; 409 stale version; 400 invalid subject/role |
| `POST …/invitations` | admin | `{subject, role}` → 201 `{id, accept_url}` |
| `GET …/invitations` | admin | → pending list |
| `DELETE …/invitations/{id}` | admin | → 204 |
| `POST …/transfer` | admin (source) + #346 admission (destination) | `{owner, repo?}` → 201 `{owner, repo}`; 401 anon; 403 non-admin / foreign destination; 404 unknown source; 409 occupied destination or mid-transfer race (§3.1) |
| `DELETE …/api` | admin | → 204 (core §9.1 lifecycle; delete + fork/GC semantics §5.1) |

## 9. UI and SDK

Pages (SolidJS SPA per 12_web_ui.md, D-WEB-6; Solid signals, `useData` 5 s TTL):

- **Org settings** `/:org/settings` — sub-tabs profile / members / teams / invitations, plus an
  owner-only Danger Zone tab (typed-confirm delete-org; non-owners see no affordance, server still
  gates); member rows inline role `<select>`; invite form shows the returned accept link.
- **Team page** `/:org/teams/:slug` — member list, add/remove, and the repos this team is bound to
  (derived by reading that org's repos' `access.json`; bounded by the org's repo count, P5-acceptable).
- **Repo Access tab** `/:owner/:repo/settings/access` — a fourth settings sub-tab: visibility
  select (Forgejo #374: `public — anyone may read` / `private — logged in only` plus the
  owner-shaped private mode — `private — owner only` for user-owned repos, `private — org
  members only` for org-owned repos; the verdict rides the #348 owner-kind marker via one
  `orgs.get` probe that 404s to null for users),
  role-binding table (subject, role, remove), add-binding form (user or team autocomplete), CAS version
  in the footer; save = full-doc PUT, 409 renders "changed under you, reload". The settings
  General tab carries the same owner-aware visibility select (same probe, shared `VisSelect`
  component — issue #410).
- **Profile** `/:owner` renders user or org profile (existing route gains the org variant).
  User owners additionally render the generated avatar from the user profile's pointer
  (issue #376 — gated on `avatar_content_type`, `?v=` cache-bust, hides on 404), with
  regenerate/remove self-service on the owner's own page; the navbar identity control
  renders the same avatar from `me().avatar_url` (username fallback when absent).

SDK additions (submodules under `web/sdk/src/`, bundled by esbuild into `repos.js`; JSDoc typedefs in
`types.js`): `users.js` (`users.get/put`, `users.avatar.url/regenerate/remove` — issue #376), `orgs.js` (`orgs.*`, members, teams,
`orgs.avatar.url/upload/remove` — issue #359), `access.js`
(`repo.access.get/put`), `invites.js` (`invites.list/mine/accept/cancel`), `transfer.js`
(`repo.transfer({owner, repo?})` — issue #358). The `/:owner` org header and the
`/:org/settings` Profile tab render `location`/`timezone`/`bio_markdown` (markdown
through the shared pipeline) and the avatar (gated on `avatar_content_type`, `?v=`
cache-bust, hides on 404); the settings form edits all five profile fields
(timezone via the `Intl.supportedValuesOf` picker) and uploads/removes the avatar
(client pre-checks the 2 MiB cap; the server enforces it).

## 10. Migration of existing repos

Repos that predate this feature have no `access.json`. Normative behavior:

- **Reads synthesize the legacy default** without writing: missing `access.json` ≡
  `{visibility:"public", role_bindings:[{subject:"user:<owner>", role:"admin"}]}` where `<owner>` is the
  repo's owner namespace (org-owned repos need none — org-owner resolution covers them); auth-`none`
  anonymous pushes stay flag-driven (everything is granted anyway).
- **Materialization** is the `access-bootstrap` task kind (Seam 5): each sweep, for every repo still
  lacking `access.json`, `Create`s the synthesized object (creator binding from the creating principal
  when recorded, else `user:<owner>`); idempotent (`Create` 412 → skip), restartable, orphan-tolerant.

### Concurrency

Hazard: bootstrap racing a first admin edit — the edit's CAS-on-synthesis would 412-loop against the
bootstrap's Create. Avoidance: edits to a repo with no `access.json` synthesize it themselves
(CAS: GET → absent → PUT Create) — one writer shape; the task's only job is untouched repos, 412 = no-op.

## Decisions

- **Frontend idiom is the SolidJS SPA (D-WEB-6; docs fix for issue #76).** The §9 page sketches read in the shipped idiom: Solid components under `@solidjs/router`, state via Solid signals/stores, Tailwind styling. Routes, gating, and wire shapes are unchanged.

- **Principal = email; profiles are the only identity objects here** — keys/tokens/sessions stay auth-provider territory (Seam 2); this doc owns zero credentials. (SUPERSEDED by the #370 entry below: the principal NAME is now the username; the email rides `Principal.Email` for alias matching only.)
- **`visibility` lives in `access.json`** — one CAS'd object already admin-write and read at every authz decision; a separate visibility object would double the read.
- **Private-repo read gating via the named `require_read` hook** (14 §14.10.1) — policy.json stays push-only (its frozen contract), the gate is a registered hook, not a policy effect.
- **`members.json` is one CAS object; team `members[]` is a string array** — human-rate writes, owner checks in one GET, and mention/policy expansion needs principals only.
- **Invites are Create-only, delete-on-transition; the inbox is a P4-style index** — an invite that can be rewritten is a second writer of role state, and "my invites" must never enumerate orgs/repos.
- **Legacy repos synthesize on read, then materialize lazily; `access.json` edits are full-document `PUT`s** (no per-binding endpoints) — zero-downtime adoption, no per-binding endpoint surface; matches the policy/settings PUT class.
- **Repo delete stays a core lifecycle op; this doc owns only its gate and fork semantics** (issue #39) — `DELETE …/api` predates the collaboration layer, so the settings Danger Zone needed no new endpoint, no new SDK method, and no new backend tests; §5.1 pins the admin gate and the fork/GC delete rule instead.
- **Invite inbox verifies issuer truth on read (issue #63, 2026-09-04)** — one manifest HEAD plus one
  issuer-object GET per repo row (org rows: one GET; probe errors fail open). Rationale: the inbox is
  a lossy cache of the issuer objects (the §7 crash rule says so), so consulting the objects on read
  is the same truth source accept already uses — no sweeper, no users LIST, no tombstone, and no
  prune path that could orphan a pending row. P6/auth is untouched: the gates still decide visibility,
  existence only decides whether a row renders.
- **Org creation is atomic-or-recoverable (issue #75, 2026-09-05)** — a failed `members.json`  seed rolls the `org.json` reservation back (version-guarded, best-effort, original error surfaces),
  and a retry on an ownerless reservation completes the owner binding via CAS (same path heals the
  crash-between-writes residue); re-create by the bound owner is idempotent. Rationale: the old
  reserve-then-seed left an ownerless namespace on any non-412 seed failure with retries 409ing
  forever. Arbitration is unchanged for genuine races: exactly one winner, losers 409 and write
   nothing, because only an ownerless roster is ever rewritten.
- **Owners page reuses the core listing endpoints; no endpoint added (issue #117, 2026-09-05)** —
  `GET /api/v1/owners` + `GET /api/v1/owners/{owner}/repos` already list everything the `/` page
  needs, so this change adds no wire surface (§8.1 pins the reuse). Caps and newest-first ordering
  stay client-side on purpose: the listing path exposes no creation timestamps and the registry returns
  sorted names, so a server-side "newest" sort or paginated shape   would need per-repo manifest/log reads (or a new endpoint carrying creation times) rather than
  inventing metadata the listing does not have. Rationale: read-only reuse keeps the round-trip budget (1 + shown-owners GETs,
  SWR-cached) and the CAS surface untouched.
- **Creation owner admission (issue #346, §5.2 — supersedes the #210 org-only gate):** owner
  must equal the principal's username or be a member org (any roster role — v1
  member-may-create); host admins bypass; anonymous 401, foreign owner 403 naming the
  allowed owners, probe errors 503. One rule (`CheckCreateOwner`) consulted by explicit
  create, import, and the mirror create-from-URL twin BEFORE any namespace write; the #347
  push guardrail reuses it verbatim for auto-create-on-push. Rationale: the #210 shape still
  let any host-writer squat any unclaimed prefix or another user's namespace. The `OrgGate`/
  `IsOrgMember` seam is replaced by `CreateOwnerGate`/`CheckCreateOwner` (+ `MemberOrgs` for
  the 403 message) in the same change — no alias, no shim.
- **Repo-scoped push rule (issue #347, §5.3):** the §5 "Push refs" row is enforced by
  `CheckPush` — P6 minus the host-write grant (flags stripped; admin bypass and 401/403
  shapes kept), plus the §5.2 self rule mirrored so slug namespaces stay pushable by
  their owners. Existing repos gate on `CheckPush`; would-be-created repos gate the
  owner segment on `CheckCreateOwner` first (same rule as explicit create — no fork).
  Both transports enforce it at receive-pack dispatch through the server `PushGate`
  seam (nil → legacy host-flag gating); `CheckRole` keeps its flag-aware contract for
  the API surfaces. Rationale: the host-wide write flag let any writer-key push to ANY
  repo and auto-create under foreign namespaces — the flag is authentication-adjacent
  (who holds a credential), never authorization (who may write THIS repo).
- **Explicit-create org gate + eager access default (issue #210, §5.2, R1 B5/S2 — gate
  part SUPERSEDED by #346 above):** creation
  under an org prefix requires membership (403 only on proven non-membership; unclaimed prefixes
  legacy-open; probe errors 503); the §10 synthesized default materializes eagerly at create
  (Create-wins, adopt-don't-overwrite); auth-none skips the creator binding (no
  `user:anonymous` — fails subject validation) and relies on flag-driven grants. Sidecar
  classification, the flag shape, and discovery live in 07_api.md §14 (this doc owns the gate +
  the default, not the marker).
- **Repo transfer + delete-org UI (issue #358, §5.4):** direct owner-initiated transfer
  (`POST /{owner}/{repo}/api/transfer`, repo admin on source + `CheckCreateOwner` on
  destination, copy-then-delete over opaque bytes with the owner-subject rewrite) lands
  together with the owner-only org Danger Zone, so the DeleteOrg 409 ("transfer or delete
  them first") names a capability that exists — no wording change needed. Rationale: the
  409 referenced a missing feature, making orgs with repos undeletable; v1 skips the
  transfer+accept dance (Forgejo parity deferred) because both gates already exist and
  the owner initiating the move holds admin on both ends by construction.
- **Auto-generated user avatars (issue #376, §8 — law-1 exception, user-authorized
  2026-09-12, AGENTS.md §1):** `github.com/dicebear/dicebear-go/v10` +
  `github.com/dicebear/styles/v10` render the deterministic avatar (DiceBear
  "constellation", seed = verified email) in-process on first login. Verified
  deviations from the issue's assumptions: (a) the library carries two
  build-required transitives (`github.com/dicebear/schema` +
  `github.com/santhosh-tekuri/jsonschema/v6` — option validation; `go mod graph`
  proof in the PR); (b) constellation defines NO options, so the issue's
  "electric" preset names nothing here (it is a notionists variant) — generation
  uses default options, documented in `avatar.go`. Storage mirrors the #359
  org-avatar shape: bytes at `users/<username>/avatar.svg` (bucket truth, law 4)
  with the pointer (`avatar_content_type`/`avatar_updated_at`) on profile.json;
  serving is `GET /api/v1/users/{principal}/avatar` (`image/svg+xml`,
  `public, max-age=86400, immutable` + `?v=<avatar_updated_at>` busting).
  Generation is a per-principal single-flight goroutine behind the server
  `AvatarHook` seam (called from the OIDC session-mint path) — NOT a task-table
  job (repo-keyed, narrated work only; login already answered, no progress to
  report). Deletion opts out (`avatar_disabled`, honored by the login check)
  until an explicit `POST …/avatar` regenerates (same email → identical image,
  the determinism note). The seed feeds the PRNG only (verified absent from the
  output) with a sanitize-first gate (SVG shape, no `<script>`, no seed leak —
  fail closed). Consumption: `me().avatar_url` → navbar identity control, plus
  the `/:owner` header with self-service regenerate/remove. Rationale: logins
  must never block on generation, emails must never leak into markup or other
  users' views, and the task table must not gain a non-repo kind.

## Explicitly out of scope

- **SSH keys, tokens, sessions, password flows** — auth-provider territory (Seam 2); this layer reads principals only.
- **SAML/SCIM, LDAP-synced teams** — P9; a Seam 2 provider maps directory groups onto teams later.
- **Fine-grained per-branch roles, CODEOWNERS-style routing, custom repo roles** — roles are the fixed five-level ladder (P6); finer control is `policy.json` rules (Seam 3).
- **Nested teams, org roles beyond owner/member; audit history; invite emails** — the fixed ladder plus owner/member is the whole org surface; overwritable objects keep no history (a Seam 4 `jsonl` audit sink is the record if needed); SMTP is operator-side.
- **Owner-profile edit gate (Forgejo #234, §8):** `PUT /api/v1/owners/{owner}/profile` (core
  `internal/api` surface, triple twins) requires the AuthWrite gate first (anonymous → 401/403 —
  the bio stays public-read), then exactly one of: (a) host `admin` flag (covers org namespaces
  with no name-matched principal and bootstrapping), (b) case-insensitive principal-name match
  against the owner slug (user-self: the `me()` principal IS the owner name on instances whose
  token mapping mints slug names), or (c) org `owner` role in `orgs/<org>/members.json` via the
  `api.OwnerEditor` seam implemented by `(*Service).CanEditOwnerProfile` (one exact-key roster
  GET, human-rate; probe errors fail closed with 503, never 403-as-404 — the creategate
  rule). Rationale: principals are emails while owner slugs are namespaces, so no pure core rule
  can express "org owner" — but core must not import identity (law 8), hence the seam (the
  CreateOwnerGate/AccessBoot shape, wired in `cmd/walhub` composition). Org `member` (non-owner) and
  team membership grant nothing: the profile speaks for the namespace, so only namespace owners
  (plus host admins) write it. The GET carries request-scoped `can_edit` (same rule, probe
  failure degrades to false) so the UI affordance never guesses — client gating stays cosmetic.
- **Mutable-collab cache class on every identity GET (issue #280).** §8 said SWR/ETag — the wrong
  call for user-mutable docs (the version ETag revalidated correctly, but SWR's stale-serve
  window licensed pre-mutation paints across refreshes). Profile/org/members/team/access GETs now
  serve `private, no-cache` with their version ETags intact; tokenless collection/singleton GETs
  (orgs/teams lists, single member) take the class without an ETag (always 200, never stale);
  invite/perms routes were already `no-store`. Rationale: mutability, not addressability, decides
  the class (07_api.md §4 third class).
- **Org profile parity + avatar (issue #359).** `org.json` gains `location`/`timezone`/
  `bio_markdown` with the exact owner-profile spelling and budgets (display/location ≤ 200
  runes, timezone IANA-shape ≤ 64 bytes, bio ≤ 64 KiB valid UTF-8 — duplicated constants,
  not an import, because identity must not import the api package per law 8), and the PUT
  body carries all five profile fields as a full-document replace (absent clears, same as
  the owner-profile PUT; `description` stays unbudgeted so long-standing taglines never
  start 400ing). NO `website` field: the parity target (owner profile) has none and
  `description` stays the tagline — a website can be linked from the bio. Avatar bytes live
  at `orgs/<org>/avatar` (law 4: bucket object, wipe-safe), size-capped at 2 MiB (avatars
  are chrome, not content — issue images allow 8 MiB) and magic-sniffed against the
  PNG/JPEG/GIF/WebP allowlist (SVG rejected: same-origin SVG executes script; the client
  Content-Type is ignored). Bytes-first-pointer-second (the attachments/releases
  philosophy): PUT writes bytes then CASes `avatar_content_type`/`avatar_updated_at` onto
  `org.json`, so GET org answers avatar presence in its single round trip (law 6) and a
  crashed pointer write leaves inert bytes, never a dangling pointer; a pruned object
  under a set pointer renders as "no avatar", never an error. Profile PUTs preserve the
  pointer; DeleteOrg removes the avatar object. Rationale: GitHub/Forgejo parity needs
  both the fields and the picture, and the pointer keeps the hot read (GET org) at one
  round trip with no probe fan-out.
- **Org marker on `owners/detailed` via the `OrgLister` seam (Forgejo #348).** The core
  `owners/detailed` rows gain `is_org` from `Env.Orgs.ListOrgs` (one call per listing —
  law 6; nil seam → all false; list error fails open to all-false, display metadata never
  fails the listing). `*identity.Service` satisfies the seam with its existing `ListOrgs`
  (compiler-checked in `cmd/walhub` composition, no new service method). Rationale: the
  explore page must distinguish org namespaces from users without a per-owner probe
  fan-out — same law-8 hook shape as `OwnerEdit`/`RepoVisibility` (core never imports
  identity). Server mutation gates are untouched (`CheckOrgOwner` still 403s non-owners);
  the UI's read-only views and Manage affordances key on the org-slug profile's
  `can_edit` (cosmetic-on-top, 12_web_ui.md).
- **Org rename is display-name-only for v1 (issue #360, survey #349 candidate 1).**
  The org id (slug) is immutable: keys are `orgs/<org>/` (`org.json`, `members.json`,
  `teams/*.json`, invitations) with no rename service, and every `repos/<org>/*` owner
  segment is pinned to it — a rename-with-redirect must move all of those under CAS
  plus rewrite `team:` subjects, which is expensive and CAS-heavy for an operation
  that is rare. `display_name` on `org.json` is already editable via
  `PUT /api/v1/orgs/{org}` (full-document replace, `PutOrg`), so display-name-only
  needs no new surface: no rename endpoint, no old-name tombstone/redirect record,
  and no doc or `web/src` UI affordance promises one (verified — the only `rename`
  hits are filesystem tmp+rename, git rename detection, label delete+create, and
  repo transfer+rename). Revisit only on a concrete rename need.
- **Team-subject picker on the Access tab (issue #361, survey #349 candidate 4).**
  `team:<org>/<slug>` bindings were fully supported server-side (`validSubject`,
  team expansion in `Resolve`, `DeleteTeam` stripping) but undiscoverable — the
  add-binding form was free text only. On org-owned repos the form now offers a
  native team `<select>` fed by one `client.orgs.teams.list` GET on the owner org
  (cached under its own data key; email owners skip the GET — an org slug can
  never contain `@` — while 404 / 403 / empty all degrade to the text-only
  form, so non-org owners see no change). Choosing a
  team composes the normalized `team:org/slug` spelling into the subject field
  (still editable — free text stays the fallback); the add path validates through
  the headless `web/src/lib/access.js` helpers (charset mirrors of
  `identity.ValidOrg`/`ValidSlug`, typo-catching email shape) with a friendly
  note on invalid, and the server revalidates on PUT as before. Native select,
  not a popover: no new CSS, no #278 viewport concern, no team mutations from
  the picker (the access PUT owns the save). No backend change, no new deps.
- **Inbox rows carry `expires_at` (Forgejo #362, survey #349 candidate 10).**
  `GET /api/v1/invitations` (`MyInvites`) served id/org|repo/role/invited_by/
  created_at but no expiry, so an inbox UI would need a per-row preview GET
  fan-out to show it. Both create paths now stamp `ExpiresAt` onto the
  `InboxEntry` (same value as the issuer object); the field is `omitempty`,
  so pre-#362 rows decode with `""`, which readers treat as unknown — never
  as expired (display fails open; accept still fails closed via the issuer
  object's 409 "invitation expired"). Rationale: one read serves the whole
  inbox (law 6 — no N+1), the index stays a cache of issuer truth (expiry is
  still enforced at the object in `findInvite`, never from the row), and the
  JSON addition is append-only (no fixture/round-trip break: no golden pins
  the inbox shape). The `/invitations` inbox page and the org-tab expiry
  column are specified in 12_web_ui.md.
- **Visibility modes split: `authenticated` added, `private` refined (Forgejo #374).**
  The Access tab's single "private — members only" never meant what it said: resolution admitted
  bindings + org owners + host admins + anyone with host-wide write, while EXCLUDING ordinary org
  members without bindings. The enum is now additive — `public` (unchanged),
  `authenticated` ("private, logged in only": any authenticated principal reads, anonymous 401s),
  `private` ("visible only by owner/org": owner bindings, org roster members without bindings,
  explicit bindings, host admin) — accepted everywhere visibility is parsed (normalizeAccess,
  the POST create path, `EnsureRepoAccess`, the SDK typedef, both UI selects). The host-write
  fallback is ruled DENY (the #347 direction extended to reads: the write flag authenticates,
  never authorizes) — `CheckRead` early-allows host admin only, `Resolve` grants the admin
  flag only, and the #345 listing filter probes writers per repo instead of bypassing them.
  Migration: existing `private` docs keep their value (no data change); the behavioral delta is
  that org members GAIN read without bindings and host-write-only outsiders LOSE private read
  (they keep public/authenticated reads through visibility). Operators who used private as
  "all logged-in users" should flip those repos to `authenticated`.
  Rationale: two distinct intents (user-owned "not anonymous" vs org-owned "members only") need
  two values — one label cannot serve both — and a host-wide credential must never imply
  per-repo authorization.
- **Usernames are the identity key; emails are owner-visible only (Forgejo #370).**
  The principal NAME is the immutable username (derived at first OIDC login
  from the verified email's local part — `auth.DeriveUsername` — uniquified
  on collision: crueber, crueber2, …), never the raw email: the repo-id
  owner segment cannot carry `@`, and every `Principal.Name` render surface
  (repo paths, explore/owner listings, issue/PR authorship, timeline,
  notifications, invite subjects, deny messages, org-hook titles) leaked the
  address. The binding lives on the bucket (law 4):
  `users/<username>/user.json` {username, email} (CAS on creation = the
  collision-uniqueness commit point) plus `users/by-email/<enc>/ref.json`
  (repeat logins cost one alias GET; law 6). `ValidPrincipal` accepts both
  spellings so pre-#370 state (rosters, access subjects, invite subjects,
  inboxes, profiles) keeps resolving via the pure `matchPrincipal`
  name-or-email alias — no extra store round trip on push paths. Grant
  sites (`SynthesizeOwner`, transfer rewrite/destination default, eager
  `EnsureRepoAccess`) bind `user:<owner>` only for user namespaces
  (`isUserNamespace`: legacy email, or registry-backed username that is
  not an org and not synthetic) — a `user:<orgslug>` binding would be a
  latent grant to whoever later claims that username, and synthesis must
  not manufacture authority for unclaimed names (the #346 pin holds:
  writeless self on a foreign/unclaimed namespace still 403s).
  `GET /users/{u}` fills `email` (never stored, `omitempty`) only when
  the caller IS the subject. Affected pre-#370 keys (no renames — the
  alias reads both): `users/<enc-email>/profile.json`,
  `users/<enc-email>/invitations/index.json`, `orgs/<o>/members.json`
  entries, `access.json` `user:<email>` subjects, team `members[]`,
  invite issuer objects + subjects, issue/PR author strings, notification
  recipient keys, `ssh-keys/k/<fp>` key docs (their `principal` still
  resolves through the email branch, so existing keys keep working; the
  per-user `ssh-keys/u/<email>/` list splits from the new
  `ssh-keys/u/<username>/` list). Operator-authored `policy.json` email spellings fail
  closed (deny) until rewritten to usernames — manual migration.
  Rationale: fail closed on the leak axis (worst case is a non-uniquified
  base, never an exposed email); additive keys (law 5); the registry
  doubles as the "does this user exist" probe.
- **Visibility save is authoritative + loud (issue #391, 2026-09-12)** — the #381 HTTP-cache
  explanation no longer covered the settings-page bounce, and instrumented repro (one-line access
  GET/PUT outcome logs, now permanent) plus handler-level evidence tests showed the save itself
  failing, not the read: a stale-version PUT 409s and a non-admin PUT 403s, and the old Settings
  save caught both into a small note while LEAVING the user's chosen value in the select — the next
  refresh reseeded server truth and the save looked "not stuck". Cross-instance staleness was ruled
  out on the memory + filesystem classes (a second instance sharing the store converges on its next
  conditional revalidation; the store contract pins the If-None-Match mapping). The fix:
  `web/src/lib/accessSave.js` is the one save path — fresh `access.get()` version immediately before
  each PUT, one re-read retry on 409, and on ANY failure the select reseeds from server truth with a
  specific note (403 = admin required, 409 = changed elsewhere). The Access tab shares the failure
  wording but keeps its no-retry full-document PUT (a blind retry there would clobber a concurrent
  binding edit). Rationale: fail closed with clear errors (law 9) applies to the UI too — the user
  must never stare at a select the server disagrees with.
- **VisSelect render path + identity-stable options (issue #410, 2026-09-12)** — the profile
  visibility selector rendered nothing through an opaque render path. Research-first verdicts
  (against the pinned `solid-js@1.9.15` sources in `web/node_modules`):
  (H1 CONFIRMED) a `<For>` children mapper is invoked per item as `mapFn(item)` for an arity-1
  mapper like ours (client `mapArray`'s `mapper`), or `fn(item, () => i)` (SSR `simpleMap`) — the
  index arrives as an accessor function, never a raw number, and the item is the option object,
  never `o.label`; a hand-rolled adapter calling `children(item, i)` / `children(o.label)`
  mismatches both real paths. (H2 CONFIRMED as an adapter bug, not framework timing) `mapArray`
  reads `list() || []`, so an undefined `each` renders nothing — the observed `Cannot read
  properties of undefined (reading 'value')` comes from invoking the mapper with an undefined
  ITEM, which the real path never does with a populated `each`. (H3 REFUTED for production) the
  vite bundle ships the runtime INSIDE `/_ui/assets/*.js` (no external runtime file exists to
  404); the 404 was a harness artifact of bypassing the vite build — plain node has no JSX
  transform, so `.jsx` cannot even be imported (that unimportability IS the "opaque" path).
  On top of that, the issue-comment finding held: `visibilityOptions()` built fresh
  arrays/objects per call, and `<For>` diffs by `===` identity, so every owner-kind refire rebuilt
  all `<option>` nodes and the select fell back to the first option (`public`) while the value
  signal stood still. The fix: `web/src/components/VisSelect.jsx` is the one shared selector
  (Settings General tab + Access tab; real `<For each={visibilityOptions(props.isOrg)}>` path,
  `value={props.value ?? ""}` keeps the #394 blank-while-unseeded rule, the Access tab gains the
  `aria-label="Visibility"` its inline select lacked); `visibilityOptions()` returns hoisted
  frozen constants (deep-equal shape unchanged); `web/src/lib/visSelect.js` is the headless row
  model (`visRows`: exactly the current row `selected`, none for unknown/empty); and
  `web/test/unit/vis-select.test.js` is the live-render rig — the REAL `For` from `solid-js`
  (no adapter stub) rendering populated rows and observing the selected mark follow visibility
  changes `public → private → authenticated` plus the user→org relabel, with the H1/H2 invocation
  shapes pinned. Rationale: law 11 (DOM thin, logic headless-tested) forbids throwaway JSX-runtime
  hacks — the component renders through the shipped path and the rig observes that path's real
  contract.
- **Membership rail is a dedicated endpoint, not a profile field (issue #423, 2026-09-12)** —
  `GET /api/v1/users/{principal}/orgs` serves the sorted names from `Service.MemberOrgsFor`
  (any roster role, #370 alias matching) instead of a `member_orgs` field on the owner-profile
  doc. Rationale (law 8): the profile route lives in core `internal/api`, which must never import
  the identity package — a field would need a new seam and would hang a LIST-plus-probes fan-out
  off every profile GET; the endpoint keeps that cost on the explicit human-rate rail (profile
  page loads) and leaves `profileETag` untouched (no #382 entanglement — a bio edit rides a
  separate route and can never stale the rail). Visibility: no filtering — every containing roster
  is served to any read-authorized caller (anonymous needs `anonymous_read`, the profile/members
  gate); org visibility governs repos, not roster facts. Client: the user-profile Organizations
  section links each org to `/:org` with an explicit "No organizations" empty state (never
  absent); org profiles omit the section (member principals are email spellings, not routable
  owner slugs per #370 — the roster is managed at organization settings).
