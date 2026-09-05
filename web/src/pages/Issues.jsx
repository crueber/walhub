// web/src/pages/Issues.jsx — route "/:owner/:name/issues" (02 §11): the
// issue list with a filter bar (state/labels/assignee/milestone/since),
// paged cards from the index (index-first, LIST fallback server-side),
// ALWAYS rendered newest-first by number descending (#48).
// new-issue + labels/milestones links. Cards upsert in place on `issue`
// SSE frames (the repo stream is shared; this page refetches its window).

import { createSignal, For, Show } from "solid-js";
import { A, useSearchParams } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";
import { sortByNumDesc } from "../lib/sort.js";
import { TTL } from "../lib/collab.js";
import { labelColorMap } from "../lib/labels.js";
import { LabelChip } from "../components/LabelPicker.jsx";
import DateTime from "../components/DateTime.jsx";
import { useCollabStream } from "../components/collab.jsx";
import Empty from "../components/Empty.jsx";

function statePill(state) {
  return state === "open" ? (
    <span class="chip chip-open">open</span>
  ) : (
    <span class="chip chip-closed">closed</span>
  );
}

export default function Issues() {
  const ctx = useRepo();
  const [search, setSearch] = useSearchParams();
  const [getAfter, setAfter] = createSignal(0);

  const query = () => ({
    state: search.state || "",
    labels: search.labels || "",
    assignee: search.assignee || "",
    milestone: search.milestone || "",
    n: 50,
    ...(getAfter() ? { after: getAfter() } : {}),
  });
  const key = () => `issues:${ctx.full}:${JSON.stringify(query())}`;
  const [getPage] = useData(key, () => ctx.repoClient.issues.list(query()));

  const reload = () => invalidate(key());

  // Label colors for the card chips (#45): the index cards carry names
  // only (02 §2 projection), so colors resolve through the cached
  // `labels:{o}/{r}` set (30 s TTL, one shared entry with the thread
  // page picker). Unknown names render as bare chips (02 §3.1).
  const [getLabelSet] = useData(() => `labels:${ctx.full}`, () => ctx.repoClient.labels.list(), TTL.labels);
  const colorMap = () => labelColorMap(getLabelSet()?.labels);

  // Live list: any `issue` frame invalidates the list windows (coalesced).
  useCollabStream(() => ctx.full, ctx.repoClient, ["issue"]);

  const setFilter = (k, v) => {
    setAfter(0);
    setSearch({ [k]: v || undefined });
  };

  return (
    <div class="issues-page">
      <div class="mb-3 flex flex-wrap items-center gap-2">
        <h2 class="text-lg font-semibold">Issues</h2>
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

      <form
        class="card mb-3 flex flex-wrap gap-2 p-3"
        onSubmit={(e) => e.preventDefault()}
        aria-label="issue filters"
      >
        <select
          class="input w-auto"
          value={search.state || ""}
          onChange={(e) => setFilter("state", e.target.value)}
          aria-label="state"
        >
          <option value="">open + closed</option>
          <option value="open">open</option>
          <option value="closed">closed</option>
        </select>
        <input
          class="input w-36"
          placeholder="labels (a,b)"
          value={search.labels || ""}
          onChange={(e) => setFilter("labels", e.target.value)}
          aria-label="labels"
        />
        <input
          class="input w-44"
          placeholder="assignee or *none"
          value={search.assignee || ""}
          onChange={(e) => setFilter("assignee", e.target.value)}
          aria-label="assignee"
        />
        <input
          class="input w-32"
          placeholder="milestone or none"
          value={search.milestone || ""}
          onChange={(e) => setFilter("milestone", e.target.value)}
          aria-label="milestone"
        />
        <button type="button" class="btn" onClick={reload}>
          Refresh
        </button>
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
              <ul class="grid gap-2">
                <For each={sortByNumDesc(page().issues)}>
                {(issue) => (
                  <li class="card flex flex-wrap items-baseline gap-2 p-3">
                    {statePill(issue.state)}
                    <A
                      class="font-medium text-emerald-700 hover:underline dark:text-emerald-400"
                      href={`/${ctx.full}/issues/${issue.num}`}
                    >
                      #{issue.num} {issue.title}
                    </A>
                    <span class="ml-auto text-xs text-zinc-500 dark:text-zinc-400">
                      {issue.comment_count} comments · <DateTime value={issue.updated_at} />
                    </span>
                    <Show when={(issue.labels ?? []).length > 0}>
                      <span class="flex w-full flex-wrap gap-1">
                        <For each={issue.labels}>
                          {(l) => <LabelChip name={l} map={colorMap()} />}
                        </For>
                      </span>
                    </Show>
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
