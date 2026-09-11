// web/src/components/ThreadTimeline.jsx — 08 §2 ThreadTimeline.
//
// The ONE P3 event-log renderer (issue threads, PR conversations,
// review threads): comment kinds (opened/commented, i.e. textFor(ev) ===
// null) render as divider-separated entries — author/date header, markdown
// body, reaction rows — with NO per-comment boxes. Every other kind
// renders as a SINGLE-LINE muted system message ("{actor} {text}", e.g.
// "anon added the approved label"), centered, clearly not a comment.
// Compensating events are normal rows (never rewrite history); comment
// bodies go through renderBody (marked GFM + DOMPurify). `aria-live="polite"` so
// SSE-appended rows announce. Dedup key (num, event_seq) is the caller's
// job (they pass seq-keyed lists); rows carry DOM ids so deep links work
// with keyboard nav.
//
// Chronological render (issue #225): the wire serves event windows
// newest-first (02 §7 Decisions — `after_seq` pages toward older), and
// EVERY caller passes that wire order through; the sort to oldest →
// newest (stable, by seq — system rows are seq-ordered already, so they
// stay in place) happens HERE, once, so issue threads and PR
// conversations can never disagree. Pagination composes above (older
// windows prepend), SSE refetches append below (newest lands last).
// Scroll policy: this component NEVER moves the viewport — no
// autoscroll on append (a reading user is never yanked; newcomers
// appear at the bottom), and prepend-anchoring is the caller's job
// (Issue.jsx `loadOlder` pins the viewport with `anchorScrollTop`).
//
// props: { events, textFor(ev) → string|null (null = comment body),
//   actionsFor?(ev) → JSX (per-comment extras, e.g. reaction buttons),
//   summaryFor?(ev) → JSX|null (per-comment summary row under the body,
//     e.g. the reaction emoji+count chips; null = no row),
//   mdCtx? → optional { owner, repo, ref?, dir? }: owner/repo feeds the #N/PRN
//     autolinker (issue #340 — thread bodies link refs even without file
//     coordinates); ref/dir additionally enable relative-URL resolution
//     (issue #182). Thread bodies carry no file coordinates, so callers pass
//     { owner, repo } and relative URLs stay verbatim — there is no repo file
//     to resolve them against. }.
// Dates render via the shared <DateTime> (issue #133).

import { For, Show } from "solid-js";
import { renderBody } from "../lib/render-md.js";
import { chronological } from "../lib/thread-order.js";
import DateTime from "./DateTime.jsx";

export default function ThreadTimeline(props) {
  const textFor = (ev) => props.textFor(ev);
  // The ONE chronological sort (#225): callers pass wire order
  // (newest-first); the rendered `<ol>` is oldest → newest.
  const ordered = () => chronological(props.events ?? []);
  return (
    <ol class="timeline" aria-live="polite" aria-label="Discussion timeline">
      <For each={ordered()} fallback={<li class="py-3 text-sm text-zinc-500 dark:text-zinc-400">No events yet.</li>}>
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
                  <div class="markdown-body" innerHTML={renderBody(ev.body ?? "", props.mdCtx)} />
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
