// web/src/pages/Owners.jsx — route "/explore": intro + every owner with their repos.
// Owner/repo names come from the store-backed core listing endpoints
// (GET /api/v1/owners?sort=activity&order=desc — 07 §8, Forgejo #283: the
// server ranks ALL owners by the per-owner max-commit rollup before the
// client's MAX_OWNERS slice, so an active owner past the name cap still
// surfaces and first paint is already ordered; per-owner rows from GET
// /api/v1/owners/{owner}/repos/detailed?sort=activity&order=desc — 07 §8,
// Forgejo #247: true most-recent-commit order server-side, stabilized
// client-side by lib/owners.js orderByActivity). Owner SECTIONS keep the
// client re-rank as fallback/enhancement (Forgejo #283): each section
// reports its newest row time (lib/owners.js ownerActivity over the same
// detailed doc it already fetched — zero extra GETs) and the page re-ranks
// sections via orderOwnersByActivity as docs land, which also heals a stale
// catalog rollup with fresher per-section times. Owners with no known
// activity (fetch pending, unbackfilled, or no commits) sort last with a
// deterministic name tiebreak; until every section reports, not-yet-loaded
// owners keep that trailing name order — first paint is server-ordered and
// only refines once, then settles.
// The page adds per-section caps (lib/owners.js) and the intro card. Star
// counts ride the shared `social:{o}/{r}` cache entries (<StarCount>,
// lib/stars.js) and last-active stamps render from the listing rows
// (<ActivityStamp at/empty props> — no per-row commits fetch on this page;
// see the component header). Rows share <RepoRow> with `/:owner`
// (Repos.jsx) in a responsive two-column grid (one column on narrow
// widths). No new deps (issues #117, #137, #142).

import repos from "../../sdk/src/index.js";
import { For, Show, createEffect, createSignal } from "solid-js";
import { A } from "@solidjs/router";
import { useData } from "../lib/data.js";
import { RepoRow } from "./Repos.jsx";
import {
  MAX_OWNERS,
  MAX_REPOS_PER_OWNER,
  orderByActivity,
  orderOwnersByActivity,
  ownerActivity,
  pageSlice,
} from "../lib/owners.js";

/** One owner's section: heading + capped repo list (own `repos:{owner}` cache key, shared with /:owner). */
function OwnerSection(props) {
  const [getDoc] = useData(`repos:${props.owner}`, () =>
    repos.owners.detailed(props.owner, { sort: "activity", order: "desc" }),
  );
  // Report this section's newest commit time upward (#283 ordering) — runs
  // on the doc the section already fetched, so ranking costs zero extra GETs.
  createEffect(() => {
    const doc = getDoc();
    if (doc && typeof props.onActivity === "function") {
      props.onActivity(props.owner, ownerActivity(doc));
    }
  });
  return (
    <section class="py-3">
      <h3 class="text-base font-bold tracking-tight">
        <A class="text-emerald-700 hover:underline dark:text-emerald-400" href={`/${props.owner}`}>
          {props.owner}
        </A>
        <Show when={getDoc()}>
          {(doc) => (
            <span class="muted ml-2 text-xs font-normal">
              {doc().repos.length} repositor{doc().repos.length === 1 ? "y" : "ies"}
            </span>
          )}
        </Show>
      </h3>
      <Show when={getDoc()} fallback={<p class="muted text-sm">loading…</p>}>
        {(doc) => {
          const ordered = orderByActivity(doc().repos);
          const { shown, extra } = pageSlice(ordered, MAX_REPOS_PER_OWNER);
          return (
            <>
              <Show when={shown.length > 0} fallback={<p class="muted mt-1 text-sm">nothing under {props.owner} yet</p>}>
                <ul class="mt-1 grid grid-cols-1 gap-x-6 gap-y-1 sm:grid-cols-2">
                  <For each={shown}>
                    {(row) => <RepoRow owner={props.owner} name={row.name} at={row.last_commit_time} empty={row.size_bytes === 0} mirror={row.mirror} mirrorUpstream={row.mirror_upstream} />}
                  </For>
                </ul>
              </Show>
              <Show when={extra > 0}>
                <p class="mt-2 text-sm">
                  <A class="text-emerald-700 hover:underline dark:text-emerald-400" href={`/${props.owner}`}>
                    +{extra} more →
                  </A>
                </p>
              </Show>
            </>
          );
        }}
      </Show>
    </section>
  );
}

export default function Owners() {
  // Server-ordered owner names (#283 rollup — correct past the cap and on
  // first paint); the client re-rank below refines as section docs land.
  const [getOwners] = useData("owners", () => repos.owners.list({ sort: "activity", order: "desc" }));
  const [getMe] = useData("me", () => repos.me().catch(() => null));
  // Per-owner newest-commit times reported by OwnerSections as their
  // detailed docs land (Forgejo #283). Missing key = fetch pending;
  // null = settled with no known activity — both sort last.
  const [getActivity, setActivity] = createSignal({});
  const reportActivity = (owner, at) => {
    setActivity((prev) => (prev[owner] === at ? prev : { ...prev, [owner]: at }));
  };
  const canWrite = () => {
    const me = getMe();
    if (!me) return false;
    if (me.anonymous) return false;
    return me.write !== false;
  };
  return (
    <div class="owners-page">
      <div class="mb-4 flex items-center justify-between">
        <h2 class="text-xl font-semibold">Owners</h2>
        <div class="flex gap-2">
          <Show when={canWrite()}>
            <A class="btn primary px-3 py-1" href="/new">
              New repository
            </A>
          </Show>
          <A class="btn px-3 py-1" href="/import">
            Import repository
          </A>
        </div>
      </div>
      <section class="card mb-6 p-4">
        <p class="text-sm leading-relaxed">
          <strong>walhub</strong> is a git host whose only database is an object store.{" "}
          <A class="text-emerald-700 hover:underline dark:text-emerald-400" href="/">
            What is walhub? →
          </A>
        </p>
      </section>
      <Show when={getOwners()} fallback={<p class="muted">loading…</p>}>
        {(owners) => {
          const ordered = orderOwnersByActivity(owners(), getActivity());
          const { shown, extra } = pageSlice(ordered, MAX_OWNERS);
          return (
            <Show
              when={shown.length > 0}
              fallback={<p class="muted">no repositories yet — push one, or use the API to create it</p>}
            >
              <div class="divide-y divide-zinc-200 dark:divide-zinc-800">
                <For each={shown}>{(o) => <OwnerSection owner={o} onActivity={reportActivity} />}</For>
              </div>
              <Show when={extra > 0}>
                <p class="muted mt-4 text-sm">
                  showing most active {shown.length} of {owners().length} owners
                </p>
              </Show>
            </Show>
          );
        }}
      </Show>
    </div>
  );
}
