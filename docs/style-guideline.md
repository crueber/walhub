# Style guideline — the walhub UI design language

> Status: **normative, not advisory.** This document codifies the app's
> de-facto design language with canonical references (the file and the
> ticket/doc that established each rule), read out of the current tree.
> It is binding for all UI work via the AGENTS.md §2 working rule
> ("The style guideline is binding for all UI work", Forgejo #533).
> Where this guideline conflicts with AGENTS.md law 1 (dependency budget)
> or the Tailwind-only rule, the law wins and this guideline is corrected.
>
> One source of truth: rules already normative in AGENTS.md or
> `docs/go/12_web_ui.md` are **referenced, not restated** — restatements
> drift. Section numbers below name the authoritative location.
>
> Line numbers cite the tree at the time of writing; the cited file +
> ticket is the reference, not the line number.

## 0. What lives where (authoritative locations)

- Stack, dependency budget, build, dark-by-default, no-TypeScript:
  AGENTS.md law 1 and `docs/go/12_web_ui.md` header note + §2.2.
- Data layer (`useData`, TTLs, generations, `tolerateMissing`,
  `tolerateDegraded`, `peekCached`), SSE lifecycle, dogfood rule:
  `docs/go/12_web_ui.md` §§2.4–2.6 (+ §6); implementation
  `web/src/lib/data.js`, `web/src/lib/sse.js`.
- Setup page rendering/validation/save contract:
  `docs/go/12_web_ui.md` §2.10; implementation `web/src/pages/Setup.jsx`,
  `web/src/lib/setup.js`.
- Repo chrome, ref picker, WAL/tasks/settings page contracts:
  `docs/go/12_web_ui.md` §§2.6, 2.9.
- Mobile-viewport check, Tailwind-only composition, rendered-proof rule:
  AGENTS.md §2 working rules (Forgejo #533).

## 1. Foundations

**F1 — Dark is the shipped default; light is the base layer.**
`web/index.html:4` carries `<html lang="en" class="dark">`; the toggle in
`web/src/lib/store.js:2-27` persists the choice in localStorage and syncs
the `.dark` class (`THEME_KEY`, dark stays the default when storage is
unavailable). `web/src/ui.css:1-14` wires it: Tailwind v4 CSS-first
(`@import "tailwindcss"`, no config file, no CDN) with the class-based
dark variant (`@custom-variant dark`, line 9 — never
`prefers-color-scheme`, because the toggle is a deliberate user choice).
Every shared class ships light as the base and carries the dark theme in
`dark:` variants. (AGENTS.md law 1, D-WEB-6; `docs/go/12_web_ui.md` §2.)

**F2 — Colors are theme tokens, never a hardcoded palette.**
Component classes use `zinc-*`/`emerald-*`/etc. pairs in both themes
(e.g. `web/src/ui.css:26` `.card`); the commit graph carries lane colors
in `--graph-*` vars for both themes (`web/src/ui.css:333-354`, Forgejo
#506) and JSX carries only `.gl-N` classes, never literals
(`web/src/pages/Commits.jsx:59`). `web/src/components/ToggleSwitch.jsx:2-7`
documents the same precedent. New UI extends the token families; it never
inlines a hex/rgb literal for themed surfaces.

**F3 — The a11y floor: every keyboard-reachable control shows focus.**
`web/src/ui.css:16-22` (`@layer base :focus-visible`, emerald outline —
`docs/go/12_web_ui.md` §8 §7 rule). Popovers keep a native-button
keyboard baseline (Enter/Space toggle, Tab walks controls, Esc closes and
returns focus — e.g. `web/src/pages/Repo.jsx:355`, Forgejo #214); tab
icons and decorative glyphs are `aria-hidden` so accessible names are
untouched (e.g. `web/src/pages/Issues.jsx` Forgejo #495 comments).

**F4 — Narrow viewports never pan the page.**
Layout changes are verified rendered at ~390px as well as desktop
(AGENTS.md §2). The established repairs: nav strips scroll internally
instead of wrapping (§2); wide tables scroll in place inside
`overflow-x-auto` wrappers (issue #275 — `web/src/pages/Tree.jsx:233-236`,
`web/src/pages/Settings.jsx:377` and siblings); the graph rail hides at
≤480px (§9); `break-words` (not scroll wrappers) contains unbroken
strings in wrapping `kv` cells (issue #275 —
`web/src/pages/Settings.jsx:434-436`).

## 2. Shared vocabulary — `web/src/ui.css` component classes

Each class below is the ONE idiom for its pattern. New UI composes these
instead of re-deciding. (Tailwind-only rule, AGENTS.md §2, Forgejo #533:
a new rule earns its place only as a shared pattern here, per the #405
opaque-popover precedent.)

**Panels and chrome.**

- `.card` (`ui.css:26`) — the panel. Floating `.card`s are opaque by
  structure (§4).
- `.card-meta` (`ui.css:27-32`, issue #277) — meta rows are a flex row
  with gap + wrap so adjacent spans never concatenate; muted `xs` text in
  the Issues/Releases row-meta language, with explicit `{" · "}`
  separators in the markup. Consumed e.g. by
  `web/src/pages/Pull.jsx:135`. (Note: `web/css/repo.css` is
  dead/unbundled — the live stylesheet is this file.)
- `.card-header` (`ui.css:120-127`, Forgejo #521) — the ONE title
  treatment (`text-sm font-semibold`) for sidebar + conversation cards
  (Mergeability, Reviewers, Checks, Merge, Reviews, Finish review,
  Files). Composes with layout utilities; cards MUST NOT drift apart
  into per-card title styles.
- `.site-header`, `.brand` (`ui.css:33-34`) — header chrome and wordmark.
- `.site-nav a`, `.nav-link` (`ui.css:35-38`, issue #238) — one link
  treatment for the primary nav and for nav-styled links in the
  right-aligned utility cluster (the API link sits `ml-auto`, left of the
  tray — `web/src/App.jsx:71-99`).
- `.owner-tabs a` (`ui.css:61-67`, Forgejo #437) — the vertical profile
  tab list (`web/src/pages/Repos.jsx:117-132`): stacked full-width links
  in a bordered box, no scrolling, no wrapping.

**Nav strips scroll internally at narrow widths — never wrap, never shrink
to unreadability.** `.site-nav` (issue #273, `ui.css:39-48`),
`.repo-tabs` (issue #274, `ui.css:49-60` — plus active-tab
`scrollIntoView` on navigation in `web/src/pages/Repo.jsx`), `.owner-tabs`
is the vertical variant (Forgejo #437, no scroll rules). The strip's own
scrollbar is hidden; links stay keyboard-reachable (Tab walks, strip
follows focus) and touch-scrollable; the truncated peek of the next tab
is the scroll affordance.

**Controls.**

- `.btn` / `.btn.primary` / `.btn.danger` (`ui.css:70-75`) — emerald is
  the primary action. Exactly ONE primary CTA per page; the issues-list
  toolbar is the reference (§3).
- `.btn-active` (`ui.css:76`) — toggled pill state (clone-menu
  protocol toggle, issue #37).
- `.pill` (`ui.css:85-86`) — filter chips, badges, doc-tab idiom.
- `.tab-badge` (`ui.css:87-91`, issue #319) — count badges are
  white-on-emerald-500 (reads in both themes, so no `dark:` variant),
  hidden at 0 client-side (`web/src/pages/Repo.jsx:895`).
- `.input` (`ui.css:130-132`) — text-field utilities live here AND ONLY
  here. Never put `.input` on a non-text control (Forgejo #533 — it
  stretched a checkbox full-width and scattered the Settings Features
  rows). Non-text controls use dedicated components:
  `web/src/components/ToggleSwitch.jsx:2-24` (real checkbox, `sr-only`
  first, Tailwind peer pattern, `role="switch"` + `aria-checked`).
- `.icon` (`ui.css:77-84`, Forgejo #465) — the one sizing rule for the
  embedded `web/src/lib/icons.jsx:1-24` set: `1em`/`currentColor`, no
  per-icon CSS, no margins — row spacing stays with the caller's gap
  (#463).

**Settings nav: `.side-nav-*`** (`ui.css:92-107`, issues #123/#276) —
sections with `side-nav-heading` (+ `-danger`), native buttons with
`aria-current="page"` as the selected style, the Danger Zone in its own
danger-styled section (`web/src/pages/Settings.jsx:1355-1421`). Stacks
full-width above the content below `sm:`, side-by-side rows at `sm:`–`lg:`,
sticky sidebar at `lg:`+.

## 3. Page anatomy — the issues list is the canonical reference

`web/src/pages/Issues.jsx` is the reference implementation for list
pages. Sibling tabs (Pulls, Releases — Releases reworked to match in
issue #270) mirror it; new list pages copy its anatomy and cite it.

- **Heading + toolbar row** (`Issues.jsx:250-270`): `h2` in
  `text-xl font-semibold tracking-tight`, toolbar right-aligned via
  `ml-auto flex gap-2` — secondary `.btn` links first (Labels, Milestones
  with leading icons, Forgejo #495), exactly one `.btn.primary` CTA last
  ("New issue"). Feature-gated CTAs hide via the summary flag failing
  CLOSED (`<Show when={!isFeatureDisabled(...)>}`, `web/src/lib/repoFeatures.js`;
  the Empty action mirrors the same gate, `Issues.jsx:345-350`).
- **Filter bar** (`Issues.jsx:272ff`, issue #232): labelled fields in a
  grid card — `card mb-3 grid grid-cols-2 … sm:grid-cols-4 lg:grid-cols-[…auto]`,
  2-up on phones → 4+action wide. Selects bind to URL params.
- **Deep-link honesty:** URL state is resolvable or visibly degraded,
  never dropped. A deep link to a deleted value stays visible (the
  milestone filter shows the raw id — `Issues.jsx:240-246`, issue #416
  binding comment). Pending states disable rather than flash raw ids
  (the select disables while the milestone set loads — same comment).
- **Row meta** reads count → milestone → time: the comment count is a
  decorative 💬 (`aria-hidden`) with the number in an accessible
  `aria-label`/hover title (Forgejo #380-#381 pattern); the milestone
  renders only when set, as a truncating `.chip` link to the filtered
  list (`milestoneFilterHref`, `milestoneDisplay` placeholder `…` while
  cold, bare-id fallback when deleted — `web/src/lib/milestones.js:22-70`).
- **Head pill follows the viewed ref** (issue #252): the pill label is
  client composition over a `viewed` signal (context-first, summary-head
  fallback — `pillHead`/`pillLabel`/`shortRef` in
  `web/src/lib/ref-pill.js:21-42`), published per tab via `createEffect`
  + `onCleanup`, reusing the resolve step (zero new fetches).
- **Clone honesty** (issues #37/#124): the clone popover
  (`web/src/pages/Repo.jsx:104`) shows the server's advertised URLs
  verbatim; the transport pill label derives from the URL scheme
  (`httpProtoLabel`, `web/src/lib/clone.js:29-32`) so pill and text never
  disagree. Copy affordance: readonly textbox + Copy button, transient
  `role="status"` indication (`copyText`, same file).

## 4. Popovers

- **Opaque backgrounds are the STRUCTURAL default** (Forgejo #405,
  `ui.css:190-205`): `.card.absolute, .card.fixed` render solid
  `bg-white / dark:bg-zinc-900` via a higher-specificity rule ordered
  after `.card` — new dropdowns are covered automatically, no per-menu
  hook class needed. (The enumerated hook classes stay in markup as
  harmless markers.)
- **Viewport bound** (issue #278, `ui.css:207-216`): every
  absolute/fixed panel (`.clone-body`, `.ref-drop`, `.tasks-drop`,
  `.notif-drop`, `.reaction-drop`, `.label-drop`, `.milestone-drop`,
  `.close-drop`, `.tag-drop`, `.tray`) carries
  `max-width: calc(100vw - 1rem)`. Desktop widths untouched — the bound
  only caps. Deliberately unbounded: `.progress` (fluid `inset-x-0`),
  inline forms, invisible helpers.
- **Behavior contract** (RefPicker in `web/src/pages/Repo.jsx:280-289` is
  the reference; IdentityMenu, Label/Milestone pickers, CreateMenu,
  NotificationTray, ReactionMenu follow it): outside-click close via a
  document-level listener removed in `onCleanup`; Esc closes and returns
  focus; the ref stream aborts the in-flight stream before opening a new
  one (50/page, 150 ms debounce, keyed rows, `onRef` incremental paint).
- **`.scroll-slim`** (issue #115, `ui.css:218-225`) for popover lists:
  thin themed scrollbars both engines, both themes.
- **Release tag combobox** (Forgejo #254) follows the same shape:
  `.tag-drop` popover, solid-panel + `.scroll-slim`, RefPicker keyboard
  conventions, outside-click close; typing filters client-side
  (`filterTagNames`, `web/src/lib/releases.js`) and never sets the value.

## 5. Empty states

`.empty-state` / `.empty-state-compact` (`ui.css:174-188`, issues
#34/#35): icon/title/hint composition, generous padding, dashed panel —
via the shared `web/src/components/Empty.jsx:1-44` (router-link actions
stay keyboard-focusable; `compact` for narrow sidebars; `role="status"`).
Zero-data renders "No X configured" + setup guidance, never a
machine-internal catch-all. The empty-repo guide (`EmptyRepoGuide`) and
mirror waiting state (`MirrorEmptyGuide` — mirrors NEVER show the push
guide, Forgejo #281) follow the same truthful-empty language. The
exactly-ONE-CTA pairing (toolbar primary + Empty action) is the issues
pattern (issue #50 as amended by #270).

## 6. State chips

`.chip-open` / `.chip-closed` / `.chip-merged` / `.chip-draft` /
`.chip-prerelease` / `.chip-neutral` (`ui.css:108-121`) — the state→color
mapping is fixed (emerald / red / purple / amber / sky / zinc). Consumers:
`web/src/pages/Issue.jsx:440` (open/closed),
`web/src/lib/pull-state.js:53-55,99` (open/closed/merged mapping),
`web/src/pages/Releases.jsx:27-30` (draft/prerelease),
`web/src/pages/Pull.jsx` `reviewVerdictChip` (Forgejo #545 — review
verdicts: APPROVED → chip-open, CHANGES_REQUESTED → chip-closed,
COMMENTED/dismissed/requested → chip-neutral, REVIEW_REQUIRED/unknown →
chip-draft). `.chip-neutral` is the no-signal chip for states that carry
no color meaning. New states extend
the family; inline colors are never used for state.

## 7. Forms

- **Setup rows** (`.setup-row` / `.setup-label` / `.setup-examples` /
  `.setup-note` / `.setup-callout`, `ui.css:133-142`;
  `web/src/pages/Setup.jsx:341-452`): label column left, control right,
  tight gutter; stacks to one column on phones. Native `<details>` for
  collapsible Advanced groups (issue #168 — `.setup-advanced`,
  `ui.css:143-147`; collapsed rows stay mounted so validate/save payloads
  are byte-identical).
- **Composers** (issues #34/#49): centered `max-w-2xl` page
  (`web/src/pages/IssueNew.jsx:84`,
  `web/src/pages/ReleaseNew.jsx:120`), section fieldsets with per-field
  help, inline `role="alert"` error line (`IssueNew.jsx:97`,
  `ReleaseNew.jsx:133`) — no native `required` bubble; the
  disabled-until-valid button plus the inline error are the single error
  channel.
- **Danger Zone** (`web/src/lib/danger.js:1-16`, issue #39): typed
  exact-match confirm — the typed text MUST equal the expected
  `owner/name` exactly (case-sensitive, no trimming); the rule module is
  headless-testable, DOM lives in the component.

## 8. Tables and code views

- **`.data-table`** (`ui.css:150-154`): every non-`kv` data table sits in
  an `overflow-x-auto` wrapper so it scrolls in place (issue #275); rows
  inside such wrappers stay single-line via the `ui.css:152` nowrap rule.
  `kv` tables are deliberately unwrapped (cells wrap; `break-words`
  contains unbroken strings — §F4).
- **`.tree-table`** (issue #223, `ui.css:155-166`;
  `web/src/pages/Tree.jsx:231-251`): no header row, no type column — the
  mode lead char + icons carry the kind. Every column but the name hugs
  its content (`w-px` + nowrap); the name absorbs spare width; the
  size number/unit pair reads as one column. The mode column hides below
  `sm:` (`hidden sm:table-cell`, `Tree.jsx:248`).
- **`.blob-table`** (issue #243, `ui.css:276-289`;
  `web/src/pages/Blob.jsx`): gutter number + code line share a per-line
  `<tr>` so the columns can never desync; same mono metrics throughout;
  line selection highlights the row and pushes a shareable `#L` hash
  (helpers in `web/src/lib/blob-lines.js`; amber tint + emerald gutter
  edge, deliberately unlayered so it wins — `ui.css:308-315`).
- **`.diff-num`** (issue #244, `ui.css:298-305`; shared
  `web/src/components/DiffTable.jsx`, numbering in
  `web/src/lib/diff-lines.js`): one gutter column unified, two (old
  left, new right) in split; file-scoped shareable hashes; same
  highlight treatment as blob (`ui.css:317-323`).
- **`.diff-add` / `.diff-del` row backgrounds** (Forgejo #544,
  `ui.css:295-296` + gutter tiebreaks `ui.css:306-313`): every `+`
  line renders a full-row light green background, every `-` line
  light red — gutter cells included, both themes (`dark:` variants
  carry the default dark theme). The classes apply per CELL via the
  shared `lineClass()` (`DiffTable.jsx:37`): unified rows color
  gutter + code, split rows color per side (a paired change row is
  red-left/green-right, never one color across). The div-based PR
  conversation diff (`DiffFile`, `web/src/pages/Pull.jsx`) imports
  the same helper onto its row divs instead of forking a mapping.
  Gutter tiebreak: a colored gutter carries BOTH classes
  (`diff-num diff-add`), and the compound
  `.diff-num.diff-add/.diff-del` rules beat the plain `.diff-num`
  background at any order (equal-specificity single-class ties would
  lose — `.diff-num` is written later). Line selection still wins:
  `.diff-row.line-hl` stays deliberately unlayered (unlayered beats
  layered at any specificity), same precedent as blob. No color
  literals in JSX — colors live in `ui.css` token composition (F2).
- **Per-line tap-to-comment affordance** (Forgejo #555, shared
  `lineTap()` in `web/src/components/DiffTable.jsx` + the conversation
  `DiffFile` "+" in `web/src/pages/Pull.jsx`): every diff code line ends
  with an inline "+" staging a single-line draft through
  `web/src/lib/review-anchor.js` (the existing single-line shape, side
  convention, and chunk clamping — never a fork). One visibility idiom on
  both surfaces: revealed on row hover (fine pointers, `group` on the
  row), always visible on coarse pointers (`pointer-coarse:inline`), on
  keyboard focus (`focus-visible:inline`, F3); hidden for anonymous
  viewers (the §10 write gate). Code text itself carries no handlers —
  taps on text select text, taps on "+" stage. The affordance wears
  explicit emerald F2 tokens (both themes), never the undefined `link`
  class. The drag/shift range flow and its "comment on selection" bar are
  untouched.
- **`.markdown-body`** (issue #182, `ui.css:234-274`): designed prose
  covering everything marked emits (headings, code, tables, blockquotes,
  nested/task lists, hr, images, links); relative URLs resolve against
  the file's coordinates (`resolveMarkdownUrls`,
  `web/src/lib/render-md.js`; issues #182/#185) and `#N`/`PRN` autolink
  to their threads (Forgejo #340).
- **Dates** (issues #133/#312): every timestamp renders through the ONE
  `<DateTime>` (`web/src/components/DateTime.jsx:1-12`, helpers in
  `web/src/lib/format.js`) — tiered relative→absolute text, local wall
  time hover title, falsy → fallback with no `<time>`. Listing stamps
  reuse it: `<ActivityStamp>` (`web/src/components/ActivityStamp.jsx:1-6`,
  issue #142 — commit time from the listing row, zero per-row fetches)
  and `<StarCount>` (`web/src/components/StarCount.jsx:1-12`, issue #137 —
  shared `social:{o}/{r}` 30 s key, placeholder-first, 404-tolerant per
  issue #150).
- **Text helpers** (`.muted`, `.tabular`, `.err-line`, `.warn-line`,
  `ui.css:168-172`).

## 9. Commit graph — the continuous-gutter rule

(Forgejo #506/#512 — `ui.css:325-400`, `web/src/pages/Commits.jsx:59-147,336-346`.)
JSX carries only `.gl-N` classes; lane colors live in `--graph-*` vars
(700-grade light, 400-grade dark). While graph-on, rows are margin-free
(`.commit-row, .commit-empty { margin: 0 }`) with padding-only separation:
row-level vertical padding is forbidden (it would sit outside the rail
track and break the gutter at every joint — each content column carries
its own `py-2` instead); `divide-y` row borders are dropped and
separation is an inset top edge on `.commit-main` only (never the row's
left edge), so the rail column stays a continuous strip. The rail hides
at ≤480px (rows fall back to 3 columns; parent info survives in text).

## 10. Interaction and state conventions

- **Long work is never a silent spinner** (AGENTS.md law 7): tasks with
  unique ids, `(repo, kind)` single-flight, progress packets, attachable
  SSE streams. The tasks overlay polls 5 s busy / 15 s idle
  (`IDLE_MS`, `web/src/pages/Repo.jsx:23` — Forgejo #396); polling is a
  recursive `setTimeout` chain, never `setInterval`.
- **Error states are human-readable with retry** (`docs/go/12_web_ui.md`
  §2.4): failed fetches reseed from a fresh GET; 404 on an auxiliary
  per-row fetch is missing data, not failure (`tolerateMissing`,
  `web/src/lib/data.js:74-77` — issue #150); degraded reads warn inline
  via `tolerateDegraded` (`data.js:144-146` — issue #209); no raw
  `TypeError` strings in toasts. The tray is fixed bottom-right, max 6,
  deduped, auto-fading.
- **Anonymous writes route to `/login-required`** (Forgejo #502):
  `web/src/lib/writeGate.js:1-8` — every write affordance consults the
  gate before rendering or firing; the SDK's 401→popup retry never fires
  for flagged calls (`noPopupAuth`).
- **SolidJS event lifecycle** (issue #270): NEVER touch
  `e.currentTarget` after an `await` — capture the element synchronously
  first (`const input = e.currentTarget`,
  `web/src/pages/Release.jsx:101-104`). NEVER call a data hook
  (`useData` et al.) inside a `createEffect` body — hooks run at setup,
  effects read their signals.
- **State-derived visuals are spelled out per surface and state**
  (`web/src/lib/icons.jsx` — tab icons belong to the TAB's identity,
  never the viewed page; `web/src/lib/pull-state.js` for PR badges).
- **Listing sources ride the payload, never N fetches** (Forgejo
  #247): ordering, stamps, counts, and badges (`mirrorRowBadge`,
  `web/src/lib/mirror.js` — Forgejo #281) derive from listing rows over
  shared cache keys; per-row probes keep placeholder-first rendering.

## 11. Maintenance

A UI pattern becomes canonical ONLY by landing as a shared implementation
(a `ui.css` class, a `web/src/components/` component, or a
`web/src/lib/` helper) PLUS a guideline entry citing it in the SAME
change. A change that introduces a new pattern where a canonical one
exists, or that re-decides what this guideline settles, is rejected
unless it either extends this guideline in the same change (new canonical
reference + rationale) or carries an amendment in the relevant doc's
"Decisions & deviations" section (AGENTS.md law 12). This guideline never
overrides AGENTS.md law 1 or the Tailwind-only rule.

## Decisions & deviations from the Rust design

- **NEW (Forgejo #537) — this guideline.** The de-facto design language
  previously lived in `ui.css` comments, per-page JSX, scattered AGENTS.md
  working rules, and the cited tickets; it is now codified here with a
  canonical reference per rule, and made binding by the AGENTS.md §2
  amendment in the same change (law 12).
- **AMENDED (Forgejo #545) — `.chip-neutral` joins the state-chip family
  (§6).** Review verdicts with no color signal (COMMENTED, dismissed,
  requested) render zinc in both themes; the PR page's `reviewVerdictChip`
  is the single mapping serving every verdict surface.
