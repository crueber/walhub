// web/src/components/CommentComposer.jsx — 08 §2 CommentComposer.
//
// The ONE new-comment editor: markdown-lite hint, mentions autocomplete
// fed by the assignables cache (repo collaborators ∪ org members),
// submit via the feature's comment endpoint. Internal busy guard doubles
// as the double-submit guard (disabled while posting + guard clause).

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
      <div>
        <button type="submit" class="btn primary" disabled={getBusy() || !getBody().trim()}>
          {getBusy() ? "Posting…" : (props.submitLabel ?? "Comment")}
        </button>
      </div>
    </form>
  );
}
