# 03 — Pull requests: opens, `refs/pull/N`, mergeability, merge tasks, forks

> Status: planning for the walhub collaboration layer. Depends on 01 (identity/permissions), 02
> (issues — shared numbering via P2), and the frozen architecture in
> [`README.md`](README.md) (primitives P1–P9). Seams: `docs/go/14_extensibility.md` (§14.3 routes,
> §14.7 task kinds, §14.10 roadmap). Git argv per `docs/go/04_git.md`; wire conventions per
> `docs/go/07_api.md`; concurrency per `docs/go/13_concurrency.md`. Doc 04 owns review/merge-base
> helper signatures; this doc owns PR state, the merge task, and forks.

## 1. Scope and the one architectural law

A pull request is a **collaboration object** (never a WAL entry) that points at ordinary git state: a
base ref and a head ref in the same repo (or in a fork of the same object network). Its commits are git
objects already in the store; its conversation is a P3 thread; its merge is a Seam-5 task kind that
publishes a ref update through the normal WAL publish path. Core (`internal/store`, `internal/wal`,
`internal/git`) never learns the word "pull"; everything registers via `internal/pulls` as a
`RouteProvider` (Seam 1), task kinds `pull-merge` / `pull-fork` / `pull-mergeable` (Seam 5), and an
event sink `pulls` (Seam 4).

## 2. Object family (buckets + schemas)

Bucket keys (all under the P1 `repos/<o>/<r>/` prefix; JSON bodies, UTF-8, `[]`-not-null, RFC 3339):

| Key | Kind | Create vs CAS'd |
|---|---|---|
| `repos/<o>/<r>/issues/<num>/thread.json` | P3 header, `kind:"pr"` | CAS'd (`Update(version)`) |
| `repos/<o>/<r>/issues/<num>/events/<seq:012x>.json` | P3 immutable events | Create only |
| `repos/<o>/<r>/pulls/<num>/pr.json` | PR-sidecar snapshot (refs + shas at open, merge outcome) | CAS'd |
| `repos/<o>/<r>/pulls/<num>/mergeable.json` | mergeability cache (derived; stamp-invalidated) | Create-then-CAS overwrite |
| `repos/<o>/<r>/issues/index.json` | P4 list index — SHARED with issues (02 owns the schema; cards carry `kind`); PR lists/sink lookups filter `kind:"pr"` | CAS'd; compaction per P4 |
| `repos/<o>/<r>/meta/forks.json` | parent-side fork index (children) | CAS'd |
| `repos/<o2>/<r2>/fork.json` | fork-side provenance | Create once, then CAS'd for `merged_upstream_at` |

`issues/index.json` (shared), `pulls/<n>/mergeable.json`, `meta/forks.json` join the frozen
overwritable-key family (D-EXT-2). The `refs/pull/<num>/head` ref is ordinary WAL ref state — no bucket
object, it lives in the ref snapshot/manifest like any branch.

### 2.1 `pulls/<num>/pr.json`

```json
{ "num": 42, "kind": "pr",
  "base": { "ref": "refs/heads/main", "sha": "<40hex at open>", "repo": "o/r" },
  "head": { "ref": "refs/heads/topic", "sha": "<40hex at open>", "repo": "o/r" },
  "fork": null,
  "merged": false, "merged_at": null, "merged_by": null,
  "merge_commit_sha": null, "head_force_pushed_at": null }
```

Written in the open handler (Create) and updated by CAS on: head sha change (force-push or the `pulls`
sink observing head-branch movement), merge outcome, base rename. `thread.json` stays the P3 header
exactly (02 owns its shape); PR-specific fields MUST NOT leak into it — 02's list/card rendering reads
the header alone.

### 2.2 `pulls/<num>/mergeable.json` (the cache)

```json
{ "base_ref": "refs/heads/main", "base_sha": "<40hex>", "head_sha": "<40hex>",
  "merge_base": "<40hex>", "state": "clean" | "dirty" | "behind" | "up_to_date" | "unknown",
  "conflicts": [], "rebaseable": true, "computed_at": "RFC3339" }
```

