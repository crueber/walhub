// web/src/pages/Releases.jsx — route "/:owner/:name/releases" (07 §8):
// name-led release rows (tag in mono, draft/prerelease badges, body
// excerpt, asset count, publish date) with client-side all/drafts/
// prereleases chips over the loaded page (no new endpoint). The Latest
// pointer folds into the list (issue #270): the matching row carries the
// `latest` chip plus the key asset quick-downloads, so the sidebar's
// content duplication is gone and the page is single-column like Issues.
// `release` frames ride
// the ONE repo collaboration stream (08 §4) and invalidate the list +
// latest coalesced; the page refetches on navigation and on demand
// (no polling loops).

import { createSignal, For, Show } from "solid-js";
import { A } from "@solidjs/router";
import { useRepo, fmtBytes } from "./Repo.jsx";
import { useData, invalidate } from "../lib/data.js";
import { keyAssets, filterReleases, excerptBody, LATEST_ASSET_LIMIT } from "../lib/releases.js";
import { useCollabStream } from "../components/collab.jsx";
import Empty from "../components/Empty.jsx";
import DateTime from "../components/DateTime.jsx";

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

// Client-side filter chips (issue #270): the list endpoint hides drafts,
// so the chips narrow the loaded page only — same params, same endpoints,
// layout + filter only.
const FILTERS = [
  { id: "all", label: "All" },
  { id: "drafts", label: "Drafts" },
  { id: "prereleases", label: "Prereleases" },
];

export default function Releases() {
  const ctx = useRepo();
  const [getAfter, setAfter] = createSignal(0);
  const [getFilter, setFilter] = createSignal("all");
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

  const newHref = () => `/${ctx.full}/releases/new`;
  const releases = () => getPage()?.releases ?? [];
  const visible = () => filterReleases(releases(), getFilter());
  const filterCount = (id) => (id === "all" ? releases().length : filterReleases(releases(), id).length);
  // Latest folds into the first card (issue #270): the chip + key assets
  // render inline on the matching row, first page with no filter only.
  const isLatestRow = (rel) =>
    getFilter() === "all" && !getAfter() && getLatest() && rel.tag === getLatest().tag;

  const pickFilter = (id) => setFilter(id);

  return (
    <div class="releases-page">
      <div class="mb-2 flex flex-wrap items-center gap-2">
        <h2 class="text-xl font-semibold tracking-tight">Releases</h2>
        <div class="ml-auto flex gap-2">
          <button type="button" class="btn" onClick={reload}>
            Refresh
          </button>
          <A class="btn primary" href={newHref()}>
            New release
          </A>
        </div>
      </div>

      <div class="mb-3 flex flex-wrap gap-1.5" role="group" aria-label="release filters">
        <For each={FILTERS}>
          {(f) => (
            <button
              type="button"
              class={`pill${getFilter() === f.id ? " btn-active" : ""}`}
              aria-pressed={getFilter() === f.id}
              onClick={() => pickFilter(f.id)}
            >
              {f.label} ({filterCount(f.id)})
            </button>
          )}
        </For>
      </div>

      <Show when={getPage() !== undefined} fallback={<p class="muted">loading…</p>}>
        <Show
          when={visible().length > 0}
          fallback={
            <Show
              when={releases().length > 0}
              fallback={
                <div class="mx-auto max-w-xl">
                  <Empty
                    icon="tag"
                    title="No releases yet"
                    hint="Tag a commit and publish release notes — drafts and prereleases are supported."
                    actionHref={newHref()}
                    actionLabel="New release"
                  />
                </div>
              }
            >
              <Empty
                icon="tag"
                title={`No ${getFilter()} releases in this view`}
                hint="The filter applies to the loaded page only — try All, or publish a matching release."
              />
            </Show>
          }
        >
          <ul>
            <For each={visible()}>
              {(rel) => {
                const detail = () => `/${ctx.full}/releases/${encodeURIComponent(rel.tag)}`;
                const title = () => rel.name || rel.tag;
                const excerpt = () => {
                  const ex = excerptBody(rel.body);
                  return ex && ex !== title() ? ex : "";
                };
                const assets = () => keyAssets(rel.assets, LATEST_ASSET_LIMIT);
                return (
                  // Divider-separated rows, never boxed (#135, echoing the
                  // Issues rows from #231). Name-led title first (the tag
                  // follows in mono); badges inline; asset quick-downloads
                  // render on the Latest row only.
                  <li class="border-t border-zinc-200 py-3 first:border-t-0 first:pt-0 dark:border-zinc-800">
                    <div class="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
                      <A
                        class="min-w-0 max-w-full truncate font-medium text-emerald-700 hover:underline dark:text-emerald-400"
                        href={detail()}
                      >
                        {title()}
                      </A>
                      <Show when={isLatestRow(rel)}>
                        <span class="chip shrink-0">latest</span>
                      </Show>
                      <ReleaseBadges release={rel} />
                      <span class="ml-auto shrink-0 text-xs text-zinc-500 dark:text-zinc-400">
                        {(rel.assets ?? []).length} assets · <DateTime value={rel.published_at ?? rel.created_at} />
                      </span>
                    </div>
                    <div class="mt-0.5 flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5 text-sm">
                      <span class="shrink-0 font-mono text-xs text-zinc-500 dark:text-zinc-400">{rel.tag}</span>
                      <Show when={excerpt()}>
                        <span class="min-w-0 truncate text-zinc-600 dark:text-zinc-400">{excerpt()}</span>
                      </Show>
                    </div>
                    <Show when={isLatestRow(rel) && assets().shown.length > 0}>
                      <div class="mt-1 flex min-w-0 flex-wrap gap-x-3 gap-y-0.5 text-xs">
                        <For each={assets().shown}>
                          {(a) => (
                            <span class="flex min-w-0 items-baseline gap-1.5">
                              <a
                                class="min-w-0 max-w-64 truncate font-mono text-emerald-700 hover:underline dark:text-emerald-400"
                                href={ctx.repoClient.releaseAssetUrl(rel.tag, a.name)}
                                download={a.name}
                                title={a.name}
                              >
                                {a.name}
                              </a>
                              <span class="muted tabular shrink-0">{fmtBytes(a.size)}</span>
                            </span>
                          )}
                        </For>
                        <Show when={assets().extra > 0}>
                          <A href={detail()} class="shrink-0 text-emerald-700 hover:underline dark:text-emerald-400">
                            +{assets().extra} more →
                          </A>
                        </Show>
                      </div>
                    </Show>
                  </li>
                );
              }}
            </For>
          </ul>
        </Show>
        <Show when={getPage()?.more}>
          <button
            type="button"
            class="btn mt-3"
            onClick={() => {
              const rels = releases();
              const last = rels[rels.length - 1];
              if (last) setAfter(`${last.created_at}|${last.tag}`);
            }}
          >
            Older
          </button>
        </Show>
      </Show>
    </div>
  );
}
