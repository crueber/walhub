// web/src/pages/PullFiles.jsx — route "/:owner/:name/pull/:num/files"
// (08 §1): the PR diff as a file list with per-file unified/split hunks
// (the DiffPage contract — parsed by lib/diff.js, rendered by the shared
// selectable DiffBody; line-thread anchoring lives on the main PR page).

import { createSignal, For, Show } from "solid-js";
import { A, useParams } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { useData, invalidate } from "../lib/data.js";
import { TTL } from "../lib/collab.js";
import { useCollabStream } from "../components/collab.jsx";
import { parsePatchFiles, normalizePatchBody } from "../lib/diff.js";
import { DiffBody } from "../components/DiffTable.jsx";
import CommentComposer from "../components/CommentComposer.jsx";
import { useRole } from "../components/perms.jsx";
import repos from "../../sdk/src/index.js";
import { isAnonymousViewer } from "../lib/writeGate.js";
import { pullCommentLock } from "../lib/pull-state.js";
import {
  findHunkForSelection,
  selectionToAnchor,
  anchorLabel,
} from "../lib/review-anchor.js";

// Per-file unified/split toggle over the shared selectable DiffBody
// (issue #244) — the same renderer as the commit page, so #L selection,
// gutters, and shareable hashes land everywhere at once.
//
// Forgejo #546: with a selection the DiffBody offers "comment on
// selection"; the staged selection builds a §4 anchor (single-line keeps
// the stageLine shape, multi-line becomes a range — both via
// lib/review-anchor.js, hash inputs byte-identical) and the shared
// CommentComposer posts it through pulls.threads.create — the same
// conversation/threads list the PR page renders, no new wire surface.
function PullDiffFile(props) {
  const [getMode, setMode] = createSignal("unified");
  const [getStaged, setStaged] = createSignal(null);
  const [getCreated, setCreated] = createSignal(null);

  const stagedAnchor = () => {
    const sel = getStaged();
    if (!sel) return null;
    const found = findHunkForSelection(props.file, sel);
    if (!found) return null;
    return selectionToAnchor({ file: props.file, hunk: found.hunk, selection: sel, head: props.head });
  };

  const submitThread = async (body) => {
    if (props.commentLocked) return; // #594: the write never fires while locked
    const anchor = stagedAnchor();
    if (!anchor) return;
    const { thread } = await props.client.pulls.threads.create(props.num, { anchor, body }, { noPopupAuth: true });
    setStaged(null);
    setCreated(thread?.tid ?? null);
    invalidate(props.threadsKey);
  };

  // Dismissal (Forgejo #566, the SplitCloseMenu convention in
  // CommentComposer.jsx:38-43): Cancel + Escape drop only the staged
  // selection composer — no onCommentSelect replay, no POST — and return
  // focus to the "comment on selection" trigger that staged it. focus()
  // never fires click, so refocus cannot re-stage (no toggle-fight).
  // getCreated is untouched: a posted thread is history, not a draft.
  // No document listener (the panel onKeyDown below catches the bubbled
  // Escape from the textarea), so no onCleanup.
  const dismissStaged = (refocus) => {
    setStaged(null);
    if (refocus) document.querySelector('[aria-label^="Comment on selected lines"]')?.focus?.();
  };

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
        <DiffBody
          file={props.file}
          mode={getMode()}
          canComment={props.canComment && !props.commentLocked}
          onCommentSelect={(sel) => {
            setCreated(null);
            setStaged(sel);
          }}
        />
      </div>
      <Show when={getStaged() && stagedAnchor()}>
        {/* Dismissable staged composer (Forgejo #566, Cancel moved to
            the bottom row by #587): the composer takes an onCancel prop
            rendering Cancel at the LEFT of its bottom action row
            (opposite the right-aligned submit — the canonical .btn
            idiom, guideline §2 Controls, readable in both themes);
            Escape anywhere inside the panel dismisses via dismissStaged
            above. Re-stage mounts a fresh empty CommentComposer (the
            Show unmounts it with its text). */}
        <div
          class="mt-2 border-t border-zinc-200 pt-2 dark:border-zinc-800"
          onKeyDown={(e) => {
            if (e.key !== "Escape") return;
            e.preventDefault();
            e.stopPropagation();
            dismissStaged(true);
          }}
        >
          <p class="text-xs text-zinc-500 dark:text-zinc-400">
            commenting on <span class="font-mono">{anchorLabel(stagedAnchor())}</span>
          </p>
          <CommentComposer
            onSubmit={submitThread}
            submitLabel="Start thread"
            onCancel={() => dismissStaged(false)}
            disabled={props.commentLocked}
            disabledReason={props.commentLockReason}
            placeholder={`Comment on ${anchorLabel(stagedAnchor())}…`}
            errorKey="thread-create"
            label={`Comment on ${anchorLabel(stagedAnchor())}`}
          />
        </div>
      </Show>
      <Show when={getStaged() && !stagedAnchor()}>
        <p class="err-line mt-2 text-xs" role="alert">
          that selection names no line in this diff anymore — pick again.
        </p>
      </Show>
      <Show when={getCreated()}>
        <p class="mt-2 text-xs text-zinc-500 dark:text-zinc-400">
          thread {getCreated()} started —{" "}
          <A class="link" href={`/${props.full}/pull/${props.num}`}>
            view it in the conversation
          </A>
        </p>
      </Show>
    </div>
  );
}