The stamp triple `(base_ref, base_sha, head_sha)` IS the invalidation key: a reader compares it against
the live ref shas; any mismatch ⇒ the cache is stale. `conflicts[]` is populated only when
`state:"dirty"` (paths from the trial merge). Writers MUST write with CAS (read-modify-write); the
object is derived state — losing it only costs a recompute, so a 412 loser simply re-runs after the
winner and converges.

### 2.3 Write discipline

Numbering, thread, index, and events all reuse their primitives verbatim: number from `meta/next_num`
(P2, CAS loop), events via the P3 two-step, index in the same transaction shape as P4, notifications
synchronously in the mutating handler (P8). No new locking primitives are introduced by this doc; every
concurrent mechanism below is a CAS loop or the canonical single-flight (`13_concurrency.md` §3).

## 3. Opening a PR

`POST …/pulls` with `{title, base_ref, head_ref, fork?}`. The handler MUST:

1. **Resolve + verify.** Resolve `base_ref` (must be an existing ref in the base repo) and `head_ref`
   (any branch, tag, or `refs/pull/N/head` in the base repo or — for cross-fork PRs — in
   `fork.repo`). Resolve both to SHAs. An unresolvable head is `404 unknown revision`.
2. **Allocate the number** via the P2 CAS loop on `meta/next_num` (shared with issues; human-rate ⇒
   CAS contention is a non-issue — do not add a lock).
3. **Create the thread** (`kind:"pr"`) + the `opened` event per P3, Create `pr.json`, update the shared
   `issues/index.json` (P4, card `kind:"pr"`), then fan out notifications/webhooks (P8) with the
   `pull_opened` action.
4. **Publish `refs/pull/<num>/head`** — a server-side ref create through the WAL publish path (doc 05
   CAS ladder), meta `{principal: <creator>, correlation_id: <request id>, agent: "pulls"}`. The server
   acts as the creator's principal: the ref create is attributed to the creator in events and audit,
   but performed by the server, so the creator needs only `write` on the base repo — not on whatever
   branch the head happens to live on.
   - The create is performed **only when the head commit is already reachable** (it was pushed through
     any branch/topic ref, or is an ancestor of an existing ref). Reachability check =
     `git rev-list --objects --stdin --not --all | git cat-file --batch-check` empty (doc 04 §D-ENG-2
     pipeline). Unreachable head ⇒ `422 head commit not reachable` — the client must push first. No
     PR ever publishes git objects; it only publishes a ref to objects that already arrived.
   - The ref is created even when the head equals another branch's sha (an unmerged topic ref with no
     commits of its own still gets `refs/pull/<num>/head`).
   - Server-side publishes bypass receive-pack/policy by construction (they never enter receive-pack);
     `refs/pull/*` is otherwise SERVER-MANAGED: client pushes to `refs/pull/**` are rejected by a
     built-in policy default (`protect` effect on the namespace, no bypass) — an operator may not
     hand-edit PR refs through git. The merge task's base-ref publish is the only other non-receive-pack
     writer, and it publishes through the identical path.

### Concurrency

Hazards: (a) two opens race `meta/next_num` — avoided by the P2 CAS loop, the loser retries with the
next number; a lost race never skips a number. (b) The ref create races a concurrent delete of the head
branch — harmless: `refs/pull/<num>/head` pins the sha; the PR head snapshot records the sha, and a
deleted branch is reported as `head.ref: null` + `head.sha` retained. (c) The ref publish fails after
the thread CAS committed (crash, WAL publish failure): the PR exists with `refs/pull/<num>/head`
absent. Recovery is a named repair: `GET …/pulls/{num}` re-verifies reachability and re-publishes
idempotently (ref create is a no-op when the ref already matches); the UI shows `head_ref: null` until
then. No cross-feature lock is introduced anywhere in this flow.

