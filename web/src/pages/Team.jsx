// web/src/pages/Team.jsx — route "/:owner/teams/:slug" (08 §1): the team
// page (members + team repos, per 01).

import { createSignal, For, Show } from "solid-js";
import { A, useParams } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { useData, invalidate, reportError } from "../lib/data.js";

export default function Team() {
  const params = useParams();
  const org = () => params.owner;
  const slug = () => params.slug;
  const key = () => `team:${org()}/${slug()}`;
  const [getTeam] = useData(key, () => repos.orgs.teams.get(org(), slug()));
  const [getEmail, setEmail] = createSignal("");
  const reload = () => invalidate(key());

  const add = async (e) => {
    e.preventDefault();
    if (!getEmail().trim()) return;
    try {
      await repos.orgs.teams.addMember(org(), slug(), getEmail().trim());
      setEmail("");
      reload();
    } catch (err) {
      reportError(err, "team-add");
    }
  };
  const remove = async (who) => {
    try {
      await repos.orgs.teams.removeMember(org(), slug(), who);
      reload();
    } catch (err) {
      reportError(err, "team-remove");
    }
  };

  return (
    <div class="mx-auto max-w-2xl">
      <p class="mb-3 text-sm">
        <A class="link" href={`/${org()}`}>
          ← {org()}
        </A>
      </p>
      <Show when={getTeam()} fallback={<p class="muted">loading…</p>}>
        <h2 class="mb-1 text-lg font-semibold">
          {org()}/{slug()}
        </h2>
        <p class="mb-3 text-sm text-zinc-500 dark:text-zinc-400">{getTeam().name ?? ""}</p>
        <div class="card p-4" aria-label="Team members">
          <h3 class="mb-2 font-semibold">Members ({(getTeam().members ?? []).length})</h3>
          <ul class="grid gap-1">
            <For each={getTeam().members ?? []} fallback={<li class="muted text-sm">no members</li>}>
              {(m) => (
                <li class="flex items-center gap-2 text-sm">
                  <code class="font-mono text-xs">{m}</code>
                  <button type="button" class="link ml-auto text-xs" onClick={() => remove(m)}>
                    remove
                  </button>
                </li>
              )}
            </For>
          </ul>
          <form class="mt-3 flex gap-2" onSubmit={add}>
            <input
              class="input flex-1 font-mono text-sm"
              value={getEmail()}
              onInput={(e) => setEmail(e.target.value)}
              placeholder="member@example.com"
              aria-label="member email"
            />
            <button type="submit" class="btn px-3 py-1">
              add
            </button>
          </form>
        </div>
      </Show>
    </div>
  );
}
