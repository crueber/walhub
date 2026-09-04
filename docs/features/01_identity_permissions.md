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
| `users/<principal>/profile.json` | CAS'd (overwritable family) | `{"version":1,"principal":"jane@example.com","display_name":"Jane Doe","bio":"","created_at":"RFC3339","updated_at":"RFC3339"}` |
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
| `orgs/<org>/org.json` | **CAS'd** (overwritable family) | `{"version":1,"org":"acme","display_name":"Acme Corp","description":"","created_at","updated_at"}` |
| `orgs/<org>/members.json` | **CAS'd** (overwritable family) | `{"version":2,"members":[{"principal":"jane@example.com","role":"owner","joined_at"},{"principal":"sam@example.com","role":"member","joined_at"}],"updated_at"}` |
| `orgs/<org>/teams/<slug>.json` | **CAS'd** (overwritable family) | `{"version":1,"org":"acme","slug":"platform","name":"Platform","description":"","members":["jane@example.com"],"created_at","updated_at"}` |

Write discipline:

- **Create** (immutable `PutMode::Create`) applies only to the *namespace*: creating an org is
  `Create` of `org.json` (a lost race loses the org name — 409 "org already exists"); creating a team
  is `Create` of its `teams/<slug>.json`. Everything else — profile edits, roster changes, membership —
  is a **CAS'd update** (`Update(version)`, retry-on-412 re-read; the canonical CAS loop, 13_concurrency
  §3). CAS is the lock; there is no separate lock object anywhere in this feature.
- **members.json is one object, human-rate.** Rosters are small (dozens), writes are rare (someone
  joins/leaves a team), so whole-roster CAS beats per-member objects: one GET answers the hot P6
  question ("is this principal an owner of `<org>`?") with no LIST. Contention is a non-issue at
  human rate (same reasoning as P2).
- Team membership is a plain string array of principals. Team listing is LIST over
  `orgs/<org>/teams/*.json` — collaboration page, not a git hot path (P5), paginated `n` default 100.
- Deleting a team or an org: team delete removes the object and its `team:<org>/<slug>` bindings from
  any `access.json` that references it (same handler, sequential CAS per affected repo — bounded by
  repos the team is bound to, discovered from `access.json` itself, never a LIST sweep of all repos).
  Org delete is owner-only and REFUSES while any repo is owned by the org (409 with the count).

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
| `visibility` | `"public"\|"private"` | gates anonymous reads (§4.1) — default `"public"` |
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

**Resolution order** is P6 verbatim, with the team expansion made exact: a `team:org/slug` binding
matches a principal iff the principal is in `orgs/<org>/teams/<slug>.json.members`. Resolution is
max-role across: (1) `access.json` bindings (direct + team), (2) org ownership (owner of the org that
owns the repo — owner role in `members.json`), (3) auth principal's `write`/`admin` flags (existing
behavior, mapping write→write, admin→admin), (4) anonymous → `read` iff `visibility == "public"`,
nothing otherwise. First match in that list that yields ANY role wins; within step 1 the max of
matching bindings applies. Resolution result is memoized per-request; no lock is involved — it is two
bucket GETs worst case (`access.json`, one `teams/<slug>.json` per referenced team, bounded by the
binding list length).

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
  `visibility == "public"`**.
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
| `users/<principal>/invitations/index.json` | **CAS'd** (overwritable family) | `{"version":1,"entries":[{"id","org"|"repo","role","invited_by","created_at"}],"updated_at"}` — P4-style hot window of pending invites |

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

### Concurrency

Hazard: accepting an invite concurrently from two tabs, or accepting a just-cancelled invite. Avoidance:
accept is a CAS sequence — binding write first (org `members.json` CAS / `access.json` CAS loop), then
invite-object DELETE, idempotent for the loser (a second accept sees 412/absent → done, like the P3
event path). A cancelled-vs-accept race is decided by which CAS lands first; the loser gets 409
"invitation no longer pending" and writes nothing. Inbox index updates happen after the commit in the
same handler (P8 shape); a crash drops one inbox entry — the issuer-side list is the truth.

## 8. API endpoints

Wire conventions per 07 §2: plain-text errors, `[]` not `null`, RFC 3339 UTC, per-segment decoding,
SWR/ETag or `no-store` cache classes, both lanes everywhere. All registered by the `identity`
RouteProvider (Seam 1).

### Top-level (`/api/v1` + `/api-browser/v1` twins)

