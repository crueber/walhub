// web/src/pages/PullFiles.jsx — route "/:owner/:name/pull/:num/files"
// (08 §1): the PR diff as a file list with unified hunks (the DiffPage
// contract — parsed by lib/diff.js; line-thread anchoring lives on the
// main PR page).

import { For, Show } from "solid-js";
import { A, useParams } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { useData } from "../lib/data.js";
import { TTL } from "../lib/collab.js";
import { parsePatchFiles } from "../lib/diff.js";

export default function PullFiles() {
  const ctx = useRepo();
  const params = useParams();
  const num = () => params.num;
  const key = () => `pulldiff:${ctx.full}:${num()}`;
  const [getView] = useData(key, async () => {
    const res = await ctx.repoClient.pulls.diff(num());
    const patch = typeof res === "string" ? res : res.patch ?? res.diff ?? "";
    return parsePatchFiles(patch);
  }, TTL.diff);
  return (
    <div>
      <p class="mb-3 text-sm">
        <A class="link" href={`/${ctx.full}/pull/${num()}`}>
          ← back to #{num()}
        </A>
      </p>
      <h2 class="mb-3 text-lg font-semibold">Files on #{num()} ({(getView()?.files ?? []).length})</h2>
      <Show when={getView()} fallback={<p class="muted">loading diff…</p>}>
        <For each={getView().files ?? []} fallback={<p class="muted">empty diff</p>}>
          {(file) => (
            <div class="card mb-4" aria-label={`Diff ${file.path}`}>
              <h3 class="mb-2 font-mono text-sm font-semibold" id={file.path}>{file.path}</h3>
              <For each={file.hunks ?? []}>
                {(hunk) => (
                  <div class="mb-3 overflow-x-auto">
                    <div class="font-mono text-xs text-zinc-500 dark:text-zinc-400">
                      @@ -{hunk.oldStart},{hunk.oldLines} +{hunk.newStart},{hunk.newLines} @@
                    </div>
                    <For each={hunk.lines ?? []}>
                      {(l) => (
                        <div class="flex font-mono text-xs">
                          <span class="w-4 shrink-0 select-none">{l.t}</span>
                          <span class="whitespace-pre">{l.text}</span>
                        </div>
                      )}
                    </For>
                  </div>
                )}
              </For>
            </div>
          )}
        </For>
      </Show>
    </div>
  );
}
