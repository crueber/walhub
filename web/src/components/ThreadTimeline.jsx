// web/src/components/ThreadTimeline.jsx — 08 §2 ThreadTimeline.
//
// The ONE P3 event-log renderer (issue threads, PR conversations,
// review threads): comment kinds (opened/commented, i.e. textFor(ev) ===
// null) render as divider-separated entries — author/date header, markdown
// body, reaction rows — with NO per-comment boxes. Every other kind
// renders as a SINGLE-LINE muted system message ("{actor} {text}", e.g.
// "anon added the approved label"), centered, clearly not a comment.
// Compensating events are normal rows (never rewrite history); comment
// bodies go through markdown-lite + sanitizer. `aria-live="polite"` so
// SSE-appended rows announce. Dedup key (num, event_seq) is the caller's
// job (they pass seq-keyed lists); rows carry DOM ids so deep links work
// with keyboard nav.
//
// props: { events, textFor(ev) → string|null (null = comment body),
//   actionsFor?(ev) → JSX (per-comment extras, e.g. reaction buttons),
//   summaryFor?(ev) → JSX|null (per-comment summary row under the body,
//     e.g. the reaction emoji+count chips; null = no row) }.
// Dates render via the shared <DateTime> (issue #133).

import { For, Show } from "solid-js";
import { renderMarkdown } from "../lib/markdown.js";
import { sanitize } from "../lib/sanitize.js";
import DateTime from "./DateTime.jsx";

export default function ThreadTimeline(props) {
  const textFor = (ev) => props.textFor(ev);
  return (
    <ol class="timeline" aria-live="polite" aria-label="Discussion timeline">
      <For each={props.events ?? []} fallback={<li class="py-3 text-sm text-zinc-500 dark:text-zinc-400">No events yet.</li>}>
        {(ev) => {
          const text = textFor(ev);
          return (
            <Show
              when={text == null}
              fallback={
                // System row: one muted line, not a comment. The actor is
                // part of the sentence so the row needs no header.
                <li
                  class="border-t border-zinc-200 py-2 first:border-t-0 first:pt-0 dark:border-zinc-800"
                  id={ev.seq != null ? `event-${ev.seq}` : undefined}
                >
                  <p class="text-center text-xs text-zinc-500 dark:text-zinc-400">
                    <span class="font-medium text-zinc-700 dark:text-zinc-200">{ev.actor}</span> {text}{" · "}
                    <DateTime value={ev.at} />
                  </p>
                </li>
              }
            >
              {/* Comment entry: divided from its neighbours, never boxed. */}
              <li
                class="border-t border-zinc-200 py-3 first:border-t-0 first:pt-0 dark:border-zinc-800"
                id={ev.seq != null ? `event-${ev.seq}` : undefined}
              >
                <article>
                  <p class="mb-1 text-xs text-zinc-500 dark:text-zinc-400">
                    <span class="font-medium text-zinc-700 dark:text-zinc-200">{ev.actor}</span>
                    {" · "}
                    <DateTime value={ev.at} />
                    <Show when={props.actionsFor}>{props.actionsFor(ev)}</Show>
                  </p>
                  <div class="prose-sm" innerHTML={sanitize(renderMarkdown(ev.body ?? ""))} />
                  {props.summaryFor?.(ev)}
                </article>
              </li>
            </Show>
          );
        }}
      </For>
    </ol>
  );
}
