# 12 — Web UI and the `repos.js` SDK (vanilla ES modules)

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

The UI is two artifacts in `web/`, served directly by the same Go binary — **standard ECMAScript, no TypeScript, no framework, zero runtime npm dependencies**. One dev-time exception: the SDK is bundled from submodules by esbuild (§1.0) — the SPA itself has no build step:

| Artifact | Path | Language | Dependencies |
|---|---|---|---|
| SDK | source `web/sdk/src/*.js` → bundle `web/dist/repos.js` | plain ES modules, dependency-free | dev: esbuild only |
| SPA | `web/src/**`, `web/index.html` | standard ECMAScript (ES modules) | none |

Files are modules: the browser loads them as-is via native `<script type="module">` and an **import map** in `index.html`. Everything — reactive helpers, SSE envelope parsing, diff parsing, markdown-lite, highlighting, progress bar, error tray, ref picker, router — is hand-rolled per the dependency budget (`01_overview.md`, law 1). The SDK is the only way the SPA talks to the server (dogfood rule, §10.2).

```html
<!-- web/index.html (excerpt): import map + entry. -->
<script type="importmap">{"imports":{"repos":"/_ui/sdk/src/index.js","ui/":"/_ui/src/","lib/":"/_ui/src/lib/"}}</script>
<script type="module" src="/_ui/src/main.js"></script>
```

Routing is hand-rolled (~60 lines): a `route(pattern)` matcher against `location.pathname` plus a `navigate(path)` that calls `history.pushState` and re-runs the active page's `mount`/`unmount`. Route inventory and lazy loading behavior are UNCHANGED from §2.3; "lazy-loaded" pages are dynamic `import()` of the page module (native, no bundler needed).

## 1. Artifact A — the SDK (`web/sdk/`)

The SDK is a **wire client**, not an app: plain ES modules, zero runtime imports, no framework, no router.
It is **authored as submodules** under `web/sdk/src/` (user decision — never one huge file) and a dev-time
esbuild step bundles the entry module into the single distribution artifact `web/dist/repos.js`, served at
`/repos.js` (one stable URL, one request for external consumers). The SPA does NOT import the bundle: the
import map maps `"repos"` to the SOURCE entry (`/_ui/sdk/src/index.js`), so the app and the bundle share
one source of truth and dev edits need no rebuild. No IIFE/global build and no `.mjs` twin — pre-1.0,
no-compat: ES modules are the only distribution.

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

Build (the ONE dev-time tool step; `esbuild` is the single devDependency — runtime budget stays zero):

```sh
cd web && pnpm install --frozen-lockfile     # devDependencies only: esbuild
pnpm run build:sdk                            # esbuild sdk/src/index.js --bundle --format=esm \
                                              #   --target=es2022 --minify --outfile=dist/repos.js
```

Rules: the bundle is a build ARTIFACT (`web/dist/` is gitignored; a placeholder `web/dist/.keep` is
committed so `go:embed` always resolves); `make web` runs it and `make build` depends on `make web`;
tests import the SOURCE modules directly (§5) and need no build; CI runs the bundle smoke test only when
`dist/repos.js` exists. No hashing/immutable scheme for the bundle — `/repos.js` stays `no-cache` + strong
ETag so a redeploy is picked up on the next fetch.

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

## 2. Artifact B — the vanilla SPA (`web/src/**`)

Rewrite of the Rust spec §10.2 SPA on **standard ECMAScript** (user decision — replaces SolidJS, which itself replaced React 19 + react-router 7). No state library, no router library, plain CSS files, no build.

### 2.1 Reactive core — `web/src/lib/reactive.js` (~40 lines, normative API)

A signal-lite primitive set. The whole app's reactivity rests on this contract:

```js
export function createSignal(initial)   // → [get, set]; get() reads, set(v) or set(prev => next) writes
export function createEffect(fn)        // runs fn immediately; re-runs when any signal read inside changes
export function createMemo(fn)          // derived value, cached until a dependency changes
export function onCleanup(fn)           // registers teardown for the current computation scope (if any)
export function createRoot(fn)          // scoped root: fn(dispose); teardown on dispose
```

**Reactivity contract:**

