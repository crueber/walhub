# 10 — Import Git repositories from GitHub / any git source (with UI)

> Status: implemented. Depends on 01 (identity/permissions — the P6 gates
> and importer-admin), 03 §7 (the fork shape this mirrors: manifest-CAS
> commit point, Create-conflict = name taken). Seams:
> `docs/go/14_extensibility.md` (§14.3 routes, §14.7 task kinds, §14.12
> additive rules); git argv per `docs/go/04_git.md` §12; wire conventions
> per `docs/go/07_api.md`; concurrency per `docs/go/13_concurrency.md`.
> Plan: Forgejo issue crueber/walhub#21 (body) as revised by R1 (newest
> comment) — on any conflict R1 wins; this doc condenses plan+R1 into the
> repo's doc style and records every R1 resolution in Decisions.

## 1. Scope and the one architectural law

Import is a **git-side operation with a collaboration-grade front door**:
the WAL stays git-only (imported refs/objects ride the normal publish
path — no new WAL kind, 14 §14.11 rule 1), while auth, progress narration,
and provenance follow the collaboration primitives (P6 gates, P7 tasks +
SSE, P1 bucket sidecars). Core (`internal/store`, `internal/wal`,
`internal/git`) never learns the word "import": everything lives in
`internal/repoimport` as a `RouteProvider` (Seam 1), task kind
`repo-import` (Seam 5), and a CLI verb (Seam 7).

**IN scope:** any `git clone`-able `https://` source (GitHub shorthand +
canonical GitHub URLs normalized to one form), `http://` without tokens,
`file://` behind `import.allow_file_urls` (tests/fixtures); all branches +
tags by default with `--ref` allowlist / default-branch-only narrowing;
sha1 + sha256 sources (target follows source, never converts); private
sources via per-request token (never stored); top-level `POST` + `GET one`
(+ SSE attach) on both lanes; `/import` UI page + `import.js` SDK;
`walhub import --url` CLI through the same Service.

**OUT of scope (v1, explicitly):** LFS content (pointer blobs imported
as-is with a server-side `Notice` + UI note — clones work, checkouts
yield pointer text); submodule recursion (gitlinks stay gitlinks);
`refs/pull/*` + `refs/changes/*` + `refs/review/*` (dropped unless
`include_pull_heads`, heads-only verbatim, never `/merge`), `refs/notes/*`
(dropped unless `include_notes`), `refs/replace/*` + `refs/meta/*` +
`refs/keep-around/*` (always dropped); GitHub API integration (pure-git
v1, no new dependency — law 1); shallow/blobless (`--depth`/`--filter`
have no knobs on this surface — full imports only, the D17 forcing
precedent); CI runs and webhook fan-out beyond the normal push events the
publish path already emits; the `GET` list endpoint and `DELETE`
kill-switch (both cut in R1 — list is instance-memory pagination across
namespaces, cancel-by-ID needs a frozen-code API that does not exist;
drain-cancel is the v1 story).

## 2. Object family (buckets + schemas)

| Key | Kind | Content |
|---|---|---|
| `repos/<o>/<r>/meta/import.json` | Provenance + import CLAIM, Create-once-then-CAS'd (same family as `fork.json`, 03 §7; joins the frozen overwritable list — 14 §14.11 rule 2, same change) | `{version: 1, source_url (canonical, token scrubbed), source_kind: "github"\|"generic"\|"file", requested_refs[], imported_at RFC3339, head_shas {ref: full sha}, importer, format, complete: bool, claim_expires_at (RFC3339, claims only)}` — `complete:false` is the in-progress claim (PutCreated BEFORE the manifest; fix #79), `complete:true` the landed sidecar |

Everything else reuses existing objects: `manifest.pb` (commit point),
`checkpoint.pb ∥ refs.pb`, `wal/*` packs + side files, `access.json`
(identity bootstrap + explicit importer-admin), and **no** `policy.json`
(absent IS the default — allow-all — so import writes none; §6).

