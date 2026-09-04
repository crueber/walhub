// web/src/pages/Releases.jsx — route "/:owner/:name/releases" (07 §8):
// release cards (tag, name, draft/prerelease badges, date, asset count)
// plus the repo "Latest" badge and the star toggle. `release` SSE frames
// refresh the list once 08 wires the collaboration stream; until then this
// page refetches on navigation and on demand (no polling loops).

import { createSignal, For, Show } from "solid-js";
import { A } from "@solidjs/router";
import { useRepo, fmtDate } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";

export function ReleaseBadges(props) {
  return (
    <span class="flex gap-1">
      <Show when={props.release.draft}>
        <span class="chip chip-draft">draft</span>
      </Show>
      <Show when={props.release.prerelease}>
        <span class="chip chip-prerelease">prerelease</span>
      </Show>
    </span>
  );
}

export default function Releases() {
  const ctx = useRepo();
  const [getAfter, setAfter] = createSignal(0);
  const query = () => ({ n: 50, ...(getAfter() ? { after: getAfter() } : {}) });
  const key = () => `releases:${ctx.full}:${JSON.stringify(query())}`;
  const [getPage] = useData(key, () => ctx.repoClient.releases.list(query()));
  const [getLatest] = useData(`latest:${ctx.full}`, () =>
    ctx.repoClient.releases.latest().catch((e) => (e?.status === 404 ? null : Promise.reject(e)))
  );

  const reload = () => {
    setAfter(0);
    invalidate(key());
    invalidate(`latest:${ctx.full}`);
  };

  return (
    <div class="grid gap-6 lg:grid-cols-[1fr_320px]">
      <section aria-label="Releases">
        <div class="mb-3 flex items-center gap-2">
          <A href={`/${ctx.full}/releases/new`} class="btn px-2 py-1">
            New release
          </A>
          <button type="button" class="btn ml-auto px-2 py-1" onClick={reload}>
            refresh
          </button>
        </div>
        <ul class="card-list">
          <For each={getPage()?.releases ?? []} fallback={<li class="card">No releases yet.</li>}>
            {(rel) => (
              <li class="card">
                <div class="flex items-center gap-2">
                  <A href={`/${ctx.full}/releases/${encodeURIComponent(rel.tag)}`} class="card-title font-mono">
                    {rel.tag}
                  </A>
                  <ReleaseBadges release={rel} />
                </div>
                <div class="card-meta">
                  <span>{rel.name}</span>
                  <span>{fmtDate(rel.published_at ?? rel.created_at)}</span>
                  <span>{(rel.assets ?? []).length} assets</span>
                </div>
              </li>
            )}
          </For>
        </ul>
        <Show when={getPage()?.more}>
          <button
            type="button"
            class="btn mt-3 px-2 py-1"
            onClick={() => {
              const rels = getPage()?.releases ?? [];
              const last = rels[rels.length - 1];
              if (last) setAfter(`${last.created_at}|${last.tag}`);
            }}
          >
            more
          </button>
        </Show>
      </section>
      <aside aria-label="Latest release" class="card h-fit">
        <h2 class="mb-2 text-sm font-semibold">Latest</h2>
        <Show when={getLatest()} fallback={<p class="muted">No published releases.</p>}>
          {(rel) => (
            <div>
              <A href={`/${ctx.full}/releases/${encodeURIComponent(rel().tag)}`} class="card-title font-mono">
                {rel().tag}
              </A>
              <p class="card-meta">{rel().name}</p>
              <p class="card-meta">{fmtDate(rel().published_at ?? rel().created_at)}</p>
            </div>
          )}
        </Show>
      </aside>
    </div>
  );
}
