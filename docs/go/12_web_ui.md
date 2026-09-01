# 12 — Web UI and the `repos.js` SDK (SolidJS)

> Source: MASTER_RUST_SPEC.md §10 (web UI + SDK), §8.8/§8.9 (auth flows and setup recipes consumed by the UI), §9 (wire contract) · Status: normative for the walhub Go implementation.

The UI is two artifacts in `web/`, built separately and served by the same Go binary:

| Artifact | Path | Language | Dependencies |
|---|---|---|---|
| SDK | `web/sdk/repos.ts` | TypeScript, dependency-free | none |
| SPA | `web/src/**` | TypeScript + SolidJS | ≤ 6 direct npm packages |

Everything else — SSE envelope parsing, diff parsing, markdown, highlighting, progress bar, error tray, ref picker — is hand-rolled per the dependency budget (`01_overview.md`, law 1). The SDK is the only way the SPA talks to the server (dogfood rule, §10.2).

## 1. Artifact A — the SDK (`web/sdk/repos.ts`)

The SDK is a **wire client**, not an app: keep it plain TypeScript with zero imports, no framework, no router. Build target: `esbuild` ONLY (no vite, no framework plugin needed — see §6). Output is dual: `web/dist/repos.js` (IIFE, global `window.repos`) and `web/dist/repos.mjs` (ESM, named exports).

### 1.1 Public surface

