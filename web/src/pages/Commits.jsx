// web/src/pages/Commits.jsx — Commits tab list (§9.2): resolve → commits?ref={sha}
// (sha-addressed, immutable, §2.4). ?skip= pages through history, ?path= limits
// it, ?ref= picks the ref (this route has no *rest segment, so ctx.rest is ""
// here and the ref arrives as a query param). The "older →" link carries the
// query forward, keeping pagination URL-addressable.

import { createEffect, createSignal, For, Show } from "solid-js";
import { A, useLocation } from "@solidjs/router";
import { useData, useResolved, SHA_TTL } from "../lib/data.js";
import { CopySha, shortSha } from "../lib/sha.jsx";
import { fmtDate, shortRef, useRepo } from "./Repo.jsx";
import { CheckPill } from "./Checks.jsx";

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

function CommitRow(props) {
  const c = () => props.commit;
  return (
    <div class="commit-row grid grid-cols-[minmax(0,1fr)_auto_auto] items-center gap-x-2.5 px-3 py-2">
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
          <span>{` · ${fmtDate(c().author_date)}`}</span>
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
      <Show when={h()} fallback={<p class="muted animate-pulse">loading history…</p>}>
        {(hist) => (
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
            </nav>
            <div class="commit-list card divide-y divide-zinc-100 overflow-hidden dark:divide-zinc-800/60">
              <For each={hist().commits ?? []}>
                {(c) => <CommitRow full={props.full} commit={c} client={props.repoClient} />}
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
        />
      )}
    </Show>
  );
}
