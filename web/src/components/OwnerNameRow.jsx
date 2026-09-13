// web/src/components/OwnerNameRow.jsx — the shared Owner/Name paired row
// (Forgejo #497): the one place the New-repository (/new) and Import
// (/import) Owner/Name idiom lives, so the two pages cannot drift again
// (#479 standing rule — reference: ReleaseNew.jsx; #486 live name-charset
// validation kept verbatim, including the reserved-height error slot and
// its aria wiring).
//
// Layout contract (all three #497 asks, layout only — submit targets,
// validation, fetches, and navigation are untouched):
// 1. Matched heights — both controls carry an explicit shared `h-9`
//    (the .input py-1.5 + text-sm box computes to ~36px; the height
//    utility pins the border-box height so the native <select> chrome
//    cannot render a few px off the <input>). Deliberately NOT
//    appearance-none: the native dropdown arrow and keyboard behavior
//    stay (criterion 5), so no custom chevron is needed.
// 2. Aligned labels + shift-free error — the grid is `items-start` (both
//    columns top-align) and the Name error slot sits OUTSIDE the
//    two-column grid as a full-width paragraph below it, so the message
//    can appear/disappear without touching the Owner column's geometry.
//    The slot keeps its `min-h-[2rem]` reserve (two text-xs lines — the
//    rule text wraps at sm:2-col and 390px widths) and its
//    aria-live="polite"; the input's aria-describedby points at it.
// 3. Asymmetric split — `sm:grid-cols-[minmax(0,1fr)_minmax(0,2fr)]`
//    (Owner 1/3, Name 2/3): the owner list (self + member orgs from
//    allowedOwners, bounded and short) needs less room than the typed
//    name. `minmax(0, …)` + `min-w-0` cells + `truncate` on the select so
//    a long owner name ellipsizes instead of stretching the column.
//    Mobile stays `grid-cols-1` (stacked, #479 collapse rule).
//
// Props (all reactive accessors, Solid render path — Show/For like the
// inlined originals): prefix ("new" | "import", id namespace),
// getOwner/setOwner, getOwners (null = still resolving → disabled
// fallback select), getName/setName (trimmed on input, as before),
// namePlaceholder, nameCharsError (the shared lib/repo-name.js rule).
//
// OrgNew.jsx has no Owner field and does not use this component.

import { Show, For } from "solid-js";

export default function OwnerNameRow(props) {
  const ownerId = () => `${props.prefix}-owner`;
  const nameId = () => `${props.prefix}-name`;
  const errorId = () => `${props.prefix}-name-error`;
  return (
    <div class="owner-name-row">
      <div class="grid grid-cols-1 items-start gap-3 sm:grid-cols-[minmax(0,1fr)_minmax(0,2fr)]">
        <label class="grid min-w-0 gap-1" for={ownerId()}>
          <span class="text-sm font-medium">Owner</span>
          <Show
            when={props.getOwners() !== null}
            fallback={
              <select id={ownerId()} class="input h-9 truncate font-mono" disabled aria-label="Owner">
                <option>{props.getOwner() || "…"}</option>
              </select>
            }
          >
            <select
              id={ownerId()}
              class="input h-9 truncate font-mono"
              value={props.getOwner()}
              onChange={(e) => props.setOwner(e.currentTarget.value)}
              aria-label="Owner"
            >
              <For each={props.getOwners() ?? []}>{(o) => <option value={o}>{o}</option>}</For>
            </select>
          </Show>
        </label>
        <label class="grid min-w-0 gap-1" for={nameId()}>
          <span class="text-sm font-medium">Name</span>
          <input
            id={nameId()}
            class="input h-9 font-mono"
            value={props.getName()}
            onInput={(e) => props.setName(e.currentTarget.value.trim())}
            placeholder={props.namePlaceholder}
            autocomplete="off"
            spellcheck={false}
            aria-label="Name"
            aria-invalid={!!props.nameCharsError()}
            aria-describedby={errorId()}
          />
        </label>
      </div>
      <p id={errorId()} class="mt-1 min-h-[2rem] text-xs text-red-700 dark:text-red-400" aria-live="polite">
        {props.nameCharsError()}
      </p>
    </div>
  );
}
