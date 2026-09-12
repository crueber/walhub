// web/src/components/VisSelect.jsx — the shared profile-visibility
// selector (Forgejo #410): one <select> rendering the owner-appropriate
// options through the REAL Solid render path —
// `<For each={visibilityOptions(props.isOrg)}>{(o) => …}</For>`,
// compiled by vite-plugin-solid against solid-js (mapArray diffs the
// identity-stable option objects, so the populated rows stay settled
// while the `value` binding marks the server value).
//
// Research notes (see docs/features/01_identity_permissions.md
// Decisions, #410 entry — full hypothesis verdicts there):
// - The <For> children mapper is arity-1 `(o) => …`; Solid invokes it
//   per item as `mapFn(item)` (client) / `fn(item, () => i)` (SSR) —
//   never with a raw numeric index, never with `o.label` as the item.
// - `each` is always the populated array (identity-stable per #410),
//   so the mapper never meets an undefined item.
// - The "jsx runtime" 404 was a harness artifact of bypassing the
//   vite build (node has no JSX transform); production ships the
//   runtime INSIDE the bundled `/_ui/assets/*.js` — no separate
//   runtime file exists to 404.
//
// Props: value (current spelling, null/undefined = unseeded → blank
// disabled select matching no option, the #394 rule), disabled,
// isOrg (owner-kind boolean, reactive through props), onChange(next
// spelling), ariaLabel (defaults to "Visibility").

import { For } from "solid-js";
import { visibilityOptions } from "../lib/visibility.js";

export default function VisSelect(props) {
  return (
    <select
      class="input"
      value={props.value ?? ""}
      disabled={props.disabled}
      onChange={(e) => props.onChange?.(e.currentTarget.value)}
      aria-label={props.ariaLabel ?? "Visibility"}
    >
      <For each={visibilityOptions(props.isOrg)}>{(o) => <option value={o.value}>{o.label}</option>}</For>
    </select>
  );
}
