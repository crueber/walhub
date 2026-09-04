// web/src/pages/ReleaseNew.jsx — route "/:owner/:name/releases/new"
// (07 §8): tag picker fed by the existing tags ref stream, autodraft
// button filling the body from merged PRs, draft/prerelease checkboxes,
// create → detail.

import { createSignal, For, Show } from "solid-js";
import { useNavigate } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { useData, reportError } from "../lib/data.js";

export default function ReleaseNew() {
  const ctx = useRepo();
  const navigate = useNavigate();
  const [getTags] = useData(`tags:${ctx.full}`, () => ctx.repoClient.tags({ n: 100 }));
  const [getTag, setTag] = createSignal("");
  const [getName, setName] = createSignal("");
  const [getBody, setBody] = createSignal("");
  const [getDraft, setDraft] = createSignal(false);
  const [getPrerelease, setPrerelease] = createSignal(false);
  const [getBusy, setBusy] = createSignal(false);
  const [getSince, setSince] = createSignal("");

  const fillAutodraft = async () => {
    if (!getTag()) return;
    setBusy(true);
    try {
      const ad = await ctx.repoClient.releases.autodraft({ tag: getTag() });
      setBody(ad.body ?? "");
      setSince(ad.since ?? "");
      if (!getName()) setName(getTag());
    } catch (err) {
      reportError(err, "autodraft");
    } finally {
      setBusy(false);
    }
  };

  const create = async (e) => {
    e.preventDefault();
    if (!getTag()) return;
    setBusy(true);
    try {
      const rel = await ctx.repoClient.releases.put(getTag(), {
        name: getName().trim() || undefined,
        body: getBody(),
        draft: getDraft(),
        prerelease: getPrerelease(),
      });
      navigate(`/${ctx.full}/releases/${encodeURIComponent(rel.tag)}`);
    } catch (err) {
      reportError(err, "release");
    } finally {
      setBusy(false);
    }
  };

  const tags = () => getTags()?.refs ?? getTags()?.tags ?? [];

  return (
    <form class="card grid max-w-2xl gap-3" onSubmit={create} aria-label="New release">
      <h1 class="text-lg font-semibold">New release</h1>
      <label class="grid gap-1">
        <span class="text-sm font-medium">Tag (must exist)</span>
        <input
          class="input font-mono"
          list="release-tags"
          value={getTag()}
          onInput={(e) => setTag(e.currentTarget.value.trim())}
          placeholder="v1.0.0"
          autocomplete="off"
        />
        <datalist id="release-tags">
          <For each={tags()}>{(t) => <option value={String(t.name ?? t).replace(/^refs\/tags\//, "")} />}</For>
        </datalist>
      </label>
      <label class="grid gap-1">
        <span class="text-sm font-medium">Title (defaults to the tag)</span>
        <input class="input" value={getName()} onInput={(e) => setName(e.currentTarget.value)} />
      </label>
      <label class="grid gap-1">
        <span class="text-sm font-medium">
          Notes
          <Show when={getSince()}>
            <span class="muted"> (since {getSince()})</span>
          </Show>
        </span>
        <textarea class="input font-mono" rows="10" value={getBody()} onInput={(e) => setBody(e.currentTarget.value)} />
      </label>
      <div class="flex gap-4">
        <label class="flex items-center gap-1 text-sm">
          <input type="checkbox" checked={getDraft()} onChange={(e) => setDraft(e.currentTarget.checked)} />
          draft
        </label>
        <label class="flex items-center gap-1 text-sm">
          <input type="checkbox" checked={getPrerelease()} onChange={(e) => setPrerelease(e.currentTarget.checked)} />
          prerelease
        </label>
      </div>
      <div class="flex gap-2">
        <button type="button" class="btn px-2 py-1" disabled={getBusy() || !getTag()} onClick={fillAutodraft}>
          autodraft from merged PRs
        </button>
        <button type="submit" class="btn primary px-3 py-1" disabled={getBusy() || !getTag()}>
          create release
        </button>
      </div>
    </form>
  );
}
