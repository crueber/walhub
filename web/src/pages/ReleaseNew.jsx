// web/src/pages/ReleaseNew.jsx — route "/:owner/:name/releases/new"
// (07 §8): tag combobox fed by the existing tags ref stream, autodraft
// button filling the body from merged PRs, draft/prerelease checkboxes,
// create → detail. Composer composition follows the IssueNew/PullNew
// convention (issue #49): centered max-w-2xl column, section fieldsets
// with help text, checkbox option rows, inline validation errors.

import { createSignal, For, Show, onCleanup } from "solid-js";
import { A, useNavigate } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { useData, reportError } from "../lib/data.js";
import { filterTagNames } from "../lib/releases.js";

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
  const [getError, setError] = createSignal("");

  const errMsg = (err) => String(err?.message ?? err);

  const fillAutodraft = async () => {
    if (!getTag()) {
      setError("Pick a tag first — autodraft summarizes merged PRs against it.");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const ad = await ctx.repoClient.releases.autodraft({ tag: getTag() });
      setBody(ad.body ?? "");
      setSince(ad.since ?? "");
      if (!getName()) setName(getTag());
    } catch (err) {
      setError(errMsg(err));
      reportError(err, "autodraft");
    } finally {
      setBusy(false);
    }
  };

  const create = async (e) => {
    e.preventDefault();
    if (!getTag()) {
      setError("Choose a tag for this release.");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const rel = await ctx.repoClient.releases.put(getTag(), {
        name: getName().trim() || undefined,
        body: getBody(),
        draft: getDraft(),
        prerelease: getPrerelease(),
      });
      navigate(`/${ctx.full}/releases/${encodeURIComponent(rel.tag)}`);
    } catch (err) {
      setError(errMsg(err));
      reportError(err, "release");
    } finally {
      setBusy(false);
    }
  };

  const tags = () => getTags()?.refs ?? getTags()?.tags ?? [];
  const tagNames = () => tags().map((t) => String(t.name ?? t).replace(/^refs\/tags\//, ""));

  // Tag combobox (issue #254): the text input stays the source of truth;
  // the ▾ trigger opens a styled popover of recent tags (tags-stream
  // order — most recent first) filtered client-side on tagNames(), so no
  // new fetch rides anything but the existing `tags:{full}` cache entry.
  // Keyboard mirrors the RefPicker conventions (arrows move, Enter
  // selects, Esc closes); a pointer click outside closes it (the
  // LabelPicker/MilestonePicker document-level idiom).
  const [getTagOpen, setTagOpen] = createSignal(false);
  const [getTagActive, setTagActive] = createSignal(-1);
  let comboRoot;
  let tagInput;
  const filteredTags = () => filterTagNames(tagNames(), getTag());
  const closeTags = () => {
    setTagOpen(false);
    setTagActive(-1);
  };
  const chooseTag = (name) => {
    setTag(name);
    closeTags();
    tagInput?.focus();
  };
  const onTagDocClick = (e) => {
    if (getTagOpen() && comboRoot && !comboRoot.contains(e.target)) closeTags();
  };
  const onTagDocKey = (e) => {
    if (getTagOpen() && e.key === "Escape") {
      closeTags();
      tagInput?.focus();
    }
  };
  document.addEventListener("click", onTagDocClick);
  document.addEventListener("keydown", onTagDocKey);
  onCleanup(() => {
    document.removeEventListener("click", onTagDocClick);
    document.removeEventListener("keydown", onTagDocKey);
  });
  const onTagKey = (e) => {
    const list = filteredTags();
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      if (!getTagOpen()) {
        if (list.length > 0) {
          setTagOpen(true);
          setTagActive(0);
        }
        return;
      }
      if (list.length === 0) return;
      const d = e.key === "ArrowDown" ? 1 : -1;
      setTagActive((i) => (i + d + list.length) % list.length);
    } else if (e.key === "Enter" && getTagOpen()) {
      const i = getTagActive();
      if (i >= 0 && i < list.length) {
        e.preventDefault();
        chooseTag(list[i]);
      } else {
        closeTags();
      }
    }
  };

  return (
    <div class="mx-auto max-w-2xl">
      <div class="mb-3 flex items-baseline gap-2">
        <h1 class="text-lg font-semibold">New release</h1>
        <A
          href={`/${ctx.full}/releases`}
          class="ml-auto text-sm text-emerald-700 hover:underline dark:text-emerald-400"
        >
          all releases
        </A>
      </div>
      <Show when={getError()}>
        <p
          class="card mb-3 border-red-300 p-3 text-sm text-red-700 dark:border-red-900 dark:text-red-400"
          role="alert"
        >
          {getError()}
        </p>
      </Show>
      <form class="card grid gap-5 p-4" onSubmit={create} aria-label="New release">
        <fieldset class="grid gap-3">
          <legend class="text-sm font-medium">Target</legend>
          <div class="grid gap-1">
            <label class="text-sm font-medium" for="release-tag">
              Tag <span class="muted font-normal">· required, must already exist</span>
            </label>
            <div class="relative" ref={comboRoot}>
              <div class="flex gap-2">
                <input
                  ref={tagInput}
                  id="release-tag"
                  role="combobox"
                  aria-expanded={getTagOpen() ? "true" : "false"}
                  aria-controls="release-tag-list"
                  aria-autocomplete="list"
                  aria-activedescendant={
                    getTagActive() >= 0 ? `release-tag-opt-${getTagActive()}` : undefined
                  }
                  class="input min-w-0 flex-1 font-mono"
                  value={getTag()}
                  onInput={(e) => {
                    setTag(e.currentTarget.value.trim());
                    setTagActive(-1);
                    if (!getTagOpen() && tagNames().length > 0) setTagOpen(true);
                  }}
                  onFocus={() => {
                    if (!getTagOpen() && tagNames().length > 0) setTagOpen(true);
                  }}
                  onKeyDown={onTagKey}
                  placeholder="v1.0.0"
                  autocomplete="off"
                  aria-describedby="release-tag-help"
                />
                <button
                  type="button"
                  class="btn shrink-0"
                  aria-haspopup="listbox"
                  aria-expanded={getTagOpen() ? "true" : "false"}
                  aria-controls="release-tag-list"
                  aria-label="Show recent tags"
                  title="Show recent tags"
                  disabled={tagNames().length === 0}
                  onClick={() => {
                    const next = !getTagOpen();
                    setTagOpen(next);
                    setTagActive(-1);
                    if (next) tagInput?.focus();
                  }}
                >
                  <span aria-hidden="true">▾</span>
                </button>
              </div>
              <Show when={getTagOpen() && tagNames().length > 0}>
                <div
                  id="release-tag-list"
                  role="listbox"
                  aria-label="Recent tags"
                  class="tag-drop scroll-slim card absolute inset-x-0 z-30 mt-1 max-h-72 overflow-y-auto p-1"
                >
                  <For
                    each={filteredTags()}
                    fallback={
                      <p class="muted px-2 py-1 text-xs">
                        No matching tags — check the name or create it from a commit.
                      </p>
                    }
                  >
                    {(name, i) => (
                      <button
                        type="button"
                        role="option"
                        id={`release-tag-opt-${i()}`}
                        aria-selected={getTagActive() === i() ? "true" : "false"}
                        aria-label={`use tag ${name}`}
                        title={name}
                        class="flex w-full items-center gap-2 rounded px-2 py-1 text-left text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800"
                        classList={{ "bg-zinc-100 dark:bg-zinc-800": getTagActive() === i() }}
                        onClick={() => chooseTag(name)}
                        onMouseMove={() => setTagActive(i())}
                      >
                        <span class="inline-block w-4 shrink-0 text-center" aria-hidden="true">
                          {getTag() === name ? "✓" : ""}
                        </span>
                        <span class="font-mono">{name}</span>
                      </button>
                    )}
                  </For>
                </div>
              </Show>
            </div>
            <p id="release-tag-help" class="muted text-xs">
              <Show when={getTags() !== undefined} fallback="Loading tags…">
                <Show
                  when={tagNames().length > 0}
                  fallback="No tags yet — create one from a commit page or push one with git."
                >
                  {tagNames().length} tags available — pick from the list or type the name.
                </Show>
              </Show>
            </p>
          </div>
        </fieldset>
        <fieldset class="grid gap-3">
          <legend class="text-sm font-medium">Content</legend>
          <div class="grid gap-1">
            <label class="text-sm font-medium" for="release-title">
              Title
            </label>
            <input
              id="release-title"
              class="input"
              value={getName()}
              onInput={(e) => setName(e.currentTarget.value)}
              placeholder={getTag() || "Defaults to the tag"}
              aria-describedby="release-title-help"
            />
            <p id="release-title-help" class="muted text-xs">
              Shown above the tag on the release page — leave blank to use the tag itself.
            </p>
          </div>
          <div class="grid gap-1">
            <label class="text-sm font-medium" for="release-notes">
              Notes
              <Show when={getSince()}>
                <span class="muted font-normal"> (changes since {getSince()})</span>
              </Show>
            </label>
            <textarea
              id="release-notes"
              class="input font-mono text-sm"
              rows="10"
              value={getBody()}
              onInput={(e) => setBody(e.currentTarget.value)}
              placeholder="What's new in this release… (markdown-lite)"
              aria-describedby="release-notes-help"
            />
            <p id="release-notes-help" class="muted text-xs">
              Markdown-lite, rendered on the release page. Autodraft below summarizes merged pull
              requests into this field.
            </p>
          </div>
        </fieldset>
        <fieldset class="grid gap-2">
          <legend class="text-sm font-medium">Visibility</legend>
          <label class="flex cursor-pointer items-start gap-2 rounded-md border border-zinc-200 p-3 dark:border-zinc-800">
            <input
              type="checkbox"
              class="mt-0.5"
              checked={getDraft()}
              onChange={(e) => setDraft(e.currentTarget.checked)}
            />
            <span>
              <span class="block text-sm font-medium">Draft</span>
              <span class="muted block text-xs">
                Keep hidden from the releases list until you publish it from the release page.
              </span>
            </span>
          </label>
          <label class="flex cursor-pointer items-start gap-2 rounded-md border border-zinc-200 p-3 dark:border-zinc-800">
            <input
              type="checkbox"
              class="mt-0.5"
              checked={getPrerelease()}
              onChange={(e) => setPrerelease(e.currentTarget.checked)}
            />
            <span>
              <span class="block text-sm font-medium">Prerelease</span>
              <span class="muted block text-xs">
                Flag as an early release (alpha, beta, rc) rather than a final one.
              </span>
            </span>
          </label>
        </fieldset>
        <div class="flex flex-wrap items-center gap-2 border-t border-zinc-100 pt-4 dark:border-zinc-800/60">
          <button type="submit" class="btn primary" disabled={getBusy() || !getTag()}>
            {getBusy() ? "Creating…" : "Create release"}
          </button>
          <button
            type="button"
            class="btn"
            disabled={getBusy() || !getTag()}
            onClick={fillAutodraft}
            title="Fill the notes from merged pull requests"
          >
            {getBusy() ? "Working…" : "Autodraft from merged PRs"}
          </button>
          <p class="muted w-full text-xs">
            Files (binaries, checksums) attach after creation, on the release page.
          </p>
        </div>
      </form>
    </div>
  );
}
