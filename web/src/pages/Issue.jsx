// web/src/pages/Issue.jsx — route "/:owner/:name/issues/:num" (02 §11):
// the thread page (header, timeline with seq-window older-on-demand,
// comment composer with close / comment-and-close controls, sidebar with
// labels/assignees/milestone/state badge). `issue_event` frames refresh the header, `issue` frames the
// header too — both ride the ONE repo collaboration stream (08 §4), one
// connection per page via mountStreamRetry; frames invalidate cache keys
// (coalesced), they never carry full state.

import { createEffect, createSignal, For, Show } from "solid-js";
import { useParams } from "@solidjs/router";
import { useRepo, fmtDate } from "./Repo.jsx";
import { useData, invalidate, patchCached, reportError } from "../lib/data.js";
import { TTL } from "../lib/collab.js";
import { toggleLabel, labelColorMap } from "../lib/labels.js";
import LabelPicker, { LabelChip } from "../components/LabelPicker.jsx";
import ThreadTimeline from "../components/ThreadTimeline.jsx";
import CommentComposer from "../components/CommentComposer.jsx";
import { useCollabStream } from "../components/collab.jsx";
import { useRole, roleAtLeast } from "../components/perms.jsx";
import { REACTIONS, reactionEmoji, summaryEntries, adjustSummary } from "../lib/reactions.js";

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
    // No reaction_changed case: those events fold into the per-comment
    // summary chips below (#42b) and never reach the timeline.
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
  const canTriage = () => roleAtLeast(role(), "triage");
  // Repo label set for the sidebar picker + chip colors (08 §6
  // `labels:{o}/{r}`, 30 s TTL; the page owns the cache per the
  // LabelPicker contract — the component only renders props).
  const [getLabelSet] = useData(() => `labels:${ctx.full}`, () => ctx.repoClient.labels.list(), TTL.labels);
  const allLabels = () => getLabelSet()?.labels ?? [];
  const colorMap = () => labelColorMap(allLabels());
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
  // Reaction activity folds into the per-comment summary chips
  // (GitHub-style, #42b): reaction_changed events never render as
  // timeline rows — the summary counts are the whole surface. The cursor
  // stays valid: after_seq is a seq cursor, not a count, so filtering
  // interleaved rows cannot skip older events.
  const events = () => [...(getView()?.events ?? []), ...getExtra()].filter((ev) => ev.type !== "reaction_changed");
  const more = () => (getExtraMore() === undefined ? getView()?.events_more : getExtraMore());

  const comment = async (body) => {
    await ctx.repoClient.issues.comment(num(), body);
    reload();
  };

  const commentAndClose = async (body) => {
    // No atomic comment+close endpoint (PATCH takes state only): post the
    // body first when non-empty (GitHub semantics — empty body just
    // closes), then close. Either step throwing keeps the composer text
    // (CommentComposer clears only on success); after a comment-posted /
    // close-failed split the plain Close button finishes the job.
    if (String(body ?? "").trim()) {
      await ctx.repoClient.issues.comment(num(), body);
    }
    await ctx.repoClient.issues.patch(num(), { state: "closed" });
    reload();
  };

  // One in-flight reaction mutation per (seq, content): the picker and
  // the chips share these keys, so a double-click (or Enter-repeat) can
  // never double-fire — and the server dedups sequential duplicate adds
  // per (actor, target, content) anyway (02 §8). Buttons disable while
  // their key is busy; both are native <button>s (keyboard free) with a
  // theme-agnostic disabled treatment.
  const [getBusy, setBusy] = createSignal(new Set());
  const busyKey = (seq, content) => `${seq}:${content}`;
  const isBusy = (seq, content) => getBusy().has(busyKey(seq, content));
  const withBusy = async (seq, content, fn) => {
    const k = busyKey(seq, content);
    if (getBusy().has(k)) return;
    setBusy((prev) => new Set(prev).add(k));
    try {
      await fn();
    } finally {
      setBusy((prev) => {
        const next = new Set(prev);
        next.delete(k);
        return next;
      });
    }
  };

  // Optimistic summary step on the cached thread view (#42a): the chip
  // paints synchronously via the generation-safe patchCached path, and the
  // guarded invalidate() refetch reconciles the guess (or rolls it back
  // when the mutation fails). Never bypasses the #41 ordering guard.
  const bump = (seq, content, delta) =>
    patchCached(key(), (view) => ({
      ...view,
      thread: {
        ...view.thread,
        reaction_summary: adjustSummary(view.thread?.reaction_summary, seq, content, delta),
      },
    }));

  const react = (seq, content) =>
    withBusy(seq, content, async () => {
      bump(seq, content, +1);
      try {
        await ctx.repoClient.issues.reactions.add(num(), { target_event_seq: seq, content });
      } catch (err) {
        reportError(err, "issue-react");
      }
      reload();
    });

  // Summary chips toggle (#36): remove when the clicker already reacted,
  // add when they did not. The summary carries no per-user state, so a
  // remove-404 falls back to add (GitHub semantics); any other remove
  // failure just reports — only a double failure reports, never a silent
  // no-op. The optimistic guess follows the same path (−1, then +2 on the
  // 404 fallback to undo the guess and apply the add).
  const toggleReaction = (seq, content) =>
    withBusy(seq, content, async () => {
      bump(seq, content, -1);
      try {
        await ctx.repoClient.issues.reactions.remove(num(), seq, content);
      } catch (err) {
        if (err?.notFound || err?.status === 404) {
          bump(seq, content, +2);
          try {
            await ctx.repoClient.issues.reactions.add(num(), { target_event_seq: seq, content });
          } catch (addErr) {
            reportError(addErr, "issue-react");
          }
        } else {
          reportError(err, "issue-react");
        }
      }
      reload();
    });

  const patch = async (fields) => {
    try {
      await ctx.repoClient.issues.patch(num(), fields);
      reload();
    } catch (err) {
      reportError(err, "issue-patch");
    }
  };

  // Sidebar label toggle (#45): one PATCH with the full next set per
  // toggle (one event per 02 §7), optimistic patchCached paint + guarded
  // invalidate reconcile — the same bump pattern as the reaction chips
  // above. One in-flight mutation per label name: a double-click computes
  // the same array twice (idempotent), and the button disables while its
  // key is busy so the pair cannot happen from one client.
  const [getLabelBusy, setLabelBusy] = createSignal(new Set());
  const toggleLabelApply = async (name) => {
    const k = String(name).toLowerCase();
    if (getLabelBusy().has(k)) return;
    setLabelBusy((prev) => new Set(prev).add(k));
    const next = toggleLabel(thread()?.labels ?? [], name);
    patchCached(key(), (view) => ({ ...view, thread: { ...view.thread, labels: next } }));
    try {
      await ctx.repoClient.issues.patch(num(), { labels: next });
    } catch (err) {
      reportError(err, "issue-labels");
    }
    reload();
    setLabelBusy((prev) => {
      const nx = new Set(prev);
      nx.delete(k);
      return nx;
    });
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
                            class="chip hover:border-zinc-400 disabled:cursor-wait disabled:opacity-50"
                            aria-label={`react ${r}`}
                            title={`react ${r}`}
                            disabled={isBusy(ev.seq, r)}
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
                      <div class="reaction-summary mt-2 flex flex-wrap gap-1" role="group" aria-label={`reactions on comment ${ev.seq}`}>
                        <For each={entries()}>
                          {([content, count]) => (
                            <button
                              type="button"
                              class="chip disabled:cursor-wait disabled:opacity-50"
                              aria-label={`toggle ${content} reaction, ${count} total`}
                              title={`${content} · ${count}`}
                              disabled={isBusy(ev.seq, content)}
                              onClick={() => toggleReaction(ev.seq, content)}
                            >
                              <span aria-hidden="true">{reactionEmoji(content)}</span> {count}
                            </button>
                          )}
                        </For>
                      </div>
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
                  onCommentAndClose={t().state === "open" ? commentAndClose : undefined}
                  commentAndCloseLabel="Comment and Close"
                  closeLabel={t().state === "open" ? "Close" : "Reopen"}
                  onClose={() => patch({ state: t().state === "open" ? "closed" : "open" })}
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
              </div>
              <div class="card grid gap-2 p-3 text-sm">
                <span class="text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400">Labels</span>
                <div class="flex flex-wrap gap-1">
                  <For each={t().labels ?? []} fallback={<span class="muted text-xs">none</span>}>
                    {(l) => <LabelChip name={l} map={colorMap()} />}
                  </For>
                </div>
                <Show when={canTriage()}>
                  <LabelPicker all={allLabels()} applied={t().labels ?? []} busy={getLabelBusy()} onToggle={toggleLabelApply} />
                </Show>
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
