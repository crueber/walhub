/**
 * VisSelect render model (Forgejo #410): the headless mirror of
 * web/src/components/VisSelect.jsx's <For> rows.
 *
 * The component renders through the real Solid render path
 * (`<For each={visibilityOptions(isOrg)}>{(o) => <option …/>}</For>`,
 * compiled by vite-plugin-solid against solid-js) with the select's
 * `value` binding marking the current option — no render adapter, no
 * JSX-runtime stub. That JSX cannot execute under plain `node --test`
 * (no vite transform), so the live-render rig
 * (web/test/unit/vis-select.test.js) drives THIS model plus the real
 * `For` from solid-js through the same mapper shape and observes the
 * rows across visibility changes.
 *
 * visRows(isOrg, value) → [{ value, label, selected }] in option
 * order. `selected` is true for exactly the row matching `value`;
 * an unknown/empty value selects nothing (never a public-looking
 * default — the same #394 blank-select rule the component honors by
 * binding `value={props.value ?? ""}`, which matches no option while
 * unseeded). The rows reuse visibilityOptions()' identity-stable
 * objects, so the selected mark is the only thing that moves on a
 * visibility change — the option nodes themselves stay put.
 */

import { visibilityOptions } from "./visibility.js";

export function visRows(isOrg, value) {
  const current = value ?? "";
  return visibilityOptions(isOrg).map((o) => ({
    value: o.value,
    label: o.label,
    selected: o.value === current,
  }));
}
