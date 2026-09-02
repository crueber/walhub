# design.md — working on the walhub UI and CSS

> How to build, style, and extend the frontend so every page looks like one product.
> Stack context: SolidJS + `@solidjs/router`, Tailwind v4 (CSS-first), dark-by-default — the decision
> of record is `DEVIATIONS.md` D-WEB-6; the normative spec is `docs/go/12_web_ui.md`.

## 1. Non-negotiables

1. **No CDN, ever.** No external font, script, or image URLs. Fonts are the system stack; icons are
   unicode glyphs or inline SVG. Everything ships in the binary.
2. **Dark mode is the default.** `<html class="dark">` ships in `web/index.html`. Never use
   `prefers-color-scheme` for the default — the theme is a deliberate user choice (`lib/store.js`,
   persisted in localStorage under `walhub-theme`). Light styling is the base layer, `dark:` variants
   carry the dark theme (the custom variant is declared in `ui.css`).
3. **No TypeScript.** Plain JSX/JS. No new npm dependencies without a written amendment (AGENTS.md
   Law 1) — runtime budget is exactly `solid-js` + `@solidjs/router`.
4. **The dogfood rule.** Every server call goes through the SDK (`web/sdk/src/*`). Sanctioned plain
   `fetch` exceptions: the three `/api/v1/setup*` calls and the `/services/setup.json` recipes.
5. **`innerHTML` only through the sanitizer.** `innerHTML={sanitize(renderMarkdown(md))}` for
   markdown, `innerHTML={highlight(code, lang)}` for code. Anything else is an XSS review.

## 2. Files and build

```
web/index.html          vite entry — <html class="dark">, <div id="root">
web/vite.config.mjs     solid() + tailwindcss() plugins, base "/_ui/", outDir dist/
web/src/index.jsx       render + route table (solid-router; nested repo routes)
web/src/App.jsx         chrome: header/nav/theme toggle/progress bar/error tray
web/src/ui.css          ALL styling: Tailwind import + tokens + component classes
web/src/lib/store.js    theme state (signals + localStorage)
web/src/lib/data.js     promise cache on Solid signals (useData/useResolved/…)
web/src/lib/{diff,markdown,sanitize,highlight,setup}.js   pure modules, no DOM
web/src/lib/sse.js      stream mount helper (ownership + cancellation rules)
web/src/pages/*.jsx     one component per route
web/dist/               build output (gitignored except .keep) — embedded by the Go binary
```

`make web` = `vite build` (SPA) + `esbuild` (SDK bundle → `dist/repos.js`); `make build` depends on
it. Assets are content-hashed → served immutable; `index.html` is no-cache + ETag.

## 3. Styling rules (ui.css)

**Tokens live in `@theme`** (`--font-sans`, `--font-mono`). Colors come from Tailwind's palette and
are used consistently:

| Role | Dark | Light | Utility pattern |
|---|---|---|---|
| Page background | `zinc-950` | `zinc-100` | on `<body>` (index.html) |
| Surface (cards) | `zinc-900/70` | `white` | `.card` |
| Borders | `zinc-800` | `zinc-200` | `.card`, `.data-table` |
| Primary text | `zinc-200` | `zinc-900` | on `<body>` |
| Muted text | `zinc-400` | `zinc-500` | `.muted` |
| Accent / links / active | `emerald-400`–`600` | `emerald-600`–`700` | links, `.btn.primary`, active tab |
| Danger | `red-400/600` | `red-600` | `.err-line`, `.tray-entry` |
| Warning | `amber-400/600` | `amber-600` | `.warn-line`, busy pills |

**Component classes** (in `@layer components`) are the vocabulary — USE THEM, don't re-invent:

- `.card` — any panel. Compose with padding utilities (`p-4`) and `mt-4` between cards.
- `.btn` / `.btn.primary` — actions. One `.primary` per view (Save/Run).
- `.pill` — compact status/selector; `.chip` — tiny label ("file", "default", booleans).
- `.input` — every text/select/textarea. Width via utilities (`md:w-72`, `md:max-w-md`).
- `.data-table` — **the table class. Never name anything `.grid`** — it collides with Tailwind's
  `display: grid` utility and silently breaks the table (this shipped once).
