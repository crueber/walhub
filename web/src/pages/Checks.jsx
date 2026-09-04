// web/src/pages/Checks.jsx — route "/:owner/:name/checks" (05 §9 + 08 §1):
// the checks table page (paged index, state pill per sha, expandable
// per-context rows, filter by state/context). Live `check` frames ride
// the ONE repo collaboration stream (08 §4) and invalidate the index +
// sha views coalesced. Also exports CheckPill, the shared
// combined-state pill used by the commit pages and the PR head box.

import { createSignal, For, Show } from "solid-js";
import { A } from "@solidjs/router";
import { useRepo, fmtDate } from "./Repo.jsx";
import { useData, invalidate } from "../lib/data.js";
import { useCollabStream } from "../components/collab.jsx";
import Empty from "../components/Empty.jsx";

const DOT = {
  success: "bg-emerald-500",
  pending: "bg-amber-400",
  failure: "bg-red-500",
  error: "bg-red-700",
};

export function stateDot(state) {
  return DOT[state] ?? "bg-zinc-400";
}

export function stateLabel(state) {
  return state ?? "pending";
}

/** CheckPill fetches the combined view for one sha and renders the
 *  colored dot + label (no-store server-side, so this uses the default
 *  TTL, never SHA_TTL — sha-addressed does NOT mean immutable here).
 *  Links to the per-sha detail page (08 §1). */
export function CheckPill(props) {
  const key = () => `checks:${props.full}:${props.sha}`;
  const [getView] = useData(key, () => props.client.checks.combined(props.sha));
  return (
    <A
      class="inline-flex items-center gap-1.5 hover:underline"
      href={`/${props.full}/checks/${props.sha}`}
      title={`checks: ${getView()?.state ?? "…"}`}
    >
      <Show when={getView()} fallback={<span class="inline-block h-2.5 w-2.5 rounded-full bg-zinc-300 dark:bg-zinc-600" aria-label="checks loading" />}>
        <span class={`inline-block h-2.5 w-2.5 rounded-full ${stateDot(getView().state)}`} aria-label={`checks ${getView().state}`} />
        <Show when={props.verbose}>
          <span class="text-xs text-zinc-500 dark:text-zinc-400">{stateLabel(getView().state)}</span>
        </Show>
      </Show>
    </A>
  );
}

/** ContextRows expands one sha into its per-context list with
 *  target_url links and descriptions. */
export function ContextRows(props) {
  const key = () => `statuses:${props.full}:${props.sha}`;
  const [getView] = useData(key, () => props.client.checks.statuses(props.sha));
  return (
    <Show when={getView()} fallback={<p class="muted px-3 py-2 text-xs">loading contexts…</p>}>
      <ul class="divide-y divide-zinc-100 dark:divide-zinc-800/60">
        <For each={getView().statuses ?? []} fallback={<li class="muted px-3 py-2 text-xs">no contexts reported</li>}>
          {(st) => (
            <li class="flex flex-wrap items-center gap-2 px-3 py-1.5 text-sm">
              <span class={`inline-block h-2.5 w-2.5 rounded-full ${stateDot(st.state)}`} title={st.state} />
              <code class="font-mono text-xs">{st.context}</code>
              <span class="text-xs text-zinc-500 dark:text-zinc-400">{st.state}</span>
              <Show when={st.description}>
                <span class="text-xs text-zinc-600 dark:text-zinc-300">{st.description}</span>
              </Show>
              <Show when={st.target_url}>
                <a class="link text-xs" href={st.target_url} target="_blank" rel="noreferrer">
                  details
                </a>
              </Show>
              <span class="muted ml-auto text-xs">
                {st.creator} · {fmtDate(st.updated_at)}
              </span>
            </li>
          )}
        </For>
      </ul>
    </Show>
  );
}

