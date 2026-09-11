// web/src/pages/Labels.jsx — route "/:owner/:name/labels" (02 §11): label
// CRUD. Reads are public; writes are triage-gated server-side (the form
// surfaces 403s plainly). Refetches after every save.

import { createSignal, For, Show } from "solid-js";
import { useRepo } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";
import { LABEL_PACKS, missingFromPack } from "../lib/label-packs.js";
import LabelColorPicker from "../components/LabelColorPicker.jsx";

export default function Labels() {
  const ctx = useRepo();
  const key = () => `labels:${ctx.full}`;
  const [getSet] = useData(key, () => ctx.repoClient.labels.list());
  const [getName, setName] = createSignal("");
  const [getColor, setColor] = createSignal("d73a4a");
  const [getBusy, setBusy] = createSignal(false);
  const [getPackBusy, setPackBusy] = createSignal(null);

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

  // One-click label pack (issue #324): sequential creates through the
  // existing one-per-request endpoint — no bulk endpoint, no backend
  // change. Human-rate (8 requests), triage enforced per request by the
  // server; the buttons render for everyone exactly like the create form
  // below (a non-triage click 403s into the error tray). Names already
  // present are skipped, so re-adding is idempotent-ish; a mid-pack
  // failure reports and keeps going, and the reload shows what landed.
  const addPack = async (pack) => {
    if (getPackBusy()) return;
    const existing = (getSet()?.labels ?? []).map((l) => l.name);
    const todo = missingFromPack(pack.labels, existing);
    if (todo.length === 0) return;
    setPackBusy(pack.id);
    try {
      for (const l of todo) {
        try {
          await ctx.repoClient.labels.create({
            name: l.name,
            color: l.color,
            description: l.description,
          });
        } catch (err) {
          reportError(err, "label-pack");
        }
      }
      reload();
    } finally {
      setPackBusy(null);
    }
  };

  return (
    <div class="labels-page mx-auto max-w-2xl">
      <h2 class="mb-3 text-lg font-semibold">Labels</h2>
      <Show when={getSet()} fallback={<p class="muted">loading…</p>}>
        {(s) => (
          <>
            <Show when={(s().labels ?? []).length === 0}>
              <div class="mb-4 grid gap-2 sm:grid-cols-2">
                <For each={LABEL_PACKS}>
                  {(pack) => (
                    <div class="card grid gap-2 p-3">
                      <div class="flex items-baseline gap-2">
                        <h3 class="text-sm font-medium">{pack.title}</h3>
                        <span class="muted text-xs">{pack.labels.length} labels</span>
                      </div>
                      <ul class="flex flex-wrap gap-1">
                        <For each={pack.labels}>
                          {(l) => (
                            <li
                              class="inline-flex items-center gap-1 rounded-full border border-zinc-300 px-2 py-0.5 text-xs dark:border-zinc-700"
                              title={l.description ?? l.name}
                            >
                              <span
                                class="inline-block h-2 w-2 rounded-full"
                                style={{ "background-color": `#${l.color}` }}
                                aria-hidden="true"
                              />
                              {l.name}
                            </li>
                          )}
                        </For>
                      </ul>
                      <button
                        type="button"
                        class="btn primary mt-1 text-sm"
                        disabled={getPackBusy() !== null}
                        onClick={() => addPack(pack)}
                      >
                        {getPackBusy() === pack.id ? "Adding…" : `Add ${pack.title}`}
                      </button>
                    </div>
                  )}
                </For>
              </div>
            </Show>
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
          </>
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
          <LabelColorPicker color={getColor()} onChange={setColor} />
          <button type="submit" class="btn primary" disabled={getBusy()}>
            Create
          </button>
        </div>
      </form>
    </div>
  );
}
