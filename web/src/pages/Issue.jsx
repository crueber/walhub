// web/src/pages/Issue.jsx — route "/:owner/:name/issues/:num" (02 §11):
// the thread page (header with the state badge, timeline with seq-window older-on-demand,
// comment composer with close / comment-and-close controls, one sidebar metadata
// card with labels/assignees/milestone sections). `issue_event` frames refresh the header, `issue` frames the
// header too — both ride the ONE repo collaboration stream (08 §4), one
// connection per page via mountStreamRetry; frames invalidate cache keys
// (coalesced), they never carry full state.

import { createEffect, createSignal, For, Show } from "solid-js";
import { useParams } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { useData, invalidate, patchCached, reportError } from "../lib/data.js";
import { TTL } from "../lib/collab.js";
import { toggleLabel, labelColorMap } from "../lib/labels.js";
import { milestoneTitle, milestonePatch } from "../lib/milestones.js";
import LabelPicker, { LabelChip } from "../components/LabelPicker.jsx";
import MilestonePicker from "../components/MilestonePicker.jsx";
import ThreadTimeline from "../components/ThreadTimeline.jsx";
import DateTime from "../components/DateTime.jsx";
import CommentComposer from "../components/CommentComposer.jsx";
import { useCollabStream } from "../components/collab.jsx";
import { useRole, roleAtLeast } from "../components/perms.jsx";
import { reactionEmoji, summaryEntries, addableReactions, adjustSummary } from "../lib/reactions.js";
import ReactionMenu from "../components/ReactionMenu.jsx";
import { issueEventText, closePatch, closedStateLabel } from "../lib/issue-events.js";

