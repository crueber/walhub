// web/src/pages/Pulls.jsx — route "/:owner/:name/pulls" (03 §9): the PR
// list (state tabs open/closed, base/head filters, paged index-first
// flat divider rows in the Issues.jsx idiom — Forgejo #531), ALWAYS rendered newest-first by number descending (#48). Opening a PR lives on the full "/pulls/new" page (issue #34 —
// the cramped sidebar box is gone; this page links to it). Cards refresh
// on `pull` SSE frames (the repo stream is shared; this page refetches its
// window).

import { createSignal, For, Show } from "solid-js";
import { A, useSearchParams } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import repos from "../../sdk/src/index.js";
import { useData, invalidate } from "../lib/data.js";
import { sortByNumDesc } from "../lib/sort.js";
import { resolvePullState, pullListState } from "../lib/pullState.js";
import { pullListChip } from "../lib/pull-state.js";
import { useCollabStream } from "../components/collab.jsx";
import Empty from "../components/Empty.jsx";
import DateTime from "../components/DateTime.jsx";
import { anonWriteTarget } from "../lib/writeGate.js";
import { isFeatureDisabled } from "../lib/repoFeatures.js";

export default function Pulls() {
  const ctx = useRepo();
  const [search, setSearch] = useSearchParams();
  const [getAfter, setAfter] = createSignal(0);

  // State default is open-only (#332, mirroring #323 for issues): an absent
  // ?state= param resolves to "open" (so the open tab highlights on a bare
  // visit); the explicit both-choice is ?state=all (URL-honest,
  // shareable), sent on the wire as an omitted param (the list endpoint
  // accepts open|closed|absent only). The tabs bind the RESOLVED value.
  const query = () => ({
    state: pullListState(resolvePullState(search.state)),
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
  // Forgejo #502: anonymous viewers land on the log-in interstitial instead
  // (shared identity cache keys — zero new requests).
  const [getMe] = useData("me", () => repos.me().catch(() => null));
  const [getDiscovery] = useData("discovery", () => repos.discovery().catch(() => null));
  const newHref = () => {
    const params = new URLSearchParams({
      ...(search.base ? { base: search.base } : {}),
      ...(search.head ? { head: search.head } : {}),
    });
    const qs = params.toString();
    const dest = `/${ctx.full}/pulls/new${qs ? `?${qs}` : ""}`;
    return anonWriteTarget({ me: getMe(), discovery: getDiscovery() }, dest, "Open a new pull request") ?? dest;
  };

  const emptyTitle = () => (resolvePullState(search.state) === "closed" ? "No closed pull requests" : "No pull requests");
  const emptyHint = () =>
    search.base || search.head
      ? `Nothing matches${search.base ? ` base ${search.base}` : ""}${search.head ? ` head ${search.head}` : ""} — clear the filters or open one from these refs.`
      : "Propose a change from a branch — pick a base and a head to compare.";

  return (
    <section aria-label="Pull requests">
      <div class="mb-3 flex items-center gap-2">
        <button
          type="button"
          class={`btn px-2 py-1 ${resolvePullState(search.state) === "open" ? "btn-active" : ""}`}
          onClick={() => setFilter("state", "open")}
        >
          open
        </button>
        <button
          type="button"
          class={`btn px-2 py-1 ${resolvePullState(search.state) === "closed" ? "btn-active" : ""}`}
          onClick={() => setFilter("state", "closed")}
        >
          closed
        </button>
        <button
          type="button"
          class={`btn px-2 py-1 ${resolvePullState(search.state) === "all" ? "btn-active" : ""}`}
          onClick={() => setFilter("state", "all")}
        >
          all
        </button>
        <button type="button" class="btn ml-auto px-2 py-1" onClick={reload}>
          refresh
        </button>
        {/* Forgejo #522: the composer affordance goes away while pulls
            are off (the shared summary — zero new requests); the list
            keeps rendering. */}
        <Show when={!isFeatureDisabled(ctx.summary?.(), "pulls")}>
          <A class="btn primary px-2 py-1" href={newHref()}>
            New pull request
          </A>
        </Show>
      </div>
      <Show when={getPage()} fallback={<p class="muted">loading…</p>}>
        <Show
          when={(getPage().pulls ?? []).length > 0}
          fallback={
            <Empty
              icon="pull"
              title={emptyTitle()}
              hint={emptyHint()}
              actionHref={isFeatureDisabled(ctx.summary?.(), "pulls") ? undefined : newHref()}
              actionLabel={isFeatureDisabled(ctx.summary?.(), "pulls") ? undefined : "New pull request"}
            />
          }
        >
          {/* Flat divider-separated rows, never boxed (Forgejo #531, the
              Issues.jsx:351–358 idiom echoing the ThreadTimeline
              comment-entry dividers): one container, per-PR row = title
              link, state chip, refs inline, right-aligned meta. The
              title truncates (min-w-0 + max-w-full) and the row wraps,
              so long titles wrap sanely instead of overflowing; the
              right meta sits ml-auto. */}
          <ul>
            <For each={sortByNumDesc(getPage().pulls)}>
              {(pr) => (
                <li class="border-t border-zinc-200 py-3 first:border-t-0 first:pt-0 dark:border-zinc-800">
                  <div class="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
                    <A
                      href={`/${ctx.owner}/${ctx.name}/pull/${pr.num}`}
                      class="min-w-0 max-w-full truncate font-medium text-emerald-700 hover:underline dark:text-emerald-400"
                    >
                      #{pr.num} {pr.title}
                    </A>
                    {/* Forgejo #530: merged wins over state (merge stamps
                        closed too) — the chip helper reads the PROut.merged
                        flag the list endpoint carries. */}
                    <span class={pullListChip(pr).cls}>{pullListChip(pr).text}</span>
                    <span class="min-w-0 text-xs text-zinc-500 dark:text-zinc-400">
                      {pr.base_ref} ← {pr.head_ref}
                    </span>
                    <span class="ml-auto shrink-0 text-xs text-zinc-500 dark:text-zinc-400">
                      <span>{pr.author}</span>
                      {" · "}
                      <span><DateTime value={pr.updated_at} /></span>
                    </span>
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
