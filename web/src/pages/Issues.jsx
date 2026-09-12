// web/src/pages/Issues.jsx — route "/:owner/:name/issues" (02 §11): the
// issue list with a filter bar (state/assignee/labels/milestone/since),
// paged divider-separated rows from the index (index-first, LIST fallback
// server-side), ALWAYS rendered newest-first by number descending (#48).
// new-issue + labels/milestones links. Rows upsert in place on `issue`
// SSE frames (the repo stream is shared; this page refetches its window).

import { createSignal, For, Show, onCleanup } from "solid-js";
import { A, useSearchParams } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";
import { sortByNumDesc } from "../lib/sort.js";
import { resolveIssueState, issueListState } from "../lib/issueState.js";
import { TTL } from "../lib/collab.js";
import { labelColorMap, toggleLabel } from "../lib/labels.js";
import { parseLabelsParam, serializeLabelsParam, resolveMilestoneFilter } from "../lib/issueFilters.js";
import { milestoneDisplay, milestoneFilterHref } from "../lib/milestones.js";
import { LabelChip } from "../components/LabelPicker.jsx";
import DateTime from "../components/DateTime.jsx";
import { useCollabStream } from "../components/collab.jsx";
import Empty from "../components/Empty.jsx";

// Open is the default — it needs no badge. Only closed gets a pill (#231).
function statePill(state) {
  if (state !== "closed") return null;
  return <span class="chip chip-closed shrink-0">closed</span>;
}

