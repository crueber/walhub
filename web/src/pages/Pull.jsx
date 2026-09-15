// web/src/pages/Pull.jsx — route "/:owner/:name/pull/:num" (03 §9 + 04 §8):
// the PR conversation (ThreadTimeline, CommentComposer, MergeBox with the
// 08 §2 state machine + task attach + double-submit guard) plus the
// code-review surface: review summary bar, reviews list, reviewers panel,
// finish-review modal, and the diff review surface (line anchors via
// lib/diff.js anchorContextSha — the ONLY §4 hash implementation
// client-side). `pull`/`review`/`thread`/`check` frames ride the ONE repo
// collaboration stream (08 §4) and invalidate coalesced keys; the merge
// box recomputes on every header fetch.

import { createSignal, For, Show } from "solid-js";
import { A, useLocation, useNavigate, useParams } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import repos from "../../sdk/src/index.js";
import { useData, invalidate, invalidatePullLists, reportError } from "../lib/data.js";
import { CheckPill, ContextRows, ZeroChecksBlock } from "./Checks.jsx";
import { isZeroChecks, requiredCheckBlockers } from "../lib/checks-empty.js";
import { parsePatchFiles, normalizePatchBody } from "../lib/diff.js";
import {
  buildAnchor,
  anchorLabel,
  freshnessOf,
  sortThreadsForIndex,
} from "../lib/review-anchor.js";
import { lineClass } from "../components/DiffTable.jsx";
import ThreadTimeline from "../components/ThreadTimeline.jsx";
import DateTime from "../components/DateTime.jsx";
import CommentComposer from "../components/CommentComposer.jsx";
import MergeBox from "../components/MergeBox.jsx";
import { useCollabStream } from "../components/collab.jsx";
import { useRole, roleAtLeast } from "../components/perms.jsx";
import { onSubmitKeys } from "../lib/submitKeys.js";
import { anonWriteTarget, isAnonymousViewer } from "../lib/writeGate.js";
import { pullBadgeView, pullCloseVisibility, pullEventText, reviewVerdictLabel } from "../lib/pull-state.js";
import { renderBody } from "../lib/render-md.js";

/** PR description block (Forgejo #521, the issue-page first-comment
 *  treatment): the live pr.body editable view with an author/date byline in
 *  the timeline comment-entry idiom (muted xs header + markdown-body — the
 *  same classes ThreadTimeline uses, unboxed). Hidden when the PR has no
 *  description; the timeline's opened row is a one-line system row (see
 *  pullEventText), so the description renders exactly once and never stale.
 *  The modal draft preview in FinishReview stays plain text by design. */
function PRDescription(props) {
  return (
    <Show when={String(props.body ?? "").trim()}>
      <article aria-label="Pull request description" class="mb-3 border-b border-zinc-200 pb-3 dark:border-zinc-800">
        <p class="mb-1 text-xs text-zinc-500 dark:text-zinc-400">
          <span class="font-medium text-zinc-700 dark:text-zinc-200">{props.actor}</span>
          {" · "}
          <DateTime value={props.at} />
        </p>
        <div class="markdown-body" innerHTML={renderBody(props.body ?? "", props.mdCtx)} />
      </article>
    </Show>
  );
}

function mergeableText(m) {
  if (!m) return "unknown";
  switch (m.state) {
    case "clean":
      return "mergeable";
    case "behind":
      return "mergeable (behind)";
    case "dirty":
      return `conflicts: ${(m.conflicts ?? []).join(", ")}`;
    case "up_to_date":
      return "already merged";
    default:
      return "checking…";
  }
}

/** Review-verdict chip (Forgejo #545): the ONE state→chip mapping for
 *  every verdict on the PR page (summary-bar decision, reviewer states,
 *  review cards). APPROVED rides the emerald chip-open, CHANGES_REQUESTED
 *  the red chip-closed, COMMENTED + dismissed/requested the neutral
 *  chip-neutral (ui.css), REVIEW_REQUIRED/unknown the amber chip-draft
 *  (attention needed). No per-callsite bg overrides — colors live in
 *  ui.css composition (F2). */
function reviewVerdictChip(state) {
  switch (state) {
    case "APPROVED":
      return "chip chip-open";
    case "CHANGES_REQUESTED":
      return "chip chip-closed";
    case "COMMENTED":
      return "chip chip-neutral";
    default:
      return "chip chip-draft";
  }
}

/** Review summary bar (§8): decision badge + reviewer chips (stale derived
 *  client-side from commit_sha != head) + requested chips + unresolved.
 *  Forgejo #531: a section VALUE inside the sidebar's one divide-y panel
 *  (the Issue.jsx:549 idiom) — no .card wrapper, no heading; the parent
 *  section owns the uppercase micro-label. */
function ReviewSummaryBar(props) {
  const summary = () => props.summary;
  const latest = () => Object.entries(summary()?.latest ?? {});
  return (
    <>
      <div class="mb-2 flex flex-wrap items-center gap-2">
        <span class={reviewVerdictChip(summary()?.decision ?? "REVIEW_REQUIRED")}>
          {reviewVerdictLabel(summary()?.decision ?? "REVIEW_REQUIRED")}
        </span>
        <span class="text-xs text-zinc-500 dark:text-zinc-400">
          {summary()?.approvals ?? 0} approvals · {summary()?.threads_unresolved ?? 0} unresolved threads
        </span>
      </div>
      <div class="flex flex-wrap gap-1.5">
        <For each={latest()} fallback={<span class="text-xs text-zinc-500 dark:text-zinc-400">no reviews yet</span>}>
          {([who, r]) => (
            <span class={reviewVerdictChip(r.state)} title={`${r.state} @ ${String(r.commit_sha ?? "").slice(0, 12)}`}>
              {who} · {reviewVerdictLabel(r.state)}
              <Show when={r.state === "APPROVED" && r.commit_sha !== props.head}>
                <span class="ml-1 font-semibold text-amber-600 dark:text-amber-400">(stale)</span>
              </Show>
            </span>
          )}
        </For>
        <For each={summary()?.requested ?? []}>
          {(who) => <span class="chip chip-neutral opacity-70" title="requested reviewer">{who} · requested</span>}
        </For>
      </div>
    </>
  );
}

/** Reviews list (Forgejo #545, padded Forgejo #554): ONE .card panel with
 *  the card-header title (the #521/#531 conversation-column idiom) holding
 *  an unstyled flat list — the old wrapper class carried zero CSS rules
 *  and each item was itself a .card, i.e. bordered chrome inside bordered
 *  chrome. The panel composes the sibling card padding (card p-3, the
 *  CommentComposer idiom) so review rows and the empty state sit inside
 *  the border instead of touching its edges. Items are plain divider rows
 *  (first row flush, like the flat PR-list rows); each reads author + chip
 *  verdict + timestamp in the .card-meta language, with the stale marker
 *  and dismiss affordance unchanged. */