| Method + path | Auth (P6) | Request → response |
|---|---|---|
| `GET /api/v1/users/{principal}` | any (public read) | → `{profile}`; 404 unknown |
| `PUT /api/v1/users/{principal}` | self or admin | body = profile → 200 profile; 400 invalid |
| `GET /api/v1/orgs` | any (SWR) | → sorted `["acme", …]` |
| `POST /api/v1/orgs` | write | `{org, display_name}` → 201 `{org}`; 409 taken; creator becomes owner |
| `GET/PUT/DELETE /api/v1/orgs/{org}` | read / owner / owner | profile CRUD; 409 on delete with repos |
| `GET/PUT/DELETE …/members/{principal}` | read / owner / owner | roster ops; last owner removal → 409 |
| `GET/POST …/teams`, `GET/PUT/DELETE …/teams/{slug}` | read / owner | team CRUD |
| `PUT/DELETE …/teams/{slug}/members/{principal}` | owner | membership edit |
| `POST /api/v1/orgs/{org}/invitations` | owner | `{email, role}` → 201 `{id, accept_url}` |
| `GET /api/v1/invitations` | any authed (no-store) | → my pending invites `[]` |
| `GET /api/v1/invitations/{id}?token=` | token OR subject match | invite summary (signed-link preview) |
| `POST /api/v1/invitations/{id}/accept` | authed, subject match | → 200 `{bound: "org"\|"repo"}`; 409 not pending |
| `DELETE /api/v1/invitations/{id}` | invitee (decline) or issuer (cancel) | → 204 |

### Repo-scoped `/{o}/{r}/api` (+ `/api-browser` twin via `api.Lanes`)

| Method + path | Auth | Request → response |
|---|---|---|
| `GET …/access` | triage | → `{version, visibility, role_bindings[]}` |
| `PUT …/access` | admin | full doc incl. `version` → 200 `{version}`; 409 stale version; 400 invalid subject/role |
| `POST …/invitations` | admin | `{subject, role}` → 201 `{id, accept_url}` |
| `GET …/invitations` | admin | → pending list |
| `DELETE …/invitations/{id}` | admin | → 204 |
| `DELETE …/api` | admin | → 204 (core §9.1 lifecycle; delete + fork/GC semantics §5.1) |

## 9. UI and SDK

Pages (vanilla ESM SPA per 12_web_ui.md; `<template>` + reactive core, `useData` 5 s TTL):

- **Org settings** `/:org/settings` — sub-tabs profile / members / teams / invitations; member rows
  inline role `<select>`; invite form shows the returned accept link.
- **Team page** `/:org/teams/:slug` — member list, add/remove, and the repos this team is bound to
  (derived by reading that org's repos' `access.json`; bounded by the org's repo count, P5-acceptable).
- **Repo Access tab** `/:owner/:repo/settings/access` — a fourth settings sub-tab: visibility toggle,
  role-binding table (subject, role, remove), add-binding form (user or team autocomplete), CAS version
  in the footer; save = full-doc PUT, 409 renders "changed under you, reload".
- **Profile** `/:owner` renders user or org profile (existing route gains the org variant).

SDK additions (submodules under `web/sdk/src/`, bundled by esbuild into `repos.js`; JSDoc typedefs in
`types.js`): `users.js` (`users.get/put`), `orgs.js` (`orgs.*`, members, teams), `access.js`
(`repo.access.get/put`), `invites.js` (`invites.list/mine/accept/cancel`).

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

- **Principal = email; profiles are the only identity objects here** — keys/tokens/sessions stay auth-provider territory (Seam 2); this doc owns zero credentials.
- **`visibility` lives in `access.json`** — one CAS'd object already admin-write and read at every authz decision; a separate visibility object would double the read.
- **Private-repo read gating via the named `require_read` hook** (14 §14.10.1) — policy.json stays push-only (its frozen contract), the gate is a registered hook, not a policy effect.
- **`members.json` is one CAS object; team `members[]` is a string array** — human-rate writes, owner checks in one GET, and mention/policy expansion needs principals only.
- **Invites are Create-only, delete-on-transition; the inbox is a P4-style index** — an invite that can be rewritten is a second writer of role state, and "my invites" must never enumerate orgs/repos.
- **Legacy repos synthesize on read, then materialize lazily; `access.json` edits are full-document `PUT`s** (no per-binding endpoints) — zero-downtime adoption, no per-binding endpoint surface; matches the policy/settings PUT class.
- **Repo delete stays a core lifecycle op; this doc owns only its gate and fork semantics** (issue #39) — `DELETE …/api` predates the collaboration layer, so the settings Danger Zone needed no new endpoint, no new SDK method, and no new backend tests; §5.1 pins the admin gate and the fork/GC delete rule instead.

## Explicitly out of scope

- **SSH keys, tokens, sessions, password flows** — auth-provider territory (Seam 2); this layer reads principals only.
- **SAML/SCIM, LDAP-synced teams** — P9; a Seam 2 provider maps directory groups onto teams later.
- **Fine-grained per-branch roles, CODEOWNERS-style routing, custom repo roles** — roles are the fixed five-level ladder (P6); finer control is `policy.json` rules (Seam 3).
- **Nested teams, org roles beyond owner/member; audit history; invite emails** — the fixed ladder plus owner/member is the whole org surface; overwritable objects keep no history (a Seam 4 `jsonl` audit sink is the record if needed); SMTP is operator-side.
