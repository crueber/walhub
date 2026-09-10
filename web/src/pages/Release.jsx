// web/src/pages/Release.jsx — route "/:owner/:name/releases/:tag" (07 §8):
// marked GFM rendered body (DOMPurify gate), assets table (name,
// size, sha256 short, download), edit/publish/delete per role, asset
// upload (client hashes via `crypto.subtle`, streams
// `POST …/assets/{name}`) and asset delete.

import { createSignal, For, Show } from "solid-js";
import { useParams, useNavigate } from "@solidjs/router";
import { useRepo, fmtBytes } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";
import { renderBody } from "../lib/render-md.js";
import { sha256Hex } from "../../sdk/src/releases.js";
import { ReleaseBadges } from "./Releases.jsx";
import DateTime from "../components/DateTime.jsx";

export default function Release() {
  const ctx = useRepo();
  const params = useParams();
  const navigate = useNavigate();
  const tag = () => params.tag;
  const key = () => `release:${ctx.full}:${tag()}`;
  const [getRel, setRel] = createSignal(null);
  useData(key, async () => {
    const rel = await ctx.repoClient.releases.get(tag());
    setRel(rel);
    return rel;
  });

  const reload = () => invalidate(key());

  const [getName, setName] = createSignal("");
  const [getBody, setBody] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);
  const [getEditing, setEditing] = createSignal(false);
  // Upload progress (issue #270): {phase: "hashing"|"uploading", name} or
  // null. The SDK hashes inside uploadAsset with no progress callback, so
  // the page reads + hashes first (crypto.subtle can take seconds on big
  // assets) and passes the digest through — the two phases stay honest.
  const [getUpload, setUpload] = createSignal(null);
  const [getDrag, setDrag] = createSignal(false);

  const startEdit = () => {
    const rel = getRel();
    if (!rel) return;
    setName(rel.name ?? "");
    setBody(rel.body ?? "");
    setEditing(true);
  };

  const save = async (patch) => {
    if (getBusy()) return; // double-submit guard (Enter during save)
    setBusy(true);
    try {
      const rel = await ctx.repoClient.releases.put(tag(), patch);
      setRel(rel);
      setEditing(false);
      reload();
    } catch (err) {
      reportError(err, "release");
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!confirm(`Delete release ${tag()} and all its assets?`)) return;
    setBusy(true);
    try {
      await ctx.repoClient.releases.remove(tag());
      navigate(`/${ctx.full}/releases`);
    } catch (err) {
      reportError(err, "release");
    } finally {
      setBusy(false);
    }
  };

  const uploadFile = async (file) => {
    if (!file) return;
    setBusy(true);
    try {
      setUpload({ phase: "hashing", name: file.name });
      const bytes = new Uint8Array(await file.arrayBuffer());
      const digest = await sha256Hex(bytes);
      setUpload({ phase: "uploading", name: file.name });
      await ctx.repoClient.releases.uploadAsset(tag(), file.name, bytes, {
        sha256: digest,
        contentType: file.type || undefined,
      });
      reload();
    } catch (err) {
      reportError(err, "asset");
    } finally {
      setUpload(null);
      setBusy(false);
    }
  };

  const onFile = (e) => {
    const input = e.currentTarget;
    const file = input.files?.[0];
    // Reset via the captured element, never the event object (issue #270
    // crash): the finally runs after awaits, when e.currentTarget is null.
    uploadFile(file).finally(() => {
      input.value = "";
    });
  };

  const onDrop = (e) => {
    e.preventDefault();
    setDrag(false);
    if (getBusy()) return;
    uploadFile(e.dataTransfer?.files?.[0]);
  };

  const deleteAsset = async (name) => {
    if (!confirm(`Delete asset ${name}?`)) return;
    setBusy(true);
    try {
      await ctx.repoClient.releases.deleteAsset(tag(), name);
      reload();
    } catch (err) {
      reportError(err, "asset");
    } finally {
      setBusy(false);
    }
  };

  const uploadLabel = () => {
    const up = getUpload();
    if (!up) return null;
    return up.phase === "hashing" ? `Hashing ${up.name}…` : `Uploading ${up.name}…`;
  };

  return (
    <div class="release-page">
      <Show when={getRel()} fallback={<p class="muted">loading…</p>}>
        {(rel) => (
          <>
            {/* Header block (issue #270, echoing the Issue page header):
                the human name leads (tag fallback — ReleaseNew defaults
                an empty title to the tag), tag + badges + date + author
                ride the meta row; actions group top-right in
                edit/lifecycle/destructive/navigation order. */}
            <header class="mb-4 border-b border-zinc-200 pb-3 dark:border-zinc-800">
              <div class="flex flex-wrap items-start justify-between gap-2">
                <h1 class="min-w-0 flex-1 text-lg font-semibold">{rel().name || rel().tag}</h1>
                <div class="flex flex-wrap gap-2" role="group" aria-label="Release actions">
                  <Show when={!getEditing()}>
                    <button type="button" class="btn" disabled={getBusy()} onClick={startEdit}>
                      Edit
                    </button>
                  </Show>
                  <Show when={rel().draft && !getEditing()}>
                    <button type="button" class="btn primary" disabled={getBusy()} onClick={() => save({ draft: false })}>
                      Publish
                    </button>
                  </Show>
                  <button type="button" class="btn danger" disabled={getBusy()} onClick={remove}>
                    Delete
                  </button>
                  <button type="button" class="btn" onClick={() => reload()}>
                    Refresh
                  </button>
                </div>
              </div>
              <p class="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-zinc-500 dark:text-zinc-400">
                <span class="font-mono">{rel().tag}</span>
                <ReleaseBadges release={rel()} />
                <span>
                  <DateTime value={rel().published_at ?? rel().created_at} />
                </span>
                <Show when={rel().author}>
                  <span>by {rel().author}</span>
                </Show>
                <Show when={rel().tag_sha}>
                  <span class="font-mono">tag {String(rel().tag_sha).slice(0, 12)}</span>
                </Show>
              </p>
            </header>
            <Show when={getEditing()}>
              {/* Edit mode is a composer-section form (issue #270): the
                  IssueNew/PullNew convention — centered max-w column,
                  fieldsets with help text — not inputs floating in the
                  action row. */}
              <section class="card mx-auto mb-4 max-w-2xl p-4" aria-label="Edit release">
                <form
                  onSubmit={(e) => {
                    e.preventDefault();
                    save({ name: getName(), body: getBody() });
                  }}
                >
                  <fieldset class="grid gap-3">
                    <legend class="text-sm font-medium">Content</legend>
                    <div class="grid gap-1">
                      <label class="text-sm font-medium" for="release-edit-title">
                        Title
                      </label>
                      <input
                        id="release-edit-title"
                        class="input"
                        value={getName()}
                        onInput={(e) => setName(e.currentTarget.value)}
                        placeholder={rel().tag}
                        aria-describedby="release-edit-title-help"
                      />
                      <p id="release-edit-title-help" class="muted text-xs">
                        Shown above the tag on the release page — blank falls back to the tag.
                      </p>
                    </div>
                    <div class="grid gap-1">
                      <label class="text-sm font-medium" for="release-edit-notes">
                        Notes
                      </label>
                      <textarea
                        id="release-edit-notes"
                        class="input font-mono text-sm"
                        rows="10"
                        value={getBody()}
                        onInput={(e) => setBody(e.currentTarget.value)}
                        placeholder="What's new in this release… (markdown-lite)"
                        aria-describedby="release-edit-notes-help"
                      />
                      <p id="release-edit-notes-help" class="muted text-xs">
                        Markdown-lite, rendered on the release page below.
                      </p>
                    </div>
                  </fieldset>
                  <div class="mt-3 flex flex-wrap gap-2 border-t border-zinc-100 pt-3 dark:border-zinc-800/60">
                    <button type="submit" class="btn primary" disabled={getBusy()}>
                      {getBusy() ? "Saving…" : "Save"}
                    </button>
                    <button type="button" class="btn" onClick={() => setEditing(false)}>
                      Cancel
                    </button>
                  </div>
                </form>
              </section>
            </Show>
            <section class="card p-4" aria-label="Release notes">
              {/* release notes have no file dir (issue #182): the tag names the
                  ref, the repo root is the base */}
              <div class="markdown-body" innerHTML={renderBody(rel().body ?? "", {
                owner: ctx.owner, repo: ctx.name, ref: rel().tag, dir: "",
              })} />
            </section>
            <section
              class="card mt-4 p-4"
              aria-label="Assets"
              onDragOver={(e) => {
                e.preventDefault();
                if (!getBusy()) setDrag(true);
              }}
              onDragLeave={() => setDrag(false)}
              onDrop={onDrop}
            >
              <div class="mb-2 flex flex-wrap items-center gap-2">
                <h2 class="text-sm font-semibold">Assets ({(rel().assets ?? []).length})</h2>
                <Show when={uploadLabel()}>
                  <span class="muted ml-auto text-xs" role="status">
                    {uploadLabel()}
                  </span>
                </Show>
              </div>
              <Show
                when={(rel().assets ?? []).length > 0}
                fallback={<p class="muted text-sm">No assets. Attach binaries or checksums below.</p>}
              >
                <div class="overflow-x-auto">
                  <table class="data-table">
                    <thead>
                      <tr>
                        <th scope="col">Name</th>
                        <th scope="col">Size</th>
                        <th scope="col">SHA-256</th>
                        <th scope="col">
                          <span class="sr-only">Actions</span>
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      <For each={rel().assets ?? []}>
                        {(a) => (
                          <tr>
                            <td>
                              <span class="flex min-w-0 items-center gap-2">
                                <svg class="h-4 w-4 shrink-0 text-zinc-400 dark:text-zinc-500" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                                  <path d="M21 8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16Z" />
                                  <path d="m3.3 7 8.7 5 8.7-5" />
                                  <path d="M12 22V12" />
                                </svg>
                                <a
                                  class="min-w-0 truncate font-mono text-[13px] text-emerald-700 hover:underline dark:text-emerald-400"
                                  href={ctx.repoClient.releaseAssetUrl(rel().tag, a.name)}
                                  download={a.name}
                                  title={a.name}
                                >
                                  {a.name}
                                </a>
                              </span>
                            </td>
                            <td class="muted tabular whitespace-nowrap">{fmtBytes(a.size)}</td>
                            <td class="muted whitespace-nowrap font-mono text-xs" title={a.sha256}>
                              {String(a.sha256 ?? "").slice(0, 12)}
                            </td>
                            <td class="whitespace-nowrap text-right">
                              <span class="inline-flex gap-2">
                                <a
                                  class="btn px-2 py-1 text-sm"
                                  href={ctx.repoClient.releaseAssetUrl(rel().tag, a.name)}
                                  download={a.name}
                                >
                                  download
                                </a>
                                <button
                                  type="button"
                                  class="btn px-2 py-1 text-sm"
                                  disabled={getBusy()}
                                  onClick={() => deleteAsset(a.name)}
                                >
                                  delete
                                </button>
                              </span>
                            </td>
                          </tr>
                        )}
                      </For>
                    </tbody>
                  </table>
                </div>
              </Show>
              <div
                class={`mt-3 flex flex-wrap items-center gap-2 border-t border-zinc-100 pt-3 dark:border-zinc-800/60${
                  getDrag() ? " rounded-md ring-2 ring-emerald-500" : ""
                }`}
              >
                <label class={`btn primary cursor-pointer${getBusy() ? " pointer-events-none opacity-50" : ""}`}>
                  {getUpload() ? uploadLabel() : "Upload asset"}
                  <input
                    type="file"
                    class="hidden"
                    disabled={getBusy()}
                    onChange={onFile}
                  />
                </label>
                <span class="muted text-xs">or drop a file here — hashed locally, then streamed.</span>
              </div>
            </section>
          </>
        )}
      </Show>
    </div>
  );
}
