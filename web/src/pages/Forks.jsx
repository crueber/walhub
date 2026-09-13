// web/src/pages/Forks.jsx — route "/:owner/:name/forks" (issue #424):
// the live fork list (the parent meta/forks.json index, queryable).
// One index-first page at a time (?n=&after=); every call through the
// SDK (dogfood rule).

import { createSignal, Show, For, onCleanup } from "solid-js";
import { A, useParams } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { reportError } from "../lib/data.js";

const PAGE = 50;

export default function Forks() {
  const params = useParams();
  const full = () => `${params.owner}/${params.name}`;
  const repoClient = repos.repo(full());
  const [getForks, setForks] = createSignal(null); // null = loading
  const [getAfter, setAfter] = createSignal("");
  const [getMore, setMore] = createSignal(false);
  const [getBusy, setBusy] = createSignal(false);
  let alive = true;
  onCleanup(() => {
    alive = false;
  });

  const load = async (after) => {
    setBusy(true);
    try {
      const page = await repoClient.forks.list({ n: PAGE, ...(after ? { after } : {}) });
      if (!alive) return;
      setForks(after ? [...(getForks() ?? []), ...(page?.forks ?? [])] : (page?.forks ?? []));
      setMore(!!page?.more);
      const rows = page?.forks ?? [];
      if (rows.length) setAfter(rows[rows.length - 1].repo);
    } catch (e) {
      reportError(e, "forks");
      if (alive) setForks(getForks() ?? []);
    } finally {
      if (alive) setBusy(false);
    }
  };
  load("");

  return (
    <div class="forks-page grid max-w-2xl gap-4">
      <h2 class="text-xl font-semibold">
        Forks of <A class="hover:underline" href={`/${full()}`}>{full()}</A>
      </h2>
      <Show when={getForks() !== null} fallback={<p class="muted">loading…</p>}>
        <Show when={(getForks() ?? []).length > 0} fallback={<p class="muted text-sm">No forks yet.</p>}>
          <ul class="card divide-y divide-zinc-200 dark:divide-zinc-800">
            <For each={getForks() ?? []}>
              {(f) => (
                <li class="flex items-baseline justify-between gap-3 px-3 py-2">
                  <A class="font-mono hover:underline" href={`/${f.repo}`}>{f.repo}</A>
                  <span class="muted text-xs">forked {String(f.forked_at ?? "").slice(0, 10)}</span>
                </li>
              )}
            </For>
          </ul>
        </Show>
        <Show when={getMore()}>
          <button type="button" class="btn px-3 py-1" disabled={getBusy()} onClick={() => load(getAfter())}>
            {getBusy() ? "loading…" : "show more"}
          </button>
        </Show>
      </Show>
    </div>
  );
}
