// web/src/pages/PullFiles.jsx — route "/:owner/:name/pull/:num/files"
// (08 §1): the PR diff as a file list with per-file unified/split hunks
// (the DiffPage contract — parsed by lib/diff.js, rendered by the shared
// selectable DiffBody; line-thread anchoring lives on the main PR page).

import { createSignal, For, Show } from "solid-js";
import { A, useParams } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { useData } from "../lib/data.js";
import { TTL } from "../lib/collab.js";
import { useCollabStream } from "../components/collab.jsx";
import { parsePatchFiles } from "../lib/diff.js";
import { DiffBody } from "../components/DiffTable.jsx";

// Per-file unified/split toggle over the shared selectable DiffBody
// (issue #244) — the same renderer as the commit page, so #L selection,
// gutters, and shareable hashes land everywhere at once.
function PullDiffFile(props) {
  const [getMode, setMode] = createSignal("unified");
  return (
    <div class="card mb-4 overflow-hidden" aria-label={`Diff ${props.file.path}`}>
      <div class="flex flex-wrap items-baseline gap-2">
        <h3 class="mb-2 font-mono text-sm font-semibold" id={props.file.path}>{props.file.path}</h3>
        <div class="seg ml-auto flex gap-1">
          <button type="button" class="pill cursor-pointer" onClick={() => setMode("unified")}>
            Unified
          </button>
          <button type="button" class="pill cursor-pointer" onClick={() => setMode("split")}>
            Split
          </button>
        </div>
      </div>
      <div class="overflow-x-auto">
        <DiffBody file={props.file} mode={getMode()} />
      </div>
    </div>
  );
}

export default function PullFiles() {
  const ctx = useRepo();
  const params = useParams();
  const num = () => params.num;
  const key = () => `pulldiff:${ctx.full}:${num()}`;
  const [getView] = useData(key, async () => {
    const res = await ctx.repoClient.pulls.diff(num());
    const patch = typeof res === "string" ? res : res.patch ?? res.diff ?? "";
    return parsePatchFiles(patch);
    // NOTE: TTL.pulls (5 s), not TTL.diff (∞) — the key is not
    // sha-addressed, so ∞ would serve a stale diff forever after the
    // head moves (08 §6 reserves ∞ for immutable content).
  }, TTL.pulls);
  // `pull` frames (opened/head_force_pushed/merged) invalidate the
  // pulldiff key coalesced — the stream is the live path, TTL the backstop.
  useCollabStream(() => ctx.full, ctx.repoClient, ["pull"], (frame) => Number(frame.num) === Number(num()));
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
          {(file) => <PullDiffFile file={file} />}
        </For>
      </Show>
    </div>
  );
}
