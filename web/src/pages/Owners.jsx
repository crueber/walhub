// web/src/pages/Owners.jsx — route "/": intro + every owner with their repos.
// Owner/repo names come from the store-backed core listing endpoints
// (GET /api/v1/owners, GET /api/v1/owners/{owner}/repos — 07 §8); the page
// adds newest-first ordering, per-section caps (lib/owners.js), and the
// intro card. Star counts ride the shared `social:{o}/{r}` cache entries
// (<StarCount>, lib/stars.js). No new endpoint, no new SDK method
// (issues #117, #137).

import repos from "../../sdk/src/index.js";
import { For, Show } from "solid-js";
import { A } from "@solidjs/router";
import { useData } from "../lib/data.js";
import StarCount from "../components/StarCount.jsx";
import {
  MAX_OWNERS,
  MAX_REPOS_PER_OWNER,
  newestFirst,
  pageSlice,
} from "../lib/owners.js";

/** One owner's section: heading + capped repo list (own `repos:{owner}` cache key, shared with /:owner). */
function OwnerSection(props) {
  const [getRepos] = useData(`repos:${props.owner}`, () => repos.owners.repos(props.owner));
  return (
    <section class="py-3">
      <h3 class="text-sm font-semibold">
        <A class="text-emerald-700 hover:underline dark:text-emerald-400" href={`/${props.owner}`}>
          {props.owner}
        </A>
        <Show when={getRepos()}>
          {(names) => (
            <span class="muted ml-2 text-xs font-normal">
              {names().length} repositor{names().length === 1 ? "y" : "ies"}
            </span>
          )}
        </Show>
      </h3>
      <Show when={getRepos()} fallback={<p class="muted text-sm">loading…</p>}>
        {(names) => {
          const ordered = newestFirst(names());
          const { shown, extra } = pageSlice(ordered, MAX_REPOS_PER_OWNER);
          return (
            <>
              <Show when={shown.length > 0} fallback={<p class="muted mt-1 text-sm">nothing under {props.owner} yet</p>}>
                <ul class="mt-1 space-y-1">
                  <For each={shown}>
                    {(n) => (
                      <li class="flex flex-wrap items-baseline gap-x-1.5">
                        <A
                          class="text-sm text-emerald-700 hover:underline dark:text-emerald-400"
                          href={`/${props.owner}/${n}`}
                        >
                          {props.owner}/{n}
                        </A>
                        <StarCount full={`${props.owner}/${n}`} />
                      </li>
                    )}
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
  const [getOwners] = useData("owners", () => repos.owners.list());
  return (
    <div class="owners-page">
      <div class="mb-4 flex items-center justify-between">
        <h2 class="text-xl font-semibold">Owners</h2>
        <A class="btn primary px-3 py-1" href="/import">
          Import repository
        </A>
      </div>
      <section class="card mb-6 p-4">
        <p class="text-sm leading-relaxed">
          <strong>walhub</strong> is a git host whose only database is an object store: every
          repository's refs, packs, config, and policy live as objects in a bucket (filesystem,
          S3, or GCS). Push over smart HTTP, browse code and manage everything from this UI —
          instances are disposable; wipe one and you lose nothing but warmth.
        </p>
      </section>
      <Show when={getOwners()} fallback={<p class="muted">loading…</p>}>
        {(owners) => {
          const ordered = newestFirst(owners());
          const { shown, extra } = pageSlice(ordered, MAX_OWNERS);
          return (
            <Show
              when={shown.length > 0}
              fallback={<p class="muted">no repositories yet — push one, or use the API to create it</p>}
            >
              <div class="divide-y divide-zinc-200 dark:divide-zinc-800">
                <For each={shown}>{(o) => <OwnerSection owner={o} />}</For>
              </div>
              <Show when={extra > 0}>
                <p class="muted mt-4 text-sm">
                  showing newest {shown.length} of {owners().length} owners
                </p>
              </Show>
            </Show>
          );
        }}
      </Show>
    </div>
  );
}