// System-row text comes from the shared honest-event lib (null = comment
// body). Never asserts a close reason the event does not carry.
// Milestone ids resolve to titles through the page-owned milestones
// cache (same source as the sidebar display); deleted milestones fall
// back to the bare id (02 §3.1 self-heal).
function eventText(ev, milestones) {
  return issueEventText(ev, milestones);
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
  // Repo milestone set for the sidebar picker + title display (02 §3.2
  // `milestones:{o}/{r}`, 30 s TTL; the page owns the cache per the
  // MilestonePicker contract — the component only renders props).
  const [getMilestoneSet] = useData(() => `milestones:${ctx.full}`, () => ctx.repoClient.milestones.list(), TTL.milestones);
  const allMilestones = () => getMilestoneSet()?.milestones ?? [];
  const [getOlder, setOlder] = createSignal(false);
  // Older event windows accumulate here (the view holds the newest page;
  // both are newest-first, so concatenation stays ordered).
  const [getExtra, setExtra] = createSignal([]);
  const [getExtraMore, setExtraMore] = createSignal(undefined);
  // Sidebar mutation guards are per-issue state (see the navigation reset
  // below); declared here so the reset effect never reads them early.
  const [getLabelBusy, setLabelBusy] = createSignal(new Set());
  const [getMilestoneBusy, setMilestoneBusy] = createSignal(false);
  // Reaction guards are per-issue state too (#146): seqs are per-issue
  // event seqs, so a `seq:content` key busy on the previous issue must
  // never disable (or swallow clicks on) the new issue's chips. Declared
  // here for the same never-read-early reason as the sidebar guards.
  const [getBusy, setBusy] = createSignal(new Set());
  const busyKey = (seq, content) => `${seq}:${content}`;
  const isBusy = (seq, content) => getBusy().has(busyKey(seq, content));
  createEffect(() => {
    num(); // reset accumulation when navigating between issues
    setExtra([]);
    setExtraMore(undefined);
    // Per-issue mutation state must not leak across issues (#143): the
    // route reuses this component instance, so an in-flight sidebar
    // mutation on the previous issue would otherwise leave the new
    // issue's picker triple-guarded (busy early-return swallows the
    // click with no PATCH and no tray entry — the menu just closes).
    // Same for in-flight reactions (#146): the busy early-return fires
    // no mutation and posts no tray entry, so the chip just does
    // nothing. Clearing is safe — the tail's finally only deletes its
    // own key, a no-op on the fresh set.
    setLabelBusy(new Set());
    setMilestoneBusy(false);
    setBusy(new Set());
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

  // Comment/close tails are pinned to their issue (#146): same-route
  // navigation reuses this component, so the async tail must reconcile
  // the mutated issue even when the view has moved on — the same
  // pin/reconcile shape as the #143 sidebar mutations. Throwing still
  // skips the tail (CommentComposer keeps the text on failure).
  const comment = async (body) => {
    const n = num();
    const ck = key();
    await ctx.repoClient.issues.comment(n, body);
    if (num() === n) reload();
    else invalidate(ck);
  };

  const commentAndClose = async (body, reason) => {
    const n = num();
    const ck = key();
    // No atomic comment+close endpoint (PATCH takes state only): post the
    // body first when non-empty (GitHub semantics — empty body just
    // closes), then close with the EXPLICIT reason chosen in the composer
    // menu — the API defaults an omitted reason to completed, so the UI
    // always sends one (closePatch throws on anything else). Either step
    // throwing keeps the composer text (CommentComposer clears only on
    // success); after a comment-posted / close-failed split the plain
    // Close menu finishes the job.
    if (String(body ?? "").trim()) {
      await ctx.repoClient.issues.comment(n, body);
    }
    await ctx.repoClient.issues.patch(n, closePatch(reason));
    if (num() === n) reload();
    else invalidate(ck);
  };

  // Explicit-reason close (#109): the composer chooser supplies
  // completed|not_planned; closePatch refuses to build a body without one.
  const close = async (reason) => {
    const n = num();
    const ck = key();
    await ctx.repoClient.issues.patch(n, closePatch(reason));
    if (num() === n) reload();
    else invalidate(ck);
  };

  // One in-flight reaction mutation per (seq, content): the plus-menu
  // and the chips share these keys, so a double-click (or Enter-repeat)
  // can never double-fire — and the server dedups sequential duplicate
  // adds per (actor, target, content) anyway (02 §8). Buttons disable
  // while their key is busy; both are native <button>s (keyboard free)
  // with a theme-agnostic disabled treatment. The guard set itself is
  // declared up top (per-issue state, reset on navigation).
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
  // when the mutation fails). Never bypasses the #41 ordering guard. The
  // cache key is the caller's pinned key (#146): the paint must name the
  // clicked issue even when the view navigates mid-flight.
  const bump = (ck, seq, content, delta) =>
    patchCached(ck, (view) => ({
      ...view,
      thread: {
        ...view.thread,
        reaction_summary: adjustSummary(view.thread?.reaction_summary, seq, content, delta),
      },
    }));

  // The target issue is pinned up front (#146): same-route navigation
  // reuses this component, so the PATCH and the reconcile name the
  // clicked issue even when the view has moved on — the same shape as
  // the #143 sidebar mutations.
  const react = (seq, content) =>
    withBusy(seq, content, async () => {
      const n = num();
      const ck = key();
      bump(ck, seq, content, +1);
      try {
        await ctx.repoClient.issues.reactions.add(n, { target_event_seq: seq, content });
      } catch (err) {
        reportError(err, "issue-react");
      }
      if (num() === n) reload();
      else invalidate(ck);
    });

  // Summary chips toggle (#36): remove when the clicker already reacted,
  // add when they did not. The summary carries no per-user state, so a
  // remove-404 falls back to add (GitHub semantics); any other remove
  // failure just reports — only a double failure reports, never a silent
  // no-op. The optimistic guess follows the same path (−1, then +2 on the
  // 404 fallback to undo the guess and apply the add). Pinned up front
  // like react above (#146).
  const toggleReaction = (seq, content) =>
    withBusy(seq, content, async () => {
      const n = num();
      const ck = key();
      bump(ck, seq, content, -1);
      try {
        await ctx.repoClient.issues.reactions.remove(n, seq, content);
      } catch (err) {
        if (err?.notFound || err?.status === 404) {
          bump(ck, seq, content, +2);
          try {
            await ctx.repoClient.issues.reactions.add(n, { target_event_seq: seq, content });
          } catch (addErr) {
            reportError(addErr, "issue-react");
          }
        } else {
          reportError(err, "issue-react");
        }
      }
      if (num() === n) reload();
      else invalidate(ck);
    });

  const patch = async (fields) => {
    const n = num();
    const ck = key();
    try {
      await ctx.repoClient.issues.patch(n, fields);
      if (num() === n) reload();
      else invalidate(ck);
    } catch (err) {
      reportError(err, "issue-patch");
    }
  };

  // Sidebar label toggle (#45): one PATCH with the full next set per
  // toggle (one event per 02 §7), optimistic patchCached paint + guarded
  // invalidate reconcile — the same bump pattern as the reaction chips
  // above. One in-flight mutation per label name: a double-click computes
  // the same array twice (idempotent), and the button disables while its
  // key is busy so the pair cannot happen from one client. The target
  // issue is pinned up front (#143): same-route navigation reuses this
  // component, so the async tail must reconcile the mutated issue even
  // when the view has moved on.
  const toggleLabelApply = async (name) => {
    const k = String(name).toLowerCase();
    if (getLabelBusy().has(k)) return;
    const n = num();
    const ck = key();
    setLabelBusy((prev) => new Set(prev).add(k));
    const next = toggleLabel(thread()?.labels ?? [], name);
    patchCached(ck, (view) => ({ ...view, thread: { ...view.thread, labels: next } }));
    try {
      await ctx.repoClient.issues.patch(n, { labels: next });
    } catch (err) {
      reportError(err, "issue-labels");
    }
    if (num() === n) reload();
    else invalidate(ck);
    setLabelBusy((prev) => {
      const nx = new Set(prev);
      nx.delete(k);
      return nx;
    });
  };

  // Sidebar milestone select (#119): one PATCH per select (one event
  // per 02 §7), optimistic patchCached paint + guarded invalidate
  // reconcile — the same bump pattern as the label toggle above. One
  // in-flight mutation at a time (the picker disables while busy), and
  // no-op selects skip the round trip entirely (milestonePatch returns
  // null). Clearing sends an explicit null — absent would mean "no
  // change" server-side. The target issue is pinned up front (#143):
  // same-route navigation reuses this component, so the async tail must
  // reconcile the mutated issue even when the view has moved on.
  const selectMilestone = async (id) => {
    if (getMilestoneBusy()) return;
    const n = num();
    const ck = key();
    const fields = milestonePatch(thread()?.milestone ?? null, id);
    if (!fields) return;
    setMilestoneBusy(true);
    patchCached(ck, (view) => ({ ...view, thread: { ...view.thread, milestone: fields.milestone } }));
    try {
      await ctx.repoClient.issues.patch(n, fields);
    } catch (err) {
      reportError(err, "issue-milestone");
    }
    if (num() === n) reload();
    else invalidate(ck);
    setMilestoneBusy(false);
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
              {/* Header block (#114): the bottom rule + padding set it apart
                  from the first timeline entry (ThreadTimeline rows are
                  divider-separated with first:pt-0, so without this the
                  byline blurred into the first comment). Number + title
                  left, state badge right — the badge text keeps the
                  existing state source of truth (open vs.
                  closedStateLabel(reason)); the h2 level is unchanged. */}
              <header class="mb-4 border-b border-zinc-200 pb-3 dark:border-zinc-800">
                <div class="flex flex-wrap items-start justify-between gap-2">
                  <h2 class="min-w-0 flex-1 text-lg font-semibold">
                    <span class="text-zinc-500 dark:text-zinc-400">#{t().num}</span> {t().title}
                  </h2>
                  <span class={t().state === "open" ? "chip chip-open mt-1 shrink-0" : "chip chip-closed mt-1 shrink-0"}>
                    {t().state === "open" ? "Open" : closedStateLabel(t().state_reason)}
                  </span>
                </div>
                <p class="mt-1 text-xs text-zinc-500 dark:text-zinc-400">
                  {t().author} opened <DateTime value={t().created_at} /> · {t().comment_count} comments
                </p>
              </header>
              <ThreadTimeline
                events={events()}
                textFor={(ev) => eventText(ev, allMilestones())}
                summaryFor={(ev) => {
                  // The ONLY reaction surface (#113): one row per comment
                  // in/near where its reactions appear — summary chips plus
                  // a "+" menu offering exactly the addable set
                  // (REACTIONS minus whatever the summary already shows;
                  // adding bumps the count optimistically, which drops the
                  // content from the menu). Chips toggle off (remove-404
                  // falls back to add); there is no always-visible picker
                  // row in the comment header anymore.
                  if (ev.type !== "opened" && ev.type !== "commented") return null;
                  const entries = summaryEntries(summary(), ev.seq);
                  const addable = addableReactions(summary(), ev.seq);
                  return (
                    <div class="reaction-row mt-2 flex flex-wrap items-center gap-1" role="group" aria-label={`reactions on comment ${ev.seq}`}>
                      <For each={entries}>
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
                      <ReactionMenu
                        seq={ev.seq}
                        addable={addable}
                        isBusy={(c) => isBusy(ev.seq, c)}
                        onAdd={(c) => react(ev.seq, c)}
                      />
                    </div>
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
                  closeChooser={t().state === "open"}
                  onClose={(reason) => (t().state === "open" ? close(reason) : patch({ state: "open" }))}
                  errorKey="issue-comment"
                  mentionId="mention-issue-comment"
                  mentionNames={thread()?.participants}
                  uploader={ctx.repoClient.attachments}
                />
              </Show>
            </>
          )}
        </Show>
      </div>
      <aside class="grid content-start gap-3">
        <Show when={thread()}>
          {(t) => (
            // One metadata container (#107): state lives in the header
            // above, so the sidebar holds labels/assignees/milestone in
            // divided sections — each value sits directly under its
            // header, so a "none" reads as that section's value.
            <section class="card divide-y divide-zinc-200 text-sm dark:divide-zinc-800" aria-label="Issue metadata">
              <div class="p-3">
                <div class="mb-1 flex items-center justify-between gap-2">
                  <span class="text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400">Labels</span>
                  <Show when={canTriage()}>
                    <LabelPicker all={allLabels()} applied={t().labels ?? []} busy={getLabelBusy()} onToggle={toggleLabelApply} />
                  </Show>
                </div>
                <div class="flex flex-wrap gap-1">
                  <For each={t().labels ?? []} fallback={<span class="muted text-xs">none</span>}>
                    {(l) => <LabelChip name={l} map={colorMap()} />}
                  </For>
                </div>
              </div>
              <div class="grid gap-1 p-3">
                <span class="text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400">Assignees</span>
                <div class="flex flex-wrap gap-1">
                  <For each={t().assignees ?? []} fallback={<span class="muted text-xs">none</span>}>
                    {(a) => <span>{a}</span>}
                  </For>
                </div>
              </div>
              <div class="grid gap-1 p-3">
                <div class="mb-1 flex items-center justify-between gap-2">
                  <span class="text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400">Milestone</span>
                  <Show when={canTriage()}>
                    <MilestonePicker milestones={allMilestones()} current={t().milestone ?? null} busy={getMilestoneBusy()} onSelect={selectMilestone} />
                  </Show>
                </div>
                <div class="flex flex-wrap gap-1">
                  <Show when={t().milestone} fallback={<span class="muted text-xs">none</span>}>
                    {(m) => <span>{milestoneTitle(allMilestones(), m())}</span>}
                  </Show>
                </div>
              </div>
            </section>
          )}
        </Show>
      </aside>
    </div>
  );
}
