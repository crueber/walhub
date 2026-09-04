// web/src/components/ThreadTimeline.jsx — 08 §2 ThreadTimeline.
//
// The ONE P3 event-log renderer (issue threads, PR conversations,
// review threads): one row per event kind, compensating events as normal
// rows (never rewrite history), comment bodies via markdown-lite +
// sanitizer. `aria-live="polite"` so SSE-appended rows announce.
// Dedup key (num, event_seq) is the caller's job (they pass seq-keyed
// lists); rows carry DOM ids so deep links work with keyboard nav.
//
// props: { events, textFor(ev) → string|null (null = markdown body),
//   actionsFor?(ev) → JSX (per-row extras, e.g. reaction buttons),
//   fmtDate }.

import { For, Show } from "solid-js";
import { renderMarkdown } from "../lib/markdown.js";
import { sanitize } from "../lib/sanitize.js";

export default function ThreadTimeline(props) {
  const textFor = (ev) => props.textFor(ev);
  return (
    <ol class="grid gap-2" aria-live="polite" aria-label="Discussion timeline">
      <For each={props.events ?? []} fallback={<li class="card">No events yet.</li>}>
        {(ev) => (
          <li class="card p-3" id={ev.seq != null ? `event-${ev.seq}` : undefined}>
            <article>
              <p class="mb-1 text-xs text-zinc-500 dark:text-zinc-400">
                <span class="font-medium text-zinc-700 dark:text-zinc-200">{ev.actor}</span>
                {" · "}
                {props.fmtDate(ev.at)}
                <Show when={props.actionsFor}>{props.actionsFor(ev)}</Show>
              </p>
              <Show when={textFor(ev)} fallback={<div class="prose-sm" innerHTML={sanitize(renderMarkdown(ev.body ?? ""))} />}>
                <p class="text-sm italic">{textFor(ev)}</p>
              </Show>
            </article>
          </li>
        )}
      </For>
    </ol>
  );
}
