// web/src/components/MilestonePicker.jsx — milestone dropdown (issue
// #119): assign/clear the milestone on an issue thread.
//
// Mirrors the LabelPicker idiom (a `+` button opening a dropdown of repo
// options) for the single-select case: one `menuitemradio` row per repo
// milestone plus a "No milestone" clear row, with a check on the current
// selection. Contract: the option source is the page-owned
// `milestones:{o}/{r}` cache (passed in as plain props — this component
// never fetches); triage+ only (the page gates; the server is
// authoritative and surfaces 403s in the error tray). Selecting a row
// PATCHes via onSelect and closes the menu (single-select needs no
// multi-add loop). Keyboard: the trigger and every row are native
// <button>s (Tab/Enter/Space free); Esc closes the menu and restores
// focus to the trigger; a pointer click outside closes it. Colors work
// in dark + light via the shared card/chip classes.
//
// ### Concurrency
// Hazard: double-clicking a row fires two PATCHes for the same target.
// Avoidance (client): the page disables the whole menu while its
// mutation is in flight (busy prop), and the server omits the event for
// no-op PATCHes — a retried select is idempotent.

import { createSignal, For, Show, onCleanup } from "solid-js";

/**
 * MilestonePicker — a `+` button opening a dropdown with a "No
 * milestone" clear row plus every repo milestone as a radio row.
 * Props: { milestones: [{id,title,state?}], current: string|null,
 * busy: bool, onSelect(id|null) }.
 */
export default function MilestonePicker(props) {
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

  const pick = (id) => {
    setOpen(false);
    props.onSelect?.(id);
  };

  return (
    <div class="milestone-picker relative inline-block" ref={root}>
      <button
        type="button"
        ref={trigger}
        class="btn px-1.5 py-0 text-xs leading-5"
        aria-haspopup="menu"
        aria-expanded={getOpen() ? "true" : "false"}
        aria-label="Set milestone"
        title="Set milestone"
        disabled={props.busy}
        onClick={() => setOpen(!getOpen())}
      >
        <span aria-hidden="true">+</span>
      </button>
      <Show when={getOpen()}>
        <div class="milestone-drop scroll-slim card absolute right-0 z-30 mt-1 max-h-72 w-64 overflow-y-auto p-1" role="menu" aria-label="Issue milestone">
          <button
            type="button"
            role="menuitemradio"
            aria-checked={props.current == null ? "true" : "false"}
            aria-label="clear milestone"
            title="No milestone"
            disabled={props.busy}
            class="flex w-full items-center gap-2 rounded px-2 py-1 text-left text-sm hover:bg-zinc-100 disabled:cursor-wait disabled:opacity-50 dark:hover:bg-zinc-800"
            onClick={() => pick(null)}
          >
            <span class="inline-block w-4 shrink-0 text-center" aria-hidden="true">
              {props.current == null ? "✓" : ""}
            </span>
            <span class="muted italic">No milestone</span>
          </button>
          <For each={props.milestones ?? []} fallback={<p class="muted px-2 py-1 text-xs">no milestones in this repo yet</p>}>
            {(m) => {
              const on = () => props.current === m.id;
              return (
                <button
                  type="button"
                  role="menuitemradio"
                  aria-checked={on() ? "true" : "false"}
                  aria-label={`set milestone ${m.title}`}
                  title={m.title}
                  disabled={props.busy}
                  class="flex w-full items-center gap-2 rounded px-2 py-1 text-left text-sm hover:bg-zinc-100 disabled:cursor-wait disabled:opacity-50 dark:hover:bg-zinc-800"
                  onClick={() => pick(m.id)}
                >
                  <span class="inline-block w-4 shrink-0 text-center" aria-hidden="true">
                    {on() ? "✓" : ""}
                  </span>
                  <span class="font-medium">{m.title}</span>
                  <Show when={m.state && m.state !== "open"}>
                    <span class="chip">{m.state}</span>
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
