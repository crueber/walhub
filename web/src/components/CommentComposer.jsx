// web/src/components/CommentComposer.jsx — 08 §2 CommentComposer.
//
// The ONE new-comment editor: markdown-lite hint, mentions autocomplete
// fed by the assignables cache (repo collaborators ∪ org members),
// submit via the feature's comment endpoint. Internal busy guard doubles
// as the double-submit guard (disabled while posting + guard clause).
// Optional close controls (issues #33): `closeLabel` + `onClose` render a
// Close/Reopen button, `onCommentAndClose` a "Comment and Close" button
// (posts the body when non-empty, then closes — GitHub semantics). All
// actions share one right-aligned row; the composer clears only when the
// handler resolves, so a failed post keeps its text for retry.

import { createSignal, Show } from "solid-js";
import { reportError } from "../lib/data.js";
import { MentionDatalist } from "../pages/Mentions.jsx";

export default function CommentComposer(props) {
  const [getBody, setBody] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);

  const submit = async (e) => {
    e.preventDefault();
    const body = getBody().trim();
    if (!body || getBusy()) return; // double-submit guard
    setBusy(true);
    try {
      await props.onSubmit(body);
      setBody("");
    } catch (err) {
      reportError(err, props.errorKey ?? "comment");
    } finally {
      setBusy(false);
    }
  };

  const listId = () => props.mentionId ?? "mention-composer";

  const commentAndClose = async (e) => {
    e.preventDefault();
    if (getBusy() || !props.onCommentAndClose) return;
    setBusy(true);
    try {
      await props.onCommentAndClose(getBody());
      setBody("");
    } catch (err) {
      reportError(err, props.errorKey ?? "comment");
    } finally {
      setBusy(false);
    }
  };

  const runClose = async (e) => {
    e.preventDefault();
    if (getBusy() || !props.onClose) return;
    setBusy(true);
    try {
      await props.onClose();
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="card mt-3 grid gap-2 p-3" onSubmit={submit} aria-label={props.label ?? "New comment"}>
      <textarea
        class="input min-h-24 font-mono text-sm"
        value={getBody()}
        onInput={(e) => setBody(e.target.value)}
        placeholder={props.placeholder ?? "Write a comment… (#N links issues, @user mentions)"}
        aria-label="comment body"
        list={listId()}
      />
      <Show when={props.mentionNames}>
        <MentionDatalist id={listId()} names={props.mentionNames} />
      </Show>
      <div class="flex flex-wrap items-center justify-end gap-2">
        <Show when={props.onClose}>
          <button type="button" class="btn" disabled={getBusy()} onClick={runClose}>
            {props.closeLabel ?? "Close"}
          </button>
        </Show>
        <Show when={props.onCommentAndClose}>
          <button type="button" class="btn" disabled={getBusy()} onClick={commentAndClose}>
            {props.commentAndCloseLabel ?? "Comment and Close"}
          </button>
        </Show>
        <button type="submit" class="btn primary" disabled={getBusy() || !getBody().trim()}>
          {getBusy() ? "Posting…" : (props.submitLabel ?? "Comment")}
        </button>
      </div>
    </form>
  );
}