function ReviewsList(props) {
  const submitDismiss = async (seq) => {
    const reason = window.prompt("Dismissal reason (recorded on the compensating event):", "stale");
    if (!reason || !reason.trim()) return;
    try {
      await props.client.pulls.reviews.dismiss(props.num, seq, { reason: reason.trim() });
      props.reload();
    } catch (err) {
      reportError(err, "review-dismiss");
    }
  };
  return (
    <div class="card p-3" aria-label="Reviews">
      <h2 class="card-header">Reviews</h2>
      <ul class="divide-y divide-zinc-200 dark:divide-zinc-800">
        <For each={props.reviews ?? []} fallback={<li class="text-sm text-zinc-500 dark:text-zinc-400">No reviews yet.</li>}>
          {(rv) => (
            <li class="py-2 first:pt-0 last:pb-0">
              <div class="card-meta">
                <span>{rv.by}</span>
                {" · "}
                <span class={rv.kind === "review_dismissed" ? "chip chip-neutral" : reviewVerdictChip(rv.state ?? rv.kind)}>{rv.kind === "review_dismissed" ? `dismissed #${rv.dismisses}` : reviewVerdictLabel(rv.state)}</span>
                {" · "}
                <span><DateTime value={rv.at} /></span>
              </div>
              <Show when={rv.kind === "review_dismissed"}>
                <p class="text-sm italic">dismissed review #{rv.dismisses}: {rv.reason}</p>
              </Show>
              <Show when={rv.body}>
                {/* Review bodies render markdown like the timeline (Forgejo
                    #521) — same renderBody + repo mdCtx, not raw pre-wrap. */}
                <div class="markdown-body" innerHTML={renderBody(rv.body ?? "", props.mdCtx)} />
              </Show>
              <p class="mt-1 text-xs text-zinc-500 dark:text-zinc-400">
                {(rv.commit_sha ?? "").slice(0, 12)}
                <Show when={rv.commit_sha && rv.commit_sha !== props.head}>
                  <span class="ml-1 font-semibold text-amber-600 dark:text-amber-400">stale</span>
                </Show>
                <Show when={props.canDismiss && rv.kind === "review"}>
                  <button type="button" class="link ml-2" onClick={() => submitDismiss(rv.seq)}>
                    dismiss
                  </button>
                </Show>
              </p>
            </li>
          )}
        </For>
      </ul>
    </div>
  );
}

/** Reviewers panel: picker fed by review-suggest (150 ms debounce,
 *  abort-on-keystroke per the §2.6 ref-picker pattern). Forgejo #531: a
 *  section VALUE inside the sidebar's one divide-y panel — no .card
 *  wrapper, no heading; the parent section owns the micro-label. */
function ReviewersPanel(props) {
  const [getQuery, setQuery] = createSignal("");
  const [getOptions, setOptions] = createSignal([]);
  const [getOpen, setOpen] = createSignal(false);
  let debounce = 0;
  let ctrl = null;

  const search = (q) => {
    setQuery(q);
    clearTimeout(debounce);
    if (ctrl) ctrl.abort();
    debounce = setTimeout(async () => {
      ctrl = new AbortController();
      try {
        const { suggestions } = await props.client.pulls.suggest(props.num, q, { signal: ctrl.signal });
        setOptions(suggestions ?? []);
        setOpen(true);
      } catch (err) {
        if (String(err?.message ?? err).toLowerCase().includes("abort")) return;
        reportError(err, "review-suggest");
      }
    }, 150);
  };

  const add = async (who) => {
    setOpen(false);
    try {
      await props.client.pulls.requests.add(props.num, [who]);
      setQuery("");
      setOptions([]);
      props.reload();
    } catch (err) {
      reportError(err, "review-request");
    }
  };

  const remove = async (who) => {
    try {
      await props.client.pulls.requests.remove(props.num, [who]);
      props.reload();
    } catch (err) {
      reportError(err, "review-request");
    }
  };

  return (
    <>
      <Show when={props.canEdit} fallback={
        <p class="text-xs text-zinc-500 dark:text-zinc-400">requesting reviewers needs the write role</p>
      }>
      <div class="relative">
        <input
          class="input w-full"
          placeholder="request a reviewer…"
          value={getQuery()}
          onInput={(e) => search(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Escape") setOpen(false);
          }}
          aria-label="Request a reviewer"
          aria-expanded={getOpen()}
        />
        <Show when={getOpen() && getOptions().length > 0}>
          <ul class="ref-list ref-drop card scroll-slim absolute z-10 mt-1 max-h-48 w-full overflow-y-auto p-1 shadow-lg">
            <For each={getOptions()}>
              {(who) => (
                <li>
                  <button type="button" class="ref-item flex w-full items-center rounded px-2 py-1 text-left text-sm text-zinc-800 hover:bg-zinc-100 dark:text-zinc-200 dark:hover:bg-zinc-800" onClick={() => add(who)}>
                    {who}
                  </button>
                </li>
              )}
            </For>
          </ul>
        </Show>
      </div>
      <ul class="mt-2 flex flex-wrap gap-1.5">
        <For each={props.requested ?? []} fallback={<li class="text-xs text-zinc-500 dark:text-zinc-400">none requested</li>}>
          {(who) => (
            <li class="pill">
              {who}
              <Show when={props.canEdit}>
                <button type="button" class="link ml-1" onClick={() => remove(who)} aria-label={`Remove ${who}`}>
                  ×
                </button>
              </Show>
            </li>
          )}
        </For>
      </ul>
      </Show>
    </>
  );
}

/** Number the display lines of one parsed hunk (hunk.newStart/oldStart
 *  counters over the parsed rows). Returns [{line, t, text, oldNo, newNo}]. */
function numberedLines(hunk) {
  let o = hunk.oldStart;
  let n = hunk.newStart;
  return (hunk.lines ?? []).map((l, i) => {
    const row = { line: l, idx: i, oldNo: null, newNo: null };
    if (l.t === " ") {
      row.oldNo = o++;
      row.newNo = n++;
    } else if (l.t === "-") {
      row.oldNo = o++;
    } else if (l.t === "+") {
      row.newNo = n++;
    }
    return row;
  });
}

