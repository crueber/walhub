// web/src/pages/Repos.jsx — route "/:owner": the owner's repositories
// (uncapped — the owners page folds overflow behind a "+N more →" link
// here). Rows arrive pre-ordered by most recent commit (Forgejo #247:
// the detailed listing, `sort=activity&order=desc`, stabilized by
// orderByActivity — the same ordering as `/explore`, shared cache key).
// Star counts ride the shared `social:{o}/{r}` cache entries
// (<StarCount>, lib/stars.js) and last-active stamps render from the
// listing rows (<ActivityStamp at/empty props>, no per-row fetch), so rows
// fetched on `/` are reused here. The shared <RepoRow> (link + star count
// + last-active stamp) renders in a responsive two-column grid, one column
// on narrow widths — the owners page builds on the same component.

import repos from "../../sdk/src/index.js";
import { For, Show } from "solid-js";
import { useParams, A } from "@solidjs/router";
import { useData } from "../lib/data.js";
import { orderByActivity } from "../lib/owners.js";
import StarCount from "../components/StarCount.jsx";
import ActivityStamp from "../components/ActivityStamp.jsx";

/** One repo row: link + star count + last-active stamp. Shared with `/`.
 *  Issue #235, explicitly descoped: rows ride the detailed owners listing,
 *  so there is no per-repo summary in hand — showing descriptions here would
 *  cost one summary fetch per row (N round trips for N repos). The repo
 *  header remains the description surface. `at`/`empty` carry the listing's
 *  activity so the stamp renders without a fetch (Forgejo #247). */
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
      <ActivityStamp full={full()} at={props.at} empty={props.empty} />
    </li>
  );
}

export default function Repos() {
  const params = useParams();
  const owner = () => params.owner;
  const [getDoc] = useData(
    () => `repos:${owner()}`,
    () => repos.owners.detailed(owner(), { sort: "activity", order: "desc" })
  );
  // Writers-only New button (mirrors require_write so the button never
  // promises what POST /api/v1/repos refuses): hidden for anonymous
  // without write. One me() fetch, no tray (missing = hidden).
  const [getMe] = useData("me", () => repos.me().catch(() => null));
  const canWrite = () => {
    const me = getMe();
    if (!me) return false;
    if (me.anonymous) return false;
    return me.write !== false;
  };
  return (
    <div class="repos-page">
      <div class="mb-1 flex items-center justify-between">
        <h2 class="text-xl font-semibold">{owner()}</h2>
        <Show when={canWrite()}>
          <A class="btn primary px-3 py-1" href={`/new?owner=${encodeURIComponent(owner())}`}>
            New repository
          </A>
        </Show>
      </div>
      <Show when={getDoc()} fallback={<p class="muted">loading…</p>}>
        {(doc) => {
          const rows = orderByActivity(doc().repos);
          return (
            <>
              <p class="muted mb-4">
                {rows.length} repositor{rows.length === 1 ? "y" : "ies"}
              </p>
              <Show
                when={rows.length > 0}
                fallback={<p class="muted">nothing under {owner()} yet</p>}
              >
                <ul class="grid grid-cols-1 gap-x-6 gap-y-1 sm:grid-cols-2">
                  <For each={rows}>
                    {(row) => <RepoRow owner={owner()} name={row.name} at={row.last_commit_time} empty={row.size_bytes === 0} />}
                  </For>
                </ul>
              </Show>
            </>
          );
        }}
      </Show>
      <p class="muted mt-4 text-xs">
        <A class="hover:underline" href={`/import?owner=${encodeURIComponent(owner())}`}>import into {owner()}</A>
        {' · '}
        <A class="hover:underline" href={`/${owner()}/settings`}>organization settings</A>
      </p>
    </div>
  );
}
