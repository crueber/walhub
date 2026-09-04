// web/src/pages/Repos.jsx — route "/:owner": the owner's repositories.

import repos from "../../sdk/src/index.js";
import { For, Show } from "solid-js";
import { useParams, A } from "@solidjs/router";
import { useData } from "../lib/data.js";

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
                    <li>
                      <A
                        class="text-emerald-700 hover:underline dark:text-emerald-400"
                        href={`/${owner()}/${n}`}
                      >
                        {owner()}/{n}
                      </A>
                    </li>
                  )}
                </For>
              </ul>
            </Show>
          </>
        )}
      </Show>
      <p class="muted mt-4 text-xs">
        <A class="hover:underline" href={`/${owner()}/settings`}>organization settings</A>
        {' '}(members, teams, invitations — 404 unless this owner is an org)
      </p>
    </div>
  );
}