/** Derive freshness for one thread against the CURRENT diff: recompute the
 *  §4 hash from the diff — mismatch (or an unlocatable anchor) renders the
 *  thread outdated (collapsed, original line shown), never relocated.
 *  Delegates to freshnessOf (lib/review-anchor.js): single-line anchors hash
 *  {start: idx, count: 1} exactly as before (pinned vectors hold); range
 *  anchors hash their own span. */
function threadFreshness(thread, files) {
  const r = freshnessOf(thread.anchor, files);
  if (r.hunkIndex < 0) return { fresh: false, hunk: null, idx: -1 };
  const f = (files ?? []).find((x) => x.path === thread.anchor?.path);
  return { fresh: r.fresh, hunk: f?.hunks?.[r.hunkIndex] ?? null, idx: r.startIdx };
}

/** One file's hunks with line comment affordances + inline thread cards
 *  (unresolved-first; unlocatable/drifted anchors render once, collapsed,
 *  at the file end — never relocated). Added/removed rows carry the shared
 *  lineClass() background on the row div (issue #544 — the same .diff-add /
 *  .diff-del tokens DiffBody uses, so the conversation diff matches the
 *  Files tab and commit diffs in both themes). */
function DiffFile(props) {
  // Inline review composers (Forgejo #560, refining #546/#555): the "+"
  // affordance lives in the left gutter (one discrete, always-visible
  // button per commentable line — no hover dependency, so the target is
  // stable and never shifts row content), and clicking anywhere on a diff
  // row stages a composer immediately below THAT line (never one <Show>
  // at the file end). Drafts are a keyed Map (`${hunkIdx}:${rowIdx}`) so
  // unlimited composers coexist per file with text preserved — each
  // CommentComposer instance stays mounted under its own row and submits
  // independently (staging into onStage, closing only its own key) in any
  // order. Anchor construction is unchanged: stageLine builds via
  // lib/review-anchor.js and submit hands {anchor, body} to onStage (the
  // finish-review modal); cancel drops only that draft. Plain click stages
  // one line; Shift+click on another "+" (or row) in the same file + side
  // + hunk extends a range (the DiffTable clamp convention), staged as
  // new_lines/old_lines > 1. The #502 gate covers all three entry points
  // (gutter button, row click, composer render — canComment !== false).
  // Row clicks never fight interactive content: the gutter button stops
  // propagation (no double-stage with the row handler) and the row handler
  // ignores clicks landing on buttons/links/inputs (closest() check), so
  // clicks inside an open composer or thread card — siblings BELOW the
  // row, never inside it — spawn nothing. No touch handlers: a tap
  // arrives as click and the permanent gutter buttons are touch-visible
  // by construction, with focus-visible affordances for keyboard users.
  const [getDrafts, setDrafts] = createSignal(new Map());
  const [getLast, setLast] = createSignal(null);

  const draftKey = (hunkIdx, rowIdx) => `${hunkIdx}:${rowIdx}`;
  const draftAt = (hunkIdx, rowIdx) => getDrafts().get(draftKey(hunkIdx, rowIdx)) ?? null;
  const closeDraft = (key) => {
    setDrafts((prev) => {
      if (!prev.has(key)) return prev;
      const next = new Map(prev);
      next.delete(key);
      return next;
    });
  };
  // Dismissal refocus (Forgejo #566, the SplitCloseMenu convention in
  // CommentComposer.jsx:38-43): Escape closes the composer and returns
  // focus to the gutter "+" that staged it. Gutter buttons register here
  // by draft key; focus() never fires click, so refocus cannot re-stage
  // (no toggle-fight). A stale entry is harmless — focus() on a detached
  // node is a no-op.
  const triggerRefs = new Map();
  const refocusTrigger = (key) => triggerRefs.get(key)?.focus?.();

  const stageLine = (file, hunk, hunkIdx, row, rowIdx, ev) => {
    const isNew = row.line.t !== "-";
    const side = isNew ? "NEW" : "OLD";
    const no = isNew ? row.newNo : row.oldNo;
    if (no == null) return;
    const key = draftKey(hunkIdx, rowIdx);
    const last = getLast();
    if (ev?.shiftKey && last && last.file === file.path && last.side === side && last.hunkIdx === hunkIdx) {
      const anchor = buildAnchor({
        file,
        hunk,
        side,
        startNo: Math.min(last.no, no),
        endNo: Math.max(last.no, no),
        head: props.head,
      });
      if (anchor) {
        setDrafts((prev) => new Map(prev).set(key, { anchor }));
        return;
      }
    }
    const anchor = buildAnchor({ file, hunk, side, startNo: no, endNo: no, head: props.head });
    if (!anchor) return;
    setLast({ file: file.path, side, no, hunkIdx });
    setDrafts((prev) => new Map(prev).set(key, { anchor }));
  };

  // Row click stages a composer below that line. Guards: the #502 gate,
  // plus the isolation check — clicks on interactive content (the gutter
  // "+" stops propagation before reaching here; links/buttons/inputs any
  // other way) never stage.
  const onRowClick = (file, hunk, hunkIdx, row, rowIdx, ev) => {
    if (props.canComment === false) return;
    if (ev?.target?.closest?.("button, a, input, textarea, select, [data-no-row-comment]")) return;
    stageLine(file, hunk, hunkIdx, row, rowIdx, ev);
  };

  // Placement is derived fresh every render: locate each thread's anchor
  // line in the CURRENT file hunks (inline, unresolved-first); anchors
  // that no longer locate fall to the file end, collapsed (outdated).
  const placement = () => {
    const mine = (props.threads ?? []).filter((t) => (t.anchor?.path ?? "") === props.file.path);
    const byKey = new Map();
    const unplaced = [];
    for (const t of mine) {
      const a = t.anchor ?? {};
      const no = a.side === "NEW" ? a.new_start : a.old_start;
      let found = null;
      (props.file.hunks ?? []).forEach((hunk, hi) => {
        if (found) return;
        numberedLines(hunk).forEach((row, ri) => {
          if (found) return;
          if ((row.newNo === no && a.side === "NEW") || (row.oldNo === no && a.side === "OLD")) {
            found = `${hi}:${ri}`;
          }
        });
      });
      const f = threadFreshness(t, props.files);
      t._fresh = f.fresh;
      if (found) {
        if (!byKey.has(found)) byKey.set(found, []);
        byKey.get(found).push(t);
      } else {
        unplaced.push(t);
      }
    }
    for (const list of byKey.values()) list.sort((x, y) => Number(x.resolved) - Number(y.resolved));
    unplaced.sort((x, y) => Number(x.resolved) - Number(y.resolved));
    return { byKey, unplaced };
  };

  const threadsAt = (hi, ri) => placement().byKey.get(`${hi}:${ri}`) ?? [];

  return (
    <div class="card" aria-label={`Diff ${props.file.path}`}>
      <h3 class="mb-2 font-mono text-sm font-semibold">{props.file.path}</h3>
      <For each={props.file.hunks ?? []}>
        {(hunk, hi) => {
          const rows = numberedLines(hunk);
          return (
            <div class="mb-3 overflow-x-auto">
              <div class="font-mono text-xs text-zinc-500 dark:text-zinc-400">
                @@ -{hunk.oldStart},{hunk.oldLines} +{hunk.newStart},{hunk.newLines} @@
              </div>
              <For each={rows}>
                {(row, ri) => (
                  <div>
                    <div
                      class={`group flex font-mono text-xs ${lineClass(row.line.t)}`}
                      onClick={(ev) => onRowClick(props.file, hunk, hi(), row, ri(), ev)}
                    >
                      {/* Gutter comment trigger (Forgejo #560): one discrete
                          "+" per commentable line in the left gutter, always
                          rendered (no hover dependency — no hidden /
                          group-hover indirection), mirroring the Files-tab
                          DiffTable gutter conventions (gutter cell +
                          line-labelling a11y). #502 gate: anonymous viewers
                          get no trigger. stopPropagation so the gutter click
                          never double-stages through the row handler. */}
                      <span class="w-6 shrink-0 select-none text-center">
                        <Show when={props.canComment !== false}>
                          <button
                            type="button"
                            class="shrink-0 px-1 text-emerald-600 hover:text-emerald-700 focus-visible:outline focus-visible:outline-2 focus-visible:outline-emerald-500 dark:text-emerald-400 dark:hover:text-emerald-300"
                            ref={(el) => el && triggerRefs.set(draftKey(hi(), ri()), el)}
                            onClick={(ev) => {
                              ev.stopPropagation();
                              stageLine(props.file, hunk, hi(), row, ri(), ev);
                            }}
                            aria-label={`Comment on line ${row.newNo ?? row.oldNo} in ${props.file.path}`}
                            title="Comment on this line (Shift+click another + for a range)"
                          >
                            +
                          </button>
                        </Show>
                      </span>
                      <span class="w-10 shrink-0 select-none text-right text-zinc-400">{row.oldNo ?? ""}</span>
                      <span class="w-10 shrink-0 select-none text-right text-zinc-400">{row.newNo ?? ""}</span>
                      <span class="w-4 shrink-0 select-none">{row.line.t}</span>
                      <span class="whitespace-pre">{row.line.text}</span>
                    </div>
                    {/* Inline staged draft (#560): the shared
                        CommentComposer rendered immediately below its row
                        (never one <Show> at the file end); each keyed draft
                        submits independently into the finish-review modal.
                        #502 gate: no composers for anonymous viewers. */}
                    <Show when={props.canComment !== false && draftAt(hi(), ri())}>
                      {(d) => (
                        // Dismissable draft composer (Forgejo #566): Cancel
                        // wears the small secondary .btn treatment (the
                        // canonical button idiom, guideline §2 Controls —
                        // readable in both themes, unlike the unstyled
                        // .link); Escape anywhere inside the panel (the
                        // CommentComposer textarea bubbles its keydown up
                        // here — no document listener, so no onCleanup)
                        // drops ONLY this keyed draft and refocuses the
                        // gutter "+" that staged it. Dismissal calls
                        // nothing: no onStage, no POST.
                        <div
                          class="ml-14 mt-1 rounded border border-zinc-200 p-2 dark:border-zinc-700"
                          aria-label={`Draft comment on ${anchorLabel(d().anchor)}`}
                          onKeyDown={(e) => {
                            if (e.key !== "Escape") return;
                            e.preventDefault();
                            e.stopPropagation();
                            const key = draftKey(hi(), ri());
                            closeDraft(key);
                            refocusTrigger(key);
                          }}
                        >
                          <p class="mb-1 text-xs text-zinc-500 dark:text-zinc-400">
                            commenting on <span class="font-mono">{anchorLabel(d().anchor)}</span>
                            <button type="button" class="btn ml-2 px-2 py-0.5 text-xs" onClick={() => closeDraft(draftKey(hi(), ri()))}>
                              Cancel
                            </button>
                          </p>
                          <CommentComposer
                            onSubmit={async (body) => {
                              props.onStage({ anchor: d().anchor, body });
                              closeDraft(draftKey(hi(), ri()));
                            }}
                            submitLabel="Stage comment"
                            placeholder={`Comment on ${anchorLabel(d().anchor)}… (staged into the finish-review modal)`}
                            errorKey="line-comment-stage"
                            label={`Comment on ${anchorLabel(d().anchor)}`}
                          />
                        </div>
                      )}
                    </Show>
                    <For each={threadsAt(hi(), ri())}>
                      {(t) => <ThreadCard thread={t} client={props.client} num={props.num} reload={props.reload} canResolve={props.canResolve} mdCtx={props.mdCtx} flashTid={props.flashTid} />}
                    </For>
                  </div>
                )}
              </For>
            </div>
          );
        }}
      </For>
      {/* Anchors that no longer locate (drifted head, deleted lines)
          render once, collapsed, at the file end — never relocated. */}
      <For each={placement().unplaced}>
        {(t) => <ThreadCard thread={t} client={props.client} num={props.num} reload={props.reload} canResolve={props.canResolve} mdCtx={props.mdCtx} flashTid={props.flashTid} />}
      </For>
    </div>
  );
}

