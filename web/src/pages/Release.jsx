// web/src/pages/Release.jsx — route "/:owner/:name/releases/:tag" (07 §8):
// markdown-lite rendered body (allowlist sanitizer), assets table (name,
// size, sha256 short, download), edit/publish/delete per role, asset
// upload (client hashes via `crypto.subtle`, streams
// `POST …/assets/{name}`) and asset delete.

import { createSignal, For, Show } from "solid-js";
import { useParams, useNavigate } from "@solidjs/router";
import { useRepo, fmtDate, fmtBytes } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";
import { renderMarkdown } from "../lib/markdown.js";
import { sanitize } from "../lib/sanitize.js";
import { ReleaseBadges } from "./Releases.jsx";

export default function Release() {
  const ctx = useRepo();
  const params = useParams();
  const navigate = useNavigate();
  const tag = () => params.tag;
  const key = () => `release:${ctx.full}:${tag()}`;
  const [getRel, setRel] = createSignal(null);
  const [, { refetch }] = useData(key, async () => {
    const rel = await ctx.repoClient.releases.get(tag());
    setRel(rel);
    return rel;
  });

  const reload = () => invalidate(key());

  const [getName, setName] = createSignal("");
  const [getBody, setBody] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);
  const [getEditing, setEditing] = createSignal(false);

  const startEdit = () => {
    const rel = getRel();
    if (!rel) return;
    setName(rel.name ?? "");
    setBody(rel.body ?? "");
    setEditing(true);
  };

  const save = async (patch) => {
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

  const onFile = async (e) => {
    const file = e.currentTarget.files?.[0];
    if (!file) return;
    setBusy(true);
    try {
      await ctx.repoClient.releases.uploadAsset(tag(), file.name, file, {
        contentType: file.type || undefined,
      });
      e.currentTarget.value = "";
      reload();
    } catch (err) {
      reportError(err, "asset");
    } finally {
      setBusy(false);
    }
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

  return (
    <div class="grid gap-6">
      <Show when={getRel()} fallback={<p class="muted">loading…</p>}>
        {(rel) => (
          <>
            <section class="card" aria-label="Release">
              <div class="mb-2 flex flex-wrap items-center gap-2">
                <h1 class="card-title font-mono text-xl">{rel().tag}</h1>
                <ReleaseBadges release={rel()} />
                <span class="card-meta ml-auto">{fmtDate(rel().published_at ?? rel().created_at)}</span>
              </div>
              <h2 class="mb-2 text-lg font-semibold">{rel().name}</h2>
              <div class="markdown-body" innerHTML={sanitize(renderMarkdown(rel().body ?? ""))} />
              <p class="card-meta mt-2">
                tag {rel().tag_sha?.slice(0, 12)} · by {rel().author}
              </p>
              <div class="mt-3 flex flex-wrap gap-2">
                <Show when={!getEditing()} fallback={
                  <>
                    <input class="input" value={getName()} onInput={(e) => setName(e.currentTarget.value)} aria-label="Release name" />
                    <textarea class="input" value={getBody()} onInput={(e) => setBody(e.currentTarget.value)} aria-label="Release notes" rows="6" />
                    <button type="button" class="btn px-2 py-1" disabled={getBusy()} onClick={() => save({ name: getName(), body: getBody() })}>
                      save
                    </button>
                    <button type="button" class="btn px-2 py-1" onClick={() => setEditing(false)}>
                      cancel
                    </button>
                  </>
                }>
                  <button type="button" class="btn px-2 py-1" disabled={getBusy()} onClick={startEdit}>
                    edit
                  </button>
                </Show>
                <Show when={rel().draft}>
                  <button type="button" class="btn primary px-2 py-1" disabled={getBusy()} onClick={() => save({ draft: false })}>
                    publish
                  </button>
                </Show>
                <button type="button" class="btn danger px-2 py-1" disabled={getBusy()} onClick={remove}>
                  delete
                </button>
                <button type="button" class="btn ml-auto px-2 py-1" onClick={() => refetch()}>
                  refresh
                </button>
              </div>
            </section>
            <section class="card" aria-label="Assets">
              <h2 class="mb-2 text-sm font-semibold">Assets ({(rel().assets ?? []).length})</h2>
              <ul class="card-list">
                <For each={rel().assets ?? []} fallback={<li class="muted">No assets.</li>}>
                  {(a) => (
                    <li class="flex flex-wrap items-center gap-2">
                      <a class="card-title font-mono" href={ctx.repoClient.releaseAssetUrl(rel().tag, a.name)}>
                        {a.name}
                      </a>
                      <span class="card-meta">{fmtBytes(a.size)}</span>
                      <span class="card-meta font-mono" title={a.sha256}>
                        {String(a.sha256 ?? "").slice(0, 12)}
                      </span>
                      <button type="button" class="btn ml-auto px-2 py-1 text-sm" disabled={getBusy()} onClick={() => deleteAsset(a.name)}>
                        delete
                      </button>
                    </li>
                  )}
                </For>
              </ul>
              <label class="mt-3 block">
                <span class="btn cursor-pointer px-2 py-1">upload asset</span>
                <input type="file" class="hidden" disabled={getBusy()} onChange={onFile} />
              </label>
            </section>
          </>
        )}
      </Show>
    </div>
  );
}
