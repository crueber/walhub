# 07 — Releases, assets, stars, watches

> Depends on: `01_identity_permissions.md` (P6 roles, principals), `03_pull_requests.md` (merged-PR
> index for changelog autodraft, fork counter), `06_notifications.md` (watch fan-out). Packages:
> `internal/releases` (releases + assets + autodraft) and `internal/social` (stars, watches, counters) —
> both register `api.RouteProvider` (Seam 1, 14_extensibility.md §14.3) and mount on both lanes via
> `api.Lanes`. Wire conventions, cache classes, plain-text errors, `[]`-not-null, and RFC 3339 per
> `docs/go/07_api.md` §2. All concurrency defers to `docs/go/13_concurrency.md`; the CAS loop is the only
> coordination tool — no cross-feature locks, ever.

## 1. Object families

| Key | Body | Ops |
|---|---|---|
| `repos/<o>/<r>/releases/<tag.json>` | Release header | **CAS** (`PutUpdate`; create = CAS against absent), Delete via endpoint only |
| `repos/<o>/<r>/releases/assets/<tag>/<name>` | Asset bytes | **Create** (immutable); Delete only via the asset DELETE endpoint |
| `repos/<o>/<r>/releases/latest.json` | Latest-release pointer | **CAS**; repaired lazily |
| `repos/<o>/<r>/meta/social.json` | Star/watch/fork counters | **CAS** |
| `users/<principal>/starred/<o>/<r>.json` | Star record | **Create** / Delete (CAS Create-Delete family) |
| `users/<principal>/watching/<o>/<r>.json` | Watch record | **Create** / Delete |

`<tag.json>` means the tag, percent-encoded into one key segment (`%` → `%25`, `/` → `%2F`). Git
tag names MAY contain `/`, and walhub does NOT lowercase or otherwise normalize them — the encoding
is the only transformation. API paths carry the same encoding (clients `encodeURIComponent` the tag
segment; handlers decode per segment per 07_api §2 and MUST re-encode when building the key).
The CAS'd JSON objects (`releases/*.json`, `latest.json`, `social.json`) MUST be added to the frozen
overwritable-key list (same amendment class as `issues/index.json` in D-EXT-2). Asset bytes and
star/watch records are Create-only: no writer ever overwrites them; removal is an explicit
store-delete performed by the DELETE endpoints (releases are deletable content, like the thread they
annotate). LIST over `releases/*.json` is permitted (P5 — collaboration page, paginated, page size
named in §6).

### 1.1 Release header schema

```json
{
  "tag": "v1.2.0",             // decoded tag; the key carries the encoded form
  "tag_sha": "cb38da1…",       // full 40/64-hex commit the tag points at, snapshotted at creation
  "name": "Release 1.2.0",     // display title; defaults to the tag
  "body": "## Changes\n- …",   // markdown, rendered client-side (markdown-lite, 12_web_ui §2.1)
  "draft": false,
  "prerelease": false,
  "author": "jane",
  "created_at": "2026-09-01T12:00:00Z",
  "published_at": "2026-09-01T12:00:00Z",   // null while draft
  "updated_at": "2026-09-01T12:05:00Z",
  "assets": []
}
```

Asset entries (in `assets[]`, `[]` when empty — never `null`):

```json
{"name": "walhub-linux-amd64", "size": 18432, "sha256": "<64-hex>",
 "content_type": "application/octet-stream", "uploaded_at": "…", "uploader": "jane"}
```

- **Create vs CAS:** a release is created by CAS-with-absent-version and thereafter mutated only by
  CAS (`Update(version)`); drafts are published by flipping `draft` in a CAS; edits (name/body/flags)
  are CAS. `tag` and `tag_sha` are immutable after creation.
- **Tag validation:** the tag MUST exist as `refs/tags/<tag>` at write time, resolved via the `RepoView`
  interface (`07_api.md` §1 — `Resolve`). The tag sha is snapshotted; later tag moves do NOT rewrite the
  release (the snapshot is the record of what shipped). A tag without a release is just a tag; a release
  whose tag is deleted keeps serving (the snapshot stands).
- **One release per tag:** create against an existing release key → `409 conflict` (plain-text body).
- Draft semantics (deviation from GitHub — see Decisions): a release is always bound to an existing
  tag; `draft: true` merely hides it from `latest`, the public list, and notifications until published.