/** One thread card: comments, resolve toggle, outdated collapse. The card
 *  carries id `thread-<tid>` so the jump-to-comments index can scroll to
 *  it; a flashed (just-jumped-to) card draws an emerald outline and
 *  expands, so the target reads in both themes. */
function ThreadCard(props) {
  const t = () => props.thread;
  const [getBody, setBody] = createSignal("");
  const [getOpen, setOpen] = createSignal(!t().resolved);
  const flashed = () => props.flashTid?.() === t().tid;
  const open = () => getOpen() || flashed();

  const comment = async (e) => {
    e.preventDefault();
    if (!getBody().trim()) return;
    try {
      await props.client.pulls.threads.comment(props.num, t().tid, getBody().trim());
      setBody("");
      props.reload();
    } catch (err) {
      reportError(err, "thread-comment");
    }
  };

  const toggle = async () => {
    try {
      if (t().resolved) await props.client.pulls.threads.unresolve(props.num, t().tid);
      else await props.client.pulls.threads.resolve(props.num, t().tid);
      props.reload();
    } catch (err) {
      reportError(err, "thread-resolve");
    }
  };

  return (
    <div
      id={`thread-${t().tid}`}
      class={`ml-14 mt-1 rounded border border-zinc-200 p-2 dark:border-zinc-700${flashed() ? " outline outline-2 outline-emerald-500" : ""}`}
      aria-label={`Thread ${t().tid}`}
    >
      <div class="mb-1 flex flex-wrap items-center gap-2 text-xs">
        <span class="font-mono text-zinc-500 dark:text-zinc-400">{t().tid}</span>
        <Show when={t()._fresh === false}>
          <span class="pill bg-zinc-200 text-zinc-700 dark:bg-zinc-700 dark:text-zinc-300" title="anchor drifted past the current diff">
            outdated · {(t().anchor?.path ?? "")}:{(t().anchor?.side === "NEW" ? t().anchor?.new_start : t().anchor?.old_start) ?? ""}
          </span>
        </Show>
        <Show when={t().resolved}>
          <span class="pill">resolved{t().resolved_by ? ` by ${t().resolved_by}` : ""}</span>
        </Show>
        <Show when={props.canResolve}>
          <button type="button" class="link" onClick={toggle}>
            {t().resolved ? "unresolve" : "resolve"}
          </button>
        </Show>
        <button type="button" class="link" onClick={() => setOpen(!getOpen())}>
          {getOpen() ? "collapse" : "expand"}
        </button>
      </div>
      <Show when={open()}>
        <ThreadComments tid={t().tid} client={props.client} num={props.num} mdCtx={props.mdCtx} />
        <form class="mt-1 flex gap-2" onSubmit={comment}>
          <input class="input flex-1" value={getBody()} onInput={(e) => setBody(e.target.value)} placeholder="reply…" aria-label="Reply" />
          <button type="submit" class="btn px-2 py-0.5">
            reply
          </button>
        </form>
      </Show>
    </div>
  );
}

