// web/src/pages/Commits.jsx — Commits tab list (§9.2): resolve → commits?ref={sha}
// (sha-addressed, immutable, §2.4). ?skip= pages through history, ?path= limits
// it, ?ref= picks the ref (this route has no *rest segment, so ctx.rest is ""
// here and the ref arrives as a query param). The "older →" link carries the
// query forward, keeping pagination URL-addressable.

import { createEffect, createMemo, createSignal, onCleanup, For, Show } from "solid-js";
import { A, useLocation } from "@solidjs/router";
import { useData, useResolved, SHA_TTL } from "../lib/data.js";
import { CopySha, shortSha } from "../lib/sha.jsx";
import {
  assignLanes,
  laneClass,
  laneX,
  railWidth,
  rowDiagonals,
  readGraphEnabled,
  writeGraphEnabled,
  GRAPH_ROW_H,
  GRAPH_ROW_MID,
} from "../lib/commit-graph.js";
import { shortRef, useRepo } from "./Repo.jsx";
import DateTime from "../components/DateTime.jsx";
import { CheckPill } from "./Checks.jsx";
import { EmptyRepoGuide, DegradedNotice } from "../components/EmptyRepoGuide.jsx";

function ParentLinks(props) {
  const parents = () => props.parents ?? [];
  return (
    <Show when={parents().length > 0}>
      <p class="commit-parents muted mt-0.5 truncate text-xs tabular-nums">
        <span>{parents().length > 1 ? `merge parents (${parents().length}) ` : "parent "}</span>
        <For each={parents()}>
          {(p, i) => (
            <>
              <Show when={i() > 0}>
                <span>{" · "}</span>
              </Show>
              <A
                class="sha font-mono text-emerald-700 hover:underline dark:text-emerald-400"
                href={`/${props.full}/commit/${p}`}
                title={`parent ${p}`}
              >
                {shortSha(p, 10)}
              </A>
            </>
          )}
        </For>
      </p>
    </Show>
  );
}

// Lane rail for one commit row (Forgejo #506): verticals ride the top/bottom
// boundary snapshots, diagonals ride rowDiagonals (branch-out + merge-in);
// the node is an HTML dot (an svg circle would ellipse under the
// preserveAspectRatio="none" stretch — rows vary in height). Merge commits
// (multiple parents) render a hollow node. Lane color is class-only
// (.gl-N → ui.css vars, light + dark) — no color literals in this file.
function GraphRail(props) {
  const row = () => props.row;
  const w = () => railWidth(props.width);
  const diag = () => rowDiagonals(row());
  const nodeMerge = () => (row().parents?.length ?? 0) > 1;
  return (
    <div
      class="commit-rail relative self-stretch shrink-0"
      style={{ width: `${w()}px` }}
      aria-hidden="true"
    >
      <svg
        class="absolute inset-0 h-full w-full"
        viewBox={`0 0 ${w()} ${GRAPH_ROW_H}`}
        preserveAspectRatio="none"
        aria-hidden="true"
      >
        <For each={row().top}>
          {(s, k) => (
            <Show when={s}>
              <line
                x1={laneX(k())}
                y1="0"
                x2={laneX(k())}
                y2={GRAPH_ROW_MID}
                class={laneClass(k())}
                stroke="currentColor"
                stroke-width="2"
                vector-effect="non-scaling-stroke"
              />
            </Show>
          )}
        </For>
        <For each={row().bottom}>
          {(s, k) => (
            <Show when={s}>
              <line
                x1={laneX(k())}
                y1={GRAPH_ROW_MID}
                x2={laneX(k())}
                y2={GRAPH_ROW_H}
                class={laneClass(k())}
                stroke="currentColor"
                stroke-width="2"
                vector-effect="non-scaling-stroke"
              />
            </Show>
          )}
        </For>
        <For each={diag()}>
          {(d) => (
            <line
              x1={laneX(d.from)}
              y1={GRAPH_ROW_MID}
              x2={laneX(d.to)}
              y2={GRAPH_ROW_H}
              class={laneClass(d.to)}
              stroke="currentColor"
              stroke-width="2"
              vector-effect="non-scaling-stroke"
            />
          )}
        </For>
      </svg>
      <span
        class={`graph-dot ${laneClass(row().lane)}${nodeMerge() ? " graph-dot-merge" : ""}`}
        style={{ left: `${laneX(row().lane)}px` }}
      />
    </div>
  );
}

