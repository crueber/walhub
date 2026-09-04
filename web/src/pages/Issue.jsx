// web/src/pages/Issue.jsx — route "/:owner/:name/issues/:num" (02 §11):
// the thread page (header, timeline with seq-window older-on-demand,
// comment composer, sidebar with labels/assignees/milestone/state for ≥
// triage). `issue_event` frames refresh the header, `issue` frames the
// header too — both ride the ONE repo collaboration stream (08 §4), one
// connection per page via mountStreamRetry; frames invalidate cache keys
// (coalesced), they never carry full state.

import { createEffect, createSignal, For, Show } from "solid-js";
import { useParams } from "@solidjs/router";
import { useRepo, fmtDate } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";
import ThreadTimeline from "../components/ThreadTimeline.jsx";
import CommentComposer from "../components/CommentComposer.jsx";
import { useCollabStream } from "../components/collab.jsx";
import { useRole } from "../components/perms.jsx";
import { REACTIONS, reactionEmoji, summaryEntries, reactionChangedText } from "../lib/reactions.js";

function eventText(ev) {
  switch (ev.type) {
    case "opened":
    case "commented":
      return null; // rendered as markdown body
    case "title_changed":
      return `retitled “${ev.from}” → “${ev.to}”`;
    case "labels_changed":
      return `labels ${(ev.added ?? []).map((l) => `+${l}`).join(" ")} ${(ev.removed ?? []).map((l) => `-${l}`).join(" ")}`.trim() || "labels changed";
    case "assignees_changed":
      return `assignees ${(ev.added ?? []).map((a) => `+${a}`).join(" ")} ${(ev.removed ?? []).map((a) => `-${a}`).join(" ")}`.trim() || "assignees changed";
    case "state_changed":
      return ev.to === "closed" ? `closed as ${ev.reason ?? "completed"}` : "reopened";
    case "milestone_changed":
      return `milestone ${ev.from ?? "none"} → ${ev.to ?? "none"}`;
    case "referenced":
      return `referenced by #${ev.source?.num ?? "?"}`;
    case "cross_referenced":
      return `referenced from ${ev.source?.repo ?? "?"}#${ev.source?.num ?? "?"}`;
    case "reaction_changed":
      return reactionChangedText(ev);
    case "closed_by_pr":
      return `closed by #${ev.pr_num} (${ev.keyword})`;
    default:
      return ev.type;
  }
}

