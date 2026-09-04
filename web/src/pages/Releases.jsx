// web/src/pages/Releases.jsx — route "/:owner/:name/releases" (07 §8):
// release cards (tag, name, draft/prerelease badges, date, asset count)
// plus the Latest release panel (issue #35: tag card with badges, publish
// date, key asset downloads, detail link; the shared Empty callout when
// none). `release` frames ride
// the ONE repo collaboration stream (08 §4) and invalidate the list +
// latest coalesced; the page refetches on navigation and on demand
// (no polling loops).

import { createSignal, For, Show } from "solid-js";
import { A } from "@solidjs/router";
import { useRepo, fmtDate, fmtBytes } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";
import { keyAssets, LATEST_ASSET_LIMIT } from "../lib/releases.js";
import { useCollabStream } from "../components/collab.jsx";
import Empty from "../components/Empty.jsx";

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

  // Live list: `release` frames invalidate the list + latest (coalesced).
  useCollabStream(() => ctx.full, ctx.repoClient, ["release"]);

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
        <Show
          when={(getPage()?.releases ?? []).length > 0}
          fallback={
            <Empty
              icon="tag"
              title="No releases yet"
              hint="Tag a commit and publish release notes — drafts and prereleases are supported."
              actionHref={`/${ctx.full}/releases/new`}
              actionLabel="New release"
            />
          }
        >
          <ul class="card-list">
            <For each={getPage()?.releases ?? []}>
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
        </Show>
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
      <aside aria-label="Latest release" class="card h-fit p-4">
        <Show when={getLatest() !== undefined} fallback={<p class="muted text-sm" role="status">loading latest…</p>}>
          <Show
            when={getLatest()}
            fallback={
              <div>
                <h2 class="mb-2 text-sm font-semibold">Latest</h2>
                <Empty
                  compact
                  icon="tag"
                  title="No published releases"
                  hint="Tag a commit and publish release notes — drafts and prereleases are supported."
                  actionHref={`/${ctx.full}/releases/new`}
                  actionLabel="New release"
                />
              </div>
            }
          >
            {(rel) => {
              const detail = () => `/${ctx.full}/releases/${encodeURIComponent(rel().tag)}`;
              const date = () => rel().published_at ?? rel().created_at;
              const assets = () => keyAssets(rel().assets, LATEST_ASSET_LIMIT);
              return (
                <div>
                  <div class="mb-2 flex items-center justify-between gap-2">
                    <h2 class="text-sm font-semibold">Latest</h2>
                    <A href={detail()} class="text-xs text-emerald-700 hover:underline dark:text-emerald-400">
                      view release →
                    </A>
                  </div>
                  <div class="flex flex-wrap items-center gap-2">
                    <A href={detail()} class="font-mono text-lg font-semibold text-emerald-700 hover:underline dark:text-emerald-400">
                      {rel().tag}
                    </A>
                    <ReleaseBadges release={rel()} />
                  </div>
                  <Show when={rel().name}>
                    <p class="mt-0.5 text-sm font-medium">{rel().name}</p>
                  </Show>
                  <Show when={date()}>
                    <p class="muted mt-0.5 text-xs">
                      Published <time dateTime={date()}>{fmtDate(date())}</time>
                    </p>
                  </Show>
                  <div class="mt-3 border-t border-zinc-100 pt-2 dark:border-zinc-800/60">
                    <h3 class="muted text-xs font-medium uppercase tracking-wide">
                      Assets ({(rel().assets ?? []).length})
                    </h3>
                    <Show when={assets().shown.length > 0} fallback={<p class="muted mt-1 text-xs">No assets.</p>}>
                      <ul class="mt-1 space-y-1">
                        <For each={assets().shown}>
                          {(a) => (
                            <li class="flex items-baseline justify-between gap-2 text-sm">
                              <a
                                class="min-w-0 truncate font-mono text-[13px] text-emerald-700 hover:underline dark:text-emerald-400"
                                href={ctx.repoClient.releaseAssetUrl(rel().tag, a.name)}
                                download={a.name}
                                title={a.name}
                              >
                                {a.name}
                              </a>
                              <span class="muted tabular shrink-0 text-xs">{fmtBytes(a.size)}</span>
                            </li>
                          )}
                        </For>
                      </ul>
                    </Show>
                    <Show when={assets().extra > 0}>
                      <A href={detail()} class="mt-1 inline-block text-xs text-emerald-700 hover:underline dark:text-emerald-400">
                        +{assets().extra} more →
                      </A>
                    </Show>
                  </div>
                </div>
              );
            }}
          </Show>
        </Show>
      </aside>
    </div>
  );
}
