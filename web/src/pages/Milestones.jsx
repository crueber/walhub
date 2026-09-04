// web/src/pages/Milestones.jsx — route "/:owner/:name/milestones" (02
// §11): milestone CRUD + progress bars (progress derived server-side,
// never stored). Writes are triage-gated; delete 409s while open issues
// reference the milestone. Refetches after every save.

import { createSignal, For, Show } from "solid-js";
import { useRepo } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";

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
                    <span class="font-medium">{m.title}</span>
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
