# 12 — Web UI and the `repos.js` SDK (SolidJS SPA)

> Source: MASTER_RUST_SPEC.md §10 (web UI + SDK), §8.8/§8.9 (auth flows and setup recipes consumed by the UI), §9 (wire contract) · Status: normative for the walhub Go implementation.

> **SUPERSEDED IN PART (2026-09-02, explicit user request — DEVIATIONS.md D-WEB-6):** the SPA is now
> **SolidJS + `@solidjs/router`** (plain JSX, no TypeScript) with Tailwind CSS v4 (CSS-first, dark mode
> by default, no CDN) built by **vite** (`vite-plugin-solid` + `@tailwindcss/vite`) into `web/dist/`.
> §1.0's import-map/raw-module serving, §2.1's hand-rolled reactive core, §2.3's hand-rolled router and
> §3's raw-module caching class are superseded; state management is Solid signals/stores + context.
> Still normative and unchanged: the dogfood rule (§6 — the SDK is the only way the SPA talks to the
> server), the hand-rolled pure modules (diff parser §2.8, markdown-lite, sanitizer, highlighter,
> §5 testing tier for them), the SSE ownership/cancellation rules (§2.5), and the `/repos.js` SDK
> contract (§1.1). The old vanilla sources were deleted in the same change (pre-1.0 rule).

The UI is two artifacts in `web/`, served by the same Go binary from the **vite-built `web/dist/`**
— a **SolidJS SPA** (plain JSX, no TypeScript; runtime npm dependencies exactly `solid-js` +
`@solidjs/router`; state via Solid signals/stores + context; Tailwind CSS v4 CSS-first, dark mode by
default, no CDN) plus the dependency-free SDK bundle. Build tooling is dev-time only: `vite` +
`vite-plugin-solid` + `@tailwindcss/vite` build the SPA; `esbuild` bundles the SDK from submodules (§1.0):

| Artifact | Path | Language | Dependencies |
|---|---|---|---|
| SDK | source `web/sdk/src/*.js` → bundle `web/dist/repos.js` | plain ES modules, dependency-free | dev: esbuild only |
| SPA | `web/src/**` (`.jsx`), `web/index.html` → `web/dist/` | SolidJS JSX, plain JS (no TypeScript) | runtime: `solid-js`, `@solidjs/router`; dev: `vite`, `vite-plugin-solid`, `@tailwindcss/vite`, `tailwindcss` |

The SPA entry is `web/src/index.jsx` (SolidJS router, `<div id="root">` in `web/index.html`); styling is
a single CSS-first Tailwind entry (`web/src/ui.css`: `@import "tailwindcss"`, class-based dark variant,
dark as the shipped default). The SDK is the only way the SPA talks to the server (dogfood rule, §10.2).

```html
<!-- web/index.html (excerpt): vite entry + dark-by-default shell. -->
<html lang="en" class="dark">
<script type="module" src="/src/index.jsx"></script>
<div id="root"></div>
```

Routing is `@solidjs/router` (`web/src/index.jsx`): the route inventory is
UNCHANGED from §2.3; pages are static route components (top-level `import`s, one vite bundle — no `lazy()` code-splitting wired up).

## 1. Artifact A — the SDK (`web/sdk/`)

The SDK is a **wire client**, not an app: plain ES modules with zero runtime imports (framework-free,
router-free, no state library).
It is **authored as submodules** under `web/sdk/src/` (user decision — never one huge file) and a dev-time
esbuild step bundles the entry module into the single distribution artifact `web/dist/repos.js`, served at
`/repos.js` (one stable URL, one request for external consumers). The SPA imports the same SDK source
modules (bundled into the app by vite); app and bundle share one source of truth. No IIFE/global build
and no `.mjs` twin — pre-1.0, no-compat: ES modules are the only distribution.

### 1.0 Source layout and the build (normative)

```
web/sdk/src/
├── index.js      entry: re-exports the public surface, creates the default client (window.repos only if unset)
├── core.js       ReposClient: options/base-URL resolution, lane selection (§1.2), fetch wrapper
│                 (credentials/redirect/manual rules), 401→popup single-flight retry
├── errors.js     ReposError (+ notFound/unauthorized getters)
├── sse.js        envelope + frame parser, readSse (also imported by the SPA's data layer — one parser)
├── auth.js       popup flow: openAuthPopup, postMessage wait, probe
├── repo.js       the repo client group: refs/tree/blob/raw/commits/commit/resolve/overview
├── admin.js      create/delete, tasks, ops, policy, settings groups
└── types.js      JSDoc @typedef blocks (§1.1) — editor IntelliSense only, no runtime
```

Build (SDK leg of `make web`; the SPA leg is `vite build` — devDependencies are `esbuild`, `vite`,
`vite-plugin-solid`, `@tailwindcss/vite`, `tailwindcss`; runtime budget is `solid-js` + `@solidjs/router`,
and the SDK itself stays dependency-free):

```sh
cd web && pnpm install --frozen-lockfile     # devDependencies: vite + esbuild
pnpm run build:sdk                            # esbuild sdk/src/index.js --bundle --format=esm \
                                              #   --target=es2022 --minify --outfile=dist/repos.js
```

Rules: `web/dist/` is build output (gitignored; created by `make web`, which runs `vite build` for the
SPA FIRST and then the SDK bundle — `vite.config.mjs` uses `emptyOutDir`, so the order is load-bearing);
`make build` depends on `make web`; tests import the SOURCE modules directly (§5) and need no build; CI runs
the bundle smoke test only when `dist/repos.js` exists. Content-hashed vite assets under `dist/assets/*`
are served immutable; `dist/index.html` and `/repos.js` stay `no-cache` + strong ETag so a redeploy is
picked up on the next fetch.

### 1.1 Public surface

```js
export class ReposClient { /* constructor(opts), configure(opts) */ }
export class ReposError extends Error { status; message; url; get notFound() {} get unauthorized() {} }
export default ReposClient;   // named exports: ReposClient, ReposError
```

`client.repo("owner/name")` → repo client. Every method accepts trailing `{signal?, onProgress?, headers?}`. Full member list — **copy of the Rust spec §10.1 table, wire-identical**:

| Member | Endpoint |
|---|---|
| `me()`, `signIn()`, `owners.list()`, `owners.repos(o)`, `configure(opts)` | `/api/v1/…` |
| `repo.urls` | `{html, clone, api, raw(rev,path), tree(rev,path), blob(rev,path), commit(sha)}` deep links |
| `repo.get/create/delete` | repo root (`PUT` create write / `DELETE` admin) |
| `repo.refs()` | O(1) head |
| `repo.branches(q)/tags(q)` | paged ref lists (`{q?, prefix?, after?, n?}`; JSON-only accept) |
| `repo.refStream(kind, q, onRef)` | SSE ref stream (`event: ref` … `event: done`) |
| `repo.resolve(rest)` | ref/path split |
| `repo.tree(rev, path)` / `repo.blob(rev, path)` / `repo.raw(rev, path)` | tree/blob/`?raw` |
| `repo.commits({ref, path, skip, n})` / `repo.commit(sha)` | history / detail |
| `repo.overview()` | WAL health |
| `repo.tasks()` / `repo.task(id, onEvent?)` | list / attach (JSON or SSE) |
| `repo.ops.list()` / `repo.ops.run(op, params, onEvent)` | op list / run (SSE events) |
| `repo.policy.{get, put, delete, validate, dryRun}` | policy surface |
| `repo.settings.{get, put(toml, message), delete, effective, history, describe, validate}` | settings surface |