## 3. API endpoints + task SSE attach + progress packets

Auth per P6 (S6 order: authenticate → authorize namespace → join-or-start;
a join never precedes auth). Import needs **create rights on the target
namespace** (host `write`/`admin`, org owner, owner-named user, or ≥`write`
from existing bindings — `CheckRole(..., RoleWrite)`; anonymous → real
401 with `WWW-Authenticate: Bearer`). Task-status reads gate on the
namespace (`CheckRead` — the task may exist before the repo does).

| METHOD + path | Auth | Request → response | Seam |
|---|---|---|---|
| `POST /api/v1/repos/imports` (+ `/api-browser/v1` twin) | create-on-namespace | `{source_url, owner, name, token?, refs?[], default_branch_only?, include_pull_heads?, include_notes?, format?, dangerous?}` → `202 {task, target}` (+`joined:true` on a B2 join); `200 {repo, import}` on the idempotent no-op | 1 + 5 |
| `GET /api/v1/repos/imports/{id}` (+ twin) | read-on-namespace | → TaskRecord JSON; with `Accept: text/event-stream` → attach (replay → live → terminal `result`/`error` exactly once) | 1 + 5 |

Wire rules (07 conventions + 14 §14.12): plain-text errors,
`[]`-never-null, RFC3339 UTC, full SHAs, `no-store` on starts, unknown
body keys 400 (fail closed). Status mapping: `400` bad URL/options/
transport, `401` walhub-auth failure (upstream auth failure is a task
`error`, never HTTP 401) + anonymous, `403` insufficient role, `404`
unknown task id on this instance, `409` name taken / foreign manifest /
params-mismatched running import, `422` empty source / no refs after
filter / format mismatch / unreachable source, `413` over
`import.max_bytes`, `503` + `Retry-After: 15` on drain interrupt /
saturated `max_concurrent`, `405` on lane/segment mismatches. Discovery:
both templates in `endpoints[]` via the additive `api.RegisterExposed`
registry (same change, law 12).

**Progress packets (law 7):** `clone` bars parsed from `git clone
--progress` stderr (`Receiving objects %`, `Resolving deltas`) +
`ingest` pack bars + `Notice` phase edges + a 15 s heartbeat floor while
git is silent. Bars dedup by label in the 200-packet replay ring;
terminal `result {repo, source_url, head_shas, format, imported_at}` / `error
{status, message}` exactly once.

## 4. Task flow (the body — HTTP and CLI share it)

Task-scoped scratch `<cache.dir>/import/<o>/<r>.<nanos>/` (unique per
attempt, `defer RemoveAll`, never the serving copy):

