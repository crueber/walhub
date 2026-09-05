// web/src/pages/Repos.jsx — route "/:owner": the owner's repositories
// (uncapped — the owners page folds overflow behind a "+N more →" link
// here). Star counts ride the shared `social:{o}/{r}` cache entries
// (<StarCount>, lib/stars.js) and last-active stamps the shared
// `activity:{o}/{r}` entries (<ActivityStamp>, lib/activity.js), so rows
// fetched on `/` are reused here. The shared <RepoRow> (link + star count
// + last-active stamp) renders in a responsive two-column grid, one column
// on narrow widths — the owners page builds on the same component.

import repos from "../../sdk/src/index.js";
import { For, Show } from "solid-js";
import { useParams, A } from "@solidjs/router";
import { useData } from "../lib/data.js";
import StarCount from "../components/StarCount.jsx";
import ActivityStamp from "../components/ActivityStamp.jsx";

/** One repo row: link + star count + last-active stamp. Shared with `/`. */
export function RepoRow(props) {
  const full = () => `${props.owner}/${props.name}`;
  return (
    <li class="flex flex-wrap items-baseline gap-x-1.5">
      <A
        class="text-emerald-700 hover:underline dark:text-emerald-400"
        href={`/${full()}`}
      >
        {full()}
      </A>
      <StarCount full={full()} />
      <ActivityStamp full={full()} />
    </li>
  );
}

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
              <ul class="grid grid-cols-1 gap-x-6 gap-y-1 sm:grid-cols-2">
                <For each={names()}>
                  {(n) => <RepoRow owner={owner()} name={n} />}
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
