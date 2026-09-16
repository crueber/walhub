# 13 — Push mirroring: fan out pushes to an upstream

> Forgejo [`crueber/walhub#623`](https://git.packden.us/crueber/walhub/issues/623) · Status: implemented.

A repo can be configured **post-hoc in Settings** (never at create or
import time — mirroring a destination is a lifecycle decision made after
the repo exists) to push its refs+objects to an upstream URL
(Forgejo/GitHub mirror or personal backup) **after every successful push
lands in walhub** (on-push trigger), with an **optional scheduled
re-sync** reusing the pull-mirror preset-cron model. This complements
the pull-only mirror (`11_mirror.md`, which fetches FROM an upstream and
refuses pushes); the two sidecars are fully independent — either, both,
or neither may be configured.

## 1. State: config + secret sidecars

`repos/<o>/<r>/meta/pushmirror.json`, Create-once-then-CAS'd, in the
frozen overwritable family (14 §14.11 rule 2, amended in the same
change):

```json
{ "version": 1, "upstream_url": "https://example.com/backup/repo.git",
  "auth_kind": "token", "username": "bot",
  "schedule": "",
  "public_key": "ssh-ed25519 AAAA… (ssh kind, generated only)",
  "key_fingerprint": "SHA256:…",
  "last_synced_at": "2026-09-16T00:00:00Z",
  "last_attempt_at": "2026-09-16T00:00:00Z",
  "last_result": "ok",
  "consecutive_failures": 0 }
```

- `upstream_url` is canonical (`repoimport.NormalizeSource`, same SSRF
  gate) and **immutable** (change = 409 delete-and-recreate).
- `auth_kind` is one of `none|password|token|ssh` (per-mirror choice).
- `schedule` is `""` (OFF — on-push only, the default) or a preset name
  (`hourly|8h|daily|weekly|monthly` — the pull-mirror map, duplicated so
  either side evolves alone). `next_sync_at` is **derived at read**
  (`bundle.Cron.Next` anchored at the last *success*); a failed sync
  never moves the anchor; a never-synced scheduled repo is due
  immediately.
- The public key is public (safe to echo); NOT a manifest proto field
  and NOT settings TOML (same trust-domain argument as 11 §1).

`repos/<o>/<r>/meta/pushmirror-secret.json`, CAS'd (read-modify-CAS
loop, human rate), in the frozen overwritable family (amended in the
same change):

```json
{ "version": 1, "auth_kind": "token", "username": "bot",
  "token": "…", "updated_at": "2026-09-16T00:00:00Z" }
```