### 1.2 Asset objects

Asset bytes are stored like LFS objects: `repos/<o>/<r>/releases/assets/<tag>/<name>`, raw bytes,
immutable `Create`. (The assets live in their own subtree rather than nested under the header
file: `releases/<tag.json>/assets/…` would make the header path both a file and a directory,
which the filesystem backend cannot store and no other object family does — keys stay
prefix-free between files and directories on every backend. The HTTP byte route is unchanged.) The JSON entry (in the release header) carries `sha256` + `size`; the object itself
carries no metadata. Serving is the static contract (`docs/go/06_server_http.md` §5): strong ETag = store
version, `Range`/`If-Range` → 206/416, `Cache-Control: public, max-age=31536000, immutable`,
`Content-Type` = the stored/declared type, accel offload eligible. Byte path:
`GET|HEAD /{o}/{r}[.git]/releases/{tag}/assets/{name}` — a new repo sub-path family (routing note
required by 14_extensibility §14.3): it lives in the `/{o}/{r}/<sub>` fallback subtree, is NOT under the
api lanes, and MUST be registered by the same provider in the static (uncompressed) group, outside the
compress groups.

**Upload flow (two-step, normative):**

1. `POST /{o}/{r}/api/releases/{tag}/assets/{name}` with the raw bytes, required `Content-Length` and
   `X-Walgit-Asset-Sha256` (hex). Cap `releases.max_asset_bytes` (default 2 GiB) → `413`. The handler
   streams to a cache-dir spool (LFS §6.2 pattern), verifies sha256 + size **before** the store write,
   then `Create`s the object. An existing object with the same sha256 is idempotent success (dedup);
   an existing object with a different sha → `409`.
2. CAS the release header, appending the asset entry (read-modify-write loop; concurrent asset uploads
   to one release both survive). If the process dies between (1) and (2): orphan bytes, harmless —
   re-upload of the same name succeeds idempotently; a DELETE of the entry removes the bytes.

Asset `name` MUST be a single path segment: no `/`, no leading `.`, 1–200 bytes, percent-decoded once
per 07_api §2 path rules.

#### Concurrency (releases + assets)

Hazard: two writers CAS the same release header (edit + asset attach) — lost update — and two asset
uploads of the same name racing the byte Create. Avoidance (13_concurrency.md: CAS loops are the only
tool; no locks): every mutation of `<tag.json>` is a read-modify-write `PutUpdate(version)` loop; asset
attach re-reads, appends, retries on `PreconditionFailed` (412). Byte `Create` arbitrates name races at
the store (create-if-absent); a 412 on the bytes Create is resolved by comparing the declared sha256 —
match = done, mismatch = `409`. The two-step order is always **bytes first, header CAS second**: a crash
leaves an unreferenced immutable object, never a header entry pointing at missing bytes. Handlers hold
no repo locks across store calls (13 §2 rule 4); the only git-layer call on this path (tag resolution,
§4) is a bounded `RepoView.Resolve`.

## 2. Latest-release resolution — DECIDED: CAS'd pointer with monotonic CAS + self-healing scan

`repos/<o>/<r>/releases/latest.json` = `{"tag": "…", "created_at": "…", "updated_at": "…"}`.

- **Write rule:** the publishing handler (create-published or draft→publish flip) CAS-updates the
  pointer **only if** the new release's `created_at` is greater than the pointer's current target's
  `created_at` (re-read inside the CAS loop). The comparison is monotonic, so concurrent publishers of
  two releases converge on the newest regardless of CAS order; the loser's CAS is a skip, not a retry.
- **Read rule:** GET latest reads the pointer, then GETs the target release and verifies it exists,
  `draft=false`, and (`prerelease=false` unless `?include_prereleases=1`). On any mismatch the handler
  falls back to a **bounded scan** — LIST `releases/*.json`, fetch bodies, filter published, pick max
  `created_at`, cap 100 objects — serves that result, and lazily repairs the pointer with the same
  monotonic CAS. Deletion of the latest release repairs the pointer synchronously in the DELETE handler
  (same scan).