/** Jump-to-comments index (Forgejo #546, padded Forgejo #557): one entry
 *  per anchored thread — path:line (or path:start-end for ranges) +
 *  resolved/outdated state — atop the conversation. Clicking scrolls to
 *  the inline ThreadCard and flashes it. The panel composes the sibling
 *  card padding (card p-3, the ReviewsList #554 / CommentComposer idiom)
 *  so the pill entries sit inside the border instead of touching its
 *  edges; the mb-4 conversation spacing stays. Entries reflect the same
 *  placement truth the cards render: drifted/outdated anchors are marked
 *  and jump to their collapsed card at the file end (never to a line
 *  that no longer exists); threads whose file left the current diff
 *  render marked with no jump target. Order is unresolved-first,
 *  matching the inline cards. */
function ThreadIndex(props) {
  const entries = () =>
    sortThreadsForIndex(props.threads ?? []).map((t) => {
      const filePresent = (props.files ?? []).some((f) => f?.path === t.anchor?.path);
      const { fresh } = freshnessOf(t.anchor, props.files ?? []);
      return { t, fresh, target: filePresent ? t.tid : null };
    });

  return (
    <Show when={(props.threads ?? []).length > 0}>
      <nav class="card mb-4 p-3" aria-label="Comments index">
        <h2 class="card-header">
          Comments ({(props.threads ?? []).length})
        </h2>
        <ul class="flex flex-wrap gap-1.5">
          <For each={entries()}>
            {(e) => (
              <li>
                <Show
                  when={e.target}
                  fallback={
                    <span
                      class="pill opacity-60"
                      title={`${anchorLabel(e.t.anchor)} is not in the current diff`}
                    >
                      {anchorLabel(e.t.anchor)} · gone
                    </span>
                  }
                >
                  <button
                    type="button"
                    class="pill cursor-pointer"
                    onClick={() => props.onJump(e.target)}
                    title={`${anchorLabel(e.t.anchor)}${e.t.resolved ? " · resolved" : ""}${e.fresh ? "" : " · outdated"}`}
                  >
                    <span class="font-mono">{anchorLabel(e.t.anchor)}</span>
                    <Show when={e.t.resolved}>
                      <span> · resolved</span>
                    </Show>
                    <Show when={!e.fresh}>
                      <span> · outdated</span>
                    </Show>
                  </button>
                </Show>
              </li>
            )}
          </For>
        </ul>
      </nav>
    </Show>
  );
}

/** Lazily loaded comments for one thread card. */
function ThreadComments(props) {
  const key = () => `thread:${props.num}:${props.tid}`;
  const [getView] = useData(key, () => props.client.pulls.threads.get(props.num, props.tid));
  return (
    <ul class="space-y-1">
      <For each={getView()?.comments ?? []} fallback={<li class="text-xs text-zinc-500 dark:text-zinc-400">loading…</li>}>
        {(c) => (
          <li class="text-xs">
            <span class="font-semibold">{c.by}</span> <span class="text-zinc-500 dark:text-zinc-400"><DateTime value={c.at} /></span>
            {/* Thread comments render markdown like the timeline (Forgejo
                #521) — same renderBody + repo mdCtx, not raw pre-wrap. */}
            <div class="markdown-body" innerHTML={renderBody(c.body ?? "", props.mdCtx)} />
          </li>
        )}
      </For>
    </ul>
  );
}

/** Finish-review modal (padded Forgejo #557): pending staged line
 *  comments + top-level body + verdict, one POST reviews (threads open
 *  atomically with the review). The form composes the sibling card
 *  padding (card p-3, the ReviewsList #554 / CommentComposer idiom) so
 *  the staged list, fields, and buttons sit inside the border instead of
 *  touching its edges. */
