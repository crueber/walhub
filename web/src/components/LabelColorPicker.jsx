// web/src/components/LabelColorPicker.jsx — label color dropdown (issue #325).
//
// Replaces the bare 6-hex text input on the label create form with a preset
// swatch dropdown + custom-hex path with live preview. Props:
// { color: string (6-hex, no `#` — single source of truth, owned by the
//   parent signal), onChange(hex) }.
//
// Layout: a trigger button (current-color swatch + hex text + ▾, `.input`
// styling like the app's filter selects) opens a `.label-drop` popover —
// the established LabelPicker/MilestonePicker panel class, so the shared
// opaque-popover and 390px viewport-bound rules in web/src/ui.css cover it
// with no new CSS. The popover holds one button per preset swatch plus a
// "Custom hex…" row that reveals the free-text input and focuses it. The
// input keeps the original `pattern` validation; beside it sits a live
// preview dot rendered with the exact list-row swatch classes
// (`h-3 w-3 rounded-full border border-zinc-300 dark:border-zinc-700` +
// inline `background-color`), neutral (no background) while invalid — never
// a broken or stale preview. Picking a preset fills the parent signal and
// collapses the custom row, so the preview always reflects the effective
// value and submit posts the resolved 6-hex unchanged.
//
// Keyboard: trigger and rows are native <button>s (Tab/Enter/Space free);
// Esc closes the popover and refocuses the trigger (LabelPicker idiom); a
// pointer click outside closes it (document listener, removed in
// onCleanup — the RefPicker pattern). No fetches, no backend change.
//
// ### Concurrency
// Hazard: none — purely local UI state (open/custom signals); the only
// shared state is the parent color signal, written synchronously from
// event handlers. Avoidance: n/a.

import { createSignal, For, Show, onCleanup } from "solid-js";
import {
  LABEL_COLOR_PRESETS,
  isPresetLabelColor,
  isValidLabelColor,
} from "../lib/label-colors.js";

export default function LabelColorPicker(props) {
  const color = () => String(props.color ?? "");
  const valid = () => isValidLabelColor(color());
  const [getOpen, setOpen] = createSignal(false);
  const [getCustom, setCustom] = createSignal(false);
  let root;
  let trigger;
  let customInput;

  // The custom row shows once revealed, or whenever the effective value
  // is not a preset (e.g. restored state) — a non-preset value must never
  // hide its own input.
  const showCustom = () => getCustom() || !isPresetLabelColor(color());

  const close = () => setOpen(false);

  const onDocClick = (e) => {
    if (getOpen() && root && !root.contains(e.target)) close();
  };
  const onDocKey = (e) => {
    if (getOpen() && e.key === "Escape") {
      close();
      trigger?.focus();
    }
  };
  document.addEventListener("click", onDocClick);
  document.addEventListener("keydown", onDocKey);
  onCleanup(() => {
    document.removeEventListener("click", onDocClick);
    document.removeEventListener("keydown", onDocKey);
  });

  const pick = (hex) => {
    props.onChange?.(hex);
    setCustom(false);
    close();
    trigger?.focus();
  };

  const revealCustom = () => {
    setCustom(true);
    close();
    customInput?.focus();
  };

  return (
    <div class="label-color-picker relative" ref={root}>
      <div class="flex flex-wrap items-center gap-2">
        <button
          ref={trigger}
          type="button"
          class="input flex w-auto cursor-pointer items-center gap-2"
          aria-haspopup="listbox"
          aria-expanded={getOpen() ? "true" : "false"}
          aria-label={`Label color ${color() || "unset"} — choose a preset or custom hex`}
          title="Choose a label color"
          onClick={() => setOpen(!getOpen())}
        >
          <span
            class="inline-block h-3 w-3 rounded-full border border-zinc-300 dark:border-zinc-700"
            style={valid() ? { "background-color": `#${color()}` } : {}}
            aria-hidden="true"
          />
          <span class="font-mono text-sm">{color() || "custom"}</span>
          <span aria-hidden="true">▾</span>
        </button>
        <Show when={showCustom()}>
          <span
            class="inline-block h-3 w-3 rounded-full border border-zinc-300 dark:border-zinc-700"
            style={valid() ? { "background-color": `#${color()}` } : {}}
            aria-hidden="true"
            title={valid() ? `Preview #${color()}` : "Enter a valid 6-hex color to preview"}
          />
          <input
            ref={customInput}
            class="input w-28 font-mono"
            value={color()}
            onInput={(e) => props.onChange?.(e.target.value)}
            pattern="[0-9a-fA-F]{6}"
            title="6-hex RGB without #"
            aria-label="label color"
            placeholder="d73a4a"
          />
        </Show>
      </div>
      <Show when={getOpen()}>
        <div
          class="label-drop card absolute left-0 z-30 mt-1 w-64 p-2"
          role="listbox"
          aria-label="Label color presets"
        >
          <div class="grid grid-cols-4 gap-1" role="presentation">
            <For each={LABEL_COLOR_PRESETS}>
              {(hex) => {
                const selected = () => color().toLowerCase() === hex;
                return (
                  <button
                    type="button"
                    role="option"
                    aria-selected={selected() ? "true" : "false"}
                    aria-label={`Preset color ${hex}${selected() ? " (selected)" : ""}`}
                    title={hex}
                    class="flex h-9 items-center justify-center rounded border border-zinc-300 hover:opacity-80 dark:border-zinc-700"
                    classList={{ "ring-2 ring-emerald-500 ring-offset-1": selected() }}
                    style={{ "background-color": `#${hex}` }}
                    onClick={() => pick(hex)}
                  >
                    <Show when={selected()}>
                      <span class="text-sm font-bold text-white drop-shadow" aria-hidden="true">
                        ✓
                      </span>
                    </Show>
                  </button>
                );
              }}
            </For>
          </div>
          <button
            type="button"
            class="mt-1 w-full rounded px-2 py-1 text-left text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800"
            onClick={revealCustom}
          >
            Custom hex…
          </button>
        </div>
      </Show>
    </div>
  );
}
