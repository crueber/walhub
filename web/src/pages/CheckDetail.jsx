// web/src/pages/CheckDetail.jsx — route "/:owner/:name/checks/:sha" (08
// §1): the statuses for one commit (linked from commit detail, CheckPill,
// and MergeBox). Reuses the shared CheckPill/ContextRows.

import { Show } from "solid-js";
import { A, useParams } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { CheckPill, ContextRows } from "./Checks.jsx";

export default function CheckDetail() {
  const ctx = useRepo();
  const params = useParams();
  const sha = () => params.sha;
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
          <ContextRows full={ctx.full} sha={sha()} client={ctx.repoClient} />
        </Show>
      </div>
    </div>
  );
}