- Rationale: the pointer keeps the repo-home "Latest" badge at O(1) GETs (hot path); releases are
  human-rate, so the extra CAS per publish is free; the scan is the correctness backstop, bounded, and
  off the git hot path (P5 permits it).

#### Concurrency (latest pointer)

Hazard: last-writer-wins could park the pointer at an older release. Avoidance: the created_at compare
inside the CAS loop makes the pointer monotonic — an older publish racing a newer one no-ops; a
stale/dangling pointer self-heals on the next read via the bounded scan, and the repair CAS is
idempotent. No cross-feature lock; a repo delete simply removes the whole prefix.

## 3. Release creation and changelog autodraft

`PUT /{o}/{r}/api/releases/{tag}` with body `{name?, body?, draft?, prerelease?}` (all optional,
defaults: name = tag, body = "", draft = false, prerelease = false). Handler order: resolve
`refs/tags/<tag>` (404 `unknown revision` per 07_api §2 when absent) → snapshot `tag_sha` → CAS-create
the header → if published, CAS the latest pointer (§2) → fan out synchronously per P8: notifications to
watchers via 06's fan-out (reading this doc's `users/<p>/watching/…` records) and webhook enqueue — in
the same request, after the CAS commits. SSE `release` event on the repo stream (§7).

**Autodraft** (`GET /{o}/{r}/api/releases/autodraft?tag=&since=`, read-level, synchronous): produces the
suggested `body` for the release form.

- `tag` (required) must resolve as a ref; `since` defaults to the current latest release's tag (§2),
  else the second-newest tag by `created_at`, else the repo's first commit.
- Source of truth: 03's PR index (`repos/<o>/<r>/pulls/index.json`, P4) — entries with a recorded
  merge commit and `merged_at` in the window. Window test is decidable locally: for each candidate
  merge sha, `git merge-base --is-ancestor <merge_sha> <tag_sha>` AND NOT
  `--is-ancestor <merge_sha> <since_sha>` (git subprocesses under the per-repo semaphore, 13 §4).
- Output: `{tag, since, body, prs:[{num, title, author}]}` with `body` =
  `"- #12 Title (@author)"` lines, newest merge first, capped at 100 PRs (`more: true` beyond).
  Field names inside 03's PR header are 03's contract; this doc depends only on "merged PRs expose
  (number, title, author, merge sha, merged_at)".

## 4. Stars

- Record: `users/<principal>/starred/<o>/<r>.json` = `{"repo": "<o>/<r>", "starred_at": "RFC3339"}` —
  **Create/Delete only**, never rewritten. The record is the source of truth for "did principal P star
  this repo"; the count is denormalized.
- Count: `repos/<o>/<r>/meta/social.json` = `{"stars": N, "watchers": N, "forks": N, "updated_at": "…"}` —
  **CAS'd**. Star = CAS loop incrementing `stars`; unstar = decrement floor 0. Contention is negligible
  (same reasoning as P2: starring is human-rate, CAS retries are ~never taken; document, don't queue).
- Auth: authenticated principal (never anonymous) AND the repo visible to them at read level (P6
  resolution — an `anonymous_read` repo is starrable by any signed-in user). PUT/DELETE star are
  idempotent (repeat = same state, 200).
- The star's visibility rule is per-user; the count is global. Starring a repo the principal can no
  longer see: allowed (unstar must always work); the record simply stands.
- `repos/<o>/<r>/meta/` is the P1 collaboration-metadata prefix; `social.json` joins the overwritable
  family alongside it.

#### Concurrency (social counters)

Hazard: star/unstar/watch/unwatch and 03's fork completion all CAS the same `social.json`; a naive
read-modify-write can clobber a concurrent field update. Avoidance: one canonical loop — read, mutate
ONLY the caller's field, `PutUpdate(version)`, retry on 412; monotonicity is not required (inc/dec
both exist), but every mutation path uses the same loop, so interleavings converge. Contention is
negligible (human-rate events; documented per P2 reasoning). Cross-process arbitration is the CAS
alone (no singleflight, no cross-feature locks — the header rule stands); same-process `Star` calls
additionally serialize on a sharded per-Service gate (#69, tightened by #97: 64 fixed stripes keyed by
(repo, principal) — the star decision spans two keys — record + counter — so two CAS loops cannot make
check-and-bump atomic, but serialization is needed only where the keys coincide). The gate is `Star`-only
(never nested, `defer`-released; `Unstar` keeps its version-conditional Delete token, reads stay lock-free),
a shard is held across at most one record GET + one record PUT + one ≤8-attempt counter CAS loop (small
control-plane JSON only — no bulk, no git, no LIST), so a slow store call stalls at most its ~1/64 shard,
and the resync decision is evaluated inside the counter CAS loop so retries re-check the latest state.
`updated_at` is set by whichever write wins last.

