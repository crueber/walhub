// web/src/pages/Repos.jsx — route "/:owner": the owner's repositories
// (uncapped — the owners page folds overflow behind a "+N more →" link
// here). Star counts ride the shared `social:{o}/{r}` cache entries
// (<StarCount>, lib/stars.js), so counts fetched on `/` are reused here.

import repos from "../../sdk/src/index.js";
import { For, Show } from "solid-js";
import { useParams, A } from "@solidjs/router";
import { useData } from "../lib/data.js";
import StarCount from "../components/StarCount.jsx";

export default function Repos() {
  const params = useParams();
  const owner = () => params.owner;
  const [getRepos] = useData(
    () => `repos:${owner()}`,
    () => repos.owners.repos(owner())
  );
  return (
    <div class="repos-page">
      <h2 class="mb-1 text-xl font-semibold">{owner()}</h2>
      <Show when={getRepos()} fallback={<p class="muted">loading…</p>}>
        {(names) => (
          <>
            <p class="muted mb-4">
              {names().length} repositor{names().length === 1 ? "y" : "ies"}
            </p>
            <Show
              when={names().length > 0}
              fallback={<p class="muted">nothing under {owner()} yet</p>}
            >
              <ul class="space-y-1">
                <For each={names()}>
                  {(n) => (
                    <li class="flex flex-wrap items-baseline gap-x-1.5">
                      <A
                        class="text-emerald-700 hover:underline dark:text-emerald-400"
                        href={`/${owner()}/${n}`}
                      >
                        {owner()}/{n}
                      </A>
                      <StarCount full={`${owner()}/${n}`} />
                    </li>
                  )}
                </For>
              </ul>
            </Show>
          </>
        )}
      </Show>
      <p class="muted mt-4 text-xs">
        <A class="hover:underline" href={`/import?owner=${encodeURIComponent(owner())}`}>import into {owner()}</A>
        {' · '}
        <A class="hover:underline" href={`/${owner()}/settings`}>organization settings</A>
      </p>
    </div>
  );
}