## 4. Mergeability

Computed server-side with the doc-04 machinery, as part of the thread fetch, cached in
`mergeable.json`:

- **Ancestry:** `git merge-base --is-ancestor <base_sha> <head_sha>` (exit 0 ⇒ base already contained —
  `up_to_date`, nothing to merge; exit 1 ⇒ not merged) and
  `git merge-base --is-ancestor <head_sha> <base_sha>` (exit 0 ⇒ head fully merged into base).
- **Trial merge:** `git merge-tree --write-tree --name-only <merge_base> <base_sha> <head_sha>` —
  exit 0 = clean (capture the would-be tree for the merge task), exit 1 = conflicts (capture the
  conflict paths). `behind` = `git rev-list --count <head>..<base>`.
- All git runs go through the git subprocess layer's bounded per-repo pool — never on the request
  goroutine without the semaphore.

**Invalidation seam (the mechanism this doc specs):** a `pulls` **event sink** (Seam 4,
`internal/events` — per-sink cursor `repos/<o>/<r>/events/cursors/pulls.json`) consumes WAL ref events.
For each event whose `ref_name` matches an open PR's `base.ref` or `head.ref` (looked up from the
shared `issues/index.json` filtered to `kind:"pr"` — the index is authoritative for open items per
P4), the sink enqueues the PR num onto a recompute batch.
The recompute runs as task kind `pull-mergeable` (Seam 5; `(repo, kind)`
single-flight batches all dirty PRs of the repo into one pass), recomputes and CAS-writes each
`mergeable.json` with the fresh stamp. The thread fetch is the second line of defense: it compares the
stamp against the live head/base shas and, on mismatch, serves `state:"unknown"` + enqueues
`pull-mergeable` for that PR. Both paths converge; neither blocks a read on git.

### Concurrency

Hazard: N readers of a just-pushed PR head trigger N concurrent `merge-tree` runs (git-pool exhaustion,
13 §4). Avoidance: recompute goes through the in-process single-flight keyed `"mergeable:" + repo + "/" + num`
(13 §3 — joiners get the same result, bounded join); the sink's per-repo catch-up is already serialized
(§14.6). Hazard: recompute races the merge task's own base-ref publish — the stamp comparison (recompute
reads the ref AFTER the merge publishes) makes stale computes harmless; the sink sees the merge's own
ref event and recomputes once more, converging to `up_to_date`/`merged`. No lock is held across any
store call or git subprocess (13 §2 rule 4).

## 5. The merge task (task kind `pull-merge`)

Started by `POST …/pulls/{num}/merge` with `{strategy, commit_title?, commit_message?, delete_head?}`.
Auth: `maintain` or above (P6). It is a narrated task (P7: unique id, progress packets, SSE attach,
`(repo, kind)` single-flight — a second start of a merge on the same repo JOINS the running one and
reuses its outcome, `13_concurrency.md` §3). `params`: `num`, `strategy`.

Steps (git is always the subprocess with exact argv, `docs/go/04_git.md`):

1. **Re-verify under the task:** re-resolve base/head SHAs from the live refs; if either moved since
   `pr.json`, refresh the mergeability stamp first (the stamp check of §4); refuse `dirty`/`up_to_date`
   with a narrated reason.
2. **Strategy argv** (stock git only; all plumbing — no worktree is created):

