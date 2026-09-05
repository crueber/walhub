// web/src/components/LabelPicker.jsx — 08 §2 LabelPicker + LabelChip
// (issues #45, #107): apply/remove labels on an issue thread.
//
// Contract (08 §2): source is the `labels:{o}/{r}` cache (owned by the
// page, passed in as plain props — this component never fetches);
// triage+ only (the page gates; the server is authoritative per 08 §5
// and surfaces 403s in the error tray). Applied labels render inline
// as chips (LabelChip, owned by the page); the `+` button opens a
// dropdown listing ALL repo label options as menuitemcheckbox rows, so
// adding a label never requires leaving the thread. Keyboard: the
// trigger and every row are native <button>s (Tab/Enter/Space free);
// Esc closes the menu and restores focus to the trigger; a pointer
// click outside closes it. Colors work in dark + light via the shared
// chip classes (dot carries the color; text stays theme foreground).
//
// ### Concurrency
// Hazard: double-clicking a row fires two PATCHes; the second's full
// label array is computed from the pre-mutation applied set, so a
// remove+remove (or add+add) pair is idempotent — the same array twice,
// one labels_changed event per actual delta (no-op PATCHes omit the
// event server-side). Avoidance (client): the page disables the row
// while its mutation is in flight (busy-name set, same idiom as the
// reaction buttons), so the pair cannot happen from one client.

import { createSignal, For, Show, onCleanup } from "solid-js";
import { labelColor } from "../lib/labels.js";

/**
 * LabelChip — one applied label: color dot + name. Unknown labels
 * (deleted after application) render as the bare string (02 §3.1).
 */
export function LabelChip(props) {
  const color = () => labelColor(props.map, props.name);
  return (
    <span class="chip w-fit items-center gap-1">
      <Show when={color()}>
        <span
          class="inline-block h-2.5 w-2.5 rounded-full"
          style={{ "background-color": `#${color()}` }}
          aria-hidden="true"
        />
      </Show>
      {props.name}
    </span>
  );
}

/**
 * LabelPicker — a `+` add button opening a dropdown with every repo
 * label as a toggle row (multi-add without reopening; outside-click
 * and Esc close it). Props: { all: [{name,color,description?}],
 * applied: string[], busy: Set<string> (lowercase names in flight),
 * onToggle(name) }.
 */
export default function LabelPicker(props) {
  const [getOpen, setOpen] = createSignal(false);
  let root;
  let trigger;

  const onDocClick = (e) => {
    if (getOpen() && root && !root.contains(e.target)) setOpen(false);
  };
  const onDocKey = (e) => {
    if (getOpen() && e.key === "Escape") {
      setOpen(false);
      trigger?.focus();
    }
  };
  document.addEventListener("click", onDocClick);
  document.addEventListener("keydown", onDocKey);
  onCleanup(() => {
    document.removeEventListener("click", onDocClick);
    document.removeEventListener("keydown", onDocKey);
  });

  const appliedSet = () => new Set((props.applied ?? []).map((l) => String(l).toLowerCase()));
  return (
    <div class="label-picker relative inline-block" ref={root}>
      <button
        type="button"
        ref={trigger}
        class="btn px-1.5 py-0 text-xs leading-5"
        aria-haspopup="menu"
        aria-expanded={getOpen() ? "true" : "false"}
        aria-label="Add or remove labels"
        title="Add or remove labels"
        onClick={() => setOpen(!getOpen())}
      >
        <span aria-hidden="true">+</span>
      </button>
      <Show when={getOpen()}>
        <div class="card absolute right-0 z-30 mt-1 max-h-72 w-64 overflow-y-auto p-1" role="menu" aria-label="Issue labels">
          <For each={props.all ?? []} fallback={<p class="muted px-2 py-1 text-xs">no labels in this repo yet</p>}>
            {(l) => {
              const on = () => appliedSet().has(String(l.name).toLowerCase());
              const isBusy = () => props.busy?.has(String(l.name).toLowerCase()) ?? false;
              return (
                <button
                  type="button"
                  role="menuitemcheckbox"
                  aria-checked={on() ? "true" : "false"}
                  aria-label={`${on() ? "remove" : "apply"} label ${l.name}`}
                  title={`${on() ? "remove" : "apply"} ${l.name}`}
                  disabled={isBusy()}
                  class="flex w-full items-center gap-2 rounded px-2 py-1 text-left text-sm hover:bg-zinc-100 disabled:cursor-wait disabled:opacity-50 dark:hover:bg-zinc-800"
                  onClick={() => props.onToggle?.(l.name)}
                >
                  <span class="inline-block w-4 shrink-0 text-center" aria-hidden="true">
                    {on() ? "✓" : ""}
                  </span>
                  <span
                    class="inline-block h-3 w-3 shrink-0 rounded-full border border-zinc-300 dark:border-zinc-700"
                    style={{ "background-color": `#${l.color}` }}
                    aria-hidden="true"
                  />
                  <span class="font-medium">{l.name}</span>
                  <Show when={l.description}>
                    <span class="muted truncate text-xs">{l.description}</span>
                  </Show>
                </button>
              );
            }}
          </For>
        </div>
      </Show>
    </div>
  );
}
