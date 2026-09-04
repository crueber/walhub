// web/src/components/MergeBox.jsx — 08 §2 MergeBox.
//
// The PR merge control as an explicit state machine (per 03/04/05):
// draft → ready → blocked{checks, reviews, conflicts} → mergeable →
// merging(task) → merged | failed. Transitions recompute on header
// fetch, `check`/`review` SSE frames (via parent reload), and task
// packets. The merge button enables only in `mergeable` AND role ≥
// maintain, with the disabled tooltip listing missing/failing contexts.
// Merge runs the pull-merge task (P7): POST …/pulls/{num}/merge attaches
// to the merge-task record; progress pills render from progress packets,
// terminal result/error flips the machine exactly once.
//
// ### Concurrency — task attach
// Hazard: double-clicking merge starts two merge tasks; unmount mid-merge
// leaks the poller. Avoidance: the server (repo, kind) single-flight
// joins a running merge; the client disables the button while merging
// AND guards the handler; the poll loop is component-scoped and stops on
// unmount (onCleanup) or terminal state.

import { createSignal, For, Show, onCleanup } from "solid-js";
import { reportError } from "../lib/data.js";
import { roleAtLeast } from "./perms.jsx";

/**
 * Derive the machine state from object state + local task state.
 * props: { pr, mergeable, checksBlockers[], reviewDecision, role,
 *   merging, task }
 */
export function mergeState(props) {
  if (props.pr?.merged) return "merged";
  if (props.task?.state === "failed" || props.task?.error) return "failed";
  if (props.merging || props.task?.state === "running") return "merging";
  if (props.pr?.draft) return "draft";
  if ((props.checksBlockers ?? []).length > 0) return "blocked";
  if (props.reviewDecision === "CHANGES_REQUESTED") return "blocked";
  if (props.mergeable?.state === "dirty") return "blocked";
  if (props.mergeable?.state === "clean" || props.mergeable?.state === "behind") return "mergeable";
  return "ready";
}

export default function MergeBox(props) {
  const [getStrategy, setStrategy] = createSignal("merge");
  const [getMerging, setMerging] = createSignal(false);
  const [getTask, setTask] = createSignal(null);
  const [getSettled, setSettled] = createSignal(false);
  let pollTimer = 0;
  let alive = true;
  onCleanup(() => {
    alive = false;
    clearTimeout(pollTimer);
  });

  const state = () => mergeState({
    pr: props.pr,
    mergeable: props.mergeable,
    checksBlockers: props.checksBlockers?.(),
    reviewDecision: props.reviewDecision?.(),
    role: props.role?.(),
    merging: getMerging(),
    task: getTask(),
  });
  const canMerge = () => roleAtLeast(props.role?.(), "maintain");
  const blockers = () => {
    const out = [...(props.checksBlockers?.() ?? [])];
    if (props.reviewDecision?.() === "CHANGES_REQUESTED") out.push("changes requested");
    if (props.mergeable?.state === "dirty") {
      out.push(`conflicts: ${(props.mergeable?.conflicts ?? []).join(", ")}`);
    }
    return out;
  };
  const enabled = () => state() === "mergeable" && canMerge() && !getMerging();
  const tooltip = () => {
    if (!canMerge()) return "merging requires the maintain role";
    const b = blockers();
    if (b.length) return `blocked: ${b.join("; ")}`;
    if (state() === "draft") return "draft PRs cannot merge";
    if (state() === "merging") return "merge already running";
    if (state() === "merged") return "already merged";
    return "merge pull request";
  };

  const settle = (task) => {
    if (getSettled()) return; // terminal flip happens exactly once
    setSettled(true);
    setTask(task);
    setMerging(false);
    props.onSettled?.(task);
  };

  const pollTask = async () => {
    for (let i = 0; i < 60 && alive && !getSettled(); i++) {
      await new Promise((r) => {
        pollTimer = setTimeout(r, 2000);
      });
      if (!alive || getSettled()) return;
      try {
        const { task } = await props.client.pulls.mergeTask(props.num);
        if (!alive) return;
        setTask(task);
        if (task?.state !== "running") {
          settle(task);
          return;
        }
      } catch {
        return; // poll errors stop the attach; the record stays visible
      }
    }
  };

  const merge = async (e) => {
    e.preventDefault();
    if (getMerging() || !enabled()) return; // double-submit guard
    setMerging(true);
    setSettled(false);
    setTask(null);
    try {
      const { task } = await props.client.pulls.merge(props.num, { strategy: getStrategy() });
      if (!alive) return;
      setTask(task);
      if (task?.state !== "running") settle(task);
      else pollTask();
    } catch (err) {
      if (alive) {
        setMerging(false);
        reportError(err, "pull-merge");
      }
    }
  };

  const updateBranch = async () => {
    if (getMerging()) return;
    setMerging(true);
    try {
      await props.client.pulls.updateBranch(props.num);
      props.onSettled?.(null);
      props.reload?.();
    } catch (err) {
      reportError(err, "pull-update-branch");
    } finally {
      if (alive) setMerging(false);
    }
  };

  return (
    <div class="grid gap-4">
      <Show when={state() === "merging" || getTask()}>
        <div class="card" aria-live="polite" aria-label="Merge task">
          <p class="text-xs">
            merge task {getTask()?.state ?? "starting…"}
            <Show when={getTask()?.error}>: {getTask()?.error}</Show>
          </p>
          <ul class="mt-1 text-xs text-zinc-500 dark:text-zinc-400">
            <For each={getTask()?.progress ?? []}>{(line) => <li>{line}</li>}</For>
          </ul>
        </div>
      </Show>
      <Show when={!props.pr?.merged}>
        <form class="card" onSubmit={merge} aria-label="Merge">
          <h2 class="mb-2 text-sm font-semibold">Merge ({state()})</h2>
          <label class="field">
            <span>Strategy</span>
            <select value={getStrategy()} onInput={(e) => setStrategy(e.target.value)} disabled={getMerging()}>
              <option value="merge">merge</option>
              <option value="squash">squash</option>
              <option value="rebase">rebase</option>
            </select>
          </label>
          <div class="mt-2 flex flex-wrap gap-2">
            <button
              type="submit"
              class="btn btn-primary px-3 py-1"
              disabled={!enabled()}
              title={tooltip()}
            >
              {getMerging() ? "merging…" : "merge pull request"}
            </button>
            <Show when={props.canUpdate?.()}>
              <button
                type="button"
                class="btn px-3 py-1"
                disabled={getMerging()}
                title="update the PR branch from base"
                onClick={updateBranch}
              >
                update branch
              </button>
            </Show>
          </div>
          <Show when={blockers().length > 0}>
            <p class="mt-2 text-xs text-amber-600 dark:text-amber-400">
              blocking merge: {blockers().join("; ")}
            </p>
          </Show>
        </form>
      </Show>
      <Show when={props.pr?.merged}>
        <p class="card text-sm">
          merged as {(props.pr?.merge_commit_sha ?? "").slice(0, 12)} by {props.pr?.merged_by}
        </p>
      </Show>
    </div>
  );
}