| Strategy | Git argv (exact) | Result |
|---|---|---|
| `merge` | `git merge-tree --write-tree --name-only <base_sha> <head_sha>` → tree `T` (exit 1 ⇒ abort, narrate conflict paths) | merge commit |
| `merge` (commit) | `git commit-tree T -p <base_sha> -p <head_sha> -m <message>` with `GIT_AUTHOR_NAME/EMAIL/DATE` = merging principal, `GIT_COMMITTER_NAME/EMAIL` = server identity (`walhub <server.identity_email>`, default `walhub@localhost`), committer date = now | the merge commit sha |
| `squash` | same trial `merge-tree`, then `git commit-tree T -p <base_sha> -m <title>\n\n<flattened body>` (author = principal, committer = server) | single-parent commit |
| `rebase` | plant temp branch `refs/heads/walhub-tmp-replay-<pid>-<nanos>` at `<head_sha>` (`git update-ref`, unique per call, deleted on every exit path), then `git replay --onto <base_sha> --ref-action=print <merge_base>..<tmpref>` → parse the `update <tmpref> <new> <old>` line for tip `R` (plumbing, no worktree); then `git commit-tree` is NOT used — `R` is published as-is | replayed tip; original authorship preserved per commit, committer = server identity |

> Rebase argv rationale (verified live against stock git 2.53): `git replay`
> (experimental) only tracks *branches* named in the revision range — a
> pure-SHA range is a silent no-op (exit 0, empty stdout), so the bare
> `<merge_base>..<head_sha>` form can never yield a tip. `--ref-action=print`
> is load-bearing: default update mode moves refs on disk directly, behind
> the WAL's back; print mode emits `update` lines and touches nothing (the
> temp branch create/delete are the only serving-repo writes, and no
> user-visible ref is ever moved except through the WAL publish in step 5).

3. **Message templates.** Default titles: merge — `Merge pull request #<num> from <head-ref-shorthand>`;
   squash/rebase — the head's subject (+ body = squashed head messages or the request's
   `commit_message`). `commit_title`/`commit_message` overrides win. Default-branch merges of protected
   refs append `(<full sha>)` per GitHub convention — the UI renders the sha link.
4. **Protected-ref gate (policy interplay).** The base-ref publish is NOT a receive-pack push, so the
   merge task MUST explicitly evaluate `policy.json` for `(principal = merger, ref = base.ref,
   op = create-or-update)`: `protect` rules deny the merge exactly as they would deny a push
   (plain-text reason `rejected by rule '<name>'`), and the required-checks gate (doc 05) MUST be
   consulted when the rule carries it — unmet required checks on the head sha reject the merge with the
   named checks in the narration. Bypass lists (e.g. `svc:merge-queue`) apply unchanged. Evaluation is
   pure and local (already-loaded policy per `14_extensibility.md` Seam 3); no network in eval.
5. **Publish:** REF_UPDATE WAL entry for `refs/heads/<base>`: `old = <base_sha at start of publish>,
   new = <merge commit>`. The WAL publish CAS (doc 05) arbitrates against concurrent pushes: if the live
   base sha moved between step 4 and publish (409 on the CAS), re-verify §4 once and either recompute or
   fail with narration — the merge task NEVER force-publishes (a non-ff base outcome is a task failure,
   never a rewrite of history).
6. **Commit (P3/P4/P8):** append `merged` event to the thread (state → `closed`, `merged:true`),
   CAS-update `pr.json` (`merged`, `merge_commit_sha`, `merged_by`, `merged_at`), update the shared
   `issues/index.json`, fan out notifications + the PR-closing cross-ref event per 02's contract: the
   merge task calls 02's `ApplyClosingReferences` seam (keyword list + event shapes owned there) with
   the merged head sha and PR title/body — 02 owns resolution mechanics; 03 only supplies the inputs.