1. `git clone --mirror --progress -- <url> <scratch>` (04 §12 argv;
   `GIT_TERMINAL_PROMPT=0`, PATH-only env, token via per-task child env
   `WALGIT_IMPORT_TOKEN_<id>` with the credential helper **host-pinned**
   to `scheme://host` so redirects can't harvest it — S5).
2. Scratch `du` gate (`import.max_bytes` → 413 naming the key — S1; git
   writes scratch directly, so no streaming enforcement is possible) +
   per-pack gate at publish.
3. `git for-each-ref` enumerate (capped by `import.max_refs`), S4 refmap,
   empty-after-filter → 422. Object format follows the source
   (`rev-parse --show-object-format`); `--format` pin mismatches → 422,
   never convert. Source HEAD read for the target symref (fallback
   `refs/heads/main` per 04 §1.2). LFS tracking → terminal `Notice` (S11).
4. Claim point: `meta/import.json` **`PutCreate` with `complete:false`**
   BEFORE the manifest CAS (fix #79). A lost CAS resolves to adopt
   (complete + same source → no-op), resume (in-progress + same
   source → converge below), takeover (in-progress + different
   source + expired lease + no manifest → version-checked CAS to our
   claim), or 409 — never a silent overwrite (B3).
5. Provisional commit: `manifest.pb` **`PutCreate`** (`min_seq =
   seq+1`, `first_state_at = as_of = now` per the `--direct` shape) —
   the CAS still arbitrates name-ownership, but the repo persists only
   when step 8 lands. A lost CAS with our claim live means a
   push/fork/create won the name → 409 (the claim is left for the
   retry; never deleted under a potential live winner).
6. Converge (idempotent — the resume path): packs already durable in
   the store are skipped (HEAD probe, never LIST) with their `.idx`
   still ensured; refs are created only when absent (already-correct
   tips skipped, a ref pointing elsewhere aborts loud 409 — never
   overwrite); then the full bitmap'd repack as tier-2 base (each pack
   through the idx discipline (§6.1): sibling `.idx` installed locally
   BEFORE `AddPack` and uploaded to `wal/<checksum>.idx` after
   (create-if-absent, 412 = success)).
7. Refs + access as before (creates through `RepoHandle.Publish`;
   annotated tags carry peel; HEAD follows the source; importer-admin
   `access.json` explicitly via the identity service (S7 —
   read-modify-write, bounded CAS retries; non-email importers skip
   to the `BootstrapRepo` backstop, narrated); absent `policy.json`
   left absent (= default)).
8. `meta/import.json` completion: version-checked CAS (`PutUpdate`
   on the claim version) flipping `complete:true` with the heads. A
   lost CAS adopts a same-source completion (benign race → no-op),
   else 409/500 loud. Terminal `Notice` + result outcome.

Cancel-before-commit leaves task scratch + possibly orphan pack objects
(harmless — the 14 §14.10.2 orphan philosophy). Any post-claim failure
rolls back to a resumable-or-clean state (fix #79): a manifest this run
created is deleted (the repo never persists half-imported) and the
owned claim follows iff the manifest delete landed; a surviving
manifest keeps its claim so the retry resumes. Safe re-POST matrix:
manifest lost the race → fresh attempt (unique scratch); won + complete
`import.json` + same source → no-op; won + in-progress + same source →
resume-to-complete (202 — never "delete and retry"); in-progress +
different source → 409; manifest without any sidecar → 409 foreign
(imports always claim first, so this is unambiguously not ours).

### Concurrency

Hazard: no repo locks exist pre-create (13 §2 rule 4 — there is no
handle to lock); two imports to one target interleave scratch/publish; a
clone holds a pool slot indefinitely. Avoidance: service single-flight
on `<target>` with the B2 params-aware join-or-409 decided in the
handler BEFORE `TaskTable.Run` (-blind table joins never decide);
exactly-one-winner is preserved at three CAS points (fix #79): the
claim `PutCreate` serializes importers (lost CAS → adopt/resume/
takeover/409, never overwrite), the manifest `PutCreate` still
arbitrates name-ownership against pushes/forks/creates (unchanged —
the literal "commit last" is impossible without core surgery since the
publish path needs the handle `Create` returns, so the commit stays
provisional instead), and the completion `PutUpdate` elects exactly one
completer (lost CAS → adopt a same-source completion); scratch unique per
attempt; clone concurrency gated by `import.max_concurrent` (default 2,
bounded channel — saturation fails loudly, never queues silently);
every git spawn in the bounded pool with ctx timeouts (`clone_timeout`
1800 s, `git_timeout` 300 s — S8); bulk pack uploads through `AddPack`,
never request goroutines; no lock held across I/O; the stream ring is
sender-owned, receivers never close, every goroutine exits via context
(the SSE attach watches the finish channel directly — polling
`snapshot()` alone misses terminals that land with no packet in flight).

## 5. UI: pages/flows, SDK, theming, gating

Top-level `/import` route (`Import.jsx`) — the target repo does not exist
yet, so it cannot live under `/:owner/:name/*`. Linked from `Owners.jsx`
(global "Import repository" button) and `Repos.jsx` (prefills `owner` via
`?owner=`). Four states on one page (form → running → done/error; Solid
signals only): URL field (shorthand/GitHub/any URL, client normalizes +
suggests owner/name), owner/name fields, private-source token field
(password, never echoed/persisted), options (default-branch-only, PR
heads with help text, notes, format), LFS-as-pointers + SSH-unsupported
notes. Running: `clone`/`ingest` bars + scrolling log tail from the SSE
attach (abort stops listening, never the import — the server runs it
detached). Done: link to `/:owner/:name` + head SHAs + no-op card on
re-import. Error: plain-text reason + back-to-form.

SDK: `web/sdk/src/import.js` (`start/get/attach`, lane-aware top-level
paths, `normalizeSource` pure helper) attached on the client in
`core.js`; envelope/SSE parsing reuses `sse.js` via `_call`+`onProgress`
(one parser). Theming: Tailwind classes with `dark:` variants on every
new surface (dark by default). Gating: the form disables for anonymous
(with a sign-in hint) AND honors server 401/403/409 (never client-only).

## 6. Config + CLI

Additive `[import]` section (14 §14.12): `clone_timeout` (1800 s),
`git_timeout` (300 s), `max_bytes` (64 GiB = `server.max_push_bytes`'
compiled default; set both when constraining), `max_refs` (100 000),
`max_concurrent` (2), `allow_private_networks` (false),
`url_allowlist` (empty = GitHub always, other hosts need the list or
`dangerous:true`), `allow_file_urls` (false). Validation fail-closed
(bounds ≥ 1, allowlist entries plain hosts; negatives already rejected
by the reflective size check). Env overlay, `config dump`, and the
reflective setup schema pick the section up with no extra code.

CLI (Seam 7, same Service → same publish/CAS path, 14 §14.9):
`walhub import --url URL owner/name [--ref …] [--default-branch-only]
[--include-pull-heads] [--include-notes] [--token-env VAR] [--format
sha1|sha256] [--dangerous]` (the `--from` classic path and the
`--direct` spec are untouched). Token arrives via env var, never argv.
Exit codes 0/1/2 per 11 §6.3.

### 6.1 Load-bearing implementation notes (code truth, not plan prose)

- **Pre-create task home (B4):** the core table's progress broadcast
  lives on `RepoHandle` (absent pre-create), and `internal/wal` is
  untouchable in this change (S12) — so narration fans out to a
  service-level id-keyed replay ring (200/16/drop, wal.Broadcast
  semantics) from the same `Notice`/`Progress` calls that feed the
  table record. The HTTP/CLI ids are service ids; the table id stays
  internal.
- **The `.idx` discipline:** `AddPack` installs/uploads the `.pack`
  only — the next `LevelServe` Sync then fails fetching
  `wal/<checksum>.idx` (the shipped classic/`wal add-pack` paths share
  this latent gap; no e2e covers them). Import installs each sibling
  `.idx` locally before `AddPack` (regenerating via `git index-pack`
  when the source lacks one) and uploads it create-if-absent after.
- **`api.RegisterExposed`:** discovery derives from the core route
  table, which a feature package must not join (law 8) — the additive
  registry lets composition register feature-owned templates in the
  same change (prior waves ship no discovery entries; this one must).
- **B6 in code terms:** no `maintain.RegisterKind` exists (doc sketch
  only) — kind constant `repo-import` + `repoimport.RegisterKind`
  (panics on duplicate) called once from `cmd/walhub` composition,
  task run through `reg.Tasks().Run(WithoutCancel…)` on the
  Registry-level table under `<target>/repo-import`.

## 7. Credentials + SSRF (never stored, S2/S5)

The token arrives in the POST body (or CLI env var), lives in task
memory only, rides a per-task child env var into the clone spawn, and
is never written to the bucket, `TaskRecord.Params` (scrubbed canonical
URL + options + `secret_set:bool` only), packets, log tails, terminal
errors, or `import.json`. Embedded-URL credentials are refused 400
("strip userinfo; use the token field"); token-over-plaintext-http is
refused 400; `ssh://`, scp-like, and `git://` are refused 400 in v1 (no
key agent on the host — use https + token). `git stderr` is scrubbed
(`password=`/`token=` redaction + userinfo strip) before it reaches any
packet, record, or error. Acceptance: bucket + record + packet + error
grep-clean (tested).

SSRF: deny-private default (loopback/RFC1918/ULA/link-local/multicast
via stdlib `net` parse — no new dep), `url_allowlist` (empty = GitHub
always reachable; other hosts need the list or the explicit
`dangerous:true` confirm), `allow_file_urls=false`. Residuals named:
DNS TOCTOU (check-time vs clone-time resolution can differ) and redirect
following (git follows; the allowlist gates the initial URL while the
token helper's host-pin keeps redirects from harvesting it).

## 8. Performance + EVIDENCE

Import is a **task**, never the push/fetch hot path — zero hot-path
round trips (the push-budget test stays green with the surface
mounted). Per-import control-plane budget over the pinned single-pack
fixture: 12 PUTs + 6 GETs/HEADs, 0 LIST at both S (50 commits) and M
(400 commits) — flat; wall grows with pack bytes only (bulk bytes
excluded by definition). Harness:
`internal/repoimport/evidence_test.go` (`TestEvidenceImportBudget` —
ops RANGE assertions, never exact N; `TestEvidenceImportFlat` —
flatness); entry E11 in `docs/EVIDENCE.md`.

## 9. Acceptance (test map)

`internal/repoimport` ≥ 95% (`make cover`), `-race` clean, the
single-flight/join test stressed `-count=100`; table-driven httptest
per handler (POST matrix, routing edges, GET/SSE, scrub, gates);
file:// e2e over real git (success, SSE replay→live→terminal, no-op,
409s, LFS notice, tag peel, sha256); `make test-web` green (SDK wire +
normalize tests); real-browser pass per ladder §8; `make fmt/vet`
clean.

## Decisions

- **R1 B1 — package `internal/repoimport`** (dir + package identical;
  `import` is a Go keyword). All plan references to `internal/import`
  mean this package; the coverage gate applies to it.
- **R1 B2 — params-aware join.** Handler flow is authenticate →
  authorize namespace → load any running task for the target key →
  compare canonical source URL + refs/options/format (NOT the
  importer — two authorized users share one import of the same
  source): match → join (bounded by joiner ctx); mismatch → **409**
  naming the running import. The comparison lives in the handler
  BEFORE `TaskTable.Run` (extracted as `joinOrConflictLocked`, unit-tested
  directly); the table's blind `(repo,kind)` join is the backstop, never
  the decision.
- **R1 B3 — manifest-present ∧ import.json-absent → 409** ("target
  exists but was not created by import; delete and retry, or pick
  another name"), unless `import.json` matches the canonical source
  (→ idempotent no-op, zero pack traffic — heads are NOT re-fetched;
  upstream movement after a landed import is delete-and-retry).
  Corrupt/unreadable `import.json` never adopts (409). Never silently
  adopt a foreign manifest.
- **R1 B4 — pre-create task home.** The task starts on the
  Registry-level core `wal.TaskTable` under `<target>/repo-import`;
  progress/attach is served from the service-level id-keyed replay
  ring (table-level, always working — before Create, after Create,
  after finish within retention) — NOT the per-handle broadcast.
- **R1 B5 — DELETE kill-switch CUT for v1.** No per-task cancel API
  exists (`Run`/`Get`/`List`/`Drain` only); drain-cancel is the v1
  cancellation story. Ship: `POST` start + `GET` one (+ SSE attach).
- **R1 B6 — registration in code terms.** No `maintain.RegisterKind`
  (doc sketch only): kind constant + `repoimport.RegisterKind`
  (panics on duplicate) from `cmd/walhub` composition; task body via
  `reg.Tasks().Run(…)` on a drain-scoped ctx (fix #74 — never
  `WithoutCancel`, see below); `opFn`-adjacent dispatch is the
  Service itself (no frozen `opFn` touch).
- **R1 S1 — `import.max_bytes` as post-clone scratch `du` gate +
  per-pack publish gate** (git writes scratch directly; streaming
  enforcement is impossible). Terminal errors name the key and fix
  (`import from a nearby mirror or seed via import --direct`).
- **R1 S2 — scrub** of params/packets/log-tails/terminal errors/bucket
  (canonical URL + options + `secret_set` only); git stderr scrubbed at
  the line handler; grep-clean acceptance covers all five surfaces.
- **R1 S3 — credential `-c` argv built dynamically per spawn**
  (per-task env name, host-pinned `credential.<scheme>://<host>.helper`;
  static copy-paste forbidden).
- **R1 S4 refmap (decided):** branches + tags kept; `refs/pull/*`,
  `refs/changes/*`, `refs/review/*` dropped (opt-in heads-only,
  `refs/pull/<N>/head` verbatim — never `/merge`); `refs/notes/*`
  dropped (flag to keep); `refs/replace/*`, `refs/meta/*`,
  `refs/keep-around/*` always dropped; everything else verbatim. Empty
  source (incl. empty GitHub repos) → **422**.
- **R1 S5 — residuals named:** DNS TOCTOU + redirect following
  (allowlist gates the initial URL; token helper host-pinned);
  `dangerous:true` confirm kept for empty-allowlist non-GitHub URLs.
- **R1 S6 — auth order normative:** authenticate → authorize namespace
  → join-or-start; joins re-check read-on-namespace.
- **R1 S7 — importer-admin written explicitly via the identity
  service** (read-modify-write + bounded CAS retries);
  `SynthesizeDefault`/`BootstrapRepo` stay the backstop only.
  Non-email importers (auth-none `anon`, CLI `$USER`) cannot bind
  (`user:<email>` subjects only) — narrated skip, backstop covers.
- **R1 S8 — `import.git_timeout` (300 s)** for the non-clone spawns
  (`for-each-ref`, format detect, `index-pack` regen, LFS probe);
  `clone_timeout` 1800 s stays; repack rides the layer's maintenance
  timeout under the task ctx.
- **R1 S9 — `max_concurrent_per_repo`-for-target dropped** (no handle
  pre-create); `import.max_concurrent` = 2 (bounded channel;
  saturation fails the task loudly, never queues).
- **R1 S10 — EVIDENCE fixture pinned to a single-pack layout;
  asserted as an ops RANGE + flatness across S/M**, never exact N.
- **R1 S11 — LFS-as-pointers gets a server-side terminal `Notice` +
  docs line + UI note** (clones work, checkouts yield pointer text).
- **R1 S12 touch list (closed):** server registration (09 §4 item 3,
  one block in `buildCollab`/`chainCollab`), additive `[import]`
  config (14 §14.12), overwritable-list (`meta/import.json`) +
  discovery (`api.RegisterExposed` + both templates) amendments in the
  same change (law 12). No `internal/wal` touch.
- **Scope cuts for v1 (per review verdict):** no list endpoint, no
  DELETE, no GitHub API, no ssh/git transports, no LFS smudging, no
  submodule recursion. KEPT: `include_pull_heads` (default off),
  `format` pinning, browser-lane twins, `endpoints[]` discovery.
- **Absent `policy.json` IS the default** (allow-all): import writes
  no policy; the effective default holds by construction.
- **Drain-scoped import cancellation (fix #74):** the leader no longer
  runs detached (`context.WithoutCancel(context.Background())` is
  gone — no `WithoutCancel` remains in the package: detachment from
  client disconnect is structural, the leader never sees the request
  ctx, so nothing needs stripping). `Service` owns a drain ctx:
  `drive` derives its `Tasks().Run` wait from it, `Begin` refuses
  fast with 503 once drained, and the commit-point guard in
  `runImport` refuses the manifest `PutCreate` after drain begins
  (service flag first, body ctx second — a post-drain CAS fails,
  never lands). Composition drains both halves at phase 1
  (`reg.Tasks().Drain()` kills the clone via `CommandContext` +
  fails store work; `importSvc.Drain()` cancels the leader wait).
  The drain terminal is always the narrated 503 "import interrupted:
  instance is draining; safe to retry" (law 7), never a bare
  `context.Canceled`. Same change fixed a latent `wal.TaskTable`
  race it exposed: `Run` assigned `rt.cancel` after `mu.Unlock`
  while `Drain` reads it — the cancel is now published with the map
  entry under the lock (join/refuse paths release their unused
  ctx). Regression: `drain_test.go` (hanging-clone fixture →
  prompt 503 + no manifest; Begin-after-drain 503; drained
  headless run refuses at the commit point).
- **Import dialog dangerous confirm (fix #23):** `/import` carries an unchecked-by-default
  dangerous checkbox (DNS-TOCTOU + redirect-following help text) wired through `imports.start()`.
- **Setup [import] section (fix #23):** the Setup UI exposes all eight `[import]` keys
  (url_allowlist, timeouts, caps, SSRF bools) with spec-spelling examples; fail-closed defaults unchanged.
- **`import.max_bytes` compiled default equals
  `server.max_push_bytes`' compiled default (64 GiB)** rather than a
  dynamic follow — visible in `config dump`/setup; operators
  constraining pushes set both keys (terminal errors name the key).
- **The `.idx` discipline (§6.1)** closes a latent `AddPack` gap (idx
  neither installed nor uploaded) with no `internal/wal` touch.
- **HTTP/CLI ids are service ids** (B4 rings); the table record id
  stays internal — GET merges the ring mirror (fresh on every packet);
  pruned windows (past the 1 h retention) answer 404 per the §3 status
  table, and `import.json` stays the durable truth.
- **Non-wedging imports (fix #79):** the manifest used to commit
  BEFORE ingest/refs/admin/doc, so any later failure left a refless,
  admin-less repo whose retry 409'd "delete and retry" to a caller
  that may lack delete rights. The fix claims first
  (`import.json` PutCreate with `complete:false` before the manifest
  CAS), commits the manifest provisionally, converges idempotently
  (presence-probed packs, create-only ref converge, CAS-completed
  sidecar), and rolls every failure back to resumable-or-clean.
  Same-source retries resume (202) instead of 409ing; "delete and
  retry" 409s only for genuinely foreign manifests (no sidecar at
  all), foreign completions, and live foreign claims. The literal
  "commit the manifest last" from the issue is impossible without
  core surgery (the publish path needs the handle `Create` returns;
  `internal/wal` is untouchable per S12/law 8) — provisional-commit
  + claim + resume + rollback is the equivalent guarantee, with the
  manifest CAS arbitration unchanged. Claim lease =
  `clone_timeout + git_timeout` (no new knob; expiry gates only
  different-source takeover of a manifest-less target, never
  same-source resume). Pre-fix sidecars (no `complete` flag) converge
  once, then complete — never mistaken for a no-op. Residuals:
  duplicate tier-0 log entries under racing same-source convergers
  (content-addressed, benign); a foreign push winning the name race
  in the claim→commit window leaves our claim + their manifest (the
  retry converges and aborts loud on divergent refs — never silent,
  never overwriting).

## Explicitly out of scope

Code search, CI running on imports, Discussions/Packages/Projects,
SAML/SCIM (P9) — unchanged. Follow-ups if wanted: live-head
comparison on re-POST (update-in-place vs 409), per-task cancel API
(the frozen-code touch B5 deferred), GitHub-API prefill (browser-side
only, §9.1 compromise stands), ssh-agent support for server-side
transports.
