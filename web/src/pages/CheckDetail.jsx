// web/src/pages/CheckDetail.jsx — route "/:owner/:name/checks/:sha" (08
// §1): the statuses for one commit (linked from commit detail, CheckPill,
// and MergeBox). Reuses the shared CheckPill/ContextRows.

import { createEffect, onCleanup, Show } from "solid-js";
import { A, useParams } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { CheckPill, ContextRows, ZeroChecksBlock } from "./Checks.jsx";
import { useData } from "../lib/data.js";
import { isZeroChecks } from "../lib/checks-empty.js";

export default function CheckDetail() {
  const ctx = useRepo();
  const params = useParams();
  const sha = () => params.sha;
  // Issue #252: sha-addressed like Commit.jsx — the pill shows the short sha.
  createEffect(() => {
    if (sha()) ctx.setViewed({ name: "", sha: sha() });
  });
  onCleanup(() => ctx.setViewed(null));
  // Zero-contexts empty state (Forgejo #518): same data key + fetcher as
  // CheckPill above, so this shares the cached combined view (no extra
  // request) and swaps the bare ContextRows fallback for the reporting
  // guidance block. No policy fetch here, so required stays unknown and
  // the block titles "reported", never "configured".
  const combinedKey = () => `checks:${ctx.full}:${sha()}`;
  const [getCombined] = useData(combinedKey, () => (sha() ? ctx.repoClient.checks.combined(sha()) : Promise.resolve(null)));
  const zero = () => isZeroChecks(getCombined());
  return (
    <div class="mx-auto max-w-3xl">
      <p class="mb-3 text-sm">
        <A class="link" href={`/${ctx.full}/checks`}>
          ← all checks
        </A>
        {" · "}
        <A class="link font-mono" href={`/${ctx.full}/commit/${sha()}`}>
          {String(sha() ?? "").slice(0, 12)}
        </A>
      </p>
      <h2 class="mb-3 flex items-center gap-2 text-lg font-semibold">
        Checks for <code class="font-mono text-sm">{String(sha() ?? "").slice(0, 12)}</code>
        <CheckPill full={ctx.full} sha={sha()} client={ctx.repoClient} verbose />
      </h2>
      <div class="card !p-0">
        <Show when={sha()} fallback={<p class="muted p-3 text-sm">no sha</p>}>
          <Show
            when={zero()}
            fallback={<ContextRows full={ctx.full} sha={sha()} client={ctx.repoClient} />}
          >
            <div class="p-3">
              <ZeroChecksBlock full={ctx.full} />
            </div>
          </Show>
        </Show>
      </div>
    </div>
  );
}
