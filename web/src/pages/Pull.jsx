// web/src/pages/Pull.jsx — route "/:owner/:name/pull/:num" (03 §9): the
// PR conversation (timeline, comment box, merge box with strategy select,
// mergeable state, and task progress). `pull` frames refresh the header;
// the merge box polls the pull-merge task record while it runs.

import { createSignal, For, Show } from "solid-js";
import { A, useParams } from "@solidjs/router";
import { useRepo, fmtDate } from "./Repo.jsx";
import { useData, invalidate, reportError } from "../lib/data.js";

function eventText(ev) {
  switch (ev.type) {
    case "opened":
    case "commented":
      return null; // rendered as body
    case "title_changed":
      return `retitled “${ev.from}” → “${ev.to}”`;
    case "state_changed":
      return ev.to === "closed" ? "closed" : "reopened";
    case "merged":
      return `merged as ${(ev.merge_commit_sha ?? "").slice(0, 12)} (${ev.strategy ?? "merge"})`;
    case "head_force_pushed":
      return `head force-pushed ${(ev.from ?? "").slice(0, 12)} → ${(ev.to ?? "").slice(0, 12)}`;
    default:
      return ev.type;
  }
}

function mergeableText(m) {
  if (!m) return "unknown";
  switch (m.state) {
    case "clean":
      return "mergeable";
    case "behind":
      return "mergeable (behind)";
    case "dirty":
      return `conflicts: ${(m.conflicts ?? []).join(", ")}`;
    case "up_to_date":
      return "already merged";
    default:
      return "checking…";
  }
}

export default function Pull() {
  const ctx = useRepo();
  const params = useParams();
  const num = () => params.num;
  const key = () => `pull:${ctx.full}:${num()}`;
  const [getView] = useData(key, () => ctx.repoClient.pulls.get(num()));
  const [getBody, setBody] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);
  const [getStrategy, setStrategy] = createSignal("merge");
  const [getTask, setTask] = createSignal(null);

  const reload = () => invalidate(key());

  const thread = () => getView()?.thread;
  const pr = () => getView()?.pr;
  const mergeable = () => getView()?.mergeable;

  const comment = async (e) => {
    e.preventDefault();
    if (!getBody().trim()) return;
    setBusy(true);
    try {
      await ctx.repoClient.pulls.comment(num(), getBody().trim());
      setBody("");
      reload();
    } catch (err) {
      reportError(err);
    } finally {
      setBusy(false);
    }
  };

  const pollTask = async () => {
    for (let i = 0; i < 60; i++) {
      try {
        const { task } = await ctx.repoClient.pulls.mergeTask(num());
        setTask(task);
        if (task?.state !== "running") {
          reload();
          return;
        }
      } catch {
        return;
      }
      await new Promise((r) => setTimeout(r, 2000));
    }
  };

  const merge = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      const { task } = await ctx.repoClient.pulls.merge(num(), { strategy: getStrategy() });
      setTask(task);
      pollTask();
    } catch (err) {
      reportError(err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="grid gap-6 lg:grid-cols-[1fr_320px]">
      <section aria-label="Conversation">
        <h1 class="mb-1 text-xl font-semibold">
          #{num()} {thread()?.title}
        </h1>
        <p class="mb-4 text-sm text-zinc-500 dark:text-zinc-400">
          {thread()?.state} · {thread()?.author} · {fmtDate(thread()?.updated_at)}
        </p>
        <ul class="card-list">
          <For each={getView()?.events ?? []} fallback={<li class="card">No events yet.</li>}>
            {(ev) => (
              <li class="card">
                <div class="card-meta">
                  <span>{ev.actor}</span>
                  <span>{fmtDate(ev.at)}</span>
                </div>
                <Show when={eventText(ev)} fallback={<p class="whitespace-pre-wrap">{ev.body}</p>}>
                  <p class="text-sm italic">{eventText(ev)}</p>
                </Show>
              </li>
            )}
          </For>
        </ul>
        <form class="card mt-3" onSubmit={comment}>
          <label class="field">
            <span>Comment</span>
            <textarea value={getBody()} onInput={(e) => setBody(e.target.value)} rows="3" />
          </label>
          <button type="submit" class="btn btn-primary mt-2 px-3 py-1" disabled={getBusy()}>
            {getBusy() ? "posting…" : "comment"}
          </button>
        </form>
      </section>
      <aside aria-label="Merge" class="flex flex-col gap-4">
        <div class="card">
          <h2 class="mb-2 text-sm font-semibold">Mergeability</h2>
          <p class="text-sm">{mergeableText(mergeable())}</p>
          <Show when={!getView()?.head_ref_ok}>
            <p class="mt-1 text-xs text-amber-600 dark:text-amber-400">head ref pending — push first.</p>
          </Show>
          <p class="mt-2 text-xs text-zinc-500 dark:text-zinc-400">
            base {pr()?.base?.ref} @ {(pr()?.base?.sha ?? "").slice(0, 12)}
            <br />
            head {pr()?.head?.ref} @ {(pr()?.head?.sha ?? "").slice(0, 12)}
          </p>
          <div class="mt-2 flex gap-2 text-xs">
            <A href={`/${ctx.owner}/${ctx.name}/pull/${num()}/commits`}>commits</A>
            <A href={`/${ctx.owner}/${ctx.name}/pull/${num()}/files`}>files</A>
          </div>
        </div>
        <Show when={!pr()?.merged && thread()?.state === "open"}>
          <form class="card" onSubmit={merge}>
            <h2 class="mb-2 text-sm font-semibold">Merge</h2>
            <label class="field">
              <span>Strategy</span>
              <select value={getStrategy()} onInput={(e) => setStrategy(e.target.value)}>
                <option value="merge">merge</option>
                <option value="squash">squash</option>
                <option value="rebase">rebase</option>
              </select>
            </label>
            <button type="submit" class="btn btn-primary mt-2 px-3 py-1" disabled={getBusy()}>
              {getBusy() ? "merging…" : "merge pull request"}
            </button>
            <Show when={getTask()}>
              <p class="mt-2 text-xs">
                task {getTask()?.state}
                <Show when={getTask()?.error}>: {getTask()?.error}</Show>
              </p>
              <ul class="mt-1 text-xs text-zinc-500 dark:text-zinc-400">
                <For each={getTask()?.progress ?? []}>{(line) => <li>{line}</li>}</For>
              </ul>
            </Show>
          </form>
        </Show>
        <Show when={pr()?.merged}>
          <p class="card text-sm">
            merged as {(pr()?.merge_commit_sha ?? "").slice(0, 12)} by {pr()?.merged_by}
          </p>
        </Show>
      </aside>
    </div>
  );
}