- Signals are synchronous pull/push: `set` notifies subscribers synchronously; effects run in registration order after a microtask-free synchronous flush (batching is NOT required; each `set` triggers dependent effects immediately — the app is small enough that consistency beats batching).
- Dependencies are tracked at read time: any `get()` called while an effect/memo is running becomes a dependency. No explicit dependency lists.
- `createEffect` inside `createRoot` registers its teardown; pages call `createRoot` in `mount` and `dispose()` in `unmount` — the lifecycle rule of §2.5.
- Memos are lazy-ish: recomputed on first read after invalidation, not eagerly.
- No props system, no JSX, no VDOM. UI updates are explicit: effects write to DOM nodes (`el.textContent = …`, `el.classList.toggle(…)`) or re-render a `<template>`-stamped list.

**Templates:** static structure lives in `<template>` elements inside `index.html` / page modules, cloned with `template.content.cloneNode(true)` and wired with a tiny `bind(el, map)` helper (query by `data-*` attribute, attach). Pages stay plain functions: `mount(container, params) → unmount()`.

### 2.2 npm dependency budget (DECIDED — zero runtime, one dev tool)

Zero **runtime** `package.json` dependencies. Exactly one devDependency: `esbuild` (the §1.0 SDK bundle); no framework packages, no lockfile-heavy toolchain under `web/`. Everything previous packages provided is now hand-rolled or native:

- **Markdown: hand-rolled markdown-lite (~150 lines).** Covers the preview surface: headings, paragraphs, fenced code blocks, inline code, bold/italic, links + autolinks, GFM tables, blockquotes, lists (nested one level), `hr`, images. No plugins, no AST — a line-based emitter feeding the sanitizer below. Preview fidelity is preview-level (prior decision preserved); the code view shows exact text.
- **Diff: hand-rolled minimal unified-diff parser in JS (~120 lines).** Unchanged from the previous decision: the server sends a single well-formed `git diff` patch (spec §9.5), so a tiny parser with an explicit grammar (§2.8) is smaller than any library.
- **Syntax highlighting: none at runtime.** Code blobs render as `<pre><code>` with line numbers via a cheap hand-rolled tokenizer for the common cases (keywords/strings/comments/numbers) driven by a filename-extension → language table; unknown extensions render plain.
- **Sanitization:** markdown preview renders via a ~40-line allowlist sanitizer (tags `p, h1–h6, ul, ol, li, a, code, pre, em, strong, blockquote, table, thead, tbody, tr, th, td, hr, br, img, span`; attributes `href/src/alt/title` only; `href`/`src` schemes restricted to `http, https, mailto, /, #`). Output set via `innerHTML` of the sanitized string. (`innerHTML` of untrusted strings is prohibited everywhere else.)
- **CSS:** plain CSS files, one per page group, `<link>`ed from `index.html`; no Tailwind, no CSS-in-JS.

### 2.3 Routes (inventory identical to Rust spec §10.2)

| Route | Page |
|---|---|
| `/` | owners (list of owners; each → their repos) |
| `/api` | API docs page (lazy `import()`) |
| `/:owner` | repos of an owner |
| `/:owner/:repo` | repo shell — tabs Code, Commits, WAL, Settings + tasks overlay |
| `/:owner/:repo/tree/*` | tree at ref/path |
| `/:owner/:repo/blob/*` | blob at ref/path |
| `/:owner/:repo/commits(/…)` | commit history |
| `/:owner/:repo/commit/:sha` | commit detail |
| `/:owner/:repo/wal` | WAL/overview page |
| `/:owner/:repo/settings` | settings (3 sub-tabs) |
| `/setup` | Setup UI (§2.10) |

All deep-linkable; every UI route returns the SPA `index.html` from the server. Lazy pages: `const Commit = () => import("./pages/commit.js")` resolved on first navigation and cached in a module-level map — dynamic import is native, nothing to wire up.

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
- **Settings page:** three sub-tabs — Scheduled tasks (strategy table + placement/host facts + upstream follow status), Push policy (textarea editor, 400 ms debounced validate, dry-run against last N pushes, save/discard/copy), Effective config & history (TOML editor with debounced validate + live fields preview, publish with a message, clear, per-revision history with "Revert to this" + line diff).

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

The SPA is **raw files in `web/`**, embedded directly. The SDK bundle is the one build output, embedded alongside:

```go
//go:embed all:web
var web embed.FS            // index.html, dist/repos.js, sdk/src/**, src/**, css — dist/.keep is committed so the embed resolves on fresh checkouts with no toolchain
```

| Path | Cache behavior |
|---|---|
| `/_ui/src/*`, `/_ui/css/*` (immutable-by-convention modules) | `Cache-Control: public, max-age=31536000, immutable` only for `/assets/*` if a hashed copy exists; modules are served `Cache-Control: no-cache` + **strong ETag** + `304` on If-None-Match |
| `/_ui/index.html` (all UI routes) | `Cache-Control: no-cache` + ETag |
| `/repos.js` | `Cache-Control: no-cache` + ETag |