```ts
export class ReposClient { /* constructor(opts), configure(opts) */ }
export class ReposError extends Error { status; message; url; get notFound(); get unauthorized(); }
export default ReposClient;   // repos.mjs named exports: ReposClient, ReposError, all types
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

Types are exported for every shape in the spec §9.5 (`Commit`, `TreeEntry`, `RefPage`, `Overview`, `TaskRecord`, `OpSpec`, …). `ReposError` carries `{status, message, url}` with `notFound`/`unauthorized` getters.

### 1.2 Lane selection (normative, order fixed)

```text
explicit `token` option                    → bearer lane    (Authorization: Bearer, credentials: omit)
page origin == API base                    → same-origin    (session cookie, credentials: same-origin)
else                                       → browser lane   (/{o}/{r}/api-browser/…, credentials: include, redirect: manual)
```

On 401 **or an opaque redirect** (a `redirect: manual` fetch reports `type: "opaqueredirect"` / status 0) in the browser lane: open `<base>/api-browser/v1/authenticate` in a popup (single-flight — if a popup is already open, await the same promise), then `await` a `postMessage` of `{type: "repos:authenticated"}` **from our own origin**, then retry the request **exactly once**.

```ts
// single-flight popup promise — one per client instance
let popupAuth: Promise<void> | null = null;
function authenticateOnce(base: string): Promise<void> {
  return (popupAuth ??= new Promise((res, rej) => {
    const w = open(`${base}/api-browser/v1/authenticate`, "repos-auth", "width=520,height=640");
    const onMsg = (ev: MessageEvent) => {
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

`configure({base})` option → `<script data-base>` attribute → `import.meta.url`/script `src` origin → page origin. Off-DOM default `http://127.0.0.1:8080` for tests only. Resolution happens at construction and is re-evaluated by `configure`.

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

## 2. Artifact B — the SolidJS SPA

Rewrite of the Rust spec §10.2 SPA on **SolidJS** (user decision — replaces React 19 + react-router 7). No state library, plain CSS, Suspense-driven data.

### 2.1 npm dependency budget (DECIDED — exactly these, ≤ 6)

| Package | Dev? | Why allowed | What it replaces |
|---|---|---|---|
| `solid-js` | runtime | the framework itself (the one thing we cannot hand-roll sanely) | React 19 |
| `@solidjs/router` | runtime | routing with data APIs; `<Suspense>`-integrated | react-router 7 |
| `marked` | runtime | markdown → HTML; tiny, zero deps | react-markdown + GFM plugin chain |
| `vite` | dev | bundler + dev server + `?raw` imports | — |
| `vite-plugin-solid` | dev | compiles Solid JSX | — |
| `typescript` | dev | type-checking | — |

Three runtime deps total. DECISIONS and hand-rolls:

- **Markdown: `marked` + hand-rolled GFM tables/autolinks.** `marked`'s core already covers GFM tables and autolinks; a react-markdown/rehype chain would drag 15+ transitive packages for no gain. Rendering happens through `sanitizedHtml()` (a hand-rolled DOMPurify-substitute, below).
- **Diff: hand-rolled minimal unified-diff parser in TS (~120 lines).** DECIDED against a diff package (even one allowed): the server sends a single well-formed `git diff` patch (spec §9.5), not arbitrary diffs, so a tiny parser with an explicit grammar (§2.7) is smaller than any npm diff viewer, and the viewer chrome (per-file toggle, anchors, diffstat) is ours anyway.
- **Syntax highlighting: DECIDED — none at runtime.** The Rust spec used shiki (heavy, lazy grammars). Budget says at most one markdown + one diff renderer; we spend it on markdown. Code blobs render as `<pre><code>` with line numbers via a cheap hand-rolled tokenizer for the common cases (keywords/strings/comments/numbers) driven by a filename-extension → language table; unknown extensions render plain. Rationale: a code view that is merely tinted is acceptable; 6th package saved.
- **Sanitization:** markdown preview renders via a ~40-line allowlist sanitizer (tags `p, h1–h6, ul, ol, li, a, code, pre, em, strong, blockquote, table, thead, tbody, tr, th, td, hr, br, img, span`; attributes `href/src/alt/title` only; `href`/`src` schemes restricted to `http, https, mailto, /, #`). Output set via `innerHTML` of the sanitized string. (`innerHTML` of untrusted strings is prohibited everywhere else.)
- **esbuild as a devDependency?** No — vite itself wraps esbuild; nothing extra needed.
- **CSS:** plain CSS files, one per page group, imported by the components; no Tailwind, no CSS-in-JS.

### 2.2 Routes (inventory identical to Rust spec §10.2)

| Route | Page |
|---|---|
| `/` | owners (list of owners; each → their repos) |
| `/api` | API docs page (lazy-loaded) |
| `/:owner` | repos of an owner |
| `/:owner/:repo` | repo shell — tabs Code, Commits, WAL, Settings + tasks overlay |
| `/:owner/:repo/tree/*` | tree at ref/path |
| `/:owner/:repo/blob/*` | blob at ref/path |
| `/:owner/:repo/commits(/…)` | commit history |
| `/:owner/:repo/commit/:sha` | commit detail |
| `/:owner/:repo/wal` | WAL/overview page |
| `/:owner/:repo/settings` | settings (3 sub-tabs) |

All deep-linkable; every UI route returns the SPA `index.html` from the server. Use `@solidjs/router` `<Route>` with lazy imports for `/api` and the repo sub-pages (`lazy(() => import("./pages/Commit"))` — Solid's `lazy` integrates with `<Suspense>` exactly like `React.lazy`).

### 2.3 Data layer (hand-rolled, React-parity)

Implement a module `web/src/lib/data.ts` providing:

- **`useData(key, fn, ttl?)`** — a Suspense promise-cache: `Map` keyed by a string key, entry `{promise, value, at}`; TTL revalidation with default **5 s**; **sha-addressed payloads cached forever** (`ttl = Infinity`); LRU cap **400** entries; background refresh keeps stale data on screen; errors go to the **global error tray** (max 6, deduped by key+message) rather than throwing into the page. Back it with `createResource(key, fetcher)` so `<Suspense>` and transitions work; the cache maps are module-level singletons (store state lives outside the component tree, so Solid's props/reactivity warnings don't apply).
- **`usePending()`** — a global counter of in-flight fetches AND lazy chunks; drives the **top progress bar** (a fixed-position bar animating width; 0 → hidden). Implement as a `createSignal(0)` in a module singleton + `createEffect` writing `style.width`.
- **`useResolved(rev)`** — the two-step resolve→sha-addressed pattern of the spec §9.2: step 1 `repo.resolve(rest)` (ref-dependent, SWR, 5 s TTL, revalidates); step 2 fetches `tree/{sha}/…`, `blob/{sha}/…`, `commits?ref={sha}`, `commit/{sha}` with `ttl = Infinity` (immutable). Implement as a `createResource` chain: `const [resolved] = createResource(() => rest, doResolve); const [tree] = createResource(() => resolved()?.sha, doFetchShaTree);` — no `useResolved` hook equivalence needed; the chain IS the idiom.
- **Error tray:** fixed bottom-right stack, max 6 entries, deduped, dismiss button, auto-fade after 10 s.
- **Progress:** counts every `useData` fetch start/finish and every lazy chunk load.

### 2.4 SSE in the SPA (hand-rolled; never `EventSource`)

`EventSource` cannot set `Accept`/auth headers, so ALL streaming goes through the SDK's fetch-based readers:

```ts
function useSse<T>(open: () => RequestInfo, onFrame: (ev: SseFrame) => void) {
  const [state, setState] = createSignal<"open"|"closed"|"error">("closed");
  createEffect(() => {
    const ctrl = new AbortController();
    onCleanup(() => ctrl.abort());                       // lifecycle: component gone → stream gone
    state.value = "open";
    (async () => { /* via client method that yields frames; SDK closes reader on abort */ })();
  });
  return { state };
}
```

### 2.5 Repo chrome

- **Tabs** computed from the pathname (Code / Commits / WAL / Settings), matching §2.2; the active tab is a derived signal of `useLocation().pathname`.
- **Repo context** (owner/name/refs) fetched **once per visit** via `useData(`repo:${owner}/${name}`, …, 5s)` and provided through Solid `createContext` (map: React context → `createContext` + `useContext` unchanged in spirit).
- **Clone menu** renders the setup recipes from `GET /services/setup.json` (spec §8.9) — never its own copy. The dogfood rule has the same two exceptions the Rust spec names: this fetch and nothing else bypass the SDK (both fetch server-rendered recipe payloads; use plain `fetch` with same-origin credentials).
- **Branch/tag picker** streams refs over SSE via `repo.refStream`: 50 per page, **150 ms debounce** on the query input, **aborts the in-flight stream on every keystroke** (`AbortController` swapped per keystroke; previous reader closed in `onCleanup`-style teardown of the swap), paints rows as they arrive (`onRef` → append to a `createSignal([])` array).

### 2.6 Blob rendering

Server returns raw text (spec §9.5). Decision tree:

```text
too_large (2 MiB cap hit)  → explanatory placeholder
binary (NUL / invalid UTF-8) → explanatory placeholder ("binary file, N bytes")
name ends .md/.markdown    → <MarkdownBlob>  (Preview | Code toggle; marked + sanitizer in preview,
                                              raw text in code view)
otherwise                  → <CodeBlob>       (line-numbered <pre>, tinted by the mini tokenizer)
```

### 2.7 Commit rendering — hand-rolled unified-diff parser

`parsePatchFiles(patch: string, sha: string)` runs client-side on the `patch` field of `commit/{sha}` (spec §9.5: unified diff vs first parent, `--no-color`, rename detection). **Grammar it MUST accept** (each state terminal unless noted):

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

### 2.8 WAL page, tasks overlay, settings page

Content requirements are identical to the Rust spec §10.2 (this doc does not restate them; implement exactly there):

- **WAL page** (`/:owner/:repo/wal`): health box (issues, deep fsck result, suggestions — each dispatches its op as a task), ops box (single-flight buttons, boolean/strategy params, live log via SSE, grouped op history), manifest + local-copy boxes, packs/checkpoints boxes, bundle chain tree (roots = fulls; children under `base_id`; sorted by `creation_token`; warning when an incremental's base vanished), bundle slot plan per strategy (built/skipped/too-small/unavailable counts + actionable rows), compactions table, WAL segments table (newest 5 + "all").
- **Tasks overlay:** polls `…/tasks` (1.5 s busy / 15 s idle), instance-aware finishing rule, 20 s linger, progress pill + dropdown. Polling via `createEffect` + `setTimeout` chain (NOT `setInterval`, so a slow poll never stacks).
- **Settings page:** three sub-tabs — Scheduled tasks (strategy table + placement/host facts + upstream follow status), Push policy (textarea editor, 400 ms debounced validate, dry-run against last N pushes, save/discard/copy), Effective config & history (TOML editor with debounced validate + live fields preview, publish with a message, clear, per-revision history with "Revert to this" + line diff).

### 2.9 React → Solid idioms mapping (for a React-familiar implementer)

| React hook | Solid primitive |
|---|---|
| `useState` | `createSignal` |
| `useEffect` (+ cleanup return) | `createEffect` (+ `onCleanup` inside) |
| `useMemo` | `createMemo` |
| `useRef` (DOM) | direct variable + `ref` attribute |
| `useContext` / `createContext` | `createContext` / `useContext` (same names) |
| `Suspense` / `React.lazy` | `<Suspense>` / `lazy(() => import(…))` |
| `useResolved` (Rust spec) | `createResource` chain (`resolve` resource → sha-keyed immutable resource) |
| `useData` promise-cache | `createResource(key, fetcher)` + module-level `Map` cache (§2.3) |
| re-render on state change | signals re-run only dependent scopes — no VDOM diff |

Mental model: in Solid components run ONCE; effects and JSX expressions, not the function body, re-run on signal changes. Never destructure props (breaks reactivity); access `props.x` directly.

## 3. Serving from the Go binary (embedding contract, byte-compatible)

Same contract as the Rust spec §10.2 tail — the SPA is compiled to `web/dist` and **embedded into the server binary**:

```go
//go:embed all:webdist
var webdist embed.FS        // populated at build time (§6); placeholder index.html keeps fresh checkouts building
```

| Path | Cache behavior |
|---|---|
| `/_ui/assets/*` | `Cache-Control: public, max-age=31536000, immutable` + **strong ETag** + `304` on If-None-Match + br/gz negotiation |
| `/_ui/index.html` (all UI routes) | `Cache-Control: no-cache` + ETag |
| `/repos.js`, `/repos.mjs` | `Cache-Control: no-cache` + ETag |

Precompression: brotli(11) + gzip(9) sibling files (`*.br`, `*.gz`) for every asset ≥ 1 KiB, negotiated by `Accept-Encoding` (Go 1.22 `net/http` + manual `Vary: Accept-Encoding`; the Go server does NOT compress on the fly — it only serves the siblings; hand-roll the negotiation, ~30 lines). Generating the siblings is part of the build (§6), not server startup. Serving details, route registration under `internal/server`, and the `X-Walgit-Capabilities` edge contract are specified in `06_server_http.md`; the API endpoints themselves in `07_api.md`.

## 4. Build pipeline

```
web/
├── sdk/repos.ts        ← artifact A (no framework imports, zero deps)
├── src/                ← artifact B (SolidJS app)
├── package.json        ← deps exactly per §2.1
├── vite.config.ts      ← solid plugin, base "/_ui/", build.outDir "dist"
└── dist/               ← build output (embedded by Go)
    ├── index.html  assets/  repos.js  repos.mjs  *.br  *.gz
```

- **SDK: esbuild ONLY.** `esbuild sdk/repos.ts --format=iife --global-name=repos --outfile=dist/repos.js` and `--format=esm --outfile=dist/repos.mjs` (+ `.min` variants). No vite, no framework plugin: the SDK imports nothing, so bundling is pure TS → JS. DECISION: this replaces the Rust pipeline's pnpm script chain; `pnpm` is replaced by plain `npm`/`bun` scripts (any runner; the spec fixes commands, not the runner).
- **SPA: vite + `vite-plugin-solid`.** DECIDED: vite, not esbuild, for the app — vite gives dev-server HMR plus the maintained Solid plugin for free; esbuild alone would mean hand-rolling HMR and JSX transform wiring for zero real gain. Dev: `vite dev` with `server.proxy` pointing `/api`, `/services`, `/_auth` at `http://127.0.0.1:8080`. Build: `vite build` → `web/dist`.
- **Precompress step:** a tiny Node script `web/scripts/precompress.mjs` (brotli via zlib.brotliCompress, gzip via zlib.gzip) walked over `dist/**` for files ≥ 1 KiB, writing `.br`/`.gz` siblings. Runs after both builds.
- **Order:** `npm run build` = `tsc --noEmit && vite build && esbuild (sdk ×2) && precompress`. The build FAILS without SPA artifacts (same rule as Rust spec).
- **Go embed:** `16_packaging.md` owns the final wiring; this doc requires only that `go:embed` reads a directory that `make ui` populates from `web/dist` and that a placeholder `index.html` exists in the repo so `go build` works on fresh checkouts without a node toolchain.

## 5. End-to-end: what an operator does

```bash
cd web && npm ci && npm run build   # → web/dist with index.html, assets/, repos.js/mjs, *.br/*.gz
cd .. && go build ./cmd/walhub      # binary embeds web/dist
./walhub serve                      # UI at http://127.0.0.1:8080/_ui/, SDK at /repos.js and /repos.mjs
```

Dev loop (UI only, against a running server):

```bash
cd web && npm run dev               # vite dev server, proxying API/auth to :8080
```

A third-party page embedding the SDK:

```html
<script src="https://walhub.example.com/repos.js" data-base="https://walhub.example.com"></script>
<script>
  const c = window.repos.default.configure({ token: "…" });   // bearer lane
  c.repo("demo/hello").tree("main", "").then(t => console.log(t.entries));
</script>
```

## 6. Dogfood rule

The UI uses the SDK for every server call, without exception except the two named in §2.5 (both fetch server-rendered recipe payloads). No raw `fetch` in `web/src/**` outside the SDK directory and those two recipe fetches. CI grep-checkable: `fetch(` must appear in `web/src/**` only inside those two files.

### Concurrency

Hazard: SSE readers leak when components unmount mid-stream; ref-picker keystrokes stack streams (one per keystroke) and repaint out of order.
Avoidance (playbook: `13_concurrency.md` — ownership and cancellation rules): every stream is owned by exactly one component-scoped `AbortController`; `onCleanup` aborts it, and the SDK closes the `ReadableStream` reader in its `finally` (the SDK owns the reader, the component owns the signal — one closer each, no shared mutable reader state). The ref picker swaps its controller per debounced keystroke and aborts the previous one BEFORE opening the new stream, so only one ref stream per picker is ever live; rows are keyed by `name` so late frames from an aborted stream (already cancelled) can never overwrite fresh ones. No unbounded buffering: SSE frames are processed as they are read, appended to bounded signal arrays (cap 1000 rows per picker page); progress callbacks fan out synchronously, never queued.

## Decisions & deviations from the Rust design

- **SPA framework: SolidJS replaces React 19 + react-router 7** (explicit user decision; wire contract untouched — the SDK defines the wire, the framework does not).
- **Dependency set fixed at `solid-js`, `@solidjs/router`, `marked` (+ dev: `vite`, `vite-plugin-solid`, `typescript`)** — 3 runtime deps under the ≤ 6 budget; each named in §2.1 with its rationale.
- **Hand-rolled unified-diff parser instead of a diff package** — the server emits one well-formed `git diff` shape; a ~120-line parser with the §2.7 grammar beats any npm diff viewer on size and control.
- **No shiki: hand-rolled mini tokenizer for code blobs** — spends the "one diff renderer" budget slot on nothing rather than a heavy highlighter; tinted code is an acceptable trade to hold 3 runtime deps.
- **Markdown sanitizer hand-rolled (~40 lines allowlist)** — replaces DOMPurify/rehype-sanitize; render surface is self-produced markdown, attack surface is small and enumerated.
- **SDK built with esbuild ONLY, SPA with vite + solid plugin** — the SDK imports nothing so it needs no framework plugin; the app gets HMR + maintained JSX transform from vite. Simplest split that works.
- **pnpm → npm/bun scripts; oxlint dropped from the required chain** — runner choice left to the operator; `tsc --noEmit` remains the required gate.
- **Import-map/SCC chunk-hash scheme from the Rust build dropped** — plain vite chunking + immutable `/assets` hashing achieves the same cache behavior with far less machinery; the embedding contract (immutable assets, strong ETag, no-cache HTML/SDK, br+gz siblings) is preserved byte-for-byte in behavior.
- **GFM/markdown extras beyond `marked`'s built-ins are not implemented** — preview fidelity is preview-level; code view remains available for exact text.