function ShaRow(props) {
  const row = () => props.row;
  const [getOpen, setOpen] = createSignal(false);
  const state = () => row().state;
  const contexts = () => row().contexts ?? [];
  const visible = () => {
    if (props.stateFilter && state() !== props.stateFilter) return false;
    if (props.contextFilter && !contexts().some((c) => c.name.includes(props.contextFilter))) return false;
    return true;
  };
  return (
    <Show when={visible()}>
      <li class="card !p-0">
        <div class="flex flex-wrap items-center gap-2 px-3 py-2">
          <span class={`inline-block h-2.5 w-2.5 rounded-full ${stateDot(state())}`} title={state()} />
          <A class="font-mono text-xs hover:underline" href={`/${props.full}/commit/${row().sha}`}>
            {String(row().sha).slice(0, 12)}
          </A>
          <span class="text-xs text-zinc-500 dark:text-zinc-400">{stateLabel(state())}</span>
          <span class="muted text-xs">
            {(contexts().length ? `${contexts().length} contexts` : "no contexts")} · {fmtDate(row().updated_at)}
          </span>
          <button type="button" class="link ml-auto text-xs" onClick={() => setOpen(!getOpen())}>
            {getOpen() ? "collapse" : "expand"}
          </button>
        </div>
        <Show when={getOpen()}>
          <ContextRows full={props.full} sha={row().sha} client={props.client} />
        </Show>
      </li>
    </Show>
  );
}

export default function Checks() {
  const ctx = useRepo();
  const [getAfter, setAfter] = createSignal("");
  const [getStateFilter, setStateFilter] = createSignal("");
  const [getContextFilter, setContextFilter] = createSignal("");
  const query = () => ({ ...(getAfter() ? { after: getAfter() } : {}), n: 50 });
  const key = () => `checks:${ctx.full}:${JSON.stringify(query())}`;
  const [getPage] = useData(key, () => ctx.repoClient.checks.list(query()));
  const reload = () => invalidate(key());

  // Live rows: `check` frames invalidate the index + sha views (coalesced —
  // a 30-run CI burst refetches once per key per tick, never 30×).
  useCollabStream(() => ctx.full, ctx.repoClient, ["check"]);

  return (
    <div class="checks-page">
      <div class="mb-3 flex flex-wrap items-center gap-2">
        <h2 class="text-lg font-semibold">Checks</h2>
        <div class="ml-auto flex flex-wrap items-center gap-2">
          <select
            class="input w-auto px-2 py-1 text-sm"
            value={getStateFilter()}
            onInput={(e) => setStateFilter(e.target.value)}
            aria-label="Filter by state"
          >
            <option value="">all states</option>
            <option value="pending">pending</option>
            <option value="success">success</option>
            <option value="failure">failure</option>
            <option value="error">error</option>
          </select>
          <input
            class="input w-44 px-2 py-1 text-sm"
            placeholder="filter by context…"
            value={getContextFilter()}
            onInput={(e) => setContextFilter(e.target.value)}
            aria-label="Filter by context"
          />
          <button type="button" class="btn px-2 py-1" onClick={reload}>
            refresh
          </button>
        </div>
      </div>
      <Show when={getPage()} fallback={<p class="muted">loading checks…</p>}>
        <Show
          when={(getPage().checks ?? []).length > 0}
          fallback={
            <Empty
              icon="check"
              title="No checks reported yet"
              hint="Check runs posted against commits in this repo will appear here — per-sha state with expandable per-context rows."
            />
          }
        >
          <ul class="card-list space-y-2">
            <For each={getPage().checks ?? []}>
              {(row) => (
                <ShaRow
                  full={ctx.full}
                  row={row}
                  client={ctx.repoClient}
                  stateFilter={getStateFilter()}
                  contextFilter={getContextFilter()}
                />
              )}
            </For>
          </ul>
        </Show>
        <Show when={getPage().more}>
          <div class="pager mt-3">
            <button
              type="button"
              class="pill cursor-pointer hover:no-underline"
              onClick={() => {
                const rows = getPage().checks ?? [];
                setAfter(rows.length ? rows[rows.length - 1].sha : "");
              }}
            >
              older →
            </button>
          </div>
        </Show>
      </Show>
    </div>
  );
}
