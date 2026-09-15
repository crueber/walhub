// web/src/components/MergeBox.jsx — 08 §2 MergeBox.
//
// Forgejo #531: the box renders as a VALUE inside the PR sidebar's one
// divide-y panel (the Issue.jsx:549 idiom) — no .card wrappers, no
// card-header heading; the parent section owns the "Merge" micro-label.
// Forgejo #592: the mergeability headline lives ONLY in the sidebar
// Mergeability section (the ONE mergeabilityDisplay call site) — the box
// renders strategy, buttons, and the amber blocking-reasons line, never
// a second status headline.
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
import { A } from "@solidjs/router";
import { reportError } from "../lib/data.js";
import { roleAtLeast } from "./perms.jsx";

/**
 * Derive the machine state from object state + local task state.
 * props: { full, pr, mergeable, checksBlockers[], reviewDecision, role,
 *   merging, task, requiresReviews }
 *
 * Forgejo #612: CHANGES_REQUESTED blocks ONLY when a required-reviews
 * policy rule applies to the base ref (requiresReviews !== false). The
 * server merges a changes-requested PR when no such rule exists
 * (GitHub-like, #586 Decision-1b), so the client must not claim blocked.
 * requiresReviews === false means "known: no applicable rule" (the page
 * derives it from the fetched policy via requiredReviewsApplies); any
 * other value (true, undefined — an older caller passing nothing) keeps
 * the old fail-closed block.
 */
export function mergeState(props) {
  if (props.pr?.merged) return "merged";
  if (props.task?.state === "failed" || props.task?.error) return "failed";
  if (props.merging || props.task?.state === "running") return "merging";
  if (props.pr?.draft) return "draft";
  if ((props.checksBlockers ?? []).length > 0) return "blocked";
  if (props.reviewDecision === "CHANGES_REQUESTED" && props.requiresReviews !== false) return "blocked";
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
    requiresReviews: typeof props.requiresReviews === "function" ? props.requiresReviews() : props.requiresReviews,
    role: props.role?.(),
    merging: getMerging(),
    task: getTask(),
  });
  const canMerge = () => roleAtLeast(props.role?.(), "maintain");
  // Forgejo #612: the "changes requested" amber entry renders ONLY when
  // it actually blocks (requiresReviews !== false, the mergeState rule).
  // Without an applicable rule the #588 sidebar headline ("Changes
  // requested") already carries the information, and listing it here
  // would feed the tooltip a "blocked:" claim the server contradicts.
  const requiresReviews = () => {
    const v = typeof props.requiresReviews === "function" ? props.requiresReviews() : props.requiresReviews;
    return v !== false;
  };
  const blockers = () => {
    const out = [...(props.checksBlockers?.() ?? [])];
    if (props.reviewDecision?.() === "CHANGES_REQUESTED" && requiresReviews()) out.push("changes requested");
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
    <>
      <Show when={state() === "merging" || getTask()}>
        <div aria-live="polite" aria-label="Merge task">
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
        <form onSubmit={merge} aria-label="Merge">
          <label class="grid gap-1 mt-2">
            <span class="text-sm font-medium">Strategy</span>
            <select class="input w-full" value={getStrategy()} onInput={(e) => setStrategy(e.target.value)} disabled={getMerging()}>
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
        <p class="text-sm">
          merged as{" "}
          {/* Forgejo #595: the merge SHA links to the commit page — the
              merge commit is a child of base, so it never appears in the
              PR commits tab (base…head by documented design); without the
              link the SHA is a dead end. Full SHA in href, 12-char text. */}
          <A class="link font-mono" href={`/${props.full}/commit/${props.pr?.merge_commit_sha ?? ""}`}>
            {(props.pr?.merge_commit_sha ?? "").slice(0, 12)}
          </A>{" "}
          by {props.pr?.merged_by}
        </p>
      </Show>
    </>
  );
}
