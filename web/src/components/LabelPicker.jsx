// web/src/components/LabelPicker.jsx — 08 §2 LabelPicker + LabelChip
// (issue #45): apply/remove labels on an issue thread.
//
// Contract (08 §2): source is the `labels:{o}/{r}` cache (owned by the
// page, passed in as plain props — this component never fetches);
// triage+ only (the page gates; the server is authoritative per 08 §5
// and surfaces 403s in the error tray). No modal, no popup: the picker
// is an inline checkbox-style list, so keyboard access and focus order
// are native (real <button>s with aria-pressed) and there is nothing to
// trap or dismiss. Colors work in dark + light via the shared chip
// classes (dot carries the color; text stays theme foreground).
//
// ### Concurrency
// Hazard: double-clicking a row fires two PATCHes; the second's full
// label array is computed from the pre-mutation applied set, so a
// remove+remove (or add+add) pair is idempotent — the same array twice,
// one labels_changed event per actual delta (no-op PATCHes omit the
// event server-side). Avoidance (client): the page disables the row
// while its mutation is in flight (busy-name set, same idiom as the
// reaction buttons), so the pair cannot happen from one client.

import { For, Show } from "solid-js";
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
 * LabelPicker — every repo label as a toggle row.
 * Props: { all: [{name,color,description?}], applied: string[],
 * busy: Set<string> (lowercase names in flight), onToggle(name) }.
 */
export default function LabelPicker(props) {
  const appliedSet = () => new Set((props.applied ?? []).map((l) => String(l).toLowerCase()));
  return (
    <div class="grid gap-1" role="group" aria-label="labels">
      <For each={props.all ?? []} fallback={<span class="muted text-xs">no labels defined — create one on the labels page</span>}>
        {(l) => {
          const on = () => appliedSet().has(String(l.name).toLowerCase());
          const isBusy = () => props.busy?.has(String(l.name).toLowerCase()) ?? false;
          return (
            <button
              type="button"
              class="chip w-fit items-center gap-1 hover:border-zinc-400 disabled:cursor-wait disabled:opacity-50"
              aria-pressed={on() ? "true" : "false"}
              aria-label={`${on() ? "remove" : "apply"} label ${l.name}`}
              title={`${on() ? "remove" : "apply"} ${l.name}`}
              disabled={isBusy()}
              onClick={() => props.onToggle?.(l.name)}
            >
              <span
                class="inline-block h-2.5 w-2.5 rounded-full border border-zinc-300 dark:border-zinc-700"
                style={{ "background-color": `#${l.color}` }}
                aria-hidden="true"
              />
              {l.name}
              <Show when={on()}>
                <span aria-hidden="true"> ✓</span>
              </Show>
            </button>
          );
        }}
      </For>
    </div>
  );
}