- `.code-view`, `.markdown-body`, `.diff-add/.diff-del/.diff-hunk`, `.tok-*` — rendering surfaces.
- `.muted`, `.tabular`, `.err-line`, `.warn-line`, `.progress`, `.tray` — chrome helpers.

**Dark/light discipline:** style the LIGHT look in the base utilities and add `dark:` for dark — the
shipped default is dark, so every component class already carries both halves. Never hardcode a
color that only works in one theme without the counterpart.

## 4. Layout and responsiveness

- The app shell is `flex min-h-screen flex-col`; `main` is `mx-auto w-full max-w-6xl px-4 py-6`.
  Pages NEVER set their own max-width — one container owns the rhythm.
- **Phone (≤ 640px) is a first-class target.** Rules that keep it working:
  - Every WIDE table (≥ 5 columns or long content) lives inside `<div class="overflow-x-auto">…</div>`
    — rows stay single-line (`.overflow-x-auto .data-table td { whitespace-nowrap }`) and the wrapper
    scrolls. 2-column KV tables may sit bare.
  - Repo header and button rows use `flex flex-wrap` + `gap-*` — never a fixed row.
  - Tab/tab-pill bars use `flex flex-wrap gap-1.5` — pills wrap, never squeeze.
  - Form inputs are `w-full` with `md:max-w-md`/`md:w-72` caps on desktop.
  - Nothing may force `document.scrollWidth > innerWidth` — test at 390px.
- Breakpoints in use: default = phone, `md:` = ≥768 desktop, occasionally `sm:`. Don't add more.

## 5. Reactivity and data conventions (Solid)

- State is Solid signals/stores; **no external state library**. App-level state lives in
  `lib/store.js`; cross-component repo state flows through the `RepoCtx` context (`Repo.jsx`,
  `useRepo()`), not prop drilling.
- Data fetching: `useData(key, fn, ttl)` / `useResolved(owner, name, rest, kind)` from `lib/data.js`.
  Keys may be GETTERS — pass getters whenever a value can change within a mounted component
  (`useResolved(() => ctx.owner, () => ctx.name, () => ctx.rest, "tree")`).
- Data flows ONE WAY through the cache: the effect copies the entry's value into a local signal.
  Don't build signal-in-signal getter chains — they don't notify reliably.
- Long-running streams: `mountStream(open, onFrame)` from `lib/sse.js` — the component owns the
  AbortController lifecycle; `onCleanup` cancels.
- Errors: `reportError(err, key)` → global tray. Never let a fetch rejection escape a component.

## 6. Adding a page (checklist)

1. Create `web/src/pages/<Name>.jsx`; add the route in `index.jsx` (repo tabs nest under
   `/:owner/:name` and read the context via `useRepo()`).
2. Fetch through the SDK; keys per §5; wrap wide tables per §4.
3. Style with the §3 component classes + Tailwind utilities; dark + light both covered.
4. Check at 1280px AND 390px in a real browser (AGENTS.md ladder step 8): no horizontal overflow,
   tables scroll inside their cards, tabs/pills wrap.
5. `pnpm --dir web run build` must pass; `node --test web/test/` for logic modules you touched.

## 7. Known-shape notes (server payloads the UI must defend against)

- `settings/describe`: `maintenance.this_host.serves` / `maintains` are **booleans**, `roles` is a
  list — render booleans as yes/no, never `.join()` them. Use `asList()` (`lib/data.js`) for any
  field that might be list-or-string-or-bool.
- `settings/effective` returns **TOML text** (`application/toml`) — the SDK's `settings.effective()`
  uses `raw: true` and resolves a string; don't JSON-parse it.
- Sha-addressed payloads (tree/blob/commits/commit) carry `ref: ""` by design — the UI takes the
  display ref from the resolve step (`useResolved` attaches it).
