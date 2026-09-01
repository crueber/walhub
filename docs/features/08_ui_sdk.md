# 08 — UI & SDK: the collaboration surfaces (issues, PRs, review, checks, releases, notifications)

> Depends on 01–07 for object shapes and endpoint semantics; **normative for `web/src/**` and `web/sdk/src/**`**.
> This is the UNIFIED frontend plan: route table, shared components, data-layer cache keys, SDK submodules,
> SSE wiring, permissions-driven gating. Feature docs sketch their pages; THIS doc owns the routes, the
> component inventory, and the wire-facing SDK surface. Where a feature doc and this doc disagree on an
> endpoint path, the feature doc owns the wire and this table is corrected in the same change.

Everything reuses the frozen patterns of `docs/go/12_web_ui.md` — reactive core (§2.1), `useData` TTL cache
(§2.4), `mountStream` SSE helper (§2.5), unified-diff parser (§2.8), hand-rolled router (§2.3) — and the wire
conventions of `docs/go/07_api.md` §2: plain-text errors shown verbatim, arrays `[]` never null, RFC 3339,
SSE envelope (§6), SWR/ETag vs no-store cache classes (§4). No new npm dependencies (law 1: zero runtime,
esbuild dev-only). No TypeScript, no framework.

## 1. Route table additions (SPA)

Routes are additions to the 12_web_ui.md §2.3 inventory; every route returns the SPA `index.html` and is
lazy `import()`ed. Repo tabs extend to: **Code, Issues, Pulls, Releases, Commits, WAL, Settings**.