#### 4.1 Deleted repos — miss-tolerant reads, guarded writes, resync on recreate (issue #63)

Repo deletion sweeps only the `repos/<o>/<r>/` prefix; star/watch records under `users/` survive it,
and there is deliberately NO reverse index (enumerating people is not a feature — 01). So:

- **Reads tolerate:** `Starred` skips entries whose manifest is absent (one `HEAD` per entry; probe
  errors keep the entry); `ViewerState` reports `(false, false)` for ghosts; `Counts` (hence
  `GET …/api/social`) is `404`.
- **Writes fail closed:** `Star` on a ghost is `404` (otherwise every star would mint a fresh record the
  sweep can never clean); `Unstar` still deletes the record but skips the counter CAS (bumping would
  lazily resurrect `social.json` for a swept repo).
- **Recreate resyncs:** a delete+recreate zeroes `social.json` while the stale record survives, so the
  already-starred path repairs — absent or freshly-zeroed counter plus a live record repairs with one
  `+1` (a corrupt counter keeps the old tolerance: record is the truth, count reads `0`). A `412` on
  the record Create is still a pure race (the concurrent Star owns the bump): recount, no repair.
  Known limit, no reverse index to fix it: only the zero state repairs, so the FIRST stale starrer
  after a recreate reconverges and later ones assume inclusion — a second stale star reconverges via
  unstar/restar.
- **Cost:** one manifest `HEAD` per star/unstar/watch-mutation and per listed record (E8 budgets carry
  the `+1 HEAD` lines); watch re-watches self-heal through the existing field-scoped CAS (the fresh
  list cannot contain the stale principal), and fork counts on a recreated parent stay reset (no
  recount without the reverse index).

## 5. Watches (cross-ref 06)

- Record: `users/<principal>/watching/<o>/<r>.json` = `{"repo": "<o>/<r>", "watched_at": "RFC3339"}` —
  Create/Delete, idempotent, same auth rule as stars (§4).
- Ownership boundary: **this doc owns the record and the count; 06 owns notifications.** 06's fan-out
  reads `users/<p>/watching/*` as the repo-watch subscription source. Release publish (and, via their
  own handlers, issue/PR/review events) enqueue notifications per P8 in the mutating handler, after the
  CAS commits.
- Count: `watchers` field of `social.json`, same CAS loop discipline as `stars`.
- Watching ≠ starring: a watch delivers notifications (06); a star is only a social signal + list entry.

## 6. Fork counts

`forks` lives in the same `social.json`. The fork flow is 03's (`ForkRepo` task, P7): its completion
step MUST CAS-increment the parent's `social.json.forks` (same loop as §4). 07 owns the field's shape
and nothing else about forks. A repo cannot fork-count itself; the field starts at 0 when
`social.json` is first created (lazily, on first mutation — a repo with no social object reports all
counts as 0).

## 7. API endpoints

