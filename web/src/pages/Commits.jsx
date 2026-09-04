// web/src/pages/Commits.jsx — Commits tab list (§9.2): resolve → commits?ref={sha}
// (sha-addressed, immutable, §2.4). ?skip= pages through history, ?path= limits
// it, ?ref= picks the ref (this route has no *rest segment, so ctx.rest is ""
// here and the ref arrives as a query param). The "older →" link carries the
// query forward, keeping pagination URL-addressable.

import { createEffect, createSignal, For, Show } from "solid-js";
import { A, useLocation } from "@solidjs/router";
import { useData, useResolved, SHA_TTL } from "../lib/data.js";
import { fmtDate, shortRef, useRepo } from "./Repo.jsx";
import { CheckPill } from "./Checks.jsx";

function CommitRow(props) {
  const c = () => props.commit;
  return (
    <div class="commit-row flex items-baseline justify-between gap-3 px-3 py-2">
      <div class="commit-main min-w-0">
        <A
          class="commit-subject font-medium text-zinc-900 hover:underline dark:text-zinc-100"
          href={`/${props.full}/commit/${c().sha}`}
        >
          {c().subject ?? "(no message)"}
        </A>
        <div class="muted commit-meta mt-0.5 truncate text-xs">
          <span>{c().author ?? ""}</span>
          <Show when={c().author_email}>
            <span>{` <${c().author_email}>`}</span>
          </Show>
          <span>{` · ${fmtDate(c().author_date)}`}</span>
          <Show when={(c().parents ?? []).length > 0}>
            <span>{` · parent ${String(c().parents[0]).slice(0, 10)}`}</span>
          </Show>
          <Show when={(c().trailers?.length ?? 0) > 0}>
            <span class="pill ml-1">{c().trailers.length} trailers</span>
          </Show>
        </div>
      </div>
      <code class="sha shrink-0 font-mono text-xs">{String(c().sha).slice(0, 12)}</code>
      <span class="shrink-0">
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
  const [getFirst] = useResolved(props.owner, props.name, props.rest, "commits");

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

  return (
    <div class="commits-page">
      <Show when={h()} fallback={<p class="muted">loading history…</p>}>
        {(hist) => (
          <>
            <nav class="crumbs mb-2 text-sm">
              <A
                class="text-emerald-700 hover:underline dark:text-emerald-400"
                href={`/${props.full}`}
              >
                {shortRef(hist().ref)}
              </A>
              <Show when={path()}>
                <span class="muted">{` · history of ${path()}`}</span>
              </Show>
            </nav>
            <div class="commit-list card divide-y divide-zinc-100 dark:divide-zinc-800/60">
              <For each={hist().commits ?? []}>
                {(c) => <CommitRow full={props.full} commit={c} client={props.repoClient} />}
              </For>
              <Show when={(hist().commits ?? []).length === 0}>
                <p class="muted p-3">no commits in this window.</p>
              </Show>
            </div>
            <Show when={hist().more}>
              <div class="pager mt-3">
                <A
                  class="pill cursor-pointer hover:no-underline"
                  href={olderHref(props.full, path(), skip(), hist().commits ?? [])}
                >
                  older →
                </A>
              </div>
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
  return (
    <Show when={`${ctx.full}?${ctx.rest || location.query.ref || ""}`} keyed>
      {() => (
        <CommitList
          full={ctx.full}
          owner={ctx.owner}
          name={ctx.name}
          rest={ctx.rest || location.query.ref || ""}
          repoClient={ctx.repoClient}
        />
      )}
    </Show>
  );
}
