// web/src/pages/Pulls.jsx — route "/:owner/:name/pulls" (03 §9): the PR
// list (state tabs open/closed, base/head filters, paged index-first
// cards), ALWAYS rendered newest-first by number descending (#48). Opening a PR lives on the full "/pulls/new" page (issue #34 —
// the cramped sidebar box is gone; this page links to it). Cards refresh
// on `pull` SSE frames (the repo stream is shared; this page refetches its
// window).

import { createSignal, For, Show } from "solid-js";
import { A, useSearchParams } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { useData, invalidate } from "../lib/data.js";
import { sortByNumDesc } from "../lib/sort.js";
import { useCollabStream } from "../components/collab.jsx";
import Empty from "../components/Empty.jsx";
import DateTime from "../components/DateTime.jsx";

export default function Pulls() {
  const ctx = useRepo();
  const [search, setSearch] = useSearchParams();
  const [getAfter, setAfter] = createSignal(0);

  const query = () => ({
    state: search.state || "",
    base: search.base || "",
    head: search.head || "",
    n: 50,
    ...(getAfter() ? { after: getAfter() } : {}),
  });
  const key = () => `pulls:${ctx.full}:${JSON.stringify(query())}`;
  const [getPage] = useData(key, () => ctx.repoClient.pulls.list(query()));

  const reload = () => invalidate(key());

  // Live list: any `pull` frame invalidates the list windows (coalesced).
  useCollabStream(() => ctx.full, ctx.repoClient, ["pull"]);

  const setFilter = (k, v) => {
    setAfter(0);
    setSearch({ [k]: v || undefined });
  };

  // Carry the head/base filters into the new-PR page so a filtered empty
  // list ("no PRs from refs/heads/topic") opens the composer prefilled.
  const newHref = () => {
    const params = new URLSearchParams({
      ...(search.base ? { base: search.base } : {}),
      ...(search.head ? { head: search.head } : {}),
    });
    const qs = params.toString();
    return `/${ctx.full}/pulls/new${qs ? `?${qs}` : ""}`;
  };

  const emptyTitle = () => (search.state === "closed" ? "No closed pull requests" : "No pull requests");
  const emptyHint = () =>
    search.base || search.head
      ? `Nothing matches${search.base ? ` base ${search.base}` : ""}${search.head ? ` head ${search.head}` : ""} — clear the filters or open one from these refs.`
      : "Propose a change from a branch — pick a base and a head to compare.";

  return (
    <section aria-label="Pull requests">
      <div class="mb-3 flex items-center gap-2">
        <button
          type="button"
          class={`btn px-2 py-1 ${!search.state ? "btn-active" : ""}`}
          onClick={() => setFilter("state", "")}
        >
          open
        </button>
        <button
          type="button"
          class={`btn px-2 py-1 ${search.state === "closed" ? "btn-active" : ""}`}
          onClick={() => setFilter("state", "closed")}
        >
          closed
        </button>
        <button type="button" class="btn ml-auto px-2 py-1" onClick={reload}>
          refresh
        </button>
        <A class="btn primary px-2 py-1" href={newHref()}>
          New pull request
        </A>
      </div>
      <Show when={getPage()} fallback={<p class="muted">loading…</p>}>
        <Show
          when={(getPage().pulls ?? []).length > 0}
          fallback={
            <Empty
              icon="pull"
              title={emptyTitle()}
              hint={emptyHint()}
              actionHref={newHref()}
              actionLabel="New pull request"
            />
          }
        >
          <ul class="card-list">
            <For each={sortByNumDesc(getPage().pulls)}>
              {(pr) => (
                <li class="card">
                  <A href={`/${ctx.owner}/${ctx.name}/pull/${pr.num}`} class="card-title">
                    #{pr.num} {pr.title}
                  </A>
                  <div class="card-meta">
                    <span class={`chip chip-${pr.state}`}>{pr.state}</span>
                    <span>
                      {pr.base_ref} ← {pr.head_ref}
                    </span>
                    <span>{pr.author}</span>
                    <span><DateTime value={pr.updated_at} /></span>
                  </div>
                </li>
              )}
            </For>
          </ul>
        </Show>
      </Show>
      <Show when={getPage()?.more}>
        <button
          type="button"
          class="btn mt-3 px-3 py-1"
          onClick={() => setAfter(getPage().pulls.at(-1)?.num ?? 0)}
        >
          older
        </button>
      </Show>
    </section>
  );
}
