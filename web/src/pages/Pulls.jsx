// web/src/pages/Pulls.jsx — route "/:owner/:name/pulls" (03 §9): the PR
// list (state tabs open/closed, base/head filters, paged index-first
// cards) plus the open-PR form (title, base/head refs). Cards refresh on
// `pull` SSE frames (the repo stream is shared; this page refetches its
// window).

import { createSignal, For, Show } from "solid-js";
import { A, useSearchParams } from "@solidjs/router";
import { useRepo, fmtDate } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";
import { useCollabStream } from "../components/collab.jsx";

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

  const [getTitle, setTitle] = createSignal("");
  const [getBase, setBase] = createSignal("refs/heads/main");
  const [getHead, setHead] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);

  const open = async (e) => {
    e.preventDefault();
    if (!getTitle().trim() || !getHead().trim()) return;
    setBusy(true);
    try {
      await ctx.repoClient.pulls.open({
        title: getTitle().trim(),
        base_ref: getBase().trim(),
        head_ref: getHead().trim(),
      });
      setTitle("");
      setHead("");
      setAfter(0);
      reload();
    } catch (err) {
      reportError(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="grid gap-6 lg:grid-cols-[1fr_320px]">
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
          <A class="btn primary px-2 py-1" href={`/${ctx.full}/pulls/new`}>
            New pull request
          </A>
        </div>
        <ul class="card-list">
          <For each={getPage()?.pulls ?? []} fallback={<li class="card">No pull requests.</li>}>
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
                  <span>{fmtDate(pr.updated_at)}</span>
                </div>
              </li>
            )}
          </For>
        </ul>
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
      <aside aria-label="Open a pull request">
        <form class="card" onSubmit={open}>
          <h2 class="mb-2 text-sm font-semibold">Open a pull request</h2>
          <label class="field">
            <span>Title</span>
            <input value={getTitle()} onInput={(e) => setTitle(e.target.value)} required maxlength="256" />
          </label>
          <label class="field">
            <span>Base ref</span>
            <input value={getBase()} onInput={(e) => setBase(e.target.value)} required />
          </label>
          <label class="field">
            <span>Head ref</span>
            <input
              value={getHead()}
              onInput={(e) => setHead(e.target.value)}
              required
              placeholder="refs/heads/topic"
            />
          </label>
          <button type="submit" class="btn btn-primary mt-2 px-3 py-1" disabled={getBusy()}>
            {getBusy() ? "opening…" : "open pull request"}
          </button>
        </form>
      </aside>
    </div>
  );
}