// LabelsFilter — multi-select labels dropdown for the filter bar (issue
// #416): the LabelPicker idiom (08 §2, #107) pointed at the filter param
// instead of a PATCH. The trigger summarizes the selection (comma list,
// truncated; "All labels" when unfiltered); the panel lists every repo
// label as a menuitemcheckbox grid row ([check+dot] [name] [description],
// #334) plus bare rows for selected-but-unknown names (a deleted label in
// a shared deep link — self-heal, never silently dropped) and a Clear row.
// Toggling never closes the panel (multi-add without reopening). Esc
// closes and restores trigger focus; outside-click closes (removed in
// onCleanup; no toggle-fight — the trigger lives inside `root`, so the
// document handler never sees its clicks as outside). The panel anchors
// left (unlike the thread pickers' right-0): this cell sits mid-grid, so
// a right-anchored w-80 panel would hang past the phone viewport's left
// edge; the shared `label-drop` hook keeps the opaque + #278 viewport
// bound either way. Props: { value: string (the raw `?labels=` param),
// all: [{name,color,description?}], pending: bool, onToggle(name),
// onClear() }.
function LabelsFilter(props) {
  const [getOpen, setOpen] = createSignal(false);
  let root;
  let trigger;

  const onDocClick = (e) => {
    if (getOpen() && root && !root.contains(e.target)) setOpen(false);
  };
  const onDocKey = (e) => {
    if (getOpen() && e.key === "Escape") {
      setOpen(false);
      trigger?.focus();
    }
  };
  document.addEventListener("click", onDocClick);
  document.addEventListener("keydown", onDocKey);
  onCleanup(() => {
    document.removeEventListener("click", onDocClick);
    document.removeEventListener("keydown", onDocKey);
  });

  const selected = () => parseLabelsParam(props.value);
  const selectedSet = () => new Set(selected().map((n) => n.toLowerCase()));
  const unknownSelected = () => {
    const known = new Set((props.all ?? []).map((l) => String(l.name).toLowerCase()));
    return selected().filter((n) => !known.has(n.toLowerCase()));
  };
  const summary = () => {
    if (props.pending) return "loading…";
    const s = selected();
    if (!s.length) return "All labels";
    return s.join(", ");
  };

  return (
    <div class="relative" ref={root}>
      <button
        type="button"
        ref={trigger}
        class="input flex w-full items-center justify-between gap-2 text-left"
        aria-haspopup="menu"
        aria-expanded={getOpen() ? "true" : "false"}
        aria-label={`Filter by labels${selected().length ? `: ${summary()}` : ""}`}
        title={summary()}
        disabled={props.pending}
        onClick={() => setOpen(!getOpen())}
      >
        <span class="min-w-0 flex-1 truncate">{summary()}</span>
        <span aria-hidden="true" class="shrink-0">
          ▾
        </span>
      </button>
      <Show when={getOpen()}>
        <div
          class="label-drop scroll-slim card absolute left-0 z-30 mt-1 max-h-72 w-80 overflow-y-auto p-1"
          role="menu"
          aria-label="Filter issues by labels"
        >
          <Show when={selected().length > 0}>
            <button
              type="button"
              role="menuitem"
              aria-label="clear labels filter"
              title="Clear the labels filter"
              class="flex w-full items-center gap-2 rounded px-2 py-1 text-left text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800"
              onClick={() => props.onClear?.()}
            >
              <span class="inline-block w-4 shrink-0 text-center" aria-hidden="true">
                ✕
              </span>
              <span class="muted italic">Clear</span>
            </button>
          </Show>
          <For each={unknownSelected()}>
            {(name) => (
              <button
                type="button"
                role="menuitemcheckbox"
                aria-checked="true"
                aria-label={`remove label ${name}`}
                title={`remove ${name} (no longer in this repo)`}
                class="grid w-full grid-cols-[auto_minmax(0,1fr)] items-center gap-2 rounded px-2 py-1 text-left text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800"
                onClick={() => props.onToggle?.(name)}
              >
                <span class="flex shrink-0 items-center gap-2" aria-hidden="true">
                  <span class="inline-block w-4 text-center">✓</span>
                </span>
                <span class="min-w-0 overflow-hidden text-ellipsis whitespace-nowrap font-medium">{name}</span>
              </button>
            )}
          </For>
          <For each={props.all ?? []} fallback={<p class="muted px-2 py-1 text-xs">no labels in this repo yet</p>}>
            {(l) => {
              const on = () => selectedSet().has(String(l.name).toLowerCase());
              return (
                <button
                  type="button"
                  role="menuitemcheckbox"
                  aria-checked={on() ? "true" : "false"}
                  aria-label={`${on() ? "remove" : "add"} label filter ${l.name}`}
                  title={`${on() ? "remove" : "add"} ${l.name}${l.description ? ` — ${l.description}` : ""}`}
                  class="grid w-full grid-cols-[auto_minmax(0,1fr)_minmax(0,1fr)] items-center gap-2 rounded px-2 py-1 text-left text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800"
                  onClick={() => props.onToggle?.(l.name)}
                >
                  <span class="flex shrink-0 items-center gap-2" aria-hidden="true">
                    <span class="inline-block w-4 text-center">{on() ? "✓" : ""}</span>
                    <span
                      class="inline-block h-3 w-3 rounded-full border border-zinc-300 dark:border-zinc-700"
                      style={{ "background-color": `#${l.color}` }}
                    />
                  </span>
                  <span class="min-w-0 overflow-hidden text-ellipsis whitespace-nowrap font-medium">{l.name}</span>
                  <Show when={l.description}>
                    <span class="muted min-w-0 truncate text-xs">{l.description}</span>
                  </Show>
                </button>
              );
            }}
          </For>
        </div>
      </Show>
    </div>
  );
}