function CommitRow(props) {
  const c = () => props.commit;
  return (
    <div class="commit-row grid grid-cols-[minmax(0,1fr)_auto_auto] items-center gap-x-2.5 px-3 py-2">
      <Show when={props.showGraph && props.graphRow}>
        {(row) => <GraphRail row={row()} width={props.graphWidth} />}
      </Show>
      <div class="commit-main min-w-0">
        <A
          class="commit-subject block truncate text-sm font-medium text-zinc-900 hover:underline dark:text-zinc-100"
          href={`/${props.full}/commit/${c().sha}`}
          title={c().subject ?? "(no message)"}
        >
          {c().subject ?? "(no message)"}
        </A>
        <p class="commit-meta muted mt-0.5 truncate text-xs tabular-nums">
          <span>{c().author ?? ""}</span>
          <Show when={c().author_email}>
            <span>{` <${c().author_email}>`}</span>
          </Show>
          <span>{" · "}<DateTime value={c().author_date} /></span>
          <Show when={(c().trailers?.length ?? 0) > 0}>
            <span class="pill ml-1">{c().trailers.length} trailers</span>
          </Show>
        </p>
        <ParentLinks full={props.full} parents={c().parents} />
      </div>
      <div class="commit-sha-col flex shrink-0 items-center gap-0.5">
        <A
          class="sha block w-28 text-right font-mono text-xs tabular-nums text-emerald-700 hover:underline dark:text-emerald-400"
          href={`/${props.full}/commit/${c().sha}`}
          title={c().sha}
        >
          {shortSha(c().sha)}
        </A>
        <CopySha sha={c().sha} />
      </div>
      <span class="commit-check flex shrink-0 items-center">
        <CheckPill full={props.full} sha={String(c().sha)} client={props.client} />
      </span>
    </div>
  );
}

function olderHref(full, path, skip, commits) {
  const params = new URLSearchParams({
    ...(path ? { path } : {}),
    skip: String(skip + commits.length),
  });
  return `/${full}/commits?${params.toString()}`;
}

/** The list for one (repo, ref). Remounted whenever the ref changes so the
 * setup-time useResolved keys stay honest (route components are reused on
 * query-only navigations). */
