// web/src/pages/PullCommits.jsx — route "/:owner/:name/pull/:num/commits"
// (08 §1): the PR commit list (base…head).

import { For, Show } from "solid-js";
import { A, useParams } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { useData } from "../lib/data.js";
import { TTL } from "../lib/collab.js";
import { useCollabStream } from "../components/collab.jsx";
import DateTime from "../components/DateTime.jsx";

export default function PullCommits() {
  const ctx = useRepo();
  const params = useParams();
  const num = () => params.num;
  const key = () => `prcommits:${ctx.full}:${num()}`;
  // NOTE: TTL.pulls (5 s), not TTL.prcommits (∞) — the key is not
  // sha-addressed, so ∞ would serve a stale list forever after the
  // head moves (08 §6 reserves ∞ for immutable content).
  const [getView] = useData(key, () => ctx.repoClient.pulls.commits(num(), { n: 100 }), TTL.pulls);
  useCollabStream(() => ctx.full, ctx.repoClient, ["pull"], (frame) => Number(frame.num) === Number(num()));
  return (
    <div>
      <p class="mb-3 text-sm">
        <A class="link" href={`/${ctx.full}/pull/${num()}`}>
          ← back to #{num()}
        </A>
      </p>
      <h2 class="mb-3 text-lg font-semibold">Commits on #{num()}</h2>
      <Show when={getView()} fallback={<p class="muted">loading…</p>}>
        <ul class="grid gap-2">
          <For each={getView().commits ?? []} fallback={<li class="card">no commits</li>}>
            {(c) => (
              <li class="card p-3">
                <A class="font-mono text-xs hover:underline" href={`/${ctx.full}/commit/${c.sha}`}>
                  {String(c.sha ?? "").slice(0, 12)}
                </A>
                <p class="text-sm">{c.subject ?? c.message}</p>
                <p class="text-xs text-zinc-500 dark:text-zinc-400">
                  {c.author} · <DateTime value={c.author_date ?? c.date} />
                </p>
              </li>
            )}
          </For>
        </ul>
      </Show>
    </div>
  );
}