| Route | Page module (`web/src/pages/`) | Gating (P6 role) | Notes |
|---|---|---|---|
| `/:owner` | `owners.js` (extended) | read | Renders an **org profile** when `owner` is an org: repos, members, description; else the existing user page |
| `/:owner/:repo/issues` | `issues.js` | read | Filterable list (state/labels/assignee/milestone), index-backed, "Load more" by `after` cursor |
| `/:owner/:repo/issues/new` | `issueNew.js` | authenticated | Title + markdown-lite composer |
| `/:owner/:repo/issues/:num` | `issue.js` | read | ThreadTimeline + sidebar (labels, assignees, milestone, state) |
| `/:owner/:repo/labels` | `labels.js` | triage+ | Label CRUD |
| `/:owner/:repo/milestones` | `milestones.js` | triage+ | Milestone CRUD + progress |
| `/:owner/:repo/pulls` | `pulls.js` | read | List with open/closed/merged tabs, same card shape as issues (`kind` distinguishes) |
| `/:owner/:repo/pulls/new` | `pullNew.js` | write+ | Head/base pickers via `repo.refStream` (§2.6 pattern) |
| `/:owner/:repo/pull/:num` | `pull.js` | read | Conversation + MergeBox (per 03; singular `pull` is 03's spelling) |
| `/:owner/:repo/pull/:num/commits` | `pullCommits.js` | read | PR commit list |
| `/:owner/:repo/pull/:num/files` | `pullFiles.js` | read | DiffPage over `parsePatchFiles`; line-thread anchoring |
| `/:owner/:repo/checks` | `checks.js` | read | Paged index of checked shas (state pills, filter, live rows via `check` frames) |
| `/:owner/:repo/checks/:sha` | `checks.js` | read | Statuses for one commit; linked from commit detail, CheckPill, and MergeBox |
| `/:owner/:repo/releases` | `releases.js` | read | Per 07: list, latest |
| `/:owner/:repo/releases/{tag}` | `releases.js` | read | Detail: markdown-lite body, assets table |
| `/:owner/:repo/releases/new` | `releaseNew.js` | maintain+ | Tag picker (`refStream`), autodraft fill, asset upload with client-side `crypto.subtle` sha256 |
| `/notifications` | `notifications.js` | authenticated | Full tray page; the dropdown tray (below) is global chrome |
| `/:owner/:repo/settings/{tab}` | `settings.js` (extended) | varies | Sub-tabs: `access`, `collaborators`, `webhooks`, `ci-tokens` (admin); `labels`, `milestones` (triage+); existing scheduled/policy/config tabs unchanged |
| `/:owner/settings/{tab}` | `orgSettings.js` | org owner/admin | Org sub-tabs: profile, members, teams, invites, webhooks |
| `/:owner/teams/:slug` | `team.js` | read | Team page: members, team repos (per 01) |

Global chrome additions: notification bell + **NotificationTray** dropdown (all pages), star/watch toggles in
the repo header (07, optimistic update + rollback), "New issue"/"New pull request" buttons in repo tabs.

## 2. Shared components (`web/src/components/`)

| Component | Purpose | Contract |
|---|---|---|
| `ThreadTimeline` | Renders a P3 event log (issue thread, PR conversation, review thread). One row per event kind: opened, commented, labeled, unlabeled, state-changed, assigned, referenced, review-posted, check-reported, merged, closed, reopened | Input: header + seq window of events (`{after_seq, n}`); compensating events render as normal rows (never rewrite history); comment bodies via markdown-lite + sanitizer; `aria-live="polite"` region so SSE-appended rows announce; dedup key `(num, event_seq)` |
| `CommentComposer` | New-comment editor | markdown-lite preview (`lib/markdown.js` + `lib/sanitize.js`); **mentions autocomplete**: `@` opens a popup fed by `useData("assignables:{o}/{r}")` (repo collaborators ∪ org members, endpoint per 01); submits via the feature's comment endpoint → `{event_seq}` |
| `LabelPicker` | Apply/remove labels on an issue/PR | Source: `labels:{o}/{r}` cache; each toggle = one PATCH (one event per 02); triage+ only |
| `AssigneePicker` | Same, for assignees | Source: `assignables:{o}/{r}`; triage+ only |
| `DiffPage` | Renders a PR/commit diff | Feeds `parsePatchFiles` (12 §2.8 grammar) the `text/plain` unified patch; per-file unified/split toggle, file anchors by `stats[].path`; **line-thread anchoring**: anchor key `(path, side: old\|new, line)`; per-line comment forms create threads via `reviews.threadCreate`; threads whose anchor no longer exists on the current head's diff render collapsed with an "outdated" flag |
| `MergeBox` | PR merge control | State machine below; renders the required-checks gate (05: `protect.require_checks`, evaluated by 03's `merge` task) and review-requirement state (04); the disabled button's tooltip lists missing/failing contexts |
| `CheckPill` | Aggregate check state for a sha | Input: `GET …/api/checks/{sha}` combined state (worst-of: error > failure > pending > success, with `total_counts`); pill tooltip lists contexts; links to `/:o/:r/checks/:sha` |
| `ReviewSummaryBar` | Review rollup on the PR page (approvals / request-changes / states) | Source: `reviews:{o}/{r}:{num}`; updates on `review` SSE frames (04) |
| `ReviewModal` | "Finish review" — comment / approve / request-changes + pending line comments collected on the diff | Submits `reviews.create(num, {event, body?, commit_sha, threads?})`; dismiss button (maintain+) |
| `ReviewerPicker` | Request reviewers | Fed by `reviews.suggest(num, q?)` (access.json roles + commit authors) |
| `NotificationTray` | Bell + dropdown + `/notifications` page | Source: per-user stream + `notifications:me` cache; unread count as a signal; "mark read"/"mark all read" |

**MergeBox state machine** (per 03/04/05): `draft → ready → blocked{checks, reviews, conflicts} → mergeable
→ merging(task) → merged | failed`. Transitions recompute on: header fetch, `check` SSE frame, `review` SSE
frame, task packets. The merge button enables only in `mergeable` AND role ≥ maintain. Merge runs the
`pull-merge` task kind (P7): `POST …/pulls/{num}/merge` attaches to the task SSE stream; progress pills
render from `progress` packets, terminal `result`/`error` per the 07 §6 envelope. An "Update branch"
action (write+) runs the `pull-update-branch` task the same way.

### Concurrency — MergeBox task attach
Hazard: double-clicking merge starts two `merge` tasks on the same PR; unmount mid-merge leaks the attach
reader. Avoidance: the `(repo, kind)` single-flight of the task table (13_concurrency.md §3) is the
authoritative dedup — the server joins a running merge; the client additionally disables the button while
`state === "merging"`. Attach uses the 12 §2.5 `mountStream` pattern: one component-scoped
`AbortController`, unmount aborts, the SDK closes the reader (12 §1.6 ownership rule). Terminal packet flips
the state machine exactly once.

## 3. SDK additions (`web/sdk/src/`)

New submodules, same style as `repo.js` (plain ES modules, `ReposError`, `_call` wrapper, trailing
`{signal, onProgress, headers}` on every method; per 12 §1.6 each method derives its own `AbortController`
and closes its reader in `finally`). `index.js` re-exports the new groups; the esbuild bundle
(`make web` → `web/dist/repos.js`) is UNAFFECTED mechanically — new modules join the bundle, no new
devDependency, no config change. JSDoc `@typedef`s for each response shape land in `types.js`.

Client wiring: `client.repo(o/r)` gains `.issues`, `.pulls`, `.reviews`, `.checks`, `.releases`, `.social`,
`.access`, `.collaborators`; `client` gains `.notifications`, `.orgs`, `.invitations`, `.forks`, `.starred`.

**Seam registration (14 §14.3):** every endpoint below is registered by its feature package's
`api.RouteProvider` on BOTH lanes (`api.Lanes`; `internal/issues`, `internal/pulls`, `internal/reviews`,
`internal/checks`, `internal/releases`, `internal/notifications`, `internal/identity`). The submodule in
this section is the client mirror of exactly one provider; auth levels shown per method resolve per P6.

### 3.1 `issues.js` → `repo.issues` (02)

| Method | Endpoint | Shape |
|---|---|---|
| `list({state, labels, assignee, milestone, since, after, n})` | `GET …/issues` | `{issues: [], more}` |
| `create({title, body, labels?, assignees?})` | `POST …/issues` | `201 {num}` |
| `get(num)` | `GET …/issues/{num}` | thread header (P3) |
| `events(num, {after_seq, n})` | `GET …/issues/{num}/events?after_seq=&n=` | `{events: [], more}` seq window |
| `patch(num, fields)` | `PATCH …/issues/{num}` | one event per field group |
| `comment(num, body)` | `POST …/issues/{num}/comments` | `201 {event_seq}` |
| `react(num, {target_event_seq, content})` | `POST …/issues/{num}/reactions` | `201` |
| `unreact(num, {target_event_seq, content})` | `DELETE …/issues/{num}/reactions/{target_event_seq}/{content}` | self-removal (read) |
| `labels.list()/get(name)/put(name, {color, description})/delete(name)` | `…/labels[/{name}]` | label CRUD |
| `milestones.list()/create(...)/patch(id)/delete(id)` | `…/milestones[/{id}]` | milestone CRUD |

### 3.2 `pulls.js` → `repo.pulls` (03)

| Method | Endpoint | Shape |
|---|---|---|
| `list({state, base, head, sort, after, n})` | `GET …/pulls` | `{pulls: [], more}` |
| `open({title, body, head, base, draft?})` | `POST …/pulls` | `201 {num}` (write) |
| `get(num)` | `GET …/pulls/{num}` | header + `{mergeable, merge_state}` (SWR + `ETag: "<head sha>"`) |
| `update(num, fields)` | `PUT …/pulls/{num}` | title/body/state (close/reopen; write) |
| `diff(num)` | `GET …/pulls/{num}/diff` | `text/plain` unified patch base…head (DiffPage parser input), `ETag: "<head sha>"` |
| `commits(num)` | `GET …/pulls/{num}/commits` | `{commits: [Commit]}` |
| `merge(num, {merge_method, commit_title?, commit_message?}, onEvent)` | `POST …/pulls/{num}/merge` | maintain; SSE task attach, task kind `pull-merge` (envelope §6) |
| `updateBranch(num, onEvent)` | `POST …/pulls/{num}/update-branch` | write; task `pull-update-branch` |
| `deleteHead(num)` | `DELETE …/pulls/{num}/head` | maintain |

`forks.js` → `client.forks.create(owner, repo, onEvent)` → `POST /api/v1/repos/{owner}/{repo}/forks`
(write + create rights on target) → `202` + TaskRecord, task kind `pull-fork` (top-level route, same
`pulls` RouteProvider per 03).

### 3.3 `reviews.js` → `repo.reviews` (04)

| Method | Endpoint | Shape |
|---|---|---|
| `list(num)` | `GET …/pulls/{num}/reviews` | `{reviews: []}` |
| `create(num, {event, body?, commit_sha, threads?})` | `POST …/pulls/{num}/reviews` | `201`; `event: APPROVE\|REQUEST_CHANGES\|COMMENT`; `threads[]` = pending line-anchored comments |
| `get(num, seq)` | `GET …/pulls/{num}/reviews/{seq}` | one review |
| `dismiss(num, seq)` | `POST …/pulls/{num}/reviews/{seq}/dismiss` | maintain+ |
| `requests.list(num)` / `requests.add(num, names)` / `requests.remove(num, names)` | `GET/POST/DELETE …/pulls/{num}/review-requests` | reviewer requests (write+) |
| `suggest(num, q?)` | `GET …/pulls/{num}/review-suggest?q=` | reviewer auto-suggest (access.json roles + commit authors) |
| `threads(num, {…})` | `GET …/pulls/{num}/threads` | list with anchors + resolution |
| `threadCreate(num, {path, side, line, body})` | `POST …/pulls/{num}/threads` | create line thread from a diff hunk |
| `thread(num, id).get()` | `GET …/pulls/{num}/threads/{tid}` | thread header (P3) |
| `thread(num, id).comment(body)` | `POST …/pulls/{num}/threads/{tid}/comments` | `201` |
| `thread(num, id).resolve()` / `.unresolve()` | `POST …/pulls/{num}/threads/{tid}/resolve` \| `/unresolve` | resolve toggle |

### 3.4 `checks.js` → `repo.checks` (05)

| Method | Endpoint | Shape |
|---|---|---|
| `combined(sha)` | `GET …/checks/{sha}` | `{sha, state, total_counts, statuses: []}` (worst-of rollup; no-store — sha-addressed but mutable) |
| `statuses.get(sha)` | `GET …/checks/statuses/{sha}` | `{sha, statuses: []}` |
| `report(sha, {context, state, target_url?, description?, started_at?, completed_at?})` | `POST …/checks/statuses/{sha}` | CI surface: `checks:write` token or write role → `200` |
| `list({after, n})` | `GET …/checks?after=&n=` | `{checks: [{sha, state, contexts: []}], more}` (n 50/200) |
| `tokens.create()/list()/revoke(id)` | `POST/GET …/checks/tokens`, `DELETE …/checks/tokens/{id}` | CI tokens (admin) |

### 3.5 `notifications.js` → `client.notifications` (06)

| Method | Endpoint | Shape |
|---|---|---|
| `list({state, after, n})` | `GET /api/v1/notifications?state=&after=&n=` | `{notifications: [], more}` |
| `unreadCount()` | `GET /api/v1/notifications/unread_count` | `{count}` |
| `markRead(id)` / `markUnread(id)` | `POST /api/v1/notifications/{id}/read` \| `/unread` | → Notification |
| `markAllRead()` | `POST /api/v1/notifications/read_all` | `{updated}` |
| `stream(onFrame, opts)` | `GET /api/v1/notifications/stream` | per-user SSE (envelope §6); frames `event: notification` with the notification object; runs until cancelled — returns a cancel function |
| `watch.get(o, r)` / `watch.set(o, r, bool)` | `GET`/`PUT`/`DELETE …/api/watch` | `{watching, count?}` (read to toggle; 06 owns the verb — see Decisions) |

### 3.6 `orgs.js` → `client.orgs` + repo permission groups (01, 06)

| Method | Endpoint | Shape |
|---|---|---|
| `orgs.list()` / `orgs.create({name, ...})` | `GET/POST /api/v1/orgs` | sorted list; create (write auth) |
| `orgs.get(org)` / `orgs.put(...)` / `orgs.delete(org)` | `GET/PUT/DELETE /api/v1/orgs/{org}` | org profile; PUT/DELETE = org owner |
| `orgs.members.list(org)` / `add/remove(org, principal)` | `…/orgs/{org}/members[/{principal}]` | membership (PUT/DELETE = owner) |
| `orgs.teams.list/create(org)` / `get/put/delete(org, slug)` | `…/orgs/{org}/teams[/{slug}]` | teams (owner) |
| `orgs.teams.addMember/removeMember(org, slug, principal)` | `…/teams/{slug}/members/{principal}` | team membership (owner) |
| `invitations.create(org, ...)` / `invitations.mine()` / `accept(id)` / `decline(id)` | `POST …/orgs/{org}/invitations`; `GET/POST/DELETE /api/v1/invitations[/{id}…]` | per 01 (owner invites; accept/decline mine) |
| `repo.permissions()` | `GET …/api/permissions` | `{role}` per P6 (anonymous → `{role: null}` or `read` when `anonymous_read`) |
| `repo.access.get()` / `repo.access.put(bindings, version)` | `GET/PUT …/api/access` | role bindings; GET triage+, PUT admin (full-doc replace, CAS version, `409` on mismatch) |
| `repo.access.invitations.list/create()/delete(id)` | `POST/GET …/api/invitations`, `DELETE …/api/invitations/{id}` | repo invites (admin) |
| `repo.collaborators.list()` | `GET …/api/collaborators` | effective bindings + resolution source |
| `repo.assignables()` | `GET …/api/assignables` | `[{principal, display}]` |
| `repo.webhooks.list/create/update/remove(id)/ping(id)/deliveries(id)` | `GET/POST …/api/webhooks`, `GET/PATCH/DELETE …/webhooks/{id}`, `POST …/webhooks/{id}/ping`, `GET …/webhooks/{id}/deliveries` | per 06; the secret is never returned (`secret_set`) |

### 3.7 `releases.js` + `social.js` (07)

`repo.releases`: `list({n, after})`, `latest({include_prereleases?})`, `get(tag)`, `put(tag, {body, name?,
draft?, prerelease?}, ifMatch?)`, `delete(tag)`, `uploadAsset(tag, name, bytes, {sha256})`,
`deleteAsset(tag, name)`, `autodraft({tag, since})`. `repo.social`: `get()` → `{stars, watchers, forks,
viewer:{starred, watching}}`, `star()/unstar()` (watch lives in `client.watch`, §3.5 — Decisions).
`client.starred({n, after})`, `client.userStarred(principal)` for the top-level starred lists. Asset bytes
download via the static `/{o}/{r}/releases/{tag}/assets/{name}` URL (`browser_download_url`).

### Concurrency — SDK additions
Same hazard set as 12 §1.6: leaked readers, unbounded parallel streams, popup-auth races. Avoidance is
inherited, not reimplemented: every new method goes through `core.js`'s `_call` (one derived
`AbortController` per call, reader closed in `finally`); `notifications.stream` and any task-attach method
return a cancellation function in addition to honoring `signal`; at most one popup-auth promise per client.
No new shared mutable state is introduced by the submodules — each is stateless over the client.

## 4. SSE wiring

One **repo collaboration stream** carries all live collaboration events for a repo; one **per-user stream**
carries notifications. Both use the 07 §6 envelope (`: walgit` opener, `notice`/`progress`/`task`/`result`/
`error` semantics, 10 s keepalives, no-store). Feature mutating handlers publish frames synchronously after
their CAS commits (P8); the streams are the transport only — **the timeline and lists remain the source of
truth**; frames invalidate cache keys, they do not carry full state.

- `GET /{o}/{r}/api/collab/stream` — repo-scoped, read auth (anonymous when `anonymous_read`). Frame
  `event: <kind>` with data `{num?, seq?, kind, sha?, tag?, actor, at}`; kinds: `issue`, `issue_event`,
  `pull`, `review`, `thread`, `check`, `release`, `access`. Defined here because it is cross-feature; each
  feature doc names the frames its handlers emit (02: `issue` `{num, card}` on header mutations and
  `issue_event` `{num, event}` on appended events; 03: `pull` with
  `{action: opened|closed|reopened|merged|head_force_pushed, num, title, state, author, base_ref, head_ref,
  head_sha}`; 04: `review` on posted/dismissed with rollup state, `thread` on comments/resolution with
  `thread_id`; 05: `check` with `{sha, context, state, combined_state, updated_at}`; 07: `release` with
  `{tag, action}`; 01: `access` with `{updated_at}` when access.json CAS commits).
- `GET /api/v1/notifications/stream` — per 06 (authenticated self-only, browser lane credentials); frames
  `event: notification` whose data is the notification object itself; runs until the client cancels.

| Page | Streams | Frame handling |
|---|---|---|
| `issue.js`, `pull.js` | repo stream (filtered by `num`) + per-user stream | matching `issue`/`issue_event`/`pull`/`review`/`thread`/`check` frames: append to ThreadTimeline if `(num, seq)` unseen, invalidate `issue:{o}/{r}:{num}` / `pull:…` / `reviews:…` / `threads:…` keys |
| `issues.js`, `pulls.js` lists | repo stream | invalidate the list cache key on any `issue`/`pull` frame |
| `checks.js`, commit detail, CheckPill | repo stream | `check` frames invalidate `checks:{o}/{r}:{sha}` and the `checkindex:{o}/{r}:*` keys |
| `releases.js` | repo stream | `release` frames invalidate `releases:{o}/{r}` |
| `MergeBox` | merge task attach (`onEvent`) | `progress`/`result`/`error` packets drive the state machine |
| NotificationTray (global, `main.js`) | per-user stream, app-lifetime | unread-count signal; frames also invalidate `notifications:me` |
| Ref/branch pickers | `repo.refStream` (existing §1.5 dialect) | unchanged |

**Invalidation storm control:** a burst of frames (CI posting 30 check runs) MUST coalesce — cache keys are
collected into a set and invalidated once per tick, and the promise-cache already single-flights per key
(one in-flight fetch per key; joiners share it — the client-side analog of 13 §3). Timeline appends bypass
refetch entirely (the frame carries `num`+`seq`; the event body arrives via the normal events window fetch).

**Abort patterns:** every stream is owned by exactly one `mountStream` slot (12 §2.5): `run()` aborts the
previous controller BEFORE opening the new one; unmount cancels; errors set state `"error"` and reconnect
with capped exponential backoff (1 s → 30 s, reset on open) — the page never throws on stream errors.
The tray's app-lifetime stream is started once in `main.js` under a `createRoot`; it is never re-opened per
navigation. Fetch-based readers only (never `EventSource` — 12 §2.5).

### Concurrency — SSE lifecycle
Hazard: leaked readers and stacked streams (one per navigation or keystroke) and out-of-order frames
overwriting fresh state — the §7.1/§7.3 class of incident in client form. Avoidance (13 §5 ownership rules
transposed): the caller (page) owns the signal, the SDK owns and closes the reader; one live stream per
component; rows/threads are keyed (`(num, seq)`, cache key) so a late frame from an aborted stream can never
overwrite fresh data; buffers are bounded (the frame set is a keyed map, never an unbounded queue). Reconnect
backoff prevents a hot-reload loop from stampeding the server.

## 5. Permissions-driven UI

The server is authoritative (P6); client gating is cosmetic — any 403 surfaces its plain-text body in the
error tray. The resolved role comes from `repo.permissions()` cached as `perms:{o}/{r}` (30 s TTL;
`admin`/`write` fallback flags from `GET /api/v1/me` while 01 is in flight).

| Role | UI enables |
|---|---|
| anonymous (`null`) | read-only pages when `anonymous_read`; composer and all pickers hidden |
| `read` | view everything; comment on issues/PRs; open new issues; star/watch |
| `triage` | + label/assignee/milestone pickers, close/reopen others' issues, resolve threads, labels & milestones settings tabs |
| `write` | + new PRs, submit reviews, request reviewers, push |
| `maintain` | + merge (MergeBox button), releases create/edit/delete, protected-ref operations |
| `admin` | + settings access/collaborators/webhooks tabs, access.json editor, repo delete |

**Hide vs disable:** hide entirely when the role is absent (anonymous, or below `read`); render disabled
with a `title` tooltip when the role is present but object state forbids (locked conversation, draft PR,
missing required checks). Pages resolve gating off the cached role signal — no per-component refetch.

## 6. Data-layer cache keys (`lib/data.js`)

| Key | TTL | Endpoint |
|---|---|---|
| `repo:{o}/{r}` | 5 s | `GET …/api` (existing) |
| `perms:{o}/{r}` | 30 s | `GET …/api/permissions` |
| `issues:{o}/{r}:{filter}` / `pulls:{o}/{r}:{filter}` | 5 s | list endpoints |
| `issue:{o}/{r}:{num}` / `pull:{o}/{r}:{num}` | 5 s | headers |
| `events:{o}/{r}:{num}:{after_seq}:{n}` | ∞ | event windows are immutable (P3: events never rewrite) |
| `labels:{o}/{r}` / `milestones:{o}/{r}` | 30 s | label/milestone lists |
| `diff:{o}/{r}:{num}:{headSha}` / `prcommits:{o}/{r}:{num}:{headSha}` | ∞ | sha-addressed content |
| `reviews:{o}/{r}:{num}` / `threads:{o}/{r}:{num}` | 5 s | review surfaces |
| `checks:{o}/{r}:{sha}` / `checkindex:{o}/{r}:{after}` | 5 s | CI updates results (05 GETs are no-store but mutable) |
| `releases:{o}/{r}` / `release:{o}/{r}:{tag}` | 30 s / 60 s | 07 |
| `social:{o}/{r}` | 30 s | 07 |
| `assignables:{o}/{r}` | 300 s | 01 |
| `notifications:me` | 5 s | 06 |

The LRU cap (400) and error-tray behavior are unchanged (12 §2.4).

## 7. i18n and accessibility

**i18n: NONE — English-only v1.** All strings are inline literals in page/component modules; no extraction
keys, no locale files, no translation layer. A future i18n layer is additive (string table swap) and does
not need scaffolding now.

**a11y baseline (no new dependencies):** semantic HTML first (`nav`/`main`/`article` per timeline event,
`form` for composers, `button`/`a` distinctions preserved); modals and pickers (LabelPicker, AssigneePicker,
mentions popup, tray dropdown) trap focus, close on `Esc`, and restore focus to the opener; live regions
(`aria-live="polite"`) for SSE-appended timeline rows and the unread badge; diff line anchors are real DOM
ids so deep links work with keyboard navigation; every action reachable by keyboard; visible focus styles in
the existing CSS files. This is the floor, not the ceiling — no ARIA beyond what semantics require.

## Decisions

- **This doc owns routes/components/SDK surface; feature docs own wire semantics** — one place to build the UI from; conflicts resolve toward the feature doc in the same change.
- **One repo collaboration stream (`/{o}/{r}/api/collab/stream`) instead of per-feature streams** — one connection per page, one parser, kinds namespaced; frames invalidate caches rather than carry state (P8 keeps handlers synchronous; the timeline is the backfill truth).
- **PR diff served as `text/plain` unified patch** — the 12 §2.8 parser is already the contract; no JSON diff shape to invent.
- **`repo.permissions()` endpoint for client gating** — P6 resolution is server-side; the UI reads one resolved role instead of re-implementing resolution order.
- **Cache-frames-not-patch for lists; append-only for timelines** — lists stay simple TTL caches; timelines use `(num, seq)` dedup so SSE and pagination compose.
- **Watch is a subscription verb owned by 06 (`client.watch.{get,set}`), not 07's `repo.social`** — both docs named `PUT/DELETE …/api/watch`; reconciled to 06 (subscriptions/notifications domain). `repo.social.get()` still returns `viewer.watching` for the header toggle's state; star stays in `repo.social`.
- **Hide for absent roles, disable for forbidden states** — anonymous users see no chrome they cannot use; authenticated users see what exists and learn why it is off.
- **English-only v1, no i18n scaffolding** — additive later, zero cost now.
- **Releases/stars SDK per 07's submodule plan** (`releases.js`, `social.js`) — absorbed verbatim to avoid conflicting paths.

## Explicitly out of scope

- Code search UI, Discussions, Projects/boards, Actions/CI running (P9) — no pages, no routes, no SDK groups.
- i18n, themes beyond the existing CSS, dark-mode variants.
- Mobile/responsive redesign beyond the existing CSS baseline; no CSS framework.
- Real-time collaborative editing (presence, live cursors) — SSE carries state changes, not keystrokes.
- Offline mode / service workers; the SPA requires a live server for every data layer key.
- Push notification channels (web push, email digests) — 06's per-user stream and webhooks are the only fan-out surfaces in v1.