Compression: `gzip` via server middleware for text assets (`text/*`, `application/javascript`, `application/json`, `text/css`) when `Accept-Encoding` contains `gzip` and the body is ≥ 1 KiB — on-the-fly `gzip.Writer` with level 6, `Vary: Accept-Encoding` set, no brotli, no precompressed sibling files (the zero-build rule means nothing generates them). Serving details, route registration under `internal/server`, and the `X-Walgit-Capabilities` edge contract are specified in `06_server_http.md`; the API endpoints themselves in `07_api.md`.

## 4. Repository layout

```
web/
├── index.html          ← SPA entry: import map, <template>s, css <link>s
├── sdk/src/            ← artifact A source, SUBMODULES (§1.0): index/core/errors/sse/auth/repo/admin/types
├── dist/               ← build output: repos.js (gitignored except .keep placeholder)
├── package.json        ← devDependencies: esbuild. scripts: build:sdk
├── src/                ← artifact B: main.js router, pages/*.js, lib/{data,reactive,sse,diff,markdown,sanitize,highlight,setup}.js
├── css/*.css           ← plain CSS, linked from index.html
└── test/               ← node --test suites (§5): unit/*.test.js, smoke/*.test.js, helpers/server.js
```

The SPA has **no build step** (raw modules in, raw modules served). The SDK has exactly one: esbuild from
`sdk/src/index.js` to `dist/repos.js` (§1.0); `make web` runs it, `make build` depends on it
(`16_packaging.md` owns the wiring).

## 5. Testing the JS (`node --test`, zero npm test deps)

Runner: Node's built-in test runner — `node --test web/test/unit/ web/test/smoke/`. No jest, no vitest, no dependencies. Logic and DOM are **separated by rule**: every module in `web/src/lib/` except `data.js`'s DOM-adjacent bits must be importable in Node with no `document`/`window` access; DOM wiring lives in `web/src/pages/` and `main.js`.

**Pure (headless-testable) modules:** `sdk/src/*.js` (api client groups, injectable `fetch` + stream shim, envelope parser), `lib/sse.js` (frame parser, one parser shared with `sdk/src/sse.js`), `lib/diff.js`, `lib/markdown.js`, `lib/sanitize.js`, `lib/reactive.js`, `lib/setup.js` (setup form validation rules, mirroring the server), `lib/highlight.js`.
**DOM-wiring modules (not unit-tested):** `pages/*.js`, `main.js`, the router glue, anything touching `document`/`template`/`location` — exercised by smoke tests instead.

```js
// web/test/unit/reactive.test.js (skeleton)
import { test } from "node:test";
import assert from "node:assert/strict";
import { createSignal, createEffect } from "../../src/lib/reactive.js";

test("effect reruns on signal change", () => {
  const [get, set] = createSignal(1);
  let runs = 0;
  createEffect(() => { get(); runs++; });
  set(2);
  assert.equal(runs, 2);
});
```

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
// web/test/smoke/server.test.js (skeleton) — real server binary, real fetch
import { test } from "node:test";
import assert from "node:assert/strict";
import { startServer } from "./helpers/server.js";   // spawns ./walhub serve on a temp data-dir, waits for /healthz