Plain-text errors, `[]`-not-null, RFC 3339, per-segment encoding (07_api §2). Cache classes: single
release / list / social GETs are ref-dependent class (SWR 60 s + `ETag: "<store version token>"`,
`If-None-Match` → 304); asset BYTES are the static contract (immutable). `If-Match` on mutating PUTs is
optional optimistic concurrency (the GET's ETag value); without it, last-writer-wins via the CAS loop.

Registration: provider `releases` (`internal/releases`) and provider `social` (`internal/social`),
both `api.RouteProvider` (Seam 1), repo-scoped routes mounted via `api.Lanes` on both lanes; the asset
byte route is registered in the static group (no compress, accel eligible).

| METHOD + path | Auth (P6) | Request → Response | Provider |
|---|---|---|---|
| GET `/{o}/{r}/api/releases?n=&after=` | read | → `{releases: [Release], more}`; created_at desc, `after` = cursor `(created_at,tag)`, n default 50 max 100 | releases |
| GET `/{o}/{r}/api/releases/latest[?include_prereleases=1]` | read | → Release \| 404 `no releases` | releases |
| GET `/{o}/{r}/api/releases/{tag}` | read | → Release | releases |
| PUT `/{o}/{r}/api/releases/{tag}` | write | `{name?, body?, draft?, prerelease?}` → Release; unknown tag 404; duplicate 409 | releases |
| DELETE `/{o}/{r}/api/releases/{tag}` | maintain | → 200; removes header + asset bytes; repairs latest pointer | releases |
| GET `/{o}/{r}/api/releases/autodraft?tag=&since=` | read | → `{tag, since, body, prs: [], more}` | releases |
| POST `/{o}/{r}/api/releases/{tag}/assets/{name}` | write | raw bytes; `Content-Length` + `X-Walgit-Asset-Sha256` → asset entry; 413 over cap, 409 sha clash | releases |
| DELETE `/{o}/{r}/api/releases/{tag}/assets/{name}` | write | → 200; CAS-removes entry + deletes bytes | releases |
| GET/HEAD `/{o}/{r}/releases/{tag}/assets/{name}` | read | bytes; static contract (06 §5) | releases |
| PUT `/{o}/{r}/api/star` | authenticated + visible | `{}` → `{stars}`; idempotent | social |
| DELETE `/{o}/{r}/api/star` | authenticated | `{}` → `{stars}` | social |
| PUT/DELETE `/{o}/{r}/api/watch` | authenticated + visible | `{}` → `{watching, watchers}` | social |
| GET `/{o}/{r}/api/social` | read | → `{stars, watchers, forks, viewer: {starred, watching}}` | social |
| GET `/api/v1/me/starred?n=&after=` | authenticated | → `{starred: [{repo, starred_at}], more}`; repo (owner/name) ascending, n default 50 max 100, `after` = `<starred_at>|<repo>` of the previous page's last entry | social |
| GET `/api/v1/users/{principal}/starred?n=&after=` | read | same shape; entries naming a deleted repo are skipped (miss-tolerant reads, §4.1) | social |

Release JSON on the wire = the §1.1 body + `browser_download_url` per asset + `assets: []` when empty.
Errors are plain text (`unknown revision`, `conflict`, `asset too large`).

#### Concurrency (handlers)

Hazard: the autodraft's per-PR `merge-base --is-ancestor` calls are git subprocesses on a request
goroutine. Avoidance: bounded (≤ 100 candidates × 2 short argv runs) under the per-repo git semaphore
(`server.max_concurrent_per_repo`, 13 §4); a bigger backfill would be a task — it is not needed at
human rate. Star/watch PUT-DELETE races are CAS-arbitrated on `social.json`; record Create/Delete are
idempotent, so a duplicate create 412s into a re-read that observes the record already present and
returns success. Handlers never hold repo locks across store calls (13 §2 rule 4); no new locks are
introduced by this feature.

## 8. UI and SDK (shapes for 08)

Pages (SolidJS SPA, `web/src/pages/*.jsx`, patterns of `12_web_ui.md`; 08 owns the route table):

| Route | Page |
|---|---|
| `/{o}/{r}/releases` | Release cards: tag, name, draft/prerelease badges, date, asset count; "New release" button (write+) |
| `/{o}/{r}/releases/{tag}` | markdown-lite rendered body (allowlist sanitizer, D-WEB-5), assets table (name, size, sha256 short, download), edit/publish/delete per role |
| `/{o}/{r}/releases/new` | tag picker fed by the existing ref stream, autodraft button fills the body, draft/prerelease checkboxes, asset upload (client hashes via `crypto.subtle.digest`, streams `POST …/assets/{name}`) |

Repo chrome gains a star toggle + watch toggle (optimistic update, rollback on error) fed by
`GET …/api/social`. SSE: pages consume the repo stream's `release` event
(`{tag, action: "published"|"edited"|"deleted"}`, same envelope per 07_api §6) to refresh lists; no new
stream is opened.

SDK submodules (bundled per D-WEB-2): `web/sdk/src/releases.js` — `listReleases, getRelease, latestRelease,
putRelease, deleteRelease, uploadAsset (computes sha256), deleteAsset, autodraft`; `web/sdk/src/social.js` —
`star, unstar, watch, unwatch, social, myStarred, userStarred`. Both exported through `ReposClient`
(12_web_ui §1.1); every call rides the lane rules and envelope handling already in the SDK.

## 9. Task kinds

None registered by this feature: asset upload/delete and autodraft are synchronous and bounded (streamed
cap 2 GiB; ≤ 100-PR autodraft). The P7 example "release asset imports" has its seam ready — a
`release-asset-import` `maintain.Kind` (Seam 5, `(repo, kind)` single-flight) is where a bulk
server-side copy (e.g. from a fork parent) lands if ever wanted; v1 does not register it.

## Decisions

- **Frontend idiom is the SolidJS SPA (D-WEB-6; docs fix for issue #76).** The §8 page sketches read in the shipped idiom: `.jsx` pages under `web/src/pages/`, shared components per 08. Routes and wire shapes are unchanged.

- Releases are always bound to an existing tag (no tag-less drafts): validation is decidable at
  receive-time via `RepoView.Resolve`; GitHub's claim-a-future-tag drafts need a tag-creation event
  channel walhub does not have.
- Latest = CAS'd pointer with monotonic created_at CAS, plus bounded self-healing scan: O(1) hot read,
  correctness never depends on the pointer.
- Asset bytes are keyed by asset NAME (not sha), immutable Create-only; sha256+size live in the header;
  same-name re-upload is idempotent only on sha match.
- Upload = spool-verify-then-Create bytes, then CAS-append the header entry (two-step, P8-style
  crash tolerance: orphans are harmless, dangling entries are impossible).
- Stars/watches are per-user Create/Delete records; counts are denormalized in `meta/social.json` via
  CAS loops — no cross-feature locks; 03 owns the fork increment.
- No private-read filtering of starred lists (14's private-read ACL deferral); unstar is always allowed.
- No task kinds in v1; the Seam 5 import hook is named, not built.
- New repo sub-path family `/{o}/{r}/releases/{tag}/assets/{name}` for bytes (static contract) — the
  spec note 14.3 requires.
- **Starred lists are repo-ordered keyset pages, not starred_at-desc (#65, 2026-09-04):** star keys
  are repo-keyed, not time-ordered, so a newest-first page would have to GET every record to sort —
  O(total stars) GETs per page load with no backend bound. Each page is now one LIST resumed at the
  `after` repo's key with at most n+1 record GETs + n+1 manifest HEADs (O(page), flat in the total;
  the n+1st probe decides `more` exactly; later pages never re-probe earlier ones). The `after`
  cursor keeps its `<starred_at>|<repo>` shape (the timestamp echoes the record; resumption keys off
  the repo), so in-flight clients keep parsing — but cursors are single-session hints and the order
  change resets in-flight pagination. Skip-on-error is preserved (dead repos, corrupt records).
  No reverse index, no users LIST, no new global scan: star history graphs / the stargazers page
  stay deferred per "Explicitly out of scope".
- **Implementation notes (2026-09-04, recorded with the code):** watch PUT/DELETE routes stay
  registered by `internal/notify` (06 §6 is landed and tested — one HTTP owner; the §7 provider
  column for watch is superseded). `social.json` converges through dual field-scoped CAS loops
  (notify: watch fields only; social: stars/forks only) per §4.1 — no route move, no second
  writer conflict. Autodraft reads the SHARED `issues/index.json` (02-owned) plus `pr.json`
  sidecars (03 owns the shape): the planned `pulls/index.json` was never adopted by 03, and the
  shared index carries the same P4 hot-window semantics; LIST backfill for pre-window merges is
  deferred (same class as P4). PUT is idempotent create-or-update (201/200): the §1.1
  "duplicate 409" describes the store-level absent-CAS (concurrent creators converge via
  re-read-as-update) — retry-safe publishes are what the P8 backfill contract needs; 409 stays
  for asset sha clashes and stale `If-Match` (exact token; `*` = update-only per RFC —
  must-exist, 404 when absent). The public list hides drafts; GET single serves
  drafts under the read gate. `latest` and `autodraft` are reserved single-segment names: a
  tag literally named `latest` stays creatable/deletable/listed but its single-GET is
  shadowed by the pointer route — forced by the spec's own §7 route table (`GET …/latest`
  and `GET …/{tag}` collide there by construction, so no implementation could serve both). The users starred twin is open (public info per "no private-read
  filtering"). Autodraft git argv, named here because 04_git pins feature argv by reference:
  `rev-parse --verify --quiet refs/tags/<tag>^{commit}` (peeled), `merge-base --is-ancestor`
  (§3), `for-each-ref --sort=-creatordate --format=%(refname:strip=2) refs/tags` (previous-tag
  default). Asset bytes serve direct (accel offload eligible, left to the edge). New config key
  `releases.max_asset_bytes` (2 GiB default; reflective section — setup/env/validation free).
- **Atomic concurrent Star (issue #69, 2026-09-04):** same-principal concurrent `Star`
  double-counted in the post-recreate zero window — the record-Create winner's `+1` plus a late
  observer's resync `+1` fired from a separate pre-read (check-then-act across two CAS windows;
  `TestStarConcurrentConverge` flaked 3–4 instead of 2). The resync decision (absent/zero ⇒ one `+1`,
  nonzero ⇒ returned as-is, corrupt ⇒ `0` with no write) is now evaluated inside the counter CAS loop
  so every retry re-checks, and `Star`'s record→counter mutation serializes on a leaf per-Service
  gate (`Star`-only, never nested, `defer`-released; a zero `Service` skips the gate and relies on CAS
  alone). Single-call semantics are unchanged — idempotent re-Star, unstar floor 0, #63 lazy tolerance
  (record-first, miss-tolerant reads) — and cross-process same-instant races keep the #63
  unstar/restar convergence. Regression: `TestStarConcurrentConverge` stress
  (`-race -count=20`) plus the deterministic `TestStarConcurrentResyncConverges` (absent/zeroed table).
- **Sharded star gate (issue #97, 2026-09-05):** route (b) — the #69 leaf gate was process-global and held
  across store calls, serializing every `Star` on the instance behind one slow call (a new lock outside the
  13 §2 closed list). Pure CAS (route a) was rejected, not disproven-lightly: the create-path bump must stay
  unconditional (conditioning it on absent/zero undercounts every already-starred repo —
  `TestStarUnstarIdempotent` pins second-principal → 2), while a late observer cannot distinguish a
  just-created record (bump in flight) from a stale one (repair due), so no per-call CAS condition separates
  them. The gate stays but is sharded: 64 fixed stripes (FNV-1a, stdlib only, no per-key map) keyed by
  (repo, principal) — the exact pair whose record Create and counter bump must be atomic with each other.
  Different principals converge via CAS alone (unconditional bump vs conditional resync); different repos
  share nothing. Hold bound is documented at the `lockStar` site (≤ 1 GET + 1 PUT + one ≤8-attempt CAS loop,
  control-plane JSON only, `Star`-only, never nested). Regression:
  `TestStarSlowStoreDoesNotStallUnrelatedRepo` (wedged store op on one repo while another repo's `Star`
  completes) plus the #69 stress tests green `-race -count=20`; coverage stays ≥95%.
- **Repo-delete userspace hygiene (issue #63, 2026-09-04):** miss-tolerant reads + guarded writes +
  recreate resync per §4.1 — no reverse index (enumerating people stays a non-feature), no tombstones
  (per-repo state must die with the prefix), no repair-on-recreate scan. Rationale: readers can probe
  the manifest (one HEAD, fail-open on store errors so a blip never mass-hides lists); writers must
  fail closed (ghost writes mint un-cleanable records); the counter heals per stale starrer because a
  global recount is unrepresentable without the index.

## Explicitly out of scope

- Release archives (auto-built zip/tar.gz of the tag) — needs a build task + cache policy; seam: a
  `release-archive` task kind + static-contract serving, same shape as assets.
- Download counts / per-asset analytics (social.json would grow unbounded).
- Star history graphs, "stargazers" listing page (would need a reverse index over `users/*/starred` —
  a global scan; deferred until a star index format is decided).
- GitHub-style draft releases with not-yet-existing tags; release notes from commit messages (only
  merged-PR autodraft here).
- Watching granularities (issues-only, releases-only — one watch flag only); team/org-level watches.
- Migrating releases from other forges; LFS-backed asset dedup across repos.