(password → `password`; ssh → `ssh_private_key` OpenSSH PEM +
`ssh_known_hosts` trust lines + `ssh_known_hosts_accepted_at`
first-learn stamp, Forgejo #625.) The secret sidecar is **never echoed
back in full** — the API renders presence + last-4 hint only
(`has_secret`, `secret_hint: "••••1234"`), and every error/`last_result`
surface is scrubbed (the pull-mirror scrub contract: `password=`/
`token=`/`secret=` redaction + userinfo masking).

Deleting both sidecars stops on-push fan-out and scheduled syncs
(probe-absent → skip).

## 2. Auth (all three, per-mirror choice)

1. **Username/password** — HTTPS basic auth (host-pinned credential
   helper, per-spawn child env, never argv/bucket/logs).
2. **Token** — bearer-style token over HTTPS (same transport shape;
   username defaults to `x-access-token`). Token over plaintext `http`
   is refused at configure time.
3. **SSH key** — deploy-key style push over `ssh://`/scp-like
   upstreams:
   - **User provides** a private key (+ optional `known_hosts` trust
     lines, + optional public line validated as `ssh-ed25519`); or
   - **Walhub generates** an ed25519 keypair (stdlib `crypto/ed25519`
     + hand-rolled OpenSSH wire format — no new module, no new binary;
     the runtime image ships no openssh-client, so an `ssh-keygen`
     subprocess would add a runtime dependency for every deployment)
     and returns the **public key + fingerprint** for upstream install.
     The private key is never returned (write-only).
    - Without pinned `known_hosts` the push uses
      `StrictHostKeyChecking=accept-new` (first-use trust, recorded —
      never the silent-insecure `no`); `BatchMode=yes` never prompts.
      Key files are 0600 per-fire scratch, swept on every exit path.
      The known_hosts file is ALWAYS a per-fire path passed via
      `UserKnownHostsFile` (never the ambient `~/.ssh/known_hosts` —
      daemon HOME is not a trust store), so the accept-new learn lands
      where the runner can read it back. `HashKnownHosts=no` pins stable
      plaintext hostnames in the scratch file (distro ssh_config often
      ships `HashKnownHosts=yes`; salted `|1|` tokens would defeat the
      host+keytype dedupe with a fresh token per fire — review #626).
    - **Host-key trust harvest (Forgejo #625).** After a successful SSH
      push the runner returns the post-push known_hosts content and the
      sync harvests it — one shared point in the task body (scheduled,
      on-push, and sync-now funnel through it, no per-trigger forks):
      learned lines MERGE into `ssh_known_hosts` (dedupe by
      host+keytype; stored lines never dropped; on a host+keytype
      conflict the operator-pinned line wins and the learned line is
      dropped — explicit pins stay authoritative, decision (k)), the
      first learn stamps `ssh_known_hosts_accepted_at` (preserved
      after; empty for pinned-only trust; reset when the operator
      clears `known_hosts` back to accept-new), and the fingerprint narrates
      (SHA256 display only). The write is a read-merge-CAS loop: on a
      412 the harvest re-loads and re-merges, so a concurrent operator
      edit folds in instead of being clobbered (review #626 — the blind
      single-version write would last-writer-win it away). A harvest miss never fails the sync
      (outcome stays ok, the miss narrates, the next accept-new fire
      re-learns and retries). Stored trust automatically pins the next
      fire (`StrictHostKeyChecking=yes` once the sidecar is non-empty).
      Learned lines live ONLY in the secret sidecar — logs, errors, and
      views carry the fingerprint at most.

Transport matrix (`ValidateTarget`, fail closed): `file://` → none
only; `https://` → none|password|token; `http://` → none only;
`ssh://`+scp → ssh only; `git://` → always refused. Missing material
for a material-needing kind is a failed outcome, never a silent
anonymous push.

## 3. Sync engine

- **Task kind** `mirror-push-sync` on the core `wal.TaskTable`
  (`(repo, kind)` single-flight — distinct from pull `mirror-sync`,
  so the directions never join each other). HTTP spawns via `SyncAsync`
  (202 with an id upfront, detached ctx).
- **Cross-instance exclusion**: the bucket lease
  `leases/pushmirror-<owner>-<name>.pb` (CAS+TTL 10m) taken BEFORE
  pushing. Held → skip + narrate (never wait, never retry in the fire).
- **On-push trigger**: `Server.OnPush` hook (injected predicate-free —
  the service itself, law 8), called fire-and-forget from
  `pushPipeline` AFTER the report is on the wire, only for landed
  pushes (≥1 ok ref). The hook's goroutine probes the config sidecar
  (one exact-key GET, 404s free — the ONLY store trip on this path, off
  the push response) and fires an async sync when configured.
  **Server-side publishes never enter `pushPipeline`** (the sync.go:434
  bypass: mirror syncs, merge tasks publish via `Publish`/`PublishRefs`
  directly), so they cannot fan out — structural, pinned by test.
- **Scheduled loop** (`RunLoop`, the follow.go shape: its own 1-minute
  cadence, never a maintenance unit): enumerates the in-memory registry
  + one config probe per repo (no LIST); `Due` = schedule on +
  next-fire reached + outside the failure backoff (`15m × 2^(n-1)`,
  24h cap, anchored at the last *attempt*). OFF repos never fire here.
  Overdue-after-restart fires ONCE. Force (`Sync-now` checkbox)
  bypasses backoff.
- **Body**: open handle → `Sync(LevelServe)` materialize (refs live in
  the manifest store — the Sync applies what the store publishes, and
  the push ships that serving copy; the read guard is held across the
  transfer, the upload-pack reader shape) → namespace-scoped forced
  refspec push with `--prune` (pinned argv, 04 §12-adjacent) with the
  per-kind credential shape → `RecordAttempt` (success clears the
  counter + stamps the next-fire anchor; failure records the scrubbed
  reason + backoff). The push ships user namespaces (heads + tags
  always, other surviving namespaces when populated) and never
  forge-internal refs (`refs/pull/**` and friends stay out — decision
  (j)); deletions propagate within live namespaces (walhub is the
  primary).

## 4. Push refusal interaction

Pull mirrors (`meta/mirror.json`) refuse every client push; push
mirrors impose no refusal (a push-mirrored repo is a normal writable
repo). Configuring one never requires the other; a repo may carry both
sidecars (pull in, push out).

## 5. API surface

Repo lanes (both lanes, `/{o}/{r}/api/...` + `/api-browser/...` — no
top-level twins by design: post-hoc config only, never at create):

```
GET    /{o}/{r}/api/pushmirror          → the view (open read; 404 when none)
PUT    /{o}/{r}/api/pushmirror          → {upstream_url?, auth_kind?, username?, password?, token?, ssh_private_key?, ssh_public_key?, ssh_known_hosts?, schedule?, dangerous?}: 201 view on create (repo must exist — 404 otherwise; no first-sync fire — the next push fans out), 200 view on update; upstream change → 409; unknown preset/kind → 400 (admin)
DELETE /{o}/{r}/api/pushmirror          → 204 (stops fan-out; admin)
POST   /{o}/{r}/api/pushmirror/sync     → {force?} → 202 {task: {id}, target} (admin; credentials come from the stored secret, never the request)
GET    /{o}/{r}/api/pushmirror/sync[?id=] → {done, task?, error?} | {active, recent} (open read)
POST   /{o}/{r}/api/pushmirror/keygen   → {known_hosts?} → 200 {public_key, key_fingerprint, has_secret, mirror} (admin; private key never returned)
```

- The summary (`summaryBody`) gains `push_mirror: {upstream_url,
  auth_kind, username?, has_secret, secret_hint?, schedule?,
  host_key_fingerprint?, host_key_accepted_at?, next_sync_at?,
  last_synced_at?, last_result?, due}` behind an
  `api.Env.PushMirrorSummary` hook (nil → no field, no probe). The
  ETag covers it (`~p` suffix, the `~m` precedent) — including the
  host-key fields, since learning trust changes no outcome field.
- Discovery lists the three repo lanes (`api.RegisterExposed` from
  composition, the #272 rule; pinned both ways).
- Strict JSON (unknown fields 400, fail closed); plain-text errors;
  secrets never echo (presence + last-4 only); every error scrubbed.

## 6. UI

- Repo header: `mirror · push` badge (tooltip: upstream + sync
  phrasing) rendered from the shared summary, no extra fetch —
  independent of the pull-only badge.
- Settings → Push mirror tab (after Mirror): status table (upstream,
  auth + stored-hint, deploy-key fingerprint, host-key trust
  (fingerprint(s) + first-accepted-at, SSH only), sync phrasing, last
  synced/result), upstream + auth-method + credential fields
  (write-only, per-kind), schedule picker (Off default), keygen panel
  (SSH only: known_hosts + generate → readonly public key for upstream
  install), Sync-now (force checkbox, polled status), removal. Both
  themes ship together (dark default); the headless suite covers the
  lib + SDK surface. Narrow-viewport (~390px): grid/flex-wrap forms +
  the shared data-table pattern — no fixed-width assumptions (same
  shape as the Mirror tab).
- SDK: `repo(full).pushmirror.{get,put,remove,syncNow,syncStatus,keygen}`
  (the mirror surface shape; sync bodies carry no credentials).

## 7. Round trips

On-push enqueue: +0 hot-path trips (async hook goroutine probes one
exact-key config GET off the response; no-config repos pay one 404).
Scheduled pass: one config probe per repo per minute (404s free); due
fires pay lease (2) + config/secret (2) + Sync (manifest-conditional) +
the git transfer (bulk lane, never the control-plane client).
Summary: +1 probe only when the hook is set (mutable-collab class +
`~p` ETag). No-op sync: lease + config/secret + Sync, zero
pack/manifest/log writes.

## 8. Tests (law 11)

`internal/pushmirror` ≥ 95% (`-race` mandatory): auth matrix,
schedule/off/next-fire/backoff, sidecar/secret CRUD/CAS + redaction
(presence/hint/scrub), keygen shape + public-line validation, file://
  push of refs+objects (real git: `git push --mirror` lands tips AND
  clone works), missing-material verdict, lease contention + steal,
  backoff skip + force escape, host-key harvest (parse/merge/dedupe,
  fingerprint, operator-pin precedence, view fields, `~p` ETag input
  change, harvest-miss-keeps-ok, stub-ssh learn→persist→surface→repin
  end to end), scheduled loop (due fires, OFF never
  fires, gated skips), on-push enqueue (configured fires, unconfigured
  silent, nil-safe), HTTP routing/authz/validation on every route +
  both lanes, discovery templates both ways. Plus: on-push hook tests in
  `internal/server` (landed fires, refused/failure silent, nil-safe),
  summary wire + `~p` ETag + pull/push independence in `internal/api`,
  composition mapping + hook wiring in `cmd/walhub`, `node --test` for
  the lib + SDK surface, live-server proof (push → upstream bare
  receives refs+objects; scheduled fire; server-publish exclusion).

## Decisions & deviations from the Rust design

- **(a) Post-hoc config only (2026-09-16, #623).** No create/import
  twin: the issue scopes mirroring destinations to Settings on existing
  repos. Rationale: a destination chosen before the repo exists invites
  half-configured fan-out; PUT-on-existing keeps one birth path.
- **(b) Config + secret sidecars, frozen-list amendment (2026-09-16,
  #623).** `meta/pushmirror.json` (Create-once-then-CAS'd) +
  `meta/pushmirror-secret.json` (CAS'd) join the frozen overwritable
  list in the same change (14 §14.11 rule 2). Rationale: the settings
  trust domain must not flip fan-out; the secret needs its own CAS
  record with no manifest churn; compute-on-read next-fire keeps one
  pure function with no writer skew (the pull-mirror R1 (b) discipline).
- **(c) Write-only credentials + scrubbed surfaces (2026-09-16, #623).**
  Presence + last-4 hint, never full echo; `last_result` and every
  error scrubbed (the pull-mirror import-S2 contract). Rationale: a
  status surface that echoes a token is a credential leak with a UI.
- **(d) Stdlib keygen, no ssh-keygen subprocess (2026-09-16, #623).**
  ed25519 via `crypto/ed25519` + hand-rolled OpenSSH wire format
  (public line + `OPENSSH PRIVATE KEY` PEM + `SHA256:` fingerprint).
  Rationale: law 1's two lawful shapes are stdlib-wire or a documented
  runtime dependency; the runtime image ships git + CA certs only (no
  openssh-client), so a subprocess would tax every deployment for one
  button. `x/crypto/ssh` stays server-transport-only (never a client).
- **(e) `git push --mirror` transfer (2026-09-16, #623).** Refs live in
  the manifest store: `Sync(LevelServe)` materializes what the store
  publishes into the serving copy, and the push ships that copy
  (ref+object transfer in one argv, deletions included — walhub is the
  primary). SSH rides `GIT_SSH_COMMAND` + materialized key files (0600,
  swept per fire); HTTPS rides the host-pinned credential helper (the
  pull-mirror shape). Rationale: law 2 (git is a subprocess, argv
  pinned in 04); no Go SSH client (law 1 sub-point).
  **Superseded in shape (not in rationale) by (j) below:** a bare
  `--mirror` ships the whole serving copy including walhub's own
  `refs/pull/**` PR heads (ordinary WAL ref state, published server-side
  through the WAL funnel and materialized by every Serve sync) — a
  forge-internal leak, and hosts like GitHub refuse writes to
  `refs/pull/*`, which would wedge every PR-carrying repo in permanent
  "failed".
- **(f) On-push via `Server.OnPush`, exclusion structural (2026-09-16,
  #623).** The hook fires from `pushPipeline` (the ONE function both
  transports land in) after the report, landed-only, fire-and-forget;
  server-side publishes never enter the pipeline (sync.go:434), so the
  exclusion needs no flag. Rationale: a flag checked at fire time can
  be forgotten; a funnel never entered cannot fan out.
- **(g) Schedule OFF by default (2026-09-16, #623).** `""` = on-push
  only; presets opt in. Rationale: the issue's default — scheduled
  re-push without consent would surprise backup-topology owners.
- **(h) No first-sync fire on create (2026-09-16, #623).** Unlike pull
  mirrors (which must fetch to exist), a push mirror's next client push
  fans out on its own; creation stays a pure config write. Rationale:
  an immediate push-back of current state is one Sync-now away, and a
  surprising bulk transfer at configure time is worse than one click.
- **(i) `accept-new` without pinned known_hosts (2026-09-16, #623).**
  No accept-insecure toggle: unpinned hosts trust on first use
  (recorded), pinned hosts enforce strictly. Rationale: the issue left
  the choice to the implementer; `no` would silently disable the only
  authentication SSH has, while `accept-new` + `BatchMode=yes` keeps a
  prompt-free daemon without downgrading pinned hosts.
- **(j) Namespace-scoped forced refspecs instead of bare `--mirror`
  (2026-09-16, #623 review).** The transfer enumerates the serving
  copy (`for-each-ref`, one cheap local spawn per fire — no store
  trip), drops forge-internal namespaces (the S4 refmap reversed:
  `replace`/`meta`/`keep-around` always, `pull`/`changes`/`review` +
  `notes` by default — the pull direction's FilterRefs discipline), and
  pushes the survivors as `+<ns>/*:<ns>/*` with `--prune` (`heads` +
  `tags` unconditionally so an emptied namespace prunes upstream;
  other namespaces when populated). Rationale: (e)'s bare `--mirror`
  cannot exclude — it would ship `refs/pull/**` upstream on every
  PR-carrying repo (leak + GitHub refusal wedge); per-ref refspecs do
  not scale to 500k-ref repos (ARG_MAX), while per-namespace wildcards
  do; `--prune` keeps the walhub-is-primary deletion semantics within
  live namespaces. A fully-deleted custom namespace leaves stale
  upstream refs (no refspec names it) — heads/tags are exempt (always
  ridden). The credential helper change in the same revision (username
  via child env, never interpolated — the `!` helper runs through a
  shell and the username is user-controlled) closes a command-injection
  surface the password/token shape introduced.
- **(k) Accept-new harvest with operator-pin precedence (2026-09-16,
  #625).** The first sync learns + records (merge into
  `ssh_known_hosts` under the secret CAS, stamp
  `ssh_known_hosts_accepted_at` once); later syncs pin against stored
  trust (`StrictHostKeyChecking=yes` as soon as the sidecar is
  non-empty). On conflict the operator pin wins (merge, never
  overwrite): a pin is an explicit trust decision, and silent
  replacement would let a MITM-shaped rotation downgrade it — the
  conflict drops, the sync still succeeds, and the UI keeps showing
  the pinned fingerprint. Fingerprints are derived at read from the
  merged lines (single source of truth, no second copy to skew; also
  covers pre-#625 pinned sidecars, which surface a fingerprint with no
  stamp). The harvest rides the existing post-push path (no new hot
  trips — law 6) and never fails the sync (record ok + narrate the
  miss + retry next fire). Rationale: until this landed the prod
  guidance was "pin known_hosts"; now the first sync bootstraps the
  pin itself, with the verification surface (fingerprint +
  first-accepted-at in the view, Settings row, and summary) the
  original shape lacked. Amended on #626 review: `HashKnownHosts=no`
  on the per-fire command (stable plaintext hostnames — distro
  `HashKnownHosts=yes` would salt a fresh token per fire and defeat
  the dedupe); the harvest write is a read-merge-CAS loop (a 412
  re-loads and re-merges, so a concurrent operator edit folds in
  instead of being clobbered); clearing `known_hosts` back to
  accept-new resets the stamp with the trust it names.