export default function PullFiles() {
  const ctx = useRepo();
  const params = useParams();
  const num = () => params.num;
  const key = () => `pulldiff:${ctx.full}:${num()}`;
  const threadsKey = () => `threads:${ctx.full}:${num()}`;
  const pullKey = () => `pull:${ctx.full}:${num()}`;
  // Issue #520: useData surfaces fetch failures only in the global tray,
  // so the page tracks its own error for the inline state + Retry below —
  // otherwise a failed diff sits on "loading diff…" forever with a lying
  // Files (0) heading.
  const [getDiffError, setDiffError] = createSignal(null);
  const [getView] = useData(key, async () => {
    setDiffError(null);
    try {
      const res = await ctx.repoClient.pulls.diff(num());
      return parsePatchFiles(normalizePatchBody(res));
    } catch (err) {
      setDiffError(err);
      throw err;
    }
    // NOTE: TTL.pulls (5 s), not TTL.diff (∞) — the key is not
    // sha-addressed, so ∞ would serve a stale diff forever after the
    // head moves (08 §6 reserves ∞ for immutable content).
  }, TTL.pulls);
  // PR header for the anchor commit_sha pin (the same head the
  // conversation page stages: live sha, falling back to pr.head.sha).
  const [getPull] = useData(pullKey, () => ctx.repoClient.pulls.get(num()).catch(() => null), TTL.pulls);
  const head = () => getPull()?.head_live_sha ?? getPull()?.pr?.head?.sha ?? "";
  const retryDiff = () => {
    setDiffError(null);
    invalidate(key());
  };
  // Honest heading (issue #520): the count renders only once the view is
  // loaded — "…" while loading, "failed to load" on error — never (0) for
  // a diff that never arrived. A loaded-empty diff ({files: []}) is
  // truthful: the For below renders "empty diff".
  const diffCount = () => {
    const v = getView();
    if (v) return String((v.files ?? []).length);
    return getDiffError() ? "failed to load" : "…";
  };
  // Thread-create gate (Forgejo #546, the #502 write-gate idiom):
  // OpenThread is authenticated + read — the same bar as the
  // conversation comment composer. Anonymous viewers get the sign-in
  // affordance, never a silent 401.
  const { role } = useRole(ctx.full, ctx.repoClient);
  const [getMe] = useData("me", () => repos.me().catch(() => null));
  const [getDiscovery] = useData("discovery", () => repos.discovery().catch(() => null));
  const anon = () => isAnonymousViewer(getMe(), getDiscovery());
  const canComment = () => role() !== null && !anon();
  // Forgejo #594: the comment lock keys off the live PR header (same
  // pull stream invalidates the key, so a reopen unlocks with no
  // reload). Locked hides the per-line staging triggers (DiffBody
  // canComment) and disables any staged composer with the reason.
  const commentLock = () => pullCommentLock(getPull()?.thread, getPull()?.pr);
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
      <h2 class="mb-3 text-lg font-semibold">Files on #{num()} ({diffCount()})</h2>
      <Show when={commentLock().locked}>
        <p class="muted mb-3 text-sm" role="note">{commentLock().reason}</p>
      </Show>
      <Show when={getView()} fallback={
        <Show when={getDiffError()} fallback={<p class="muted">loading diff…</p>}>
          <div class="card" role="alert">
            <p class="text-sm">Couldn't load the diff: {String(getDiffError()?.message ?? getDiffError())}</p>
            <button type="button" class="btn mt-2" onClick={retryDiff}>
              Retry
            </button>
          </div>
        </Show>
      }>
        <For each={getView().files ?? []} fallback={<p class="muted">empty diff</p>}>
          {(file) => (
            <PullDiffFile
              file={file}
              num={num()}
              full={ctx.full}
              client={ctx.repoClient}
              head={head()}
              threadsKey={threadsKey()}
              canComment={canComment()}
              commentLocked={commentLock().locked}
              commentLockReason={commentLock().reason}
            />
          )}
        </For>
      </Show>
    </div>
  );
}
