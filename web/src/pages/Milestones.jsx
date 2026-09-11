// web/src/pages/Milestones.jsx — route "/:owner/:name/milestones" (02
// §11): milestone CRUD + progress bars (progress derived server-side,
// never stored) + per-milestone linked issues (issue #119: each
// milestone lists and links its issues via the existing server-side
// `milestone=` list filter — no client-side filtering of the world).
// Writes are triage-gated; delete 409s while open issues reference the
// milestone. Refetches after every save.
//
// Layout (issue #314): open milestones render as full cards — plain
// title heading + state chip + counts, progress bar, <MilestoneIssues>
// list, then a footer row with an explicit "View N issues" button
// (left, carries the filter affordance the old title-link hid) and
// Close/Delete right-aligned (Delete = btn danger). Closed milestones
// collapse to a single line each (linked title → issue filter, Reopen
// button, counts) in their own "Closed" section below the open cards:
// no bar, no list, no Delete. Dropping Delete from the closed shape is
// safe — a closed milestone deletes after reopening, or via the API.

import { createSignal, For, Show } from "solid-js";
import { A } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";
import { splitMilestones, milestoneFilterHref, milestoneTotal } from "../lib/milestones.js";

// MilestoneIssues — the linked-issue list for one milestone (02 §7
// `GET …/issues?milestone=<id>`, server-side filter; cards carry the
// milestone id per the §2 card shape). One no-store list window per
// milestone (n=100); milestones are human-rate so the parallel fetch
// fan-out stays small.
function MilestoneIssues(props) {
  const ctx = useRepo();
  const key = () => `issues:${ctx.full}:milestone:${props.id}`;
  const [getPage] = useData(key, () => ctx.repoClient.issues.list({ milestone: props.id, n: 100 }));
  return (
    <Show when={getPage()} fallback={<p class="muted px-1 text-xs">loading issues…</p>}>
      {(page) => (
        <ul class="grid gap-1">
          <For each={page().issues ?? []} fallback={<li class="muted px-1 text-xs">no issues on this milestone</li>}>
            {(issue) => (
              <li class="flex flex-wrap items-baseline gap-2 px-1 text-sm">
                <span class={issue.state === "open" ? "chip chip-open" : "chip chip-closed"}>{issue.state}</span>
                <A
                  class="font-medium text-emerald-700 hover:underline dark:text-emerald-400"
                  href={`/${ctx.full}/issues/${issue.num}`}
                >
                  #{issue.num} {issue.title}
                </A>
              </li>
            )}
          </For>
        </ul>
      )}
    </Show>
  );
}

export default function Milestones() {
  const ctx = useRepo();
  const key = () => `milestones:${ctx.full}`;
  const [getSet] = useData(key, () => ctx.repoClient.milestones.list());
  const [getTitle, setTitle] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);

  const reload = () => invalidate(key());

  const create = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      await ctx.repoClient.milestones.create({ title: getTitle() });
      setTitle("");
      reload();
    } catch (err) {
      reportError(err, "milestone-create");
    } finally {
      setBusy(false);
    }
  };

  const close = async (m) => {
    try {
      await ctx.repoClient.milestones.update(m.id, { state: m.state === "open" ? "closed" : "open" });
      reload();
    } catch (err) {
      reportError(err, "milestone-update");
    }
  };

  const remove = async (id) => {
    try {
      await ctx.repoClient.milestones.delete(id);
      reload();
    } catch (err) {
      reportError(err, "milestone-delete");
    }
  };

  // viewLabel — the open-card footer affordance (issue #314): always
  // rendered, even at 0 issues, so the filter entry point is
  // consistent ("View 0 issues" lands on the empty filtered list).
  const viewLabel = (m) => {
    const n = milestoneTotal(m);
    return `View ${n} issue${n === 1 ? "" : "s"}`;
  };

  return (
    <div class="milestones-page mx-auto max-w-2xl">
      <h2 class="mb-3 text-lg font-semibold">Milestones</h2>
      <Show when={getSet()} fallback={<p class="muted">loading…</p>}>
        {(s) => {
          const split = () => splitMilestones(s().milestones);
          return (
            <>
              <ul class="mb-4 grid gap-2">
                <For
                  each={split().open}
                  fallback={
                    <li class="muted">{split().closed.length > 0 ? "no open milestones" : "no milestones yet"}</li>
                  }
                >
                  {(m) => (
                    <li class="card grid gap-1 p-3">
                      <div class="flex items-baseline gap-2">
                        <h3 class="font-medium">{m.title}</h3>
                        <span class="chip">{m.state}</span>
                        <span class="muted ml-auto text-xs">
                          {m.open_issues} open · {m.closed_issues} closed
                        </span>
                      </div>
                      <div
                        class="h-1.5 overflow-hidden rounded bg-zinc-200 dark:bg-zinc-800"
                        role="progressbar"
                        aria-valuenow={m.percent}
                        aria-valuemin="0"
                        aria-valuemax="100"
                      >
                        <div class="h-full bg-emerald-500" style={{ width: `${m.percent ?? 0}%` }} />
                      </div>
                      <MilestoneIssues id={m.id} />
                      <div class="flex flex-wrap items-center gap-1">
                        <A
                          class="btn px-2 py-0.5 text-xs"
                          href={milestoneFilterHref(ctx.full, m.id)}
                          title={`issues on milestone ${m.title}`}
                        >
                          {viewLabel(m)}
                        </A>
                        <span class="ml-auto flex gap-1">
                          <button type="button" class="btn px-2 py-0.5 text-xs" onClick={() => close(m)}>
                            Close
                          </button>
                          <button
                            type="button"
                            class="btn danger px-2 py-0.5 text-xs"
                            onClick={() => remove(m.id)}
                          >
                            Delete
                          </button>
                        </span>
                      </div>
                    </li>
                  )}
                </For>
              </ul>
              <Show when={split().closed.length > 0}>
                <h3 class="muted mb-1 text-xs font-semibold uppercase tracking-wide">Closed</h3>
                <ul class="mb-4 grid gap-1">
                  <For each={split().closed}>
                    {(m) => (
                      <li class="card flex flex-wrap items-baseline gap-x-2 gap-y-1 px-3 py-2">
                        <A
                          class="min-w-0 flex-1 truncate font-medium text-emerald-700 hover:underline dark:text-emerald-400"
                          href={milestoneFilterHref(ctx.full, m.id)}
                          title={`issues on milestone ${m.title}`}
                        >
                          {m.title}
                        </A>
                        <button type="button" class="btn px-2 py-0.5 text-xs" onClick={() => close(m)}>
                          Reopen
                        </button>
                        <span class="muted ml-auto text-xs">
                          {m.open_issues} open · {m.closed_issues} closed
                        </span>
                      </li>
                    )}
                  </For>
                </ul>
              </Show>
            </>
          );
        }}
      </Show>
      <form class="card flex flex-wrap gap-2 p-3" onSubmit={create}>
        <input
          class="input flex-1"
          value={getTitle()}
          onInput={(e) => setTitle(e.target.value)}
          maxlength="256"
          required
          placeholder="v1.1 (triage)"
          aria-label="milestone title"
        />
        <button type="submit" class="btn primary" disabled={getBusy()}>
          Create
        </button>
      </form>
    </div>
  );
}