7. **Head cleanup:** if `delete_head`, delete `refs/heads/<head-ref>` via the same WAL publish path
   (delete op, same policy evaluation against the head ref's rules). Fork heads are never deleted by
   the base repo.

### Concurrency

Merge arbitration is exactly two mechanisms, no cross-feature locks: **(a) task single-flight** —
`(repo, "pull-merge")` serializes merges per repo (a second `POST …/merge` for a different PR joins the
running task and then re-verifies its own stamp — serialization is per-repo, matching the git-pool
granularity, and merge volume is human-rate); **(b) the protected-ref CAS** — the final REF_UPDATE is
subject to the manifest CAS ladder (doc 05 publish path); a base sha that moved underneath loses the CAS
and the task re-plans or fails loudly. No repo lock is held across the git subprocesses (13 §2 rule 4);
the task holds no `syncMu`/`packMu`/`rw` — it goes through the same `RepoHandle` sync/read-guard path as
any reader.

## 6. Force-push to the PR head branch

Force-pushing the head branch is ALLOWED (subject to that ref's own policy rules). Review threads
survive: review comments anchor to (path, blob-sha-of-line-content) diff anchors per doc 04 — an
outdated anchor renders as "outdated" and never blocks. On a forced update the `pulls` sink recomputes
mergeability (the head stamp changed) and the thread records a `head_force_pushed` event (compensating
evidence, never an edit). The base side can never be force-pushed by a PR: merges publish only
fast-forward outcomes (§5 step 5).

## 7. Forks

`POST /api/v1/repos/{owner}/{repo}/forks` (top-level; see §8) creates `repos/<o2>/<r2>/` as a fork:

- **Fork = new repo prefix sharing the SAME `wal/<checksum>` objects by construction.** The fork's
  `manifest.pb` references the parent's pack set verbatim (same pack checksums, same side files) plus a
  **fresh refs snapshot** copied from the parent's current ref state. No pack bytes are copied or
  re-uploaded: dedup is a property of content addressing, not a step.
- Execution: task kind `pull-fork` (`(repo, kind)` single-flight) reusing the `import --direct`
  machinery (doc 11) in its "already-on-bucket" mode: skip pack uploads entirely (HEAD-skip treats
  shared packs as existing), verify closure of the referenced pack set, write fork-side state, ref
  snapshot + checkpoint, then `Create` the fork's `manifest.pb` (`min_seq = seq+1`, `first_state_at`
  fresh). A `Create` conflict on the new repo's manifest = the target name is taken (the CAS decides
  ownership, exactly like repo create, 13 §3 `"create:"` key).
- The fork gets its own refs namespace, own `policy.json`, own `access.json` (creator = admin), own
  collaboration families (issues/PRs numbering starts fresh). Only `wal/<checksum>` objects (packs,
  bitmaps, commit-graph) and the eventual side files are shared; `refs.pb`/checkpoints/manifest are
  per-repo by definition.
