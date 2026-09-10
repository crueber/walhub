// web/src/pages/Owners.jsx — route "/explore": intro + the TOP 5 most active owners.
// Owner/repo names come from the store-backed core listing endpoints
// (GET /api/v1/owners/detailed?sort=activity&order=desc — 07 §8, Forgejo
// #283: the server ranks ALL owners by the per-owner max-commit rollup
// before the client's slice, so an active owner past any cap still surfaces
// and first paint is already ordered; the rows carry each owner's
// last_commit_time, so the page filters to active owners WITHOUT extra
// GETs — Forgejo #295; per-owner rows from GET
// /api/v1/owners/{owner}/repos/detailed?sort=activity&order=desc — 07 §8,
// Forgejo #247: true most-recent-commit order server-side, stabilized
// client-side by lib/owners.js orderByActivity). Owner SECTIONS keep the
// client re-rank as fallback/enhancement (Forgejo #283): each section
// reports its newest row time (lib/owners.js ownerActivity over the same
// detailed doc it already fetched — zero extra GETs) and the page re-ranks
// the shown sections via orderOwnersByActivity as docs land, which also
// heals a stale catalog rollup with fresher per-section times. Sections
// that never report (fetch pending) sort last with a deterministic name
// tiebreak; until any section reports, not-yet-loaded owners keep the
// server order — first paint is server-ordered and only refines once, then
// settles. Inactive owners (null last_commit_time) never mount at all.
// The page adds per-section caps (lib/owners.js) and the intro card, which
// carries the instance owner total (the payload's uncapped row count —
// never the page's slice). Star
// counts ride the shared `social:{o}/{r}` cache entries (<StarCount>,
// lib/stars.js) and last-active stamps render from the listing rows
// (<ActivityStamp at/empty props> — no per-row commits fetch on this page;
// see the component header). Rows share <RepoRow> with `/:owner`
// (Repos.jsx) in a responsive two-column grid (one column on narrow
// widths). No new deps (issues #117, #137, #142).
//
// CLOSED (Forgejo #307): the instance repo total now rides the
// owners/detailed rows (`repo_count` per row — the manifest-gated live-repo
// count, ghost-filtered like liveRepos). The intro card sums it over the
// uncapped payload via lib/owners.js instanceRepoTotal — never the top-5
// slice, never a per-owner listing walk.

import repos from "../../sdk/src/index.js";
import { For, Show, createEffect, createSignal } from "solid-js";
import { A } from "@solidjs/router";
import { useData } from "../lib/data.js";
import { RepoRow } from "./Repos.jsx";
import {
  MAX_OWNERS,
  MAX_REPOS_PER_OWNER,
  activeOwnerNames,
  hasKnownActivity,
  instanceRepoTotal,
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
  // Server-ranked per-owner activity rows (#283 rollup — correct past any
  // cap and on first paint); the client re-rank below refines the shown
  // sections as their docs land. The payload's row count is the instance
  // owner total (uncapped — totals never derive from the page's slice).
  const [getOwners] = useData("owners", () =>
    repos.owners.listDetailed({ sort: "activity", order: "desc" }),
  );
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
        <Show when={getOwners()}>
          {(doc) => {
            const rows = doc().owners;
            const owners = Array.isArray(rows) ? rows.length : 0;
            // Instance repo total (Forgejo #307): the sum of the served
            // repo_count fields over the UNCAPPED payload rows — never the
            // top-5 slice, never a per-owner listing walk.
            const repos = instanceRepoTotal(rows);
            return (
              <p class="muted mt-2 text-sm">
                Home to {owners} owner{owners === 1 ? "" : "s"} and {repos} repositor{repos === 1 ? "y" : "ies"} — showing the most active below.
              </p>
            );
          }}
        </Show>
      </section>
      <Show when={getOwners()} fallback={<p class="muted">loading…</p>}>
        {(doc) => {
          const payload = doc();
          const rows = Array.isArray(payload.owners) ? payload.owners : [];
          // Active-only, server order first (#295 over the #283 rows):
          // owners with no known commit time never reach the slice. Until
          // a section reports a known time there is nothing to re-rank
          // with, and re-sorting an all-unknown map would fall back to
          // name order — discarding the server's activity ranking before
          // the slice (Forgejo #283 follow-up).
          const ranked = activeOwnerNames(rows);
          const activity = getActivity();
          const ordered = hasKnownActivity(activity)
            ? orderOwnersByActivity(ranked, activity)
            : ranked;
          const { shown, extra } = pageSlice(ordered, MAX_OWNERS);
          return (
            <Show
              when={shown.length > 0}
              fallback={
                rows.length > 0 ? (
                  <p class="muted">no commit activity yet — push a commit and the active owners land here</p>
                ) : (
                  <p class="muted">no repositories yet — push one, or use the API to create it</p>
                )
              }
            >
              <div class="divide-y divide-zinc-200 dark:divide-zinc-800">
                <For each={shown}>{(o) => <OwnerSection owner={o} onActivity={reportActivity} />}</For>
              </div>
              <Show when={extra > 0}>
                <p class="muted mt-4 text-sm">
                  showing top {shown.length} of {ranked.length} active owners
                </p>
              </Show>
            </Show>
          );
        }}
      </Show>
    </div>
  );
}