function CommitList(props) {
  const location = useLocation();
  const skip = () => Math.max(0, Number(location.query.skip ?? 0) || 0);
  const path = () => String(location.query.path ?? "");

  // The §9.2 idiom: resolve rest → sha-addressed first window (skip 0).
  // On a known-empty repo useResolved settles EMPTY_REPO with zero fetches
  // (issue #209) — the doomed `commits?n=1` probe is suppressed, not trayed.
  const [getFirst] = useResolved(props.owner, props.name, props.rest, "commits");

  // Issue #252: publish the resolved ref for the header pill (same contract
  // as Tree.jsx; bare /commits resolves to the default branch, which matches
  // the summary-head fallback anyway). Cleared on unmount.
  createEffect(() => {
    const h0 = getFirst();
    if (h0 && h0.sha && !h0.empty && !h0.degraded) props.setViewed({ name: h0.ref ?? "", sha: h0.sha });
  });
  onCleanup(() => props.setViewed(null));

  // Windows beyond the first page (?skip=/?path=): same resolve → sha chain,
  // keyed sha+path+skip so each window is as immutable as its sha.
  const [getPage, setPage] = createSignal(undefined);
  createEffect(() => {
    const first = getFirst();
    if (!first || (skip() === 0 && !path())) return setPage(undefined);
    const sha = first.sha;
    const key = `sha:${sha}:commits:${path()}:${skip()}`;
    const [get] = useData(
      key,
      () => props.repoClient.commits({ ref: sha, path: path() || undefined, skip: skip() || undefined }),
      SHA_TTL,
    );
    setPage(get());
  });
  const h = () => (skip() === 0 && !path() ? getFirst() : getPage());

  // Forgejo #506: the graph toggle (default OFF, persisted like the theme
  // in lib/store.js) and its per-window lane derivation. The memo reads
  // h() — the sha+path+skip-keyed useData window — so lanes re-derive
  // exactly when the visible window changes, with zero new fetches. The
  // derivation is per-window by design: crossing the pager restarts the
  // lane layout (called out under the pager when the graph is on).
  const [graphOn, setGraphOn] = createSignal(readGraphEnabled());
  const flipGraph = () => {
    const next = !graphOn();
    setGraphOn(next);
    writeGraphEnabled(next);
  };
  const graph = createMemo(() => (graphOn() ? assignLanes(h()?.commits ?? []) : null));

  return (
    <div class="commits-page">
      <Show when={h()} fallback={<p class="muted animate-pulse">loading history…</p>}>
        {(hist) => (
          <>
            <Show when={hist().empty}>
              <EmptyRepoGuide full={props.full} summary={props.summary?.()} />
            </Show>
            <Show when={hist().degraded}>
              <DegradedNotice full={props.full} cacheKey={`sha:${hist().sha}:commits:${hist().path ?? ""}`} />
            </Show>
            <Show when={!hist().empty && !hist().degraded}>
            <>
            <nav class="crumbs mb-2 flex flex-wrap items-baseline gap-x-1.5 text-sm">
              <A
                class="text-emerald-700 hover:underline dark:text-emerald-400"
                href={`/${props.full}`}
              >
                {shortRef(hist().ref)}
              </A>
              <Show when={path()}>
                <span class="muted" aria-hidden="true">/</span>
                <span>
                  history of <code class="font-mono text-xs">{path()}</code>
                </span>
              </Show>
              <span class="ml-auto">
                <button
                  type="button"
                  class="pill cursor-pointer"
                  classList={{ "btn-active": graphOn() }}
                  onClick={flipGraph}
                  aria-pressed={graphOn()}
                  aria-label={graphOn() ? "Hide commit graph" : "Show commit graph"}
                  title="Show the commit graph (lanes derive per page)"
                >
                  graph
                </button>
              </span>
            </nav>
            <div
              class="commit-list card divide-y divide-zinc-100 overflow-hidden dark:divide-zinc-800/60"
              classList={{ "graph-on": graphOn() }}
            >
              <For each={hist().commits ?? []}>
                {(c, i) => (
                  <CommitRow
                    full={props.full}
                    commit={c}
                    client={props.repoClient}
                    showGraph={graphOn()}
                    graphRow={graph()?.rows[i()]}
                    graphWidth={graph()?.width ?? 0}
                  />
                )}
              </For>
              <Show when={(hist().commits ?? []).length === 0}>
                <p class="muted p-4 text-sm">No commits in this view.</p>
              </Show>
            </div>
            <Show when={hist().more}>
              <div class="pager mt-3 flex items-center gap-2">
                <A
                  class="pill cursor-pointer hover:no-underline"
                  href={olderHref(props.full, path(), skip(), hist().commits ?? [])}
                  title={`show older commits (showing ${skip() + (hist().commits ?? []).length} so far)`}
                >
                  older →
                </A>
                <span class="muted text-xs tabular-nums">
                  {`showing ${skip() + (hist().commits ?? []).length} so far`}
                </span>
              </div>
            </Show>
            <Show when={graphOn()}>
              <p class="muted mt-2 text-xs">
                Graph lanes derive per page — crossing “older →” restarts the lane layout.
              </p>
            </Show>
            </>
            </Show>
          </>
        )}
      </Show>
    </div>
  );
}

export default function Commits() {
  const ctx = useRepo();
  const location = useLocation();
  // Keyed on repo+ref: @solidjs/router reuses route components on query-only
  // navigations, and useResolved captures its keys at setup — so a ref change
  // must recreate the list (owner/name/rest are read fresh at that moment).
  // The keyed child MUST take the key as its argument: Show returns a no-arg
  // closure as-is (same reference), so `keyed` + `{() => …}` never remounts
  // and the list sticks on the first ref (same flaw as #38).
  return (
    <Show when={`${ctx.full}?${ctx.rest || location.query.ref || ""}`} keyed>
      {(_key) => (
        <CommitList
          full={ctx.full}
          owner={ctx.owner}
          name={ctx.name}
          rest={ctx.rest || location.query.ref || ""}
          repoClient={ctx.repoClient}
          summary={ctx.summary}
          setViewed={ctx.setViewed}
        />
      )}
    </Show>
  );
}