test("pages and SDK serve", async () => {
  const base = await startServer();
  const ui = await fetch(`${base}/_ui/`);
  assert.equal(ui.status, 200);
  assert.match(await ui.text(), /importmap/);
  const sdk = await fetch(`${base}/repos.js`);
  assert.equal(sdk.status, 200);
  const mod = await import(sdk.url);                   // the served bytes parse as ESM
  assert.equal(typeof mod.ReposClient, "function");
});
```

`web/test/smoke/setup-api.test.js` (same shape): on a fresh data-dir (D5 defaults boot), `GET /api/v1/setup` → `{access: "open"}`, `POST /api/v1/setup/test` with `{server: {listen: "0.0.0.0:9999"}}` → 200, `PUT /api/v1/setup` → 200 with `restart_required` containing `"server.listen"`. Also required: `web/test/unit/setup.test.js` — table-driven cases for the `lib/setup.js` validators mirroring the server (valid values pass; range/enum/format/cross-field violations fail with the server's message).

Make wiring (D3 — all dev/CI targets are Make targets): `test-web:` runs `node --test web/test/unit/ web/test/smoke/`.

## 5b. End-to-end: what an operator does

```bash
make build   # go build; web/ is embedded raw — no UI build step exists
make dev     # ./walhub serve with defaults (D5) — UI at http://127.0.0.1:8080/_ui/, SDK at /repos.js
```

A third-party page embeds the SDK as a module — the only supported integration (no IIFE/global build exists): `<script type="module">import repos from "https://walhub.example.com/repos.js"; repos.configure({token:"…"}).repo("demo/hello").tree("main","").then(console.log)</script>`.

## 6. Dogfood rule

The UI uses the SDK for every server call, without exception except the ones named in §2.6 and §2.10 (both recipe fetches and the three Setup API calls; all fetch server-rendered JSON payloads). No raw `fetch` in `web/src/**` outside the SDK directory and those files. CI grep-checkable: `fetch(` must appear in `web/src/**` only inside those files.

### Concurrency

Hazard: SSE readers leak when components unmount mid-stream; ref-picker keystrokes stack streams (one per keystroke) and repaint out of order.
Avoidance (playbook: `13_concurrency.md` — ownership and cancellation rules): every stream is owned by exactly one component-scoped `AbortController`; unmount tears it down via the §2.5 pattern's returned cancel function, and the SDK closes the `ReadableStream` reader in its `finally` (the SDK owns the reader, the component owns the signal — one closer each, no shared mutable reader state). The ref picker swaps its controller per debounced keystroke and aborts the previous one BEFORE opening the new stream, so only one ref stream per picker is ever live; rows are keyed by `name` so late frames from an aborted stream (already cancelled) can never overwrite fresh ones. No unbounded buffering: SSE frames are processed as they are read, appended to bounded signal arrays (cap 1000 rows per picker page); progress callbacks fan out synchronously, never queued.

## Decisions & deviations from the Rust design

- **SolidJS + Tailwind v4 SPA (2026-09-02, explicit user request — supersedes the two vanilla decisions above; DEVIATIONS.md D-WEB-6).** The user reversed the vanilla-ESM direction: runtime deps exactly `solid-js` + `@solidjs/router`; state via Solid signals/stores/context; Tailwind v4 CSS-first with `@custom-variant dark` and dark as the default theme (class on `<html>`, persisted, no CDN — system font stack); vite + vite-plugin-solid + @tailwindcss/vite build `web/src/**` (JSX, no TypeScript) into `web/dist/` (hashed assets immutable, `index.html` no-cache + ETag — reverts D-WEB-3's caching class for built assets). The SDK remains dependency-free and esbuild-bundled; `/repos.js`, `/repos.mjs`-absence, and the dogfood rule are untouched. Pure modules (diff, markdown-lite, sanitizer, highlighter, setup validation, SSE frame parsing) carried over unchanged.
- **SPA framework: vanilla standard ECMAScript replaces SolidJS** (D2, explicit user decision — itself a supersession of the earlier SolidJS-over-React decision; wire contract untouched — the SDK defines the wire, the framework does not). Native ES modules + import map, `<template>` + a ~40-line hand-rolled reactive core (§2.1), hand-rolled router. No state library, no JSX, no VDOM.
- **Superseded — dependency set `solid-js`, `@solidjs/router`, `marked` (+ dev: `vite`, `vite-plugin-solid`, `typescript`)**: the npm budget is now **zero runtime dependencies; exactly one devDependency (`esbuild`, §1.0)**. `solid-js`/`@solidjs/router` are replaced by the §2.1 reactive core + hand-rolled router; `marked` by hand-rolled markdown-lite (§2.1) with unchanged preview-fidelity stance and the same allowlist sanitizer.
- **Superseded (again) — "there is no build"**: a build step returns, scoped tightly (user decision): the SDK is authored as submodules and bundled by **esbuild** (the single devDependency) into `web/dist/repos.js`; the SPA remains unbuilt raw ES modules; runtime npm budget stays zero.
- **Superseded — `tsc --noEmit` as required gate; vite/pnpm script chain**: no TypeScript anywhere; the only UI-path tooling is the §1.0 esbuild bundle via `make web`. Gates are `make test-web` (§5) and CI greps (dogfood rule, no-TS rule).
- **Superseded — vite chunk-hash immutable `/assets` scheme with br+gz precompressed siblings**: modules are served raw with `no-cache` + strong ETag; compression is on-the-fly gzip middleware (§3). The behavioral contract (fresh content on deploy, cheap revalidation, compressed text assets) is preserved.
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
