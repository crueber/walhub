// web/src/pages/Owners.jsx — route "/": every owner on the host (store-backed list).

import repos from "../../sdk/src/index.js";
import { For, Show } from "solid-js";
import { A } from "@solidjs/router";
import { useData } from "../lib/data.js";

export default function Owners() {
  const [getOwners] = useData("owners", () => repos.owners.list());
  return (
    <div class="owners-page">
      <div class="mb-4 flex items-center justify-between">
        <h2 class="text-xl font-semibold">Owners</h2>
        <A class="btn primary px-3 py-1" href="/import">
          Import repository
        </A>
      </div>
      <Show when={getOwners()} fallback={<p class="muted">loading…</p>}>
        <Show
          when={(getOwners() ?? []).length > 0}
          fallback={<p class="muted">no repositories yet — push one, or use the API to create it</p>}
        >
          <ul class="space-y-1">
            <For each={getOwners()}>
              {(o) => (
                <li>
                  <A class="text-emerald-700 hover:underline dark:text-emerald-400" href={`/${o}`}>
                    {o}
                  </A>
                </li>
              )}
            </For>
          </ul>
        </Show>
      </Show>
    </div>
  );
}