export default function Issue() {
  const ctx = useRepo();
  const params = useParams();
  const num = () => params.num;
  const key = () => `issue:${ctx.full}:${num()}`;
  const [getView] = useData(key, () => ctx.repoClient.issues.get(num()));
  const { role } = useRole(ctx.full, ctx.repoClient);
  const canComment = () => role() !== null;
  const [getOlder, setOlder] = createSignal(false);
  // Older event windows accumulate here (the view holds the newest page;
  // both are newest-first, so concatenation stays ordered).
  const [getExtra, setExtra] = createSignal([]);
  const [getExtraMore, setExtraMore] = createSignal(undefined);
  createEffect(() => {
    num(); // reset accumulation when navigating between issues
    setExtra([]);
    setExtraMore(undefined);
  });

  const reload = () => {
    setExtra([]);
    setExtraMore(undefined);
    invalidate(key());
  };

  const thread = () => getView()?.thread;
  const summary = () => thread()?.reaction_summary ?? {};
  const events = () => [...(getView()?.events ?? []), ...getExtra()];
  const more = () => (getExtraMore() === undefined ? getView()?.events_more : getExtraMore());

  const comment = async (body) => {
    await ctx.repoClient.issues.comment(num(), body);
    reload();
  };

  const react = async (seq, content) => {
    try {
      await ctx.repoClient.issues.reactions.add(num(), { target_event_seq: seq, content });
      reload();
    } catch (err) {
      reportError(err, "issue-react");
    }
  };

  // Summary chips toggle: remove when the clicker reacted, add when they
  // did not (the summary carries no per-user state, so a remove-404 falls
  // back to add — GitHub semantics; only a double failure reports).
  const toggleReaction = async (seq, content) => {
    try {
      await ctx.repoClient.issues.reactions.remove(num(), seq, content);
    } catch (err) {
      try {
        await ctx.repoClient.issues.reactions.add(num(), { target_event_seq: seq, content });
      } catch (addErr) {
        reportError(addErr, "issue-react");
        return;
      }
    }
    reload();
  };

  const patch = async (fields) => {
    try {
      await ctx.repoClient.issues.patch(num(), fields);
      reload();
    } catch (err) {
      reportError(err, "issue-patch");
    }
  };

  const loadOlder = async () => {
    const all = events();
    if (!all.length || getOlder()) return;
    setOlder(true);
    try {
      const oldest = all[all.length - 1].seq;
      const page = await ctx.repoClient.issues.events(num(), { after_seq: oldest });
      setExtra([...getExtra(), ...(page.events ?? [])]);
      setExtraMore(page.more);
    } catch (err) {
      reportError(err, "issue-events");
    } finally {
      setOlder(false);
    }
  };

  // The ONE repo collaboration stream (08 §4): one connection for this
  // page, capped reconnect; matching issue frames invalidate (coalesced)
  // — the header refetch (with its newest events page) is the backfill.
  useCollabStream(() => ctx.full, ctx.repoClient, ["issue", "issue_event"], (frame) => Number(frame.num) === Number(num()));

  return (
    <div class="issue-page grid gap-4 md:grid-cols-[1fr_16rem]">
      <div class="min-w-0">
        <Show when={thread()} fallback={<p class="muted">loading…</p>}>
          {(t) => (
            <>
              <h2 class="mb-1 text-lg font-semibold">
                <span class={t().state === "open" ? "text-emerald-700 dark:text-emerald-400" : "text-zinc-500"}>
                  #{t().num} {t().state === "open" ? "Open" : "Closed"}
                </span>{" "}
                {t().title}
              </h2>
              <p class="mb-3 text-xs text-zinc-500 dark:text-zinc-400">
                {t().author} opened {fmtDate(t().created_at)} · {t().comment_count} comments
              </p>
              <ThreadTimeline
                events={events()}
                textFor={eventText}
                fmtDate={fmtDate}
                actionsFor={(ev) => (
                  <Show when={ev.type === "opened" || ev.type === "commented"}>
                    <span class="ml-2 inline-flex gap-1" role="group" aria-label={`reactions for comment ${ev.seq}`}>
                      <For each={REACTIONS}>
                        {(r) => (
                          <button
                            type="button"
                            class="chip hover:border-zinc-400"
                            aria-label={`react ${r}`}
                            title={`react ${r}`}
                            onClick={() => react(ev.seq, r)}
                          >
                            <span aria-hidden="true">{reactionEmoji(r)}</span>
                          </button>
                        )}
                      </For>
                    </span>
                  </Show>
                )}
                summaryFor={(ev) => {
                  const entries = () => summaryEntries(summary(), ev.seq);
                  return (
                    <Show when={(ev.type === "opened" || ev.type === "commented") && entries().length > 0}>
                      <p class="reaction-summary mt-2 flex flex-wrap gap-1" aria-label={`reactions on comment ${ev.seq}`}>
                        <For each={entries()}>
                          {([content, count]) => (
                            <button
                              type="button"
                              class="chip"
                              aria-label={`toggle ${content} reaction, ${count} total`}
                              title={`${content} · ${count}`}
                              onClick={() => toggleReaction(ev.seq, content)}
                            >
                              <span aria-hidden="true">{reactionEmoji(content)}</span> {count}
                            </button>
                          )}
                        </For>
                      </p>
                    </Show>
                  );
                }}
              />
              <Show when={more()}>
                <button type="button" class="btn mt-2" disabled={getOlder()} onClick={loadOlder}>
                  {getOlder() ? "Loading…" : "Older events"}
                </button>
              </Show>
              <Show when={canComment()}>
                <CommentComposer
                  onSubmit={comment}
                  errorKey="issue-comment"
                  mentionId="mention-issue-comment"
                  mentionNames={thread()?.participants}
                />
              </Show>
            </>
          )}
        </Show>
      </div>
      <aside class="grid content-start gap-3">
        <Show when={thread()}>
          {(t) => (
            <>
              <div class="card grid gap-1 p-3 text-sm">
                <span class="text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400">State</span>
                <span>{t().state}{t().state_reason ? ` (${t().state_reason})` : ""}</span>
                <div class="flex gap-1">
                  <button type="button" class="btn px-2 py-0.5 text-xs" onClick={() => patch({ state: t().state === "open" ? "closed" : "open" })}>
                    {t().state === "open" ? "Close" : "Reopen"}
                  </button>
                </div>
              </div>
              <div class="card grid gap-1 p-3 text-sm">
                <span class="text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400">Labels</span>
                <For each={t().labels ?? []} fallback={<span class="muted text-xs">none</span>}>
                  {(l) => <span class="chip w-fit">{l}</span>}
                </For>
                <span class="muted text-xs">label / milestone edits live on their pages (triage)</span>
              </div>
              <div class="card grid gap-1 p-3 text-sm">
                <span class="text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400">Assignees</span>
                <For each={t().assignees ?? []} fallback={<span class="muted text-xs">none</span>}>
                  {(a) => <span>{a}</span>}
                </For>
                <span class="text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400">Milestone</span>
                <span>{t().milestone ?? "none"}</span>
              </div>
            </>
          )}
        </Show>
      </aside>
    </div>
  );
}
