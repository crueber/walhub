// web/src/pages/ReleaseNew.jsx — route "/:owner/:name/releases/new"
// (07 §8; Forgejo #503): tag dropdown (a native single-select) fed by the
// existing tags ref stream, autodraft button filling the body from merged
// PRs, draft/prerelease checkboxes, create → detail. Composer composition
// follows the IssueNew/PullNew convention (issue #49): centered max-w-2xl
// column, section fieldsets with help text, checkbox option rows, inline
// validation errors.

import { createSignal, For, Show } from "solid-js";
import { A, useLocation, useNavigate } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import repos from "../../sdk/src/index.js";
import { useData, reportError } from "../lib/data.js";
import { onSubmitKeys } from "../lib/submitKeys.js";
import { anonWriteTarget, write401Target } from "../lib/writeGate.js";

export default function ReleaseNew() {
  const ctx = useRepo();
  const navigate = useNavigate();
  const location = useLocation();
  // Forgejo #502: anonymous viewers route to the log-in interstitial
  // instead of firing the create (shared identity cache keys — zero new
  // requests); the write opts out of popup auth so a stale-identity 401
  // routes the same way.
  const [getMe] = useData("me", () => repos.me().catch(() => null));
  const [getDiscovery] = useData("discovery", () => repos.discovery().catch(() => null));
  const here = () => location.pathname + location.search;
  const writeGateHref = () =>
    anonWriteTarget({ me: getMe(), discovery: getDiscovery() }, here(), "Create a new release");
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
    const gateHref = writeGateHref();
    if (gateHref) {
      navigate(gateHref);
      return;
    }
    if (!getTag()) {
      setError("Choose a tag for this release.");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const rel = await ctx.repoClient.releases.put(
        getTag(),
        {
          name: getName().trim() || undefined,
          body: getBody(),
          draft: getDraft(),
          prerelease: getPrerelease(),
        },
        { noPopupAuth: true },
      );
      navigate(`/${ctx.full}/releases/${encodeURIComponent(rel.tag)}`);
    } catch (err) {
      const target = write401Target(err, { me: getMe(), discovery: getDiscovery() }, here(), "Create a new release");
      if (target) {
        navigate(target);
        return;
      }
      setError(errMsg(err));
      reportError(err, "release");
    } finally {
      setBusy(false);
    }
  };

  const tags = () => getTags()?.refs ?? getTags()?.tags ?? [];
  const tagNames = () => tags().map((t) => String(t.name ?? t).replace(/^refs\/tags\//, ""));

  // Tag dropdown (Forgejo #503): a native single-select of existing tags —
  // the release service resolves the tag and rejects anything else, so a
  // picker of real tags is the honest control (the #254 free-text input let
  // users type names the server could never accept). Options ride the existing
  // `tags:{full}` cache entry in tags-stream order (most recent first) — no
  // new fetch. Native select per the #416 milestone precedent
  // (single-select consistency with the State field): type-ahead matching,
  // arrows/Enter/Esc, and screen-reader listbox semantics come free from
  // the platform, and there is no custom popover to keep opaque (#405) or
  // viewport-bound (#278). No type-to-filter remains (browsers match options
  // by prefix on keypress); the tag is "" until the user picks a real tag,
  // which is what the submit/autodraft gates below disable on.
  const tagsLoading = () => getTags() === undefined;
  const hasTags = () => tagNames().length > 0;

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
            <select
              id="release-tag"
              class="input font-mono"
              value={getTag()}
              disabled={tagsLoading() || !hasTags()}
              onChange={(e) => setTag(e.currentTarget.value)}
              aria-describedby="release-tag-help"
            >
              <Show
                when={!tagsLoading() && hasTags()}
                fallback={
                  <option value="">{tagsLoading() ? "Loading tags…" : "No tags yet"}</option>
                }
              >
                <option value="" disabled>
                  Pick a tag…
                </option>
                <For each={tagNames()}>{(name) => <option value={name}>{name}</option>}</For>
              </Show>
            </select>
            <p id="release-tag-help" class="muted text-xs">
              <Show when={!tagsLoading()} fallback="Loading tags…">
                <Show
                  when={hasTags()}
                  fallback="No tags yet — create one from a commit page or push one with git."
                >
                  {tagNames().length} tags available — pick one from the list.
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
              onKeyDown={onSubmitKeys(create, { isBusy: () => getBusy() })}
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