Payloads are plain JSON objects — no type layer, no runtime schema. JSDoc `@typedef` blocks document each shape from spec §9.5 (`Commit`, `TreeEntry`, `RefPage`, `Overview`, `TaskRecord`, `OpSpec`, …) for editor IntelliSense without a type-check step. `ReposError` carries `{status, message, url}` with `notFound`/`unauthorized` getters.

### 1.2 Lane selection (normative, order fixed)

```text
explicit `token` option                    → bearer lane    (Authorization: Bearer, credentials: omit)
page origin == API base                    → same-origin    (session cookie, credentials: same-origin)
else                                       → browser lane   (/{o}/{r}/api-browser/…, credentials: include, redirect: manual)
```

On 401 **or an opaque redirect** (a `redirect: manual` fetch reports `type: "opaqueredirect"` / status 0) in the browser lane: open `<base>/api-browser/v1/authenticate` in a popup (single-flight — if a popup is already open, await the same promise), then `await` a `postMessage` of `{type: "repos:authenticated"}` **from our own origin**, then retry the request **exactly once**. A `304 Not Modified` response resolves the exported `NOT_MODIFIED` sentinel (never throws — the data layer treats it as silent keep-current). `_call` accepts a `cache` fetch-cache-mode option, and `ReposClient.withNoStore(fn)` issues every SDK call initiated synchronously inside `fn` with `cache: "no-store"` + `no-cache` headers (the data layer's mutation-refetch bypass, #41).

```js
// single-flight popup promise — one per client instance
let popupAuth = null;
function authenticateOnce(base) {
  return (popupAuth ??= new Promise((res, rej) => {
    const w = open(`${base}/api-browser/v1/authenticate`, "repos-auth", "width=520,height=640");
    const onMsg = (ev) => {
      if (ev.origin !== location.origin) return;               // our origin only
      if (ev.data?.type !== "repos:authenticated") return;
      cleanup(); w?.close(); res();
    };
    const timer = setTimeout(() => { cleanup(); rej(new ReposError(504, "popup auth timed out")); }, 120_000);
    const cleanup = () => { removeEventListener("message", onMsg); clearTimeout(timer); };
    addEventListener("message", onMsg);
  }));
}
```

Hazard: a second 401 while a popup is open MUST reuse the in-flight promise (single-flight), never open a second window; a rejected popupAuth clears `popupAuth` so a later call can retry. Use plain `fetch` (NOT `EventSource`) everywhere — the server streams only when `Accept` carries `text/event-stream`, and only fetch lets us set that header plus auth.

### 1.3 Base URL resolution

`configure({base})` option → `<script data-base>` attribute → `import.meta.url` origin (the SDK is a module, so this always works) → page origin. Off-DOM default `http://127.0.0.1:8080` for tests only. Resolution happens at construction and is re-evaluated by `configure`.

### 1.4 Envelope handling

Every GET sends `Accept: application/json, text/event-stream`. If the response `Content-Type` is JSON → parse and resolve. If `text/event-stream`:

1. Read via `response.body.getReader()`; split frames on `\n\n` (CR tolerated), buffer the remainder.
2. Parse each frame's `event:` and `data:` lines; JSON-decode `data`.
3. `notice` / `progress` / `task` → surface through the call's `onProgress` AND the client-global handler set in `configure`.
4. `result` → resolve with its payload (terminal). `error` → throw `ReposError(status, message)`. End-of-stream with no `result` → `ReposError(502, "stream ended without a result")`.
5. Honor `signal` at every read; abort → throw `ReposError(499, "aborted")` (client-side convention, never sent by the server).

### 1.5 Ref stream (`refStream`)

`repo.refStream(kind, q, onRef)` consumes the ref-list SSE dialect (spec §9.3: `event: ref` / `{"name","sha"}` per match, terminal `event: done` / `{"more":bool}`) with **plain fetch** (`credentials` per lane; this endpoint is GET-with-query, so no special headers beyond lane auth). `onRef({name, sha})` is invoked as frames arrive — the UI paints incrementally.

### 1.6 Concurrency (SDK)

Hazard: leaked readers and unbounded parallel streams if callers forget to abort.
Avoidance: every method derives its own `AbortController` from the caller's `signal` and closes the `ReadableStream` reader in a `finally`; `refStream` and task-attach loops return a cancellation function in addition to honoring the signal; at most one popup auth promise per client. Playbook: `13_concurrency.md` (ownership: the caller owns the signal, the SDK owns the reader, the SDK closes it).

## 2. Artifact B — the SolidJS SPA (`web/src/**`)

The §10.2 SPA surface implemented as a **SolidJS SPA** (D-WEB-6 — replaces the earlier vanilla-ESM
rewrite, which had itself replaced SolidJS; the route inventory and page contracts below are unchanged).
Reactivity is Solid's own signals/stores + context; routing is `@solidjs/router`; styling is Tailwind v4
CSS-first (`web/src/ui.css`), dark mode by default; pages are `.jsx` components built by vite.

### 2.1 Reactivity — Solid signals/stores + context (normative API)

Solid's primitives ARE the app's reactivity (D-WEB-6 — supersedes the earlier ~40-line hand-rolled
reactive core, which is deleted with the vanilla sources). The contract the app relies on:

```js
import { createSignal, createEffect, createMemo, onCleanup, createRoot } from "solid-js";
import { createStore } from "solid-js/store";
```

**Reactivity contract:**

- Signals update through the reactive graph: `set` marks dependents dirty and Solid flushes effects in topological order afterward — synchronous `set`s batch, so an effect reading two signals set together runs once, not twice.
- Dependencies are tracked at read time: any signal read while an effect/memo is running becomes a dependency. No explicit dependency lists.
- `onCleanup` inside a component registers teardown on unmount; long-lived non-component scopes use `createRoot` with explicit `dispose()` — the lifecycle rule of §2.5.
- Memos are cached derivations: recomputed when a dependency changes, read cheaply otherwise.
- UI updates are Solid components + JSX: effects write to DOM nodes (`el.textContent = …`, `el.classList.toggle(…)`) or re-render a list; no VDOM diffing beyond Solid's fine-grained updates.

**Components:** pages are Solid components (`web/src/pages/*.jsx`) mounted by the router; shared UI is the
router root (`web/src/App.jsx`: header, nav, theme toggle). Data fetching composes over the §2.4 cache via Solid primitives.

### 2.2 npm dependency budget (DECIDED — `solid-js` + `@solidjs/router` runtime, D-WEB-6)

Runtime `package.json` dependencies are exactly **`solid-js`** + **`@solidjs/router`**; state management is
Solid's own signals/stores + context (no additional state library). DevDependencies are `vite`,
`vite-plugin-solid`, `@tailwindcss/vite`, `tailwindcss` (the SPA build) plus `esbuild` (the §1.0 SDK
bundle). Everything previous packages provided that is NOT back in the budget stays hand-rolled or native:

- **Markdown: hand-rolled markdown-lite (~150 lines).** Covers the preview surface: headings, paragraphs, fenced code blocks, inline code, bold/italic, links + autolinks, GFM tables, blockquotes, lists (nested one level), `hr`, images. No plugins, no AST — a line-based emitter feeding the sanitizer below. Preview fidelity is preview-level (prior decision preserved); the code view shows exact text.
- **Diff: hand-rolled minimal unified-diff parser in JS (~120 lines).** Unchanged from the previous decision: the server sends a single well-formed `git diff` patch (spec §9.5), so a tiny parser with an explicit grammar (§2.8) is smaller than any library.
- **Syntax highlighting: none at runtime.** Code blobs render as `<pre><code>` with line numbers via a cheap hand-rolled tokenizer for the common cases (keywords/strings/comments/numbers) driven by a filename-extension → language table; unknown extensions render plain.
- **Sanitization:** markdown preview renders via a ~40-line allowlist sanitizer (tags `p, h1–h6, ul, ol, li, a, code, pre, em, strong, blockquote, table, thead, tbody, tr, th, td, hr, br, img, span`; attributes `href/src/alt/title` only; `href`/`src` schemes restricted to `http, https, mailto, /, #`). Output set via `innerHTML` of the sanitized string. (`innerHTML` of untrusted strings is prohibited everywhere else.)
- **CSS:** Tailwind CSS v4, CSS-first (`@import "tailwindcss"` in `web/src/ui.css`, `@tailwindcss/vite`
  plugin, no tailwind.config, no CDN); dark mode is the default theme (class on `<html>`, persisted).

### 2.3 Routes (inventory identical to Rust spec §10.2)

| Route | Page |
|---|---|
| `/` | owners (list of owners; each → their repos) |
| `/api` | API docs page |
| `/:owner` | repos of an owner |
| `/:owner/:repo` | repo shell — tabs Code, Commits, Issues, Pulls, Checks, Releases, Settings + tasks overlay |
| `/:owner/:repo/tree/*` | tree at ref/path |
| `/:owner/:repo/blob/*` | blob at ref/path |
| `/:owner/:repo/commits(/…)` | commit history |
| `/:owner/:repo/commit/:sha` | commit detail |
| `/:owner/:repo/wal` | WAL/overview page (kept route; menu entry lives in the settings sidebar) |
| `/:owner/:repo/settings` | settings (left-sidebar sections, issue #123) |
| `/setup` | Setup UI (§2.10) |

All deep-linkable; every UI route returns the SPA `index.html` from the server. Pages are static route
components (`component={Commit}` with a top-level `import` in `web/src/index.jsx`), bundled into the app
by vite — no code-splitting wired up.

### 2.3.1 Owners page (`/`, issue #117)

`web/src/pages/Owners.jsx`: an intro card, then one section per owner with that owner's repos.

- **Intro card** (`.card`, one short paragraph): what walhub is (git host whose only database is an
  object store), what it does (push over smart HTTP; browse/manage repos here), how (refs, packs,
  config, policy as bucket objects; disposable instances).
- **Data** (dogfood rule intact): `useData("owners", …owners.list())`, then one `OwnerSection` per
  shown owner with `useData(`repos:${owner}`, …owners.repos(owner))` — the same cache key the
  `/:owner` page uses, so the two pages share fetches. No new endpoint, no new SDK method (the core
  listing endpoints in 07 §8 already list everything).
- **Order**: owners newest-first, repos newest-first. Ordering key: reverse of server order — the
  listing path exposes no creation timestamps (endpoints return store-sorted ascending name
  lists of plain strings; per-repo `first_state_at`/first-entry `created_at` proxies exist
  deeper in the bucket but cost a manifest/log GET per repo, so true reverse-chronological
  order wants a server-side shape), so reverse-lexicographic is the closest deterministic
  newest-first proxy. The single function to replace when the backend carries creation times
  is `newestFirst` in `web/src/lib/owners.js`.
- **Caps**: `MAX_OWNERS` 50 owners, `MAX_REPOS_PER_OWNER` 10 repos per section (constants in
  `web/src/lib/owners.js`, headless-tested). Overflow folds behind links, never a spinner: per-owner
  `+N more →` to `/:owner` (uncapped there), and a "showing newest 50 of N owners" line. The page
  costs 1 + shown-owners SWR GETs; per-owner sections render independently (one slow owner never
  blocks the rest).
- **Styling** (issues #137, #142): the intro keeps its `.card`, but owner sections are flat —
  no cards-in-cards. Sections sit in a `divide-y divide-zinc-200 dark:divide-zinc-800`
  stack (dividers, not nested containers); the owner heading is a clear section header at
  `text-base font-bold tracking-tight` (a full step above the `text-sm` rows — #142 promotes
  the quieter #137 `text-sm`, so the header reads as a header by scale and weight, not just
  link color). Shared `.muted`/emerald-link classes — correct in dark (default) and light
  with no page-specific colors. Links stay plain anchors (keyboard flow unchanged).
- **Star counts** (issue #137): every repo row on `/` and on `/:owner` carries a `<StarCount>`
  (`web/src/components/StarCount.jsx`) rendering `(3 ⭐)`-style beside the link via
  `GET …/api/social` under the shared `social:{o}/{r}` 30 s `useData` key (single-flighted,
  LRU-capped per §2.4) — the two pages share fetches, repeat visits within the TTL cost
  zero GETs while entries survive under the 400-entry LRU cap (a fully-maxed 500-row
  cold load evicts the oldest entries, so the absolute worst case refetches some), and the link renders first with a muted `(…)` placeholder, so counts never
  block page render. Worst case stays bounded by the #117 caps (50 × 10 = 500 GETs cold,
  each independent). Formatting/TTL constants live in headless-testable `web/src/lib/stars.js`
  (`web/test/unit/stars.test.js`). Scope note: the org page has no repo listing, starred
  lists are SDK-only (no listing UI yet), and the repo chrome already shows its count via
  the star toggle — so `/` + `/:owner` is everywhere a repo list renders.
- **Two-column rows + last-active stamps** (issue #142): repo rows on `/` and `/:owner` share
  one `<RepoRow>` (`Repos.jsx`: link + `<StarCount>` + `<ActivityStamp>`) in a responsive
  `grid grid-cols-1 sm:grid-cols-2` (one column on narrow widths) — newest-first order is
  preserved (grid fills row-wise in source order) and the #117 caps/overflow links are
  unchanged. Each row also carries `active <DateTime>` via `<ActivityStamp>`
  (`web/src/components/ActivityStamp.jsx`, the #133 shared date component — relative text,
  local-tz hover title). Source decision: the stamp is the latest COMMIT date from
  `GET …/commits?n=1` (ref defaults to HEAD server-side, 07 §9.6 — one GET per repo, no
  summary fetch first). The summary (`GET …/api`, §9.1) carries no date field, so it cannot
  source a stamp without a new backend field; the overview's `manifest.last_push` is
  push-time (not commit-time) and the overview is a no-store heavyweight
  (health/bundles/plan recomputed per call) — more cost per row for a less precise signal.
  Honest-proxy note: a push that adds no commits (branch delete, tag-only push) does not
  move the stamp; "last commit" is the documented meaning, hence the `active` label, and
  empty repos render "no commits yet" (never a fake epoch — the unborn-HEAD 404 maps to
  `{commits: []}` in the fetch, so it stays out of the error tray). Cost mirrors star counts
  (shared `activity:{o}/{r}` 30 s `useData` key, placeholder-first, tray-on-error, bound by
  the #117 caps); extraction/date-choice constants live in headless-testable
  `web/src/lib/activity.js` (`web/test/unit/activity.test.js`). No new endpoint, no new SDK
  method (`repo.commits()`, 07 §9.6); no new deps.

### 2.4 Data layer (hand-rolled)

Implement a module `web/src/lib/data.js` providing:

- **`useData(key, fn, ttl?)`** — a promise-cache: `Map` keyed by a string key, entry `{promise, value, at}`; TTL revalidation with default **5 s**; **sha-addressed payloads cached forever** (`ttl = Infinity`); LRU cap **400** entries; background refresh keeps stale data on screen; errors go to the **global error tray** (max 6, deduped by key+message) rather than throwing into the page. The cache maps are module-level singletons; the value is exposed as a signal so components `createEffect` over it. Every fetch is an **ordering generation** (`entry.seq`): a body or error commits only while its generation is still newest, so a stale disk-cache hit or reordered response resolving after a newer fetch started is dropped and can never overwrite fresh state (#41). **`invalidate(key)` always starts a new generation** — even over an in-flight fetch — and runs the refetch under the SDK's HTTP-cache bypass (`ReposClient.withNoStore`: `cache: "no-store"` + `no-cache` headers), so mutation-triggered refetches always read a post-mutation body; background reads stay ordinary cached reads and still single-flight. An SDK `NOT_MODIFIED` (HTTP 304) resolution is silent keep-current: value untouched, freshness timestamp bumped, no tray entry. Headless tests drive the same path via `prefetchData(key, fn)` (the `ensureEntry → start` path minus reactivity — `useData`'s effect cannot run under the solid-js server build).
- **`usePending()`** — a global counter of in-flight fetches AND pending dynamic imports; drives the **top progress bar** (a fixed-position bar animating width; 0 → hidden). A module-singleton signal + an effect writing `style.width`.
- **`useResolved(rev)`** — the two-step resolve→sha-addressed pattern of the spec §9.2: step 1 `repo.resolve(rest)` (ref-dependent, SWR, 5 s TTL, revalidates); step 2 fetches `tree/{sha}/…`, `blob/{sha}/…`, `commits?ref={sha}`, `commit/{sha}` with `ttl = Infinity` (immutable). Implemented as two chained caches keyed `resolve:{rest}` → then `sha:{sha}:…` — the chain IS the idiom; a tiny `useResolved(rest, kind)` helper composes it so pages don't repeat it.
- **Error tray:** fixed bottom-right stack, max 6 entries, deduped, dismiss button, auto-fade after 10 s.
- **Progress:** counts every `useData` fetch start/finish and every dynamic import.

### 2.5 SSE in the SPA (hand-rolled; never `EventSource`)

`EventSource` cannot set `Accept`/auth headers, so ALL streaming goes through the SDK's fetch-based readers. Stream lifecycle is owned by the page, not a framework — a shared helper `mountStream(open, onFrame)` in `lib/sse.js` (used by every streaming component): holds one `AbortController` in a closure slot; `run()` aborts any previous controller, swaps in a fresh one, sets the state signal (`"open"|"closed"|"error"`), and calls `open(signal)`; it returns a cancel function that aborts and sets `"closed"` (unmount → stream gone). Errors set `"error"`, never throw into the page.

### 2.6 Repo chrome

- **Tabs** computed from the pathname (Code / Commits / WAL / Settings), matching §2.3; the active tab is a memo over a `location` signal updated by the router's `popstate`/`navigate` hooks.
- **Repo context** (owner/name/refs) fetched **once per visit** via `useData(`repo:${owner}/${name}`, …, 5s)` and stored in a module-level signal consumed by the repo pages.
- **Clone menu** renders the setup recipes from `GET /services/setup.json` (spec §8.9) — never its own copy. The dogfood rule has the same two exceptions the Rust spec names: this fetch and nothing else bypass the SDK (both fetch server-rendered recipe payloads; use plain `fetch` with same-origin credentials).
- **Branch/tag picker** streams refs over SSE via `repo.refStream`: 50 per page, **150 ms debounce** on the query input, **aborts the in-flight stream on every keystroke** (controller swapped per keystroke via the §2.5 pattern; previous stream aborted BEFORE the new one opens), paints rows as they arrive (`onRef` → append to a signal array).

### 2.7 Blob rendering

Server returns raw text (spec §9.5). Decision tree:

```text
too_large (2 MiB cap hit)  → explanatory placeholder
binary (NUL / invalid UTF-8) → explanatory placeholder ("binary file, {human size}"; exact bytes in title)
name ends .md/.markdown    → MarkdownBlob   (Preview | Code toggle; markdown-lite + sanitizer in preview,
                                              raw text in code view)
otherwise                  → CodeBlob       (line-numbered <pre>, tinted by the mini tokenizer)
```

### 2.8 Commit rendering — hand-rolled unified-diff parser

`parsePatchFiles(patch, sha)` runs client-side on the `patch` field of `commit/{sha}` (spec §9.5: unified diff vs first parent, `--no-color`, rename detection). **Grammar it MUST accept** (each state terminal unless noted):

```text
patch      := header+ body* EOF
header     := "diff --git a/<path> b/<path>" LF
            ( "old mode" | "new mode" | "deleted file mode" | "new file mode" | "copy from" | "copy to"
            | "rename from" | "rename to" | "similarity index" | "dissimilarity index" | "index " )*
            ( "--- a/<path>" | "--- /dev/null" ) LF
            ( "+++ b/<path>" | "+++ /dev/null" ) LF
            ( "@@ -<o>[,<n>] +<p>[,<q>] @@ <ctx>?" LF body )+
body       := ( " " line | "-" line | "+" line | LF )*
```

Parse into `{path, oldPath?, added?, deleted?, isBinary?, hunks: [{oldStart, oldLines, newStart, newLines, lines: [{t: " "|"-"|"+", text}]}]}`. File boundaries: the next `diff --git` line; binary files appear as `Binary files … differ` / `GIT binary patch` — mark `isBinary` and skip. Renames come from `rename from/to` (use the NEW path as the display key, matching the server's `stats[]` convention). Diffstat totals = Σ additions/deletions from `stats[]` (authoritative) — the parser's counts are for per-file badges only.

Rendering: per-file unified/split toggle (unified default; split = two columns built by pairing `-`/`+` runs via a 20-line LCS window — hand-rolled, ~60 lines); file anchors by `stats[].path`; commit bodies linkified (sha → `commit/:sha` links, bare URLs → anchors); trailers grouped (People / merge-queue keys / Other) with sha → commit links and `mailto:` rendering.

### 2.9 WAL page, tasks overlay, settings page

Content requirements are identical to the Rust spec §10.2 (this doc does not restate them; implement exactly there):

- **WAL page** (`/:owner/:repo/wal`): health box (issues, deep fsck result, suggestions — each dispatches its op as a task), ops box (single-flight buttons, boolean/strategy params, live log via SSE, grouped op history), manifest + local-copy boxes, packs/checkpoints boxes, bundle chain tree (roots = fulls; children under `base_id`; sorted by `creation_token`; warning when an incremental's base vanished), bundle slot plan per strategy (built/skipped/too-small/unavailable counts + actionable rows), compactions table, WAL segments table (newest 5 + "all").
- **Tasks overlay:** polls `…/tasks` (1.5 s busy / 15 s idle), instance-aware finishing rule, 20 s linger, progress pill + dropdown. Polling via a signal + recursive `setTimeout` chain (NOT `setInterval`, so a slow poll never stacks).
- **Settings page** (issue #123): left sidebar + content column — a `nav[aria-label="Settings sections"]` with the standard listing (Scheduled tasks: strategy table + placement/host facts + upstream follow status; Push policy: textarea editor, 400 ms debounced validate, dry-run against last N pushes, save/discard/copy; Effective config & history: TOML editor with debounced validate, publish with a message, clear, per-revision history with "Revert to this" + line diff; Access; CI tokens; Webhooks; WAL rendered inline) plus the Danger Zone in its own danger-styled section. WAL moved here from the repo tab bar (the `/wal` route is kept, highlights Settings now); entries are `#id` deep-linkable with stale-hash fallback, native buttons with `aria-current="page"`, `.side-nav-*` classes in `ui.css` (dark + light); below `lg` the sidebar stacks above the content and each section row scrolls internally. Every tab's form is reworked for the narrower column (grid/flex-wrap rows, no fixed-width inputs that overflow, every data table in an `overflow-x-auto` wrapper). Nav model + hash helpers are the headless-testable `web/src/lib/settingsNav.js`.

### 2.10 Setup UI (`/setup`, D6)

First-class page backed by the Setup API (specified in `05_config.md`): `GET /api/v1/setup` (full schema + effective values + file state), `POST /api/v1/setup/test` (validate without saving), `PUT /api/v1/setup` (validate + write), and `POST /api/v1/setup/auth/test` (OIDC discovery probe).

- **Rendering:** one section card per schema section (`server`, `auth`, `store`, …) listing every key with its type, default, and the **current effective value** (file value if present, else default, flagged as such). Booleans render switches, enums `<select>`, everything else text/number inputs; TOML struct lists (bundles.strategy, server.auth.tokens) render textareas over a `[[name]]` fragment. **Auth is its own card directly after `server`** (keys keep their `server.auth.*` names), and its rows are **mode-conditional**: `lib/setup.js FIELDS[].modes` names the `server.auth.mode` values a row applies to (`token`+`oidc` for `anonymous_read`, `tokens`, `trusted_forwarders`; `oidc` for everything else), and rows outside the effective mode are not rendered.
- **Client-side validation mirrors the server rules exactly** (same field checks as the server's validator: ranges, enum membership, URL/listen formats, cross-field rules like auth mode vs listen address). Validation runs per-keystroke (debounced 250 ms) for inline hints and on submit as the gate.
- **Buttons:** **Validate** → `POST /api/v1/setup/test` with the full normalized payload (unknown keys are a validation error, not a silent drop); **Save** → run validate, then `PUT /api/v1/setup`. On success the server writes `<data-dir>/walhub.toml` atomically (tmp + rename) and returns `{written: true, restart_required: [keys]}`; the UI shows which changed keys need a restart (hint list: keys under `server.*` and `store.backend`/`store.root` always require restart; others only when the server caches them — the response is authoritative, the UI just renders it).
- **OIDC helpers in the auth card:** the card states the **redirect URI to register at the issuer** — `<public_url or page origin>/_auth/callback`, rendered copyable — and a **Test OIDC setup** button POSTs `{issuer, client_id, redirect_uri}` from the FORM (not the saved config) to `/api/v1/setup/auth/test`, which fetches `<issuer>/.well-known/openid-configuration` (10 s timeout, 1 MiB cap) and fails 422 with the reason: unreachable, not a discovery document, issuer mismatch (an OIDC document must name its own issuer), or a missing `authorization_endpoint`/`token_endpoint`/`jwks_uri`.
- **Setup-only mode:** when `GET /api/v1/setup` reports the config is invalid, the page renders the exact validation errors at the top (section, key, message, offending value), disables nothing (the operator must be able to fix and re-validate), and shows a banner that the server is serving only `/setup`, `/healthz`, `/readyz` (everything else 503) until a fixed config is saved AND the server restarted.
- **Access state from the API response** (`open | admin_required | token_required`): open — page renders fully; admin/token required — page renders a 403 explanation with the retry instruction (reload after authenticating; `WALHUB_SETUP_TOKEN` hosts take the token as a query param on the PUT/POST, never stored).
- **Dogfood exception:** like the recipe fetches, the Setup page may use plain `fetch` for the three `/api/v1/setup*` endpoints because the SDK surface does not include them (§6 dogfood rule).

## 3. Serving from the Go binary (embedding contract, byte-compatible)

The SPA is the **vite-built `web/dist/`**, embedded into the binary. The SDK bundle is embedded alongside:

```go
//go:embed all:dist
var web embed.FS            // dist/index.html, dist/assets/*, dist/repos.js — dist/ is build output,
//                           created by `make web` (vite SPA first, then the esbuild SDK bundle)
```

| Path | Cache behavior |
|---|---|
| `/_ui/assets/*` (content-hashed vite output, immutable) | `Cache-Control: public, max-age=31536000, immutable` |
| `/_ui/index.html` and every UI route (the SPA shell) | `Cache-Control: no-cache` + ETag |
| `/repos.js` | `Cache-Control: no-cache` + ETag |

Compression: `gzip` via server middleware for text assets (`text/*`, `application/javascript`, `application/json`, `text/css`) when `Accept-Encoding` contains `gzip` and the body is ≥ 1 KiB — on-the-fly `gzip.Writer` with level 6, `Vary: Accept-Encoding` set, no brotli, no precompressed sibling files. Serving details, route registration under `internal/server`, and the `X-Walgit-Capabilities` edge contract are specified in `06_server_http.md`; the API endpoints themselves in `07_api.md`.

## 4. Repository layout

```
web/
├── index.html          ← SPA shell source: #root, vite entry (built to dist/index.html)
├── vite.config.mjs     ← SPA build: solid + tailwindcss plugins, base "/_ui/", outDir dist
├── sdk/src/            ← artifact A source, SUBMODULES (§1.0): index/core/errors/sse/auth/repo/admin/types
├── dist/               ← build output, gitignored: index.html, assets/* (hashed), repos.js
├── package.json        ← runtime: solid-js, @solidjs/router; dev: vite, vite-plugin-solid, @tailwindcss/vite, tailwindcss, esbuild. scripts: build:ui, build:sdk, build
├── src/                ← artifact B: index.jsx router, pages/*.jsx, lib/*.js, ui.css (Tailwind entry)
└── test/               ← node --test suites (§5): unit/*.test.js (incl. smoke), helpers/fetch.js
```

The SPA is vite-built from `web/src/**` into `web/dist/`; the SDK is esbuild-bundled from
`sdk/src/index.js` to `dist/repos.js` (§1.0); `make web` runs both (`pnpm run build`),
`make build` depends on it (`16_packaging.md` owns the wiring).

## 5. Testing the JS (`node --test`, zero npm test deps)

Runner: Node's built-in test runner — `node --test web/test/unit/*.test.js`. No jest, no vitest, no test-framework dependencies. Logic and DOM are **separated by rule**: every module in `web/src/lib/` except `data.js`'s DOM-adjacent bits must be importable in Node with no `document`/`window` access; DOM wiring lives in `web/src/pages/` and `index.jsx`.

**Pure (headless-testable) modules:** `sdk/src/*.js` (api client groups, injectable `fetch` + stream shim, envelope parser), `lib/sse.js` (frame parser, one parser shared with `sdk/src/sse.js`), `lib/diff.js`, `lib/markdown.js`, `lib/sanitize.js`, `lib/setup.js` (setup form validation rules, mirroring the server), `lib/highlight.js`.
**DOM-wiring modules (not unit-tested):** `pages/*.jsx`, `index.jsx`, the router glue, anything touching `document`/`location` — exercised by smoke tests instead.

```js
// web/test/unit/sdk-envelope.test.js (skeleton)
import { test } from "node:test";
import assert from "node:assert/strict";
import { ReposClient } from "../../sdk/src/index.js";

test("SSE envelope → result payload", async () => {
  const fakeFetch = async () => new Response(
    `event: progress\ndata: {"n":1}\n\nevent: result\ndata: {"entries":[]}\n\n`,
    { headers: { "content-type": "text/event-stream" } });
  const c = new ReposClient({ base: "http://x", fetch: fakeFetch });
  assert.deepEqual(await c.repo("a/b").tree("main", ""), { entries: [] });
});
```

```js
// web/test/unit/smoke.test.js (skeleton) — real server binary, real fetch.
// CI serves a build for this file (WALHUB_TEST_WEB_BASE_URL); locally the file
// skips when no server is up, so `make test` stays green on a cold machine.
import { test } from "node:test";
import assert from "node:assert/strict";
const base = process.env.WALHUB_TEST_WEB_BASE_URL ?? "http://127.0.0.1:8080";

test("pages and SDK serve", async () => {
  const ui = await fetch(`${base}/`);
  assert.equal(ui.status, 200);
  assert.match(await ui.text(), /id="root"/);
  const sdk = await fetch(`${base}/repos.js`);
  assert.equal(sdk.status, 200);
  const mod = await import(sdk.url);                   // the served bytes parse as ESM
  assert.equal(typeof mod.ReposClient, "function");
});
```

Required alongside it: `web/test/unit/setup-api.test.js` (same shape): on a fresh data-dir (D5 defaults boot), `GET /api/v1/setup` → `{access: "open"}`, `POST /api/v1/setup/test` with `{server: {listen: "0.0.0.0:9999"}}` → 200, `PUT /api/v1/setup` → 200 with `restart_required` containing `"server.listen"`. Also required: `web/test/unit/setup-form.test.js` — table-driven cases for the `lib/setup.js` validators mirroring the server (valid values pass; range/enum/format/cross-field violations fail with the server's message).

Make wiring (D3 — all dev/CI targets are Make targets): `test-web:` runs `node --test web/test/unit/*.test.js`.

## 5b. End-to-end: what an operator does

```bash
make build   # go build; web/dist/ is vite-built first (SPA) + esbuild-bundled (SDK) via `make web`
make dev     # ./walhub serve with defaults (D5) — UI at http://127.0.0.1:8080/, SDK at /repos.js
```

A third-party page embeds the SDK as a module — the only supported integration (no IIFE/global build exists): `<script type="module">import repos from "https://walhub.example.com/repos.js"; repos.configure({token:"…"}).repo("demo/hello").tree("main","").then(console.log)</script>`.

## 6. Dogfood rule

The UI uses the SDK for every server call, without exception except the ones named in §2.6 and §2.10 (both recipe fetches and the three Setup API calls; all fetch server-rendered JSON payloads). No raw `fetch` in `web/src/**` outside the SDK directory and those files. CI grep-checkable: `fetch(` must appear in `web/src/**` only inside those files.

### Concurrency

Hazard: SSE readers leak when components unmount mid-stream; ref-picker keystrokes stack streams (one per keystroke) and repaint out of order.
Avoidance (playbook: `13_concurrency.md` — ownership and cancellation rules): every stream is owned by exactly one component-scoped `AbortController`; unmount tears it down via the §2.5 pattern's returned cancel function, and the SDK closes the `ReadableStream` reader in its `finally` (the SDK owns the reader, the component owns the signal — one closer each, no shared mutable reader state). The ref picker swaps its controller per debounced keystroke and aborts the previous one BEFORE opening the new stream, so only one ref stream per picker is ever live; rows are keyed by `name` so late frames from an aborted stream (already cancelled) can never overwrite fresh ones. No unbounded buffering: SSE frames are processed as they are read, appended to bounded signal arrays (cap 1000 rows per picker page); progress callbacks fan out synchronously, never queued.

## Decisions & deviations from the Rust design

- **SolidJS + Tailwind v4 SPA (2026-09-02, explicit user request — supersedes the two vanilla decisions above; DEVIATIONS.md D-WEB-6).** The user reversed the vanilla-ESM direction: runtime deps exactly `solid-js` + `@solidjs/router`; state via Solid signals/stores/context; Tailwind v4 CSS-first with `@custom-variant dark` and dark as the default theme (class on `<html>`, persisted, no CDN — system font stack); vite + vite-plugin-solid + @tailwindcss/vite build `web/src/**` (JSX, no TypeScript) into `web/dist/` (hashed assets immutable, `index.html` no-cache + ETag — reverts D-WEB-3's caching class for built assets). The SDK remains dependency-free and esbuild-bundled; `/repos.js`, `/repos.mjs`-absence, and the dogfood rule are untouched. Pure modules (diff, markdown-lite, sanitizer, highlighter, setup validation, SSE frame parsing) carried over unchanged.
- **SPA framework: vanilla standard ECMAScript replaces SolidJS** (D2, explicit user decision — itself a supersession of the earlier SolidJS-over-React decision; wire contract untouched — the SDK defines the wire, the framework does not). Native ES modules + import map, `<template>` + a ~40-line hand-rolled reactive core (§2.1), hand-rolled router. No state library, no JSX, no VDOM. **SUPERSEDED 2026-09-02 by explicit user request (DEVIATIONS.md D-WEB-6, first bullet above) — historical only; the shipped stack is SolidJS + Tailwind, vite-built.**
- **Superseded — dependency set `solid-js`, `@solidjs/router`, `marked` (+ dev: `vite`, `vite-plugin-solid`, `typescript`)**: the npm budget became **zero runtime dependencies; exactly one devDependency (`esbuild`, §1.0)** — and was then superseded AGAIN by D-WEB-6 (first bullet above): runtime is `solid-js` + `@solidjs/router` again, dev tooling is `vite` + `vite-plugin-solid` + `@tailwindcss/vite` + `tailwindcss` + `esbuild`, still no TypeScript. `marked` stays replaced by hand-rolled markdown-lite with unchanged preview-fidelity stance and the same allowlist sanitizer.
- **Superseded (again) — "there is no build"**: a build step returned, scoped tightly (user decision): the SDK is authored as submodules and bundled by **esbuild** into `web/dist/repos.js` — and, under D-WEB-6, the SPA is built by **vite** into `web/dist/` (first bullet above). Nothing is served unbuilt anymore.
- **Superseded — `tsc --noEmit` as required gate; vite/pnpm script chain**: no TypeScript anywhere — that half stands. The toolchain half is superseded by D-WEB-6: the UI-path tooling is `pnpm run build` (vite SPA + esbuild SDK) via `make web`. Gates are `make test-web` (§5) and CI greps (dogfood rule, no-TS rule).
- **Superseded — vite chunk-hash immutable `/assets` scheme with br+gz precompressed siblings**: under the vanilla reading, modules were served raw with `no-cache` + strong ETag and compression was on-the-fly gzip middleware (§3) — REVERTED by D-WEB-6: content-hashed `dist/assets/*` are immutable again (first bullet above); on-the-fly gzip middleware (§3) is preserved. The behavioral contract (fresh content on deploy, cheap revalidation, compressed text assets) is preserved throughout.
- **Hand-rolled unified-diff parser instead of a diff package** — unchanged: the server emits one well-formed `git diff` shape; a ~120-line parser with the §2.8 grammar beats any library on size and control.
- **No shiki: hand-rolled mini tokenizer for code blobs** — unchanged: tinted code is an acceptable trade.
- **Markdown sanitizer hand-rolled (~40 lines allowlist)** — unchanged: render surface is self-produced markdown, attack surface is small and enumerated.
- **GFM/markdown extras beyond the hand-rolled markdown-lite are not implemented** — preview fidelity is preview-level; code view remains available for exact text.
- **NEW — Setup UI is a first-class page** (`/setup`, §2.10) backed by the D6 Setup API, with setup-only-mode error display and restart-required hints.
- **NEW — JS testing is normative** (§5): `node --test`, zero npm test deps, strict logic/DOM separation, unit + server-smoke suites wired as `make test-web`.
- **NEW — human-readable sizes in the Code tab (issue #27):** Tree entry sizes and all Blob size spots (header, too-large and binary placeholders) render via the shared headless-testable `fmtSize` helper (`web/src/lib/format.js`: `B/k/MB/GB…`, exact byte count kept in a `title` tooltip); the blob code `<pre>` (and the MD code-fallback `<pre>`) is `flex-1 min-w-0` so the pane fills the card.
- **AMENDED (issue #29) — tree size style, rwx modes, header meta:** `fmtSize` byte suffix drops the space and lowercases the suffix (`0b`/`92b`/`1023b`; `k`/`MB`/`GB` unchanged) so the last character is always a letter; Tree entry modes (blobs AND folders, whenever the API provides one) render as rwx triplets via the headless-testable `fmtMode` helper (100644→`rw-r--r--`, 100755→`rwxr-xr-x`, 120000→`rwxrwxrwx`, 040000/40000→`rwxr-xr-x`, 160000 gitlink→`m---------`, raw octal kept in the `title` tooltip, blank when the API omits the mode); the size column is right-aligned (`text-right` on th + td, `tabular-nums` kept); the tree meta line (`{sha12} · {n} entries`) is removed — the repo-header `{branch} @ {sha}` pill stays the single branch@sha source and the breadcrumb already carries ref+path, so no crumb-adjacent sha suffix is kept.
- **AMENDED (issue #34) — shared empty-state callouts + dynamic open-PR page:** the pulls/checks/releases/issues lists share one centered icon/title/hint/action callout (`web/src/components/Empty.jsx`, `.empty-*` classes — replaces the bare `No …` fallback lines; actions are router links so they stay keyboard-focusable); the cramped "Open a pull request" sidebar box on the pulls list is deleted — opening a PR happens only on the full `/pulls/new` page, which the list's "New pull request" button links to (carrying `?base=`/`?head=` filters as composer prefill). The composer preview is deliberately client-side: 300 ms after the last picker keystroke it fetches both histories (`commits?ref=`, n = 100, parallel, one AbortController per debounced run with stale-run drop) and intersects them with the headless-testable `web/src/lib/compare.js` helpers (merge-base approximated by history intersection, exact inside the window; ahead/behind as lower bounds with `+` only when the meeting point falls past a `more` window; head-only commit list, title from the head-tip subject and body from the head-only subjects until the user edits, swap-base/head button). No server compare endpoint was added — merge-base stays a backend concern for the merge box, not the composer.
- **AMENDED (issue #35) — Latest release panel redesign:** the bare "Latest / No published releases." sidebar box becomes a latest-release card (tag link, draft/prerelease badges, name, `time`-stamped publish date, first 3 asset download links with sizes via the headless-testable `keyAssets` helper in `web/src/lib/releases.js`, "+N more" + "view release" links to the detail page; a `loading latest…` status while the 404-means-none fetch settles so the empty state never flashes) and the shared `Empty` callout (`compact` prop + `.empty-state-compact` styles for the 320 px sidebar) when no release is published. Main list untouched; no backend change — `GET …/releases/latest` already returns the full release including assets.
- **FIXED (issue #41) — mutation-refetch ordering guard + HTTP-cache bypass:** post-mutation `invalidate()` refetches raced in-flight (SSE/coalesced) reads with no ordering guard, and ref-dependent GETs are SWR (`private, max-age=0, stale-while-revalidate=60`) so Chrome could serve the pre-mutation body as 200 from disk cache — the stale commit landed last. Three centralized changes, no per-page churn: (1) `web/src/lib/data.js` generations — every fetch bumps `entry.seq` and commits only while newest (stale bodies AND stale errors dropped); `invalidate()` always starts a new generation and runs the refetch under `ReposClient.withNoStore` (`cache: "no-store"` + `no-cache` headers), falling back to a plain refetch when no SDK client is wired; (2) SDK `_call` forwards a `cache` fetch option and resolves HTTP 304 as the exported `NOT_MODIFIED` sentinel instead of `ReposError(304)` (silent keep-current in the data layer — value untouched, freshness bumped, no tray); (3) background reads keep single-flight + TTL semantics, so the 08 §4 storm-coalescing story is unchanged apart from invalidations no longer joining in-flight fetches. Headless cover: `web/test/unit/data-guard.test.js` (stale-last drop, stale-error drop, invalidate-over-inflight, 304 keep-current, bypass arming) + `web/test/unit/sdk-nostore-304.test.js` (304 sentinel on JSON/raw paths, `cache` forwarding, `withNoStore` arm/disarm/throw-restore).
- **FIXED (issue #37) — clone dialog redesign:** the bare `git clone <https-url>` code block becomes a protocol-aware popover — an HTTPS/SSH toggle (`role="group"`, `aria-pressed`, shared `.btn` + `.btn-active` styling), a readonly command textbox that auto-copies on click (Clipboard API with an execCommand fallback, `lib/clone.js` `copyText`) plus a keyboard-reachable Copy button, and a transient `role="status"` "copied" indication (2 s, cleared on unmount; "copy failed — press Ctrl+C" when neither path works, textbox left selected). The `<details>`/`<summary>` popover stays the keyboard baseline (Enter/Space toggle, Tab walks controls); Esc closes and returns focus to the trigger. URL logic is unchanged: HTTPS is the server's `clone_url` verbatim, SSH reuses its hostname at the default ssh port (`ssh://git@host/owner/repo.git` — the served `internal/sshd` form per README/`bind_ssh_test.go`/the /keys copy; the server never advertises its ssh listen port to the browser, so no port is guessed and the HTTP port is never carried over). Setup recipes stay on the HTTPS URL (their bodies are https-only: bundle-URI URLs, `http.extraHeader`). The `.clone-body` panel gets a solid background (the shared `.card` is translucent in dark mode and tab text bled through). Headless cover: `web/test/unit/clone.test.js` (https verbatim/fallback, ssh host reuse + port drop + host fallbacks, clipboard/fallback/failure copy paths).
- **FIXED (issue #49) — new-release composer redesign:** the bare boxed form adrift in the page's right whitespace becomes a centered `max-w-2xl` composer (the IssueNew/PullNew convention) — heading row with an "all releases" back link, section fieldsets (Target / Content / Visibility) with per-field help text, the tag datalist picker kept (fed by the existing `tags({n: 100})` stream) plus a tag-count/empty/loading line under it, title placeholder defaulting to the typed tag, draft/prerelease as bordered checkbox option rows with one-line descriptions, an action row (primary create + secondary autodraft, busy labels, post-creation asset-upload note), and an inline `role="alert"` error line (IssueNew pattern) for validation ("choose a tag") and server failures — no native `required` bubble, so the disabled-until-tagged button plus the inline error are the single error channel. No backend or SDK change — autodraft fill, create→detail navigation, and disabled logic are unchanged; assets stay on the detail page.
- **FIXED (issue #50) — empty releases page dedupe:** when the release list is empty the Latest sidebar is not rendered at all and the shared `Empty` callout sits in a centered `max-w-xl` composition, leaving exactly ONE "New release" CTA (the callout's router-link action — the toolbar button and the sidebar's own Empty+CTA go away with their containers; the `refresh` button stays right-aligned). Non-empty state is unchanged: toolbar CTA + list + Latest panel (#35), including the drafts-only case where the sidebar still shows its compact Empty. No backend or SDK change.
- **FIXED (issue #115) — opaque dropdown panels + styled scrollbars:** the refs dropdown (`.ref-drop`) rendered semi-transparent — the shared `.card` is `dark:bg-zinc-900/70`, so page text bled through. The issue-#37 `.clone-body` solid-panel treatment is generalized: `.ref-drop`, `.tasks-drop`, `.notif-drop`, `.reaction-drop`, `.label-drop`, `.close-drop` all get solid `bg-white` / `dark:bg-zinc-900` (same-specificity rule ordered after `.card`, so no JSX churn for the background). Scrollable popovers (refs list, label menu, notification tray) carry the new `.scroll-slim` class: thin styled webkit scrollbar plus Firefox `scrollbar-width: thin` + `scrollbar-color`, themed in both modes. Scope: clone menu (already solid), refs picker, tasks overlay, notification tray, reaction menu, label picker, comment close/reopen menu — every absolutely-positioned `.card` popover. No backend or SDK change; no new deps.
- **FIXED (issue #117) — owners page shows per-owner repos + intro:** `/` was a bare owner-name list; it now renders the intro card (§2.3.1) plus one capped section per owner with that owner's repos, newest-first both levels. No new endpoint (core `GET /api/v1/owners` + `/{owner}/repos` already list everything), no new SDK method (`owners.list()/repos()`), no new deps. Caps (`MAX_OWNERS` 50 / `MAX_REPOS_PER_OWNER` 10) and the newest-first proxy (reverse of server order — the listing path exposes no creation timestamps) live in the headless-testable `web/src/lib/owners.js` (`web/test/unit/owners.test.js`); the §2.3 route-table row already promised "each → their repos" and is now true.
- **FIXED (issue #137) — calm owners page + star counts:** per-owner `.card` sections flatten to a `divide-y` divider stack (intro card kept; caps, overflow links, newest-first order from #117 unchanged; owner heading quieter at `text-sm`) and every repo row on `/` and `/:owner` shows `(N ⭐)` via `<StarCount>` (`web/src/components/StarCount.jsx`) on the shared `social:{o}/{r}` 30 s `useData` key — single-flighted, non-blocking (muted `(…)` placeholder, link renders first), worst case bounded by the #117 caps (50 × 10 = 500 GETs cold, each independent). Formatting/TTL constants live in headless-testable `web/src/lib/stars.js` (`web/test/unit/stars.test.js`). Scope: the org page has no repo listing, starred lists are SDK-only (no listing UI yet), and the repo chrome already shows its count via the star toggle — so `/` + `/:owner` is everywhere a repo list renders. No backend change (`GET …/api/social` exists, 07 §7); no new deps. Links stay plain anchors (keyboard flow unchanged); `.muted`/emerald-link classes carry dark + light.
- **FIXED (issue #124) — clone-menu protocol honesty + dead "git" block removal:** the #37 toggle labeled its first pill a hardcoded "HTTPS" while the command text showed the server's `clone_url` verbatim — on plain-http servers (or TLS-terminating proxies, where `baseURL` sees no `r.TLS` and advertises `http://`) the pill and the text disagreed. Decision: the honest rule is (a) — show exactly what the server advertises (that URL is known to work) and derive the pill label from its scheme (`lib/clone.js` `httpProtoLabel`: "HTTPS" for `https://`, "HTTP" otherwise), so pill and text always agree and the command always works. Upgrading `http→https` for display was rejected: the client cannot distinguish "http behind a TLS proxy" from "plain-http only", and on the latter the upgraded command is broken (verified live: `hub.packden.us` advertises `http://`, `git clone http://…` works via the proxy's 301 to https — but a LAN `http://192.168.2.48:8080` box has no TLS to upgrade to). Toggle values become `http`/`ssh` ("http" = the HTTP(S) transport, label by scheme); SSH derivation and recipe pinning are unchanged. Also deletes the leftover pre-#37 `<div class="clone-recipe"><strong>git</strong>…` block that duplicated the command textbox below the recipes. No backend or SDK change; no new deps. Headless cover: `clone.test.js` gains the `httpProtoLabel` scheme-agreement cases. (Follow-up, not this change: `baseURL` could honor `X-Forwarded-Proto`/require `public_url` so proxied servers advertise `https://` in the first place.)
- **FIXED (issue #123) — settings left sidebar (+ WAL move):** the top pill menu becomes a left sidebar with two clear sections — the standard settings listing (scheduled, policy, config, access, CI tokens, webhooks, WAL) plus the Danger Zone in its own danger-styled section (red heading + red entry, active state included). WAL moves from the repo tab bar into the sidebar and renders inline (`Wal.jsx` unchanged — it already reads `useRepo()` itself); the `/wal` route is kept for old links and now highlights Settings (`lib/tabs.js` `wal → settings`). Narrower content column is handled per-tab: webhook add-row becomes URL-full-width + events/secret/button grid row, config publish message input grows (`flex-1 basis-48`), CI-token name input grows with a cap, policy dry-run N+button stay grouped, access add-row becomes a responsive grid, every data table scrolls (`overflow-x-auto` added where missing: tokens, dry-run, webhooks), danger-confirm input is `max-w-full`, and the WAL manifest/local pair gets its missing `grid` (was `data-table` + `md:grid-cols-2` with no `grid`, so the two-up never applied). Responsive: below `lg` the sidebar stacks above content with internally-scrolling section rows; nav semantics are `nav` + section `h3`s + native buttons with `aria-current="page"`, tabs deep-link via `#id`. No backend or SDK change; no new deps. Headless cover: `web/test/unit/settings-nav.test.js` (sections, WAL placement, id/label uniqueness, resolver + hash fallback) with `repo-tabs.test.js` updated for the `/wal → settings` highlight.
- **FIXED (issue #133) — app-wide relative date design:** every timestamp renders through one shared `<DateTime>` (`web/src/components/DateTime.jsx`: `<time dateTime>` + hover `title`) backed by the headless-testable `web/src/lib/format.js` helpers. Three tiers — age < 1 day relative only ("just now" < 60 s, "N minute(s) ago" < 60 min, "N hour(s) ago" < 24 h), 1–30 days "N day(s) ago - {ordinal} of {Month}", 31+ days "{Month} {ordinal}, {Year}"; the hover title is always the user's-local wall time "YYYY-MM-DD HH:MM <zone>" (`fmtDateTitle`), never UTC-suffixed. Decisions: "month" is a fixed 31-day boundary (whole-day age ≤ 30 middle tier, ≥ 31 absolute — calendar months vary 28–31 days, which would flip the tier on unknowable days); "just now" covers sub-minute ages (the issue's examples start at minutes — a bare "0 minutes ago" must never render); future timestamps clamp to "just now" (clock skew / optimistic rows must never render "-3 minutes ago"); month names are fixed English (deterministic in every browser locale — only the hover title localizes); fallbacks match the old per-page formatters (falsy → "", unparseable → String(input)). Sweeps all previous `fmtDate`/`toLocaleString`/ISO-slice call sites (issues, pulls, commits, releases, checks, comments, WAL, settings incl. webhook deliveries, keys, orgs); `Repo.jsx`/`Org.jsx` per-page formatters deleted. Display-only — no backend, SDK, or data change; no new deps. Headless cover: `web/test/unit/dates.test.js` (tier boundaries, ordinals incl. 11/12/13, future clamp, fallbacks, title shape/zone, `dateTimeAttr` normalization).
- **FIXED (issue #142) — owners two-column + last-active + headers:** repo rows on `/` and `/:owner` share one `<RepoRow>` (`Repos.jsx`: link + `<StarCount>` + `<ActivityStamp>`) in a responsive `grid grid-cols-1 sm:grid-cols-2` (one column on narrow widths; grid fills row-wise so newest-first order is preserved; caps/overflow links from #117 unchanged). Each row gains `active <DateTime>` via `<ActivityStamp>` (`web/src/components/ActivityStamp.jsx`, the #133 component). Source decision: latest COMMIT date from `GET …/commits?n=1` (ref defaults to HEAD server-side — one GET per repo, no summary fetch first); the summary carries no date field (a stamp from it would need a new backend field) and the overview's `manifest.last_push` is push-time from a no-store heavyweight — more cost per row for a less precise signal.   Honest-proxy note: pushes that add no commits do not move the stamp; empty repos render "no commits yet", never a fake epoch (the unborn-HEAD 404 maps to `{commits: []}` in the fetch, keeping it out of the error tray). Cost mirrors star counts (shared `activity:{o}/{r}` 30 s key, placeholder-first, tray-on-error, #117-cap-bounded). Owner headers promote from the #137 quiet `text-sm` to `text-base font-bold tracking-tight` (header by scale + weight, not just link color). §2.3.1 documents the grid, the source decision, and the header treatment. No new endpoint, no new SDK method, no new deps; links stay plain anchors (keyboard flow unchanged); `.muted`/emerald-link classes carry dark + light. Headless cover: `web/test/unit/activity.test.js` (commit_date preference, author_date fallback, empty/missing → null, TTL contract).
