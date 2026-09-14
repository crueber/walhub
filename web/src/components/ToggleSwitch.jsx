// web/src/components/ToggleSwitch.jsx — the shared toggle-switch control
// (Forgejo #533): a styled checkbox on the Tailwind peer pattern. A real
// <input type="checkbox"> (keyboard-operable, `role="switch"` with
// `aria-checked`) sits `sr-only` first; the two visual spans follow it as
// siblings so the `peer-checked:` / `peer-focus-visible:` variants fire.
// Track is a w-9 h-5 rounded-full pill (zinc when off, emerald when on);
// the knob is an h-4 w-4 circle that slides via peer-checked:translate-x-4.
// Theme-derived colors in both light and dark — no hardcoded palette.
// The root is a plain span (never a <label>): callers place it inside
// their own row label, so clicking anywhere on the row toggles the input
// natively with no nested-label markup.
//
// Props: checked (boolean), onChange (input change handler),
// label (accessible name, wired as aria-label), disabled (optional).

export default function ToggleSwitch(props) {
  const checked = () => props.checked === true;
  return (
    <span class="relative inline-flex shrink-0 items-center">
      <input
        type="checkbox"
        class="peer sr-only"
        role="switch"
        aria-checked={checked() ? "true" : "false"}
        aria-label={props.label ?? "Toggle"}
        checked={checked()}
        disabled={props.disabled}
        onChange={(e) => props.onChange?.(e)}
      />
      <span
        aria-hidden="true"
        class="block h-5 w-9 rounded-full bg-zinc-300 transition-colors peer-checked:bg-emerald-500 peer-focus-visible:ring-2 peer-focus-visible:ring-emerald-500 peer-focus-visible:ring-offset-2 peer-focus-visible:ring-offset-white dark:bg-zinc-700 dark:peer-checked:bg-emerald-600 dark:peer-focus-visible:ring-offset-zinc-900"
      />
      <span
        aria-hidden="true"
        class="absolute left-0.5 top-0.5 h-4 w-4 rounded-full bg-white shadow transition-transform peer-checked:translate-x-4"
      />
    </span>
  );
}