export default function Issues() {
  const ctx = useRepo();
  const [search, setSearch] = useSearchParams();
  const [getAfter, setAfter] = createSignal(0);

  // State default is open-only (#323): an absent ?state= param resolves to
  // "open"; the explicit both-choice is ?state=all (URL-honest,
  // shareable), sent on the wire as an omitted param (the list endpoint
  // accepts open|closed|absent only). The select binds the RESOLVED value
  // so a bare visit visibly reads "open". Milestone-filtered landings
  // (?milestone=, no state) inherit the open default — deliberate (#323).
  const query = () => ({
    state: issueListState(resolveIssueState(search.state)),
    labels: search.labels || "",
    assignee: search.assignee || "",
    milestone: search.milestone || "",
    n: 50,
    ...(getAfter() ? { after: getAfter() } : {}),
  });
  const key = () => `issues:${ctx.full}:${JSON.stringify(query())}`;
  const [getPage] = useData(key, () => ctx.repoClient.issues.list(query()));

  const reload = () => invalidate(key());

  // Label colors for the row chips (#45): the index rows carry names
  // only (02 §2 projection), so colors resolve through the cached
  // `labels:{o}/{r}` set (30 s TTL, one shared entry with the thread
  // page picker). Unknown names render as bare chips (02 §3.1).
  const [getLabelSet] = useData(() => `labels:${ctx.full}`, () => ctx.repoClient.labels.list(), TTL.labels);
  const colorMap = () => labelColorMap(getLabelSet()?.labels);

  // Milestone titles for the row chips (issue #380): the index rows
  // carry the stored id only (02 §3.2 projection), so titles resolve
  // through the cached `milestones:{o}/{r}` set (30 s TTL, one shared
  // entry with the thread page sidebar). Rows render before it loads —
  // milestoneDisplay's pending state renders a placeholder, never a
  // bare-id flash.
  const [getMilestoneSet] = useData(() => `milestones:${ctx.full}`, () => ctx.repoClient.milestones.list(), TTL.milestones);

  // Live list: any `issue` frame invalidates the list windows (coalesced).
  useCollabStream(() => ctx.full, ctx.repoClient, ["issue"]);

  const setFilter = (k, v) => {
    setAfter(0);
    setSearch({ [k]: v || undefined });
  };

  // Milestone select binding (#416): the raw `?milestone=` param against
  // the cached set — "" (unfiltered), "none", a known id, or an unknown
  // id shown as its raw value (deep link to a deleted milestone — the
  // filter stays visible, never silently dropped). Pending while the set
  // loads, so the select disables instead of flashing bare ids.
  const msFilter = () => resolveMilestoneFilter(search.milestone, getMilestoneSet()?.milestones);
  const toggleFilterLabel = (name) =>
    setFilter("labels", serializeLabelsParam(toggleLabel(parseLabelsParam(search.labels), name)));

  return (
    <div class="issues-page">
      <div class="mb-2 flex flex-wrap items-center gap-2">
        <h2 class="text-xl font-semibold tracking-tight">Issues</h2>
        <div class="ml-auto flex gap-2">
          <A class="btn" href={`/${ctx.full}/labels`}>
            Labels
          </A>
          <A class="btn" href={`/${ctx.full}/milestones`}>
            Milestones
          </A>
          <A class="btn primary" href={`/${ctx.full}/issues/new`}>
            New issue
          </A>
        </div>
      </div>

      {/* Full-width filter grid (#232): labelled fields share the row via
          flexible columns (2-up on phones, 4+action on wide screens) so the
          controls fill the card instead of drifting in whitespace. Same
          params, same endpoints — layout only. */}
      <form
        class="card mb-3 grid grid-cols-2 gap-x-3 gap-y-2 p-3 sm:grid-cols-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_minmax(0,1fr)_minmax(0,1fr)_auto]"
        onSubmit={(e) => e.preventDefault()}
        aria-label="issue filters"
      >
        <label class="flex min-w-0 flex-col gap-1 text-xs font-medium text-zinc-500 dark:text-zinc-400">
          State
          <select
            class="input"
            value={resolveIssueState(search.state)}
            onChange={(e) => setFilter("state", e.target.value)}
          >
            <option value="all">open + closed</option>
            <option value="open">open</option>
            <option value="closed">closed</option>
          </select>
        </label>
        <label class="flex min-w-0 flex-col gap-1 text-xs font-medium text-zinc-500 dark:text-zinc-400">
          Assignee
          <input
            class="input"
            placeholder="assignee or *none"
            value={search.assignee || ""}
            onChange={(e) => setFilter("assignee", e.target.value)}
          />
        </label>
        <div class="flex min-w-0 flex-col gap-1 text-xs font-medium text-zinc-500 dark:text-zinc-400">
          Labels
          <LabelsFilter
            value={search.labels || ""}
            all={getLabelSet()?.labels}
            pending={getLabelSet() === undefined}
            onToggle={toggleFilterLabel}
            onClear={() => setFilter("labels", "")}
          />
        </div>
        <label class="flex min-w-0 flex-col gap-1 text-xs font-medium text-zinc-500 dark:text-zinc-400">
          Milestone
          <select
            class="input"
            value={msFilter().value}
            disabled={msFilter().pending}
            onChange={(e) => setFilter("milestone", e.target.value)}
            aria-label="Filter by milestone"
            title={msFilter().unknown ? `Unknown milestone ${msFilter().value} (no longer in this repo)` : undefined}
          >
            <option value="">All milestones</option>
            <option value="none">No milestone</option>
            <For each={getMilestoneSet()?.milestones ?? []}>{(m) => <option value={m.id}>{m.title}</option>}</For>
            <Show when={msFilter().unknown}>
              <option value={msFilter().value}>{msFilter().value}</option>
            </Show>
          </select>
        </label>
        <div class="col-span-2 flex items-end sm:col-span-4 lg:col-span-1">
          <button type="button" class="btn w-full lg:w-auto" onClick={reload}>
            Refresh
          </button>
        </div>
      </form>

      <Show when={getPage()} fallback={<p class="muted">loading…</p>}>
        {(page) => (
          <>
            <Show
              when={(page().issues ?? []).length > 0}
              fallback={
                <Empty
                  icon="issue"
                  title="No issues match"
                  hint="Try widening the filters — or file the first issue for this repo."
                  actionHref={`/${ctx.full}/issues/new`}
                  actionLabel="New issue"
                />
              }
            >
              <ul>
                <For each={sortByNumDesc(page().issues)}>
                {(issue) => (
                  // Divider-separated rows, never boxed (#135, echoing the
                  // ThreadTimeline comment-entry dividers from #109). Title
                  // first (#231: open issues carry no state pill — only
                  // closed gets one), label chips inline right of the title.
                  // Truncation safety: the title truncates (min-w-0 +
                  // max-w-full) and chips wrap, so long titles + many
                  // labels wrap sanely instead of overflowing. The
                  // right-aligned meta reads comment count (bubble icon,
                  // #380) → milestone chip (only when set) → updated time;
                  // the milestone chip truncates (max-w + title tooltip,
                  // same #334 safety as the title).
                  <li class="border-t border-zinc-200 py-3 first:border-t-0 first:pt-0 dark:border-zinc-800">
                    <div class="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
                      <A
                        class="min-w-0 max-w-full truncate font-medium text-emerald-700 hover:underline dark:text-emerald-400"
                        href={`/${ctx.full}/issues/${issue.num}`}
                      >
                        #{issue.num} {issue.title}
                      </A>
                      {statePill(issue.state)}
                      <Show when={(issue.labels ?? []).length > 0}>
                        <span class="flex min-w-0 flex-wrap gap-1">
                          <For each={issue.labels}>
                            {(l) => <LabelChip name={l} map={colorMap()} />}
                          </For>
                        </span>
                      </Show>
                      <span class="ml-auto shrink-0 text-xs text-zinc-500 dark:text-zinc-400">
                        <span title={`${issue.comment_count} comments`} aria-label={`${issue.comment_count} comments`}>
                          <span aria-hidden="true">💬 </span>
                          {issue.comment_count}
                        </span>
                        <Show when={issue.milestone != null}>
                          {" · "}
                          {(() => {
                            // The title waits on the page-owned milestone
                            // set — a placeholder, never the bare id, until
                            // the set settles (deleted ids still fall back
                            // to the bare id via milestoneDisplay's unknown
                            // path, same self-heal as the sidebar).
                            const d = () => milestoneDisplay(getMilestoneSet()?.milestones, issue.milestone);
                            return (
                              <Show when={!d().pending} fallback={<span class="muted">…</span>}>
                                <A
                                  class="chip max-w-40 truncate align-bottom"
                                  href={milestoneFilterHref(ctx.full, issue.milestone)}
                                  title={`issues on milestone ${d().text}`}
                                >
                                  {d().text}
                                </A>
                              </Show>
                            );
                          })()}
                        </Show>
                        {" · "}
                        <DateTime value={issue.updated_at} />
                      </span>
                    </div>
                  </li>
                )}
              </For>
              </ul>
            </Show>
            <Show when={page().more}>
              <button
                type="button"
                class="btn mt-3"
                onClick={() => {
                  const items = page().issues ?? [];
                  if (items.length) setAfter(items[items.length - 1].num);
                }}
              >
                Older
              </button>
            </Show>
          </>
        )}
      </Show>
    </div>
  );
}
