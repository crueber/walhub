// web/src/pages/Milestones.jsx — route "/:owner/:name/milestones" (02
// §11): milestone CRUD + progress bars (progress derived server-side,
// never stored) + per-milestone linked issues (issue #119: each
// milestone lists and links its issues via the existing server-side
// `milestone=` list filter — no client-side filtering of the world).
// Writes are triage-gated; delete 409s while open issues reference the
// milestone. Refetches after every save.

import { createSignal, For, Show } from "solid-js";
import { A } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";

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

  return (
    <div class="milestones-page mx-auto max-w-2xl">
      <h2 class="mb-3 text-lg font-semibold">Milestones</h2>
      <Show when={getSet()} fallback={<p class="muted">loading…</p>}>
        {(s) => (
          <ul class="mb-4 grid gap-2">
            <For each={s().milestones ?? []} fallback={<li class="muted">no milestones yet</li>}>
              {(m) => (
                <li class="card grid gap-1 p-3">
                  <div class="flex items-baseline gap-2">
                    <A
                      class="font-medium text-emerald-700 hover:underline dark:text-emerald-400"
                      href={`/${ctx.full}/issues?milestone=${encodeURIComponent(m.id)}`}
                      title={`issues on milestone ${m.title}`}
                    >
                      {m.title}
                    </A>
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
                  <div class="flex gap-1">
                    <button type="button" class="btn px-2 py-0.5 text-xs" onClick={() => close(m)}>
                      {m.state === "open" ? "Close" : "Reopen"}
                    </button>
                    <button type="button" class="btn px-2 py-0.5 text-xs" onClick={() => remove(m.id)}>
                      Delete
                    </button>
                  </div>
                </li>
              )}
            </For>
          </ul>
        )}
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
