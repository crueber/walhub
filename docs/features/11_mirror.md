# 11 — Pull-only mirror repos with scheduled upstream syncs

> Forgejo [`crueber/walhub#240`](https://git.packden.us/crueber/walhub/issues/240) · Status: implemented (plan revision R1 + plan review are normative on conflict; R1 wins).

A repo can be created as a **mirror** of an external git repository: walhub
pulls from the upstream on a schedule, the repo is marked read-only
(pull-only), pushes to it fail for every principal, and the UI shows the
mirror badge plus the next scheduled sync. Schedules are presets only —
**hourly, 8h, daily (default), weekly, monthly**. No freeform cron.

## 1. Mirror state: the sidecar (R1 (b))

`repos/<o>/<r>/meta/mirror.json`, Create-once-then-CAS'd, in the frozen
overwritable family (14 §14.11 rule 2, amended in the same change):

```json
{ "version": 1, "upstream_url": "https://github.com/acme/up.git",
  "schedule": "daily",
  "last_synced_at": "2026-09-09T00:00:00Z",
  "last_attempt_at": "2026-09-09T00:00:00Z",
  "last_result": "ok",
  "consecutive_failures": 0 }
```

- `upstream_url` is canonical (`repoimport.NormalizeSource` — same SSRF
  gate) and **immutable** (change = 409 delete-and-recreate, so the
  next-fire anchor never silently switches sources).
- Only the preset name is stored. The preset→cron map is fixed in code
  (`hourly→@hourly`, `8h→0 0 */8 * * *`, `daily→@daily`,
  `weekly→@weekly`, `monthly→@monthly`); unknown presets fail closed.
- `next_sync_at` is **derived at read** (`bundle.Cron.Next` anchored at
  the last *success*). A failed sync never moves the anchor, so it never
  moves the computed next fire. A never-synced mirror is due immediately.
- NOT a manifest proto field (frozen contract, Rust interop, and sync
  churn does not belong on the git linearization point) and NOT settings
  TOML (any settings writer could flip read-only off — wrong trust
  domain for an enforcement flag).
- Deleting the sidecar stops the loop for the repo (probe-absent → skip).

## 2. Credentials: public-upstreams-only v1 (R1 (a))

No stored secrets. A token is accepted ONLY for the creation-time first
sync and the manual "Sync now" POST body — memory-only (per-spawn child
env, host-pinned credential helper, never argv/bucket/logs/task params;
import S2 scrub rules apply) — and dropped after. Scheduled fires fetch
anonymously. A persisted-credential design is explicitly out of scope
(it needs its own issue: key management, CAS'd secret records, rotation,
audit). The single operator-scoped env token was considered and rejected
(one credential across all mirrors, unjustifiable blast radius).

## 3. Sync engine (R1 (d)(e)(f))

- **Task kind** `mirror-sync` on the core `wal.TaskTable`
  (`(repo, kind)` single-flight: concurrent fires join, never overlap).
  HTTP spawns via `SyncAsync` (202 with an id upfront; the body runs
  detached — client disconnect cannot cancel it, drain still can).
- **Cross-instance exclusion**: the bucket lease
  `leases/mirror-<owner>-<name>.pb` (CAS+TTL 10m, skew 0) taken BEFORE
  cloning. Held → skip + narrate (never wait, never retry in the fire).
- **Enumeration**: the in-memory repo registry + one `mirror.json` probe
  per repo (no LIST). Due = computed next fire reached and outside the
  failure backoff. Overdue-after-restart fires ONCE (the fire moves the
  anchor). The loop (`RunLoop`) is its own goroutine at a 1-minute
  cadence on maintain-role hosts — the follow.go shape: never a
  maintenance unit, never blocking maintenance.
- **Body**: clone `--mirror` (pinned argv, 04 §12) into task scratch →
  `for-each-ref` enumerate → S4 refmap (`FilterRefs`, no pull heads, no
  notes) → ingest scratch packs as tier-0 (`AddPack` + the idx install
  discipline, resume-skips durable checksums; no tier-2 repack —
  compaction owns the base) → **followOnce-shaped converge** (compare +
  ff-only via `merge-base --is-ancestor` in the scratch + atomic
  `PublishRefs`). Deleted upstream refs are left alone (a human's call);
  HEAD follows the source when kept.
- **Import-vs-sync exclusion**: `Begin` (import) on a mirrored target →
  409; a sync fire under a live `repo-import` claim (`import.json` with
  `Complete=false`; landed provenance is NOT live) → skip + narrate.
  Forks of mirrors are born writable normal repos (03 §7); a mirror as
  fork-parent flows through the existing `removeSuperseded` gate.
- **Failures**: `consecutive_failures` + backoff (`15m × 2^(n-1)`,
  capped 24h, anchored at the last *attempt*). Failures narrate via the
  task terminal + `last_result`; a failed sync never moves next fire.
- **Rewind**: ff-only default — a rewound upstream is refused + narrated
  (recorded WITHOUT touching the failure counter: policy, not outage;
  the refusal repeats every round until handled) — plus the manual
  `force` resync escape hatch (bypasses ff-only AND backoff).

## 4. Push refusal (R1 (c))

Repo-level read-only, enforced for every principal (admins included):

1. **Discovery**: `gitInfoRefs` receive-pack advertisement → 403
   plain-text `"this repository is a read-only mirror; pushes are rejected"`.
2. **Funnel**: `pushPipeline` top (the ONE function HTTP
   `receivePackLocal` and SSH both land in) → per-ref `ng` lines, no
   ingest/connectivity/publish. Covers clients that skip discovery.
3. **SSH advertisement**: refusal precedes the v0 advertisement write
   (client stderr `walhub: <msg>`), else the client hangs.
4. Fetches/clones are unaffected (read paths never consult the guard).

The sync engine publishes via `Publish`/`PublishRefs` directly and never
enters `pushPipeline`, so it cannot refuse itself (follow §8.4:
configuration, not a principal). Audit: settings PUT stays admin-only
(the mirror-flag flip path); ops ref-writers are server-side publishes
that bypass the funnel by construction; auto-create never creates mirror
targets (an unborn push creates a normal repo — mirror targets are born
only through the mirror flow). Core never imports the feature: the
refusal is an injected `MirrorGuard` predicate over the same store
(nil → legacy).

Cost (measured, see EVIDENCE.md): each push revalidates the guard — one
exact-key probe (404-free class) at discovery + one in the funnel. The
push-budget test wires the shipped guard and bounds it (≤ 6 probe ops
for 2 pushes, every other collab family at zero).

## 5. API surface

Repo lanes (both lanes, `/{o}/{r}/api/...` + `/api-browser/...`):

```
GET    /{o}/{r}/api/mirror        → the view (open read — no secrets in it; 404 when not a mirror)
PUT    /{o}/{r}/api/mirror        → {upstream_url?, schedule?}: create (+ anonymous first sync, 202 {task, target, mirror}) or reschedule (200 view); unborn repo → 404 (PUT never creates repos); upstream change → 409 (admin)
DELETE /{o}/{r}/api/mirror        → 204 (stops the loop; admin)
POST   /{o}/{r}/api/mirror/sync   → {token?, force?} → 202 {task: {id}, target} (admin)
GET    /{o}/{r}/api/mirror/sync[?id=] → {done, task?, error?} | {active, recent} (open read)
POST   /api/v1/repos/mirrors (+ /api-browser/v1 twin) → {source_url, owner, name, schedule?, token?, dangerous?} → create repo + sidecar + first sync (202; write-gated, dangerous needs the import authority rule)
```

- The summary (`summaryBody`) gains `mirror: {upstream_url, schedule,
  next_sync_at, last_synced_at, last_result, consecutive_failures, due}`
  behind an `api.Env.MirrorSummary` hook (the ReadGate/OrgGate shape —
  api never imports the feature); nil hook → no field, no probe. The
  ETag covers it (`~m` suffix, the #235 `~d` precedent).
- Discovery lists ONLY the top-level twin (`api.RegisterExposed`, the
  import precedent); repo-lane routes stay out (the 01/02/03/C2 rule).
- Strict JSON (unknown fields 400, fail closed); plain-text errors;
  `secret_set` presence-only in task params (never the token).

## 6. UI

- Repo header: `mirror · pull-only` badge (tooltip: upstream + next
  sync) + next-sync/due line next to the title — rendered from the
  shared summary, no extra fetch.
- Settings → Mirror tab (in the sidebar after Scheduled tasks): status
  table (upstream, next fire, last synced/result, backoff), schedule
  preset picker, Sync-now (optional memory-only token, force checkbox,
  polled status), create form on non-mirrors, removal.
- New repository → mirror-from-URL mode (source + preset → create twin
  → navigate to the repo). Schedule presets only — no freeform cron
  anywhere. Both themes ship together (dark default); the built bundle
  carries the badge/tab/preset strings (verified in `web/dist`), the
  headless suite covers the lib + SDK surface, and the live server proof
  covers create → sync → refusal → summary data (see EVIDENCE.md E15).
  In-browser render (badge/tab in both themes, console clean) was
  BLOCKED in this environment — the shared Chrome daemon's network guard
  refuses all private/loopback destinations and starting a private daemon
  is forbidden — so it is recorded as open, not claimed.

## 7. Round trips (measured — EVIDENCE.md #240)

First sync of a 2-commit fixture (memory backend, real git): 22 store
ops, 0 LIST (lease 4 + sidecar 5 + import-claim probe 1 + packs 3 +
manifest/log 5 + probes). No-op fire: 11 ops, zero pack/manifest/log
writes (converge-only). Push: +2 exact-key probes (cold 10 / warm 9
bucket ops for the budget pushes). Summary: +1 probe only when the hook
is set (SWR-cached + ETag'd).

## 8. Tests (law 11)

`internal/mirror` ≥ 95% (`-race` mandatory): preset→cron→fire shapes,
sidecar CRUD/CAS, backoff windows, first/no-op/ff/rewind+force syncs
against fixture repos (real git), import-claim skip (in-progress vs
landed), lease contention + steal, failure counter + backoff skip,
loop (due fires, gated skips, deleted-mirror stops, overdue fires
once), async/status, HTTP routing/authz/validation on every route, and
the round-trip harness above. Plus: push-refusal tests (HTTP discovery
403, HTTP funnel ng, direct funnel with nil repo, SSH pre-advertisement)
in `internal/server`; summary wire + `~m` ETag tests in `internal/api`;
`Begin`-on-mirror 409 in `internal/repoimport`; the push-budget test
wires the shipped guard; `node --test` covers the lib + SDK surface;
live-server proof covers create → sync lands → scheduled fire →
push refused (discovery 403, funnel ng, real git client) → summary/badge
data (EVIDENCE.md E15). In-browser render is recorded open (blocked —
see §6).

## Decisions & deviations from the Rust design

- **(a) Public-upstreams-only v1 (2026-09-09, #240 R1).** No stored
  secrets; token memory-only for creation first-sync + manual Sync-now;
  scheduled fires anonymous. Rationale: the import never-stored-token
  decision (#10) cannot be satisfied by a scheduled sync any other way
  without a whole secret-management feature; v1 keeps the zero-secret
  posture and zero new attack surface.
- **(b) Sidecar + frozen-list amendment + computed next-fire
  (2026-09-09, #240 R1).** `meta/mirror.json`, Create-once-then-CAS'd,
  amended into the frozen list in the same change; preset name stored,
  `next_sync_at` derived. Rationale: sync churn off the linearization
  point and out of the settings trust domain; compute-on-read is one
  pure function with no writer skew.
- **(c) Funnel placement + sync bypass (2026-09-09, #240 R1).**
  `pushPipeline` top + discovery 403 + SSH pre-advertisement refusal;
  sync publishes direct. Rationale: the funnel is the one place every
  client write traverses (verified both transports land there); the
  managed-ref precedent proves the shape.
- **(f) FF-only + backoff + force escape (2026-09-09, #240 R1).**
  Rewind refused + narrated (no counter touch), exponential backoff on
  failures, manual force resync. Rationale: silent history rewrites
  deserve a rule (follow §8.3 precedent); backoff is capped delay, and
  failures never move next fire.
- **No new config section, no CLI verb, no new deps (2026-09-09,
  #240).** `[import]` owns SSRF/timeouts/caps for both flows; the
  `mirror` CLI stub stays a stub (out of scope); the loop cadence is a
  code constant (1m). Rationale: one gate/one clock, smallest seam
  footprint; knobs can be added additively later per 14 §14.12.
- **Listing flag + waiting state + import creation (2026-09-10,
  #281).** The summary `mirror` view stays the repo page's source, but
  listing rows cannot afford per-row summaries: the detailed owner
  listing (07_api.md) now carries `mirror` + `mirror_upstream?` from
  bounded-parallel sidecar probes, and the empty-repo guide branches on
  the already-in-hand shell summary projection (waiting state, never
  push guidance — pushes to a pull-only mirror are rejected). `/import`
  creates mirrors through the existing create-from-URL endpoint (mode
  toggle, shared presets/validation). Rationale: the data was already
  adjacent in both cases (listing payload, shell summary) — this change
  only surfaces it, adding no client fetches (no per-row summary fetch)
  and no new endpoint; the server adds bounded-parallel sidecar probes
  (request count, no sequential depth — see 07_api.md).