function FinishReview(props) {
  const [getBody, setBody] = createSignal("");
  const [getVerdict, setVerdict] = createSignal("COMMENTED");
  const [getBusy, setBusy] = createSignal(false);

  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      await props.client.pulls.reviews.submit(props.num, {
        state: getVerdict(),
        body: getBody(),
        commit_sha: props.head,
        threads: props.pending,
      });
      props.onDone();
    } catch (err) {
      reportError(err, "review-submit");
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="card p-3" aria-label="Finish review" onSubmit={submit}>
      <h2 class="card-header">Finish review</h2>
      <Show when={(props.pending ?? []).length > 0} fallback={<p class="text-xs text-zinc-500 dark:text-zinc-400">no staged line comments</p>}>
        <ul class="mb-2 space-y-1">
          <For each={props.pending}>
            {(p, i) => (
              <li class="flex items-start gap-2 text-xs">
                <span class="font-mono">
                  {anchorLabel(p.anchor)}
                </span>
                <span class="flex-1 whitespace-pre-wrap">{p.body}</span>
                <button type="button" class="link" onClick={() => props.onUnstage(i())}>
                  remove
                </button>
              </li>
            )}
          </For>
        </ul>
      </Show>
      {/* Forgejo #545: the #479 canonical form shape (IssueNew.jsx) —
          label.grid.gap-1 + text-sm font-medium span + .input control,
          help scoped to its fields via id + aria-describedby. */}
      <label class="grid gap-1">
        <span class="text-sm font-medium">Body (optional)</span>
        <textarea id="finish-review-body" class="input w-full" value={getBody()} onInput={(e) => setBody(e.target.value)} rows="3" onKeyDown={onSubmitKeys(submit, { isBusy: () => getBusy() })} aria-describedby="finish-review-head-help" />
      </label>
      <label class="grid gap-1 mt-2">
        <span class="text-sm font-medium">Verdict</span>
        <select id="finish-review-verdict" class="input w-full" value={getVerdict()} onInput={(e) => setVerdict(e.target.value)} aria-describedby="finish-review-head-help">
          <option value="COMMENTED">comment</option>
          <option value="APPROVED">approve</option>
          <option value="CHANGES_REQUESTED">request changes</option>
        </select>
      </label>
      <p id="finish-review-head-help" class="muted mt-1 text-xs">reviewing {(props.head ?? "").slice(0, 12)}</p>
      <div class="mt-2 flex gap-2">
        <button type="submit" class="btn primary px-3 py-1" disabled={getBusy()}>
          {getBusy() ? "submitting…" : "submit review"}
        </button>
        <button type="button" class="btn px-3 py-1" onClick={props.onDone}>
          cancel
        </button>
      </div>
    </form>
  );
}

