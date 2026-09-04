// web/src/components/Empty.jsx — the shared empty-state callout (issue
// #34): icon/title/hint composition, centered with generous padding, used
// by the pulls, checks, releases, and issues lists. Dark + light via the
// .empty-* classes (ui.css); the action is a plain router link, so it is
// keyboard-focusable with the global :focus-visible ring — no extra tab
// stops, no JS. `compact` shrinks the callout for narrow sidebars (the
// releases Latest panel, issue #35). `role="status"` announces the settled state to
// screen readers (a result, not progress — never a silent spinner).

import { Show } from "solid-js";
import { A } from "@solidjs/router";

const ICONS = {
  pull: (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <circle cx="18" cy="18" r="3" />
      <circle cx="6" cy="6" r="3" />
      <path d="M13 6h3a2 2 0 0 1 2 2v7" />
      <line x1="6" x2="6" y1="9" y2="21" />
    </svg>
  ),
  check: (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <circle cx="12" cy="12" r="10" />
      <path d="m9 12 2 2 4-4" />
    </svg>
  ),
  tag: (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <path d="M12.586 2.586A2 2 0 0 0 11.172 2H4a2 2 0 0 0-2 2v7.172a2 2 0 0 0 .586 1.414l8.704 8.704a2.426 2.426 0 0 0 3.42 0l6.58-6.58a2.426 2.426 0 0 0 0-3.42z" />
      <circle cx="7.5" cy="7.5" r=".5" fill="currentColor" />
    </svg>
  ),
  issue: (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      <circle cx="12" cy="12" r="10" />
      <circle cx="12" cy="12" r="1" fill="currentColor" />
    </svg>
  ),
};

export default function Empty(props) {
  return (
    <div class="empty-state" classList={{ "empty-state-compact": props.compact }} role="status" aria-label={props.title}>
      <span class="empty-icon">{ICONS[props.icon] ?? ICONS.issue}</span>
      <p class="empty-title">{props.title}</p>
      <Show when={props.hint}>
        <p class="empty-hint">{props.hint}</p>
      </Show>
      <Show when={props.actionHref && props.actionLabel}>
        <A class="btn primary mt-1" href={props.actionHref}>
          {props.actionLabel}
        </A>
      </Show>
    </div>
  );
}