- **GC rule (load-bearing for forks):** pack removal (maintain's `removeSuperseded`) deletes a pack only
  when NO manifest in the fork network still references it. The parent's `meta/forks.json` lists
  children (`[{repo: "o2/r2", forked_at: RFC3339}]`); before deleting superseded packs, the maintain
  unit consults the children's manifests' pack sets (bounded: one conditional GET per direct child;
  grand-children discovered transitively, one level per pass — repairable, not blocking). A pack whose
  removal is blocked stays pending per the existing TryLock-or-defer protocol (13 §2.1).
- Cross-fork PRs: the PR's `head.repo` names the fork; diff/commits endpoints read objects through the
  fork's manifest (they share the packs, so reads are as cheap as same-repo); the head ref lives in the
  fork's ref namespace and `refs/pull/<num>/head` is published in the BASE repo only when the head
  commit is reachable in the base's object set — cross-fork heads that are not yet merged-reachable are
  spec'd as: publish `refs/pull/<num>/head` in the base only if reachable; otherwise the PR records the
  fork-local head ref and the diff endpoint resolves through the fork (the merge task fetches nothing —
  shared packs make it local).

### Concurrency

Fork creation is `(repo, "pull-fork")` single-flight plus the `Create`-on-manifest arbitration (two
forks to the same target name: one Create wins, the other reports 409 with the winner's URL). Compaction
vs fork GC: pack deletion stays under the existing try-lock-or-defer protocol; the fork-network liveness
check runs before the TryLock attempt and re-reads manifests — it never holds locks across store calls
(13 §2 rule 4).

## 8. API endpoints

Registered by the `pulls` RouteProvider via `api.Lanes` (both lanes). Wire conventions per
`docs/go/07_api.md` §2: plain-text errors, `[]` never null, RFC 3339 UTC, full SHAs, no-store on task
starts. Auth levels are P6 roles resolved per P6 §1–4.

| METHOD + path (repo-scoped `/{o}/{r}/api/…`) | Auth | Request → response | Seam |
|---|---|---|---|
| `GET …/pulls?state=&base=&head=&sort=&n=&after=` | read | → `{pulls:[{num, title, state, author, base_ref, head_ref, head_sha, draft, updated_at}], more}` (index-first per P4) | RouteProvider |
| `POST …/pulls` | write | `{title, base_ref, head_ref, body?, fork?}` → `201` PR header (`409` if an OPEN pr already pairs base+head; `422` unresolvable refs) | RouteProvider |
| `GET …/pulls/{num}` | read | → header + `pr.json` + live `mergeable` (stamped; §4) — SWR + ETag `<head sha>` | RouteProvider |
| `GET …/pulls/{num}/diff` | read | → `text/plain` unified diff `base…head` (one well-formed `git diff` patch per spec §9.5; the 12_web_ui.md parser's exact input) | RouteProvider |
| `GET …/pulls/{num}/commits` | read | → `{commits:[Commit], more}` (doc 07 `Commit` shape; skip/n pagination) | RouteProvider |
| `PUT …/pulls/{num}` | write | `{title, body?, state?}` — title/state edits; close/reopen append events; triage may close others' | RouteProvider |
| `POST …/pulls/{num}/merge` | maintain | `{strategy, commit_title?, commit_message?, delete_head?}` → SSE task attach (`pull-merge`) | RouteProvider + task kind `pull-merge` |
| `POST …/pulls/{num}/update-branch` | write | `{expected_head_sha?}` → task `pull-update-branch` (merge base→head; 409 if dirty or sha mismatch) | task kind |
| `DELETE …/pulls/{num}/head` | maintain | delete the head branch post-merge (policy-checked like any ref delete) | RouteProvider |
| `POST /api/v1/repos/{owner}/{repo}/forks` | write (+create rights on target) | `{target_owner?, name?}` → `202` + `TaskRecord` (`pull-fork`) | RouteProvider (top-level) + task kind `pull-fork` |

SSE: long work (`pull-merge`, `pull-fork`, `pull-mergeable` cold recompute) is a task with the §9.3
envelope; live PR events ride the repo collaboration SSE stream (the single stream endpoint named by
06/08) with event name `pull` — data `{action:"opened"|"closed"|"reopened"|"merged"|"head_force_pushed",
num, title, state, author, base_ref, head_ref, head_sha}`; comment events arrive as 02's `comment`
event; check results as 05's `check` event. Errors map per 07 §2 (unknown PR → `404`, unmet protection
→ `409` with the rule-named reason).

## 9. UI and SDK

Pages (vanilla ESM SPA, `12_web_ui.md` patterns — `useData` two-step, SSE via the SDK readers, lazy
`import()` routes; exact-shape coordination with 08):

| Route | Page | Notes |
|---|---|---|
| `/:o/:r/pulls` | PR list (state tabs open/closed/merged, index-first, SSE-refreshing cards) | extends the repo shell tabs |
| `/:o/:r/pull/{num}` | Conversation: timeline (P3 events), comment box, merge box (strategy select, mergeable state, task progress via SSE) | |
| `/:o/:r/pull/{num}/commits` | commits of `base…head` (reuses `commits` page) | |
| `/:o/:r/pull/{num}/files` | diff view: `parsePatchFiles` on the diff endpoint's patch (12 §2.8 grammar), per-file unified/split toggle, anchors for review threads | |

SDK (`web/sdk/src/` submodule additions, esbuild-bundled into `repos.js` per 12 §1.0): `pulls.js` —
`list/open/get/diff/commits/merge/updateBranch/deleteHead`, plus `forks.js` (`fork(repo, opts)`) and a
`pulls` group on the repo client; diff rendering reuses `lib/diff.js` unchanged (dogfood rule:
every call goes through the SDK).

## Decisions

- PR threads reuse the issue thread pattern and numbering wholesale (P2/P3) — one conversation
  implementation, `kind` is the only difference.
- PR list state lives in the shared `issues/index.json` (kind-filtered per 02's contract) — one index,
  one compaction story; `pulls/` holds only PR-sidecar state.
- `refs/pull/<num>/head` is published server-side ONLY for already-reachable heads; the PR is a
  ref-publisher, never an object publisher (WAL stays git-only, README law).
- `refs/pull/**` is server-managed via a built-in policy default, not a new receive-pack branch.
- Mergeability is a stamped derived cache (CAS'd), invalidated by a `pulls` event sink — no polling, no
  cross-feature locks.
- Merge = task kind with per-repo single-flight + publish-time CAS; strategies are pure-plumbing git
  argv (`merge-tree`/`commit-tree`/`replay`) — no worktree, no index state.
- Fork = fresh manifest referencing the parent's packs; sharing is by construction, and compaction's
  pack removal consults fork-network manifests before deleting.
- Closing keywords ride 02's cross-ref contract; `merged` events are the PR-side trigger.

## Explicitly out of scope

- Merge queues, batch/auto-merge, mergeability of stacked PRs beyond simple ancestry.
- Draft PRs beyond a `draft` flag (no review gating semantics — doc 04 owns review requirements).
- Cross-server PRs (heads on unrelated hosts); fork networks are same-store by construction.
- PR templates, milestone/assignee merge gates, project boards (P9).
- Automatic conflict resolution, mergeability of secrets/large-file rewrites, rebase-with-merge
  strategies beyond the three named.

## Wave C1 implementation notes (2026-09-04, `internal/pulls` landed)

- **Seam 1 in code is the `server.ExtraRoutes` chain**, not `api.Lanes` — the
  package fronts the core mux via `Handle(w, r) bool` on both lanes, exactly
  like `internal/identity` and `internal/issues` (see the Wave A amendment in
  14_extensibility.md Decisions). Route-for-route the §8 table is implemented
  verbatim, plus two additive endpoints (below).
- **`POST …/pulls/{num}/comments` is additive** (write, authenticated): the
  conversation box needs it; 02's comment endpoints 404 PR-kind threads by
  contract, so 03 owns PR comments with the same P3 two-step.
- **`GET …/pulls/{num}/merge/task` is additive** (read): the SSE-attach poll
  for the running pull-merge task — the record carries progress packets plus
  the terminal outcome, so lagged attachers never miss it (13 §6). Finished
  records stay in a bounded (128) recent cache for late attachers.
- **`pr.json` carries an additive optional `body`** (editable description;
  14 §14.12 field rule): the opened event carries the original body (P3
  immutable), `Body` is the editable view. `GET …/pulls/{num}` also serves
  the last-K timeline (`events`, newest-first, `after_seq`/`n` windows) —
  the Conversation page needs it and no separate events endpoint is
  warranted.
- **Trial merge uses the two-positional form** `git merge-tree
  --write-tree --name-only <base_sha> <head_sha>` everywhere (the §5 argv
  verbatim): the §4 three-positional form (`<merge_base> <base> <head>`) is
  a usage error (exit 129) on modern git — verified against stock git 2.53
  in `gitexec_test.go`, which runs every SubprocessGit verb against real
  repos (clean AND conflicting merges, commit-tree, rev-list counts,
  reachability, diff, log ranges). The merge base is still computed
  separately (the mergeable doc needs it; rebase needs it) and conflict
  paths parse as lines-after-tree up to the first blank line.
- **Log pagination uses `--skip=N --max-count=M`** (`-n=N` is rejected by
  stock git).
- **Required-checks runs as a raw-JSON pre-scan before the strict parse**:
  `protect` is strict (an unknown `required_checks` key would die at parse),
  so the gate scan runs first and fails closed (`rule '<name>' requires
  checks: no checks backend`) only when a rule actually carries the gate;
  plain protect rules merge fine. Pending Wave 05 (`internal/checks`
  absent) — the scan is where the head-sha verdict plugs in.
- **Wave 05 landed (2026-09-04, `internal/checks`):** the pre-scan is
  gone. `require_checks` is a strict optional field inside `protect`
  (1–32 validated contexts; the push half ignores it), and the merge
  task consults 05's stored-combined-view gate at the step-4 call site
  through pulls' `ChecksGate` seam (next to the 04 gate) — the merge
  logic is NOT forked. Nil backend fails closed only when a rule
  actually carries the gate (same rule as the pre-scan kept).
- **First fetch serves `unknown`** (§4 second line of defense): stamp
  mismatch (or miss) serves `unknown` + enqueues `pull-mergeable`; the
  background pass converges the cache and the next fetch serves the stamp
  (proven by polling tests, never by sleeping).
- **List enriches from `pr.json` sidecars** (one GET per listed row,
  page-bounded ≤ 100, never a LIST): the shared card has no base/head
  fields, and 02's card rendering must keep working header-alone.
- **Update-branch merges base INTO head** (trial with base=head, head=base;
  commit parents `[headSHA, baseSHA]`; head publish `old=headSHA`).
- **Fork lands the collaboration records + task** (`fork.json` Create
  arbitrates the target name; parent `forks.json` CAS lists the child; the
  `pulls` stream fans a `forked` notice): the manifest-sharing step
  (verbatim pack-set reference + fresh refs snapshot + checkpoint) is a
  `ForkExecutor` seam, nil in this wave — the task narrates the delegation
  instead of pretending. The fork-network GC rule (§7) is specified now
  (pack removal consults children's manifests); enforcement lands with the
  executor. Completion additionally CAS-increments the parent's
  `social.json` forks counter through pulls' `ForksCounter` seam (07 §6
  owns the field; nil-safe, best-effort with a task notice — the fork
  objects are committed either way).
- **Backend outages propagate as 503, never misreported**: `ResolveRef`
  maps only genuine git exits to unknown-revision; transport failures
  (pool, timeout, missing binary) propagate as unavailable — same for
  `MergeBase` and `IsAncestor` (a missing binary reports an error, never a
  false non-ancestor). Found by tests, fixed in the same change.
- **Mergeable single-flight key** `"mergeable:"+repo+"/"+num` (joiners share
  the leader, bounded wait); **merge arbitration** is the `(repo,
  "pull-merge")` task join plus the WAL publish CAS (never force).
- **ETag `<head sha>` + SWR** on `GET …/pulls/{num}`; `no-store` on task
  starts; plain-text errors with the 07 §2 status mapping (unknown PR →
  `404`, unmet protection → `409` with the rule-named reason, unreachable
  head → `422`).
- **Ancestry correction (found live, 2026-09-04):** §4's first ancestry
  parenthetical reads "`merge-base --is-ancestor <base> <head>` (exit 0 ⇒
  … `up_to_date`)". Taken literally, every ordinary PR (head branched off
  base ⇒ base ⊆ head) reports `up_to_date` and the merge task refuses it —
  demonstrated against the running stack before the fix. The implementation
  follows the section's own second clause instead: `up_to_date` ⟺
  `merge-base --is-ancestor <head> <base>` exit 0 (head fully merged into
  base, nothing to merge); base-⊆-head is the normal shape and proceeds to
  the trial merge. Code and doc agree as of this note; the normative fix is
  recorded here, not silently applied.
- **Evidence E4** (`docs/EVIDENCE.md`): mergeability recompute and merge
  task budgets measured on the real code path (memory store + scripted git;
  git argv proven separately against stock git) — 0 LIST on every path,
  bounded GETs/PUTs/git calls at 1 and 50 open PRs, 16-reader single-flight
  collapse to 1 merge-tree.