export default function Pull() {
  const ctx = useRepo();
  const params = useParams();
  const num = () => params.num;
  const key = () => `pull:${ctx.full}:${num()}`;
  const reviewsKey = () => `reviews:${ctx.full}:${num()}`;
  const threadsKey = () => `threads:${ctx.full}:${num()}`;
  const requestsKey = () => `requests:${ctx.full}:${num()}`;
  const diffKey = () => `pulldiff:${ctx.full}:${num()}`;
  const [getView] = useData(key, () => ctx.repoClient.pulls.get(num()));
  const [getReviews] = useData(reviewsKey, () => ctx.repoClient.pulls.reviews.list(num(), { n: 50 }));
  const [getThreads] = useData(threadsKey, () => ctx.repoClient.pulls.threads.list(num(), { n: 100 }));
  const [getRequests] = useData(requestsKey, () => ctx.repoClient.pulls.requests.list(num()));
  // Issue #520: the inline diff tracks its own fetch error for the
  // error state + Retry below — useData reports failures only to the
  // global tray, otherwise this section sits on "loading diff…" forever.
  const [getDiffError, setDiffError] = createSignal(null);
  const [getDiff] = useData(diffKey, async () => {
    setDiffError(null);
    try {
      const res = await ctx.repoClient.pulls.diff(num());
      return parsePatchFiles(normalizePatchBody(res));
    } catch (err) {
      setDiffError(err);
      throw err;
    }
  });
  const retryDiff = () => {
    setDiffError(null);
    invalidate(diffKey());
  };
  // Honest count (issue #520): renders only once loaded — never (0) for
  // a diff that never arrived.
  const diffCount = () => {
    const v = getDiff();
    if (v) return String((v.files ?? []).length);
    return getDiffError() ? "failed to load" : "…";
  };
  const { role } = useRole(ctx.full, ctx.repoClient);
  const navigate = useNavigate();
  const location = useLocation();
  // Forgejo #502: anonymous viewers execute zero writes — the PR composer
  // routes to the log-in interstitial (shared identity cache keys — zero
  // new requests). Review/merge affordances already hide below write.
  const [getMe] = useData("me", () => repos.me().catch(() => null));
  const [getDiscovery] = useData("discovery", () => repos.discovery().catch(() => null));
  const anon = () => isAnonymousViewer(getMe(), getDiscovery());
  const here = () => location.pathname + location.search;
  const writeGateHref = (action) => anonWriteTarget({ me: getMe(), discovery: getDiscovery() }, here(), action);
  const canComment = () => role() !== null && !anon();
  const canReview = () => roleAtLeast(role(), "write");
  const canResolve = () => roleAtLeast(role(), "triage");
  const canDismiss = () => roleAtLeast(role(), "maintain");
  const canUpdateBranch = () => roleAtLeast(role(), "write");
  const [getPending, setPending] = createSignal([]);
  const [getFinishing, setFinishing] = createSignal(false);
  // Jump-to-comments target (Forgejo #546): the index sets this tid and
  // scrolls to `thread-<tid>`; the matching card flashes + expands.
  const [getFlashTid, setFlashTid] = createSignal(null);
  const jumpToThread = (tid) => {
    setFlashTid(tid);
    document.getElementById(`thread-${tid}`)?.scrollIntoView({ block: "center" });
  };

  // Cross-page reconcile (issue #319, the #318 pattern for PRs): a merge
  // or close/reopen moves the open_pulls badge numerator with no ref
  // move, so the shell's shared `repo:{full}` summary entry reconciles at
  // the mutation site — the mutation's own `pull` frame may arrive at a
  // page that already unmounted.
  const reload = () => {
    invalidate(key());
    invalidate(reviewsKey());
    invalidate(threadsKey());
    invalidate(requestsKey());
    invalidate(diffKey());
    invalidate(checksKey());
    invalidate(`repo:${ctx.full}`);
  };
  const reloadReview = () => {
    invalidate(key());
    invalidate(reviewsKey());
    invalidate(threadsKey());
    invalidate(requestsKey());
  };

  const thread = () => getView()?.thread;
  const pr = () => getView()?.pr;
  const mergeable = () => getView()?.mergeable;
  const head = () => getView()?.head_live_sha ?? pr()?.head?.sha ?? "";
  // Head checks (05 §9): the combined view + per-context rows for the
  // live head sha, and the required-checks advisory (union of
  // require_checks over policy rules matching the base branch — exact-ref
  // match client-side; the merge task decides server-side). The pill
  // updates on reload; live `check` frames will refresh it once the repo
  // collaboration stream lands (06/08 own that endpoint).
  const checksKey = () => `checks:${ctx.full}:${head()}`;
  const [getCombined] = useData(checksKey, () => (head() ? ctx.repoClient.checks.combined(head()) : Promise.resolve(null)));
  const [getPolicy] = useData(`policy:${ctx.full}`, () => ctx.repoClient.policy.get().catch(() => null));
  const requiredChecks = () => {
    const base = pr()?.base?.ref ?? "";
    const rules = getPolicy()?.rules ?? [];
    const out = new Set();
    for (const r of rules) {
      const refs = r?.match?.refs ?? [];
      if (refs.length && !refs.includes(base)) continue;
      for (const c of r?.effect?.protect?.require_checks ?? []) out.add(c);
    }
    return [...out].sort();
  };
  const checksBlockers = () => requiredCheckBlockers(requiredChecks(), getCombined()?.statuses);
  // Zero-contexts empty state (Forgejo #518): the combined view's wire
  // state reads pending when nothing reported, so the card keys off the
  // statuses array it already fetches — never the state string.
  const zeroChecks = () => isZeroChecks(getCombined());
  const summary = () => thread()?.review_summary;
  const threads = () => getThreads()?.threads ?? [];

  const stage = (draft) => setPending((list) => [...list, draft]);
  const unstage = (i) => setPending((list) => list.filter((_, j) => j !== i));

  const comment = async (body) => {
    await ctx.repoClient.pulls.comment(num(), body, { noPopupAuth: true });
    reload();
  };

  // PR close/reopen (Forgejo #517): plain state flips via
  // repo.pulls.update(num, {state}) — no reason (PR state is open|closed
  // only), no review, no merge. Auth mirrors the server (author or triage;
  // UpdatePR 403s anyone else and 409s a merged close). Errors propagate to
  // the composer's tray path (toast, never silent); the reconcile runs only
  // on success. Cross-page reconcile is the #318 pattern: the own thread
  // key reloads NOW (so the badge flips + the timeline entry lands with no
  // full-page reload) while pulls windows + the repo summary (open_pulls
  // numerator) invalidate at the mutation site; other tabs follow the
  // closed/reopened `pull` frames on the repo stream (the accept filter in
  // useCollabStream below runs before invalidateCollab).
  const afterPRMutation = (n, ck) => {
    if (num() === n) reload();
    else invalidate(ck);
    invalidatePullLists(ctx.full);
  };
  const closePR = async () => {
    const n = num();
    const ck = key();
    await ctx.repoClient.pulls.update(n, { state: "closed" }, { noPopupAuth: true });
    afterPRMutation(n, ck);
  };
  const reopenPR = async () => {
    const n = num();
    const ck = key();
    await ctx.repoClient.pulls.update(n, { state: "open" }, { noPopupAuth: true });
    afterPRMutation(n, ck);
  };
  const commentAndClosePR = async (body) => {
    const n = num();
    const ck = key();
    // GitHub semantics (the issue-page shape): post the body first when
    // non-empty (empty body just closes), then close. Either step throwing
    // keeps the composer text; after a comment-posted / close-failed split
    // the plain Close button finishes the job.
    if (String(body ?? "").trim()) {
      await ctx.repoClient.pulls.comment(n, body, { noPopupAuth: true });
    }
    await ctx.repoClient.pulls.update(n, { state: "closed" }, { noPopupAuth: true });
    afterPRMutation(n, ck);
  };
  // Header badge + Close/Reopen visibility read the live thread/pr fetch
  // (the page's state source of truth, like the issue page) — never the
  // repo-level summary (ref/state-blind by design). No closeChooser: PR
  // closes carry no reason, so both controls stay plain buttons.
  const badge = () => pullBadgeView(thread(), pr());
  // Repo mdCtx for every renderBody call site on this page (the #340
  // contract: thread bodies carry no file coordinates, so relative URLs
  // stay verbatim; owner/repo feeds the #N/PRN autolinker). One object —
  // the timeline, description block, reviews, and thread comments share it.
  const mdCtx = { owner: ctx.owner, repo: ctx.name };
  // Attribution for the description block: the opened event's actor/at
  // (the history record), falling back to the live thread while loading.
  const openedEvent = () => (getView()?.events ?? []).find((ev) => ev?.type === "opened");
  const closeVis = () =>
    pullCloseVisibility({ thread: thread(), pr: pr(), mePrincipal: getMe()?.principal, role: role() });
  const closeAction = () => {
    const v = closeVis();
    if (v.showClose) return closePR;
    if (v.showReopen) return reopenPR;
    return undefined;
  };

  // The ONE repo collaboration stream (08 §4): pull/review/thread/check
  // frames for this PR invalidate coalesced keys; the header refetch
  // recomputes the MergeBox machine.
  useCollabStream(() => ctx.full, ctx.repoClient, ["pull", "review", "thread", "check"], (frame) => Number(frame.num) === Number(num()));

  return (
    <div class="issue-page grid gap-4 md:grid-cols-[1fr_16rem]">
      <section aria-label="Conversation" class="min-w-0">
        {/* Header block (Forgejo #517, the Issue.jsx convention, #521
            sibling read): title left, state badge right — the badge is the
            state surface (open/closed/merged via pullBadgeView), so the
            byline carries author + time only (no bare state token). The
            number rides the muted span like the issue heading; h1 stays —
            it is the page title above the h2 card titles. */}
        <header class="mb-4 border-b border-zinc-200 pb-3 dark:border-zinc-800">
          <div class="flex flex-wrap items-start justify-between gap-2">
            <h1 class="min-w-0 flex-1 text-xl font-semibold">
              <span class="text-zinc-500 dark:text-zinc-400">#{num()}</span> {thread()?.title}
            </h1>
            <Show when={thread()}>
              <span class={`${badge().cls} mt-1 shrink-0`}>{badge().text}</span>
            </Show>
          </div>
          <p class="mt-1 text-xs text-zinc-500 dark:text-zinc-400">
            {thread()?.author} · <DateTime value={thread()?.updated_at} />
          </p>
        </header>
        <PRDescription
          body={pr()?.body}
          actor={openedEvent()?.actor ?? thread()?.author}
          at={openedEvent()?.at ?? thread()?.updated_at}
          mdCtx={mdCtx}
        />
        {/* #340: thread bodies have no file coordinates (relative URLs stay
            verbatim) but carry the repo — owner/repo feeds the #N/PRN autolinker. */}
        <ThreadTimeline events={getView()?.events ?? []} textFor={pullEventText} mdCtx={mdCtx} />
        <ThreadIndex threads={threads()} files={getDiff()?.files ?? []} onJump={jumpToThread} />
        <Show
          when={canComment()}
          fallback={
            <Show when={anon()}>
              <p class="muted text-sm">
                <A class="hover:underline" href={writeGateHref("Comment on this pull request") ?? here()}>
                  Sign in to comment
                </A>
              </p>
            </Show>
          }
        >
          <CommentComposer
            onSubmit={comment}
            onCommentAndClose={closeVis().showClose ? commentAndClosePR : undefined}
            commentAndCloseLabel="Comment and Close"
            closeLabel={thread()?.state === "open" ? "Close" : "Reopen"}
            onClose={closeAction()}
            errorKey="pull-comment"
            mentionId="mention-pull-comment"
            mentionNames={thread()?.participants}
          />
        </Show>
        <div class="mt-4 space-y-4">
          <ReviewsList
            num={num()}
            client={ctx.repoClient}
            reviews={getReviews()?.reviews}
            head={head()}
            canDismiss={canDismiss()}
            reload={reloadReview}
            mdCtx={mdCtx}
          />
          <Show when={canReview()}>
            <Show when={getFinishing()} fallback={
              <button type="button" class="btn px-3 py-1" onClick={() => setFinishing(true)}>
                Finish review{(getPending().length ? ` (${getPending().length} staged)` : "")}
              </button>
            }>
              <FinishReview
                num={num()}
                client={ctx.repoClient}
                head={head()}
                pending={getPending()}
                onUnstage={unstage}
                onDone={() => {
                  setFinishing(false);
                  setPending([]);
                  reloadReview();
                }}
              />
            </Show>
          </Show>
          <div aria-label="Files">
            <h2 class="card-header">Files ({diffCount()})</h2>
            <Show when={getDiff()} fallback={
              <Show when={getDiffError()} fallback={<p class="text-sm text-zinc-500 dark:text-zinc-400">loading diff…</p>}>
                <div class="card" role="alert">
                  <p class="text-sm">Couldn't load the diff: {String(getDiffError()?.message ?? getDiffError())}</p>
                  <button type="button" class="btn mt-2" onClick={retryDiff}>
                    Retry
                  </button>
                </div>
              </Show>
            }>
              <For each={getDiff().files ?? []} fallback={<p class="text-sm text-zinc-500 dark:text-zinc-400">empty diff</p>}>
                {(file) => (
                  <div class="mb-4">
                    <DiffFile
                      file={file}
                      files={getDiff()?.files ?? []}
                      threads={threads()}
                      num={num()}
                      client={ctx.repoClient}
                      head={head()}
                      onStage={stage}
                      reload={reloadReview}
                      canResolve={canResolve()}
                      canComment={canComment()}
                      mdCtx={mdCtx}
                      flashTid={getFlashTid}
                    />
                  </div>
                )}
              </For>
            </Show>
          </div>
        </div>
      </section>
      {/* Sidebar (Forgejo #531, the Issue.jsx:549 one-container idiom):
          ONE card divide-y panel — review summary first, then
          mergeability / reviewers / checks / merge as divided sections.
          Each section is a p-3 block with an uppercase micro-label above
          its value, so a "none" reads as that section's value. No
          stacked sibling .card blocks, no card-header headings. */}
      <aside aria-label="Details" class="grid content-start gap-3">
        <section class="card divide-y divide-zinc-200 text-sm dark:divide-zinc-800" aria-label="Pull request metadata">
          <div class="p-3">
            <span class="mb-1 block text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400">Review summary</span>
            <ReviewSummaryBar summary={summary()} head={head()} />
          </div>
          <div class="grid gap-1 p-3">
            {/* Mergeability is a VALUE, not a heading: the micro-label
                above, mergeableText(mergeable()) as the value line, the
                base/head branches, pending-branch warning, and
                commits/files links as secondary value detail in the same
                section. */}
            <span class="text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400">Mergeability</span>
            <p class="text-sm">{mergeableText(mergeable())}</p>
            <Show when={!getView()?.head_ref_ok}>
              <p class="text-xs text-amber-600 dark:text-amber-400">From branch pending — push first.</p>
            </Show>
            <p class="text-xs text-zinc-500 dark:text-zinc-400">
              base {pr()?.base?.ref} @ {(pr()?.base?.sha ?? "").slice(0, 12)}
              <br />
              head {pr()?.head?.ref} @ {(pr()?.head?.sha ?? "").slice(0, 12)}
              {/* Forgejo #328: cross-repo PRs name the fork holding the head. */}
              <Show when={pr()?.fork?.repo}>
                <br />
                from {pr()?.fork?.repo}
              </Show>
            </p>
            <div class="flex gap-2 text-xs">
              <A href={`/${ctx.owner}/${ctx.name}/pull/${num()}/commits`}>commits</A>
              <A href={`/${ctx.owner}/${ctx.name}/pull/${num()}/files`}>files</A>
            </div>
          </div>
          <div class="grid gap-1 p-3">
            <span class="text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400">Reviewers</span>
            <ReviewersPanel num={num()} client={ctx.repoClient} requested={getRequests()?.reviewers?.map((r) => r.principal)} reload={reloadReview} canEdit={canReview()} />
          </div>
          <Show when={head()}>
            <div class="grid gap-1 p-3" aria-label="Checks">
              <div class="mb-1 flex items-center justify-between gap-2">
                <span class="text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400">Checks</span>
                <CheckPill full={ctx.full} sha={head()} client={ctx.repoClient} verbose />
              </div>
              <Show
                when={zeroChecks()}
                fallback={<ContextRows full={ctx.full} sha={head()} client={ctx.repoClient} />}
              >
                <ZeroChecksBlock full={ctx.full} required={requiredChecks()} />
              </Show>
              <Show when={requiredChecks().length > 0}>
                <p class="text-xs text-zinc-500 dark:text-zinc-400">
                  required: {requiredChecks().join(", ")}
                </p>
              </Show>
              <Show when={checksBlockers().length > 0}>
                <p class="text-xs text-amber-600 dark:text-amber-400">
                  blocking merge: {checksBlockers().join(", ")}
                </p>
              </Show>
            </div>
          </Show>
          <Show when={thread()?.state === "open" || pr()?.merged}>
            <div class="grid gap-1 p-3">
              <span class="text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400">Merge</span>
              <MergeBox
                client={ctx.repoClient}
                num={num()}
                pr={pr()}
                mergeable={mergeable()}
                checksBlockers={checksBlockers}
                reviewDecision={() => summary()?.decision}
                role={role}
                canUpdate={canUpdateBranch}
                onSettled={() => reload()}
                reload={() => reload()}
              />
            </div>
          </Show>
        </section>
      </aside>
    </div>
  );
}
