// web/src/pages/Labels.jsx — route "/:owner/:name/labels" (02 §11): label
// CRUD. Reads are public; writes are triage-gated server-side (the form
// surfaces 403s plainly). Refetches after every save.

import { createSignal, For, Show } from "solid-js";
import { useRepo } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";

export default function Labels() {
  const ctx = useRepo();
  const key = () => `labels:${ctx.full}`;
  const [getSet] = useData(key, () => ctx.repoClient.labels.list());
  const [getName, setName] = createSignal("");
  const [getColor, setColor] = createSignal("d73a4a");
  const [getBusy, setBusy] = createSignal(false);

  const reload = () => invalidate(key());

  const create = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      await ctx.repoClient.labels.create({ name: getName(), color: getColor() });
      setName("");
      reload();
    } catch (err) {
      reportError(err, "label-create");
    } finally {
      setBusy(false);
    }
  };

  const remove = async (name) => {
    try {
      const res = await ctx.repoClient.labels.delete(name);
      reportError(`label deleted — ${res.threads_affected ?? 0} threads updated`, "label-delete");
      reload();
    } catch (err) {
      reportError(err, "label-delete");
    }
  };

  return (
    <div class="labels-page mx-auto max-w-2xl">
      <h2 class="mb-3 text-lg font-semibold">Labels</h2>
      <Show when={getSet()} fallback={<p class="muted">loading…</p>}>
        {(s) => (
          <ul class="mb-4 grid gap-2">
            <For each={s().labels ?? []} fallback={<li class="muted">no labels yet</li>}>
              {(l) => (
                <li class="card flex items-center gap-2 p-3">
                  <span
                    class="inline-block h-3 w-3 rounded-full border border-zinc-300 dark:border-zinc-700"
                    style={{ "background-color": `#${l.color}` }}
                    aria-hidden="true"
                  />
                  <span class="font-medium">{l.name}</span>
                  <Show when={l.description}>
                    <span class="muted text-sm">{l.description}</span>
                  </Show>
                  <button type="button" class="btn ml-auto px-2 py-0.5 text-xs" onClick={() => remove(l.name)}>
                    Delete
                  </button>
                </li>
              )}
            </For>
          </ul>
        )}
      </Show>
      <form class="card grid gap-2 p-3" onSubmit={create}>
        <h3 class="text-sm font-medium">New label (triage)</h3>
        <div class="flex flex-wrap gap-2">
          <input
            class="input w-40"
            value={getName()}
            onInput={(e) => setName(e.target.value)}
            maxlength="64"
            required
            placeholder="bug"
            aria-label="label name"
          />
          <input
            class="input w-28"
            value={getColor()}
            onInput={(e) => setColor(e.target.value)}
            pattern="[0-9a-fA-F]{6}"
            title="6-hex RGB without #"
            aria-label="label color"
          />
          <button type="submit" class="btn primary" disabled={getBusy()}>
            Create
          </button>
        </div>
      </form>
    </div>
  );
}
