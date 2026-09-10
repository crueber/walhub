// web/src/components/CommentComposer.jsx — 08 §2 CommentComposer.
//
// The ONE new-comment editor: markdown-lite hint, mentions autocomplete
// fed by the assignables cache (repo collaborators ∪ org members),
// submit via the feature's comment endpoint. Internal busy guard doubles
// as the double-submit guard (disabled while posting + guard clause).
// Optional close controls (issues #33, honest-close #109, split-button
// #311): `closeLabel` + `onClose` render a Close/Reopen control,
// `onCommentAndClose` a "Comment and Close" control (posts the body when
// non-empty, then closes — GitHub semantics). When `closeChooser` is set
// (open issue), the close controls are SPLIT buttons — primary segment
// closes immediately as completed (one click, the API default made
// explicit via CLOSE_COMPLETED), ▾ segment opens a menu with the
// not-planned alternate only (CLOSE_NOT_PLANNED). The reason is passed as
// onClose(reason) / onCommentAndClose(body, reason), so the UI never
// silently claims completion. Reopen stays a plain button (no reason
// to choose). Menus are native buttons (role menu/menuitem), Escape
// closes and refocuses the toggle, any outside click dismisses. All
// actions share one right-aligned row; the composer clears only when the
// handler resolves, so a failed post keeps its text for retry.

import { createSignal, For, Show, onCleanup } from "solid-js";
import { reportError } from "../lib/data.js";
import { MentionDatalist } from "../pages/Mentions.jsx";
import { CLOSE_COMPLETED, CLOSE_NOT_PLANNED } from "../lib/issue-events.js";
import { filesFromPasteEvent, filesFromDropEvent, uploadFilesSequential } from "../lib/attachUpload.js";

/** Split-button close control (issue #311, GitHub pattern): the primary
 * segment runs the default action immediately (close as completed — one
 * click, no menu); the ▾ segment opens a menu with the alternate
 * (not-planned) variant only — the button IS the completed path, so the
 * menu never needs a "completed" item.
 *
 * Props: { primaryLabel, primaryAria, menuLabel, alternateVerb,
 * alternateReason, disabled, onPrimary, onPick }.
 *
 * Dismissal: Escape (refocuses the ▾ toggle), Tab-out, and any pointer
 * click outside the whole split root — the document listener ignores
 * clicks inside `root` (both segments + menu), so clicking ▾ itself
 * never close-then-reopens; it just toggles. Listener removed in
 * onCleanup (RefPicker/ReactionMenu pattern). Menu opens upward
 * (bottom-full) beneath the composer, as before. */
function SplitCloseMenu(props) {
  const [getOpen, setOpen] = createSignal(false);
  let root;
  let toggleRef;
  let menuRef;

  const close = (refocus) => {
    setOpen(false);
    if (refocus) toggleRef?.focus();
  };
  const toggle = (e) => {
    e.preventDefault();
    setOpen((o) => !o);
    if (!getOpen()) {
      // Focus the item once the menu renders.
      queueMicrotask(() => menuRef?.querySelector("button")?.focus());
    }
  };
  const onDocClick = (e) => {
    if (getOpen() && root && !root.contains(e.target)) close(false);
  };
  document.addEventListener("click", onDocClick);
  onCleanup(() => document.removeEventListener("click", onDocClick));

  const onMenuKey = (e) => {
    const items = [...(menuRef?.querySelectorAll("button") ?? [])];
    const i = items.indexOf(document.activeElement);
    if (e.key === "Escape") {
      e.preventDefault();
      close(true);
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      items[(i + 1) % items.length]?.focus();
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      items[(i - 1 + items.length) % items.length]?.focus();
    } else if (e.key === "Tab") {
      // Tabbing out dismisses without choosing.
      setOpen(false);
    }
  };
  const choose = (reason) => {
    setOpen(false);
    props.onPick(reason);
  };

  return (
    <div
      class="relative inline-flex"
      ref={root}
      onKeyDown={(e) => e.key === "Escape" && getOpen() && (e.preventDefault(), close(true))}
    >
      <button
        type="button"
        class="btn rounded-r-none border-r-0"
        aria-label={props.primaryAria ?? props.primaryLabel}
        disabled={props.disabled}
        onClick={(e) => {
          e.preventDefault();
          props.onPrimary();
        }}
      >
        {props.primaryLabel}
      </button>
      <button
        ref={toggleRef}
        type="button"
        class="btn rounded-l-none px-2"
        aria-haspopup="menu"
        aria-expanded={getOpen() ? "true" : "false"}
        aria-label={`${props.menuLabel ?? props.primaryLabel}: more options`}
        disabled={props.disabled}
        onClick={toggle}
      >
        <span aria-hidden="true">▾</span>
      </button>
      <Show when={getOpen()}>
        <div
          ref={menuRef}
          role="menu"
          aria-label={props.menuLabel ?? props.primaryLabel}
          class="close-drop card absolute bottom-full right-0 z-10 mb-1 grid min-w-52 gap-0.5 p-1"
          onKeyDown={onMenuKey}
        >
          <For each={[{ reason: props.alternateReason, verb: props.alternateVerb }]}>
            {(c) => (
              <button
                type="button"
                role="menuitem"
                class="rounded-md px-3 py-1.5 text-left text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800"
                disabled={props.disabled}
                onClick={() => choose(c.reason)}
              >
                {c.verb}
              </button>
            )}
          </For>
        </div>
      </Show>
    </div>
  );
}

export default function CommentComposer(props) {
  const [getBody, setBody] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);
  // Paste/drop image upload (02 §12): Issue.jsx passes `uploader` (the
  // repo attachments surface); surfaces without it (PRs, until #120's
  // follow-up switches Pull.jsx rendering to markdown+sanitize) keep a
  // plain textarea. Sequential uploads, placeholder at the cursor.
  let textRef;
  const uploader = () => props.uploader ?? null;

  const onPaste = async (e) => {
    const up = uploader();
    if (!up) return;
    const files = filesFromPasteEvent(e);
    if (!files.length) return; // text paste: leave the default behavior alone
    e.preventDefault();
    await uploadFilesSequential({
      files,
      textarea: textRef,
      getText: getBody,
      setText: setBody,
      upload: (f) => up.upload(f),
      onError: (err) => reportError(err, props.errorKey ?? "comment"),
    });
  };

  const onDrop = async (e) => {
    const up = uploader();
    if (!up) return;
    const files = filesFromDropEvent(e);
    if (!files.length) return;
    e.preventDefault();
    await uploadFilesSequential({
      files,
      textarea: textRef,
      getText: getBody,
      setText: setBody,
      upload: (f) => up.upload(f),
      onError: (err) => reportError(err, props.errorKey ?? "comment"),
    });
  };

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

  const commentAndClose = async (body, reason) => {
    if (getBusy() || !props.onCommentAndClose) return;
    setBusy(true);
    try {
      await props.onCommentAndClose(body, reason);
      setBody("");
    } catch (err) {
      reportError(err, props.errorKey ?? "comment");
    } finally {
      setBusy(false);
    }
  };

  const runClose = async (reason) => {
    if (getBusy() || !props.onClose) return;
    setBusy(true);
    try {
      await props.onClose(reason);
    } catch (err) {
      reportError(err, props.errorKey ?? "comment");
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="card mt-3 grid gap-2 p-3" onSubmit={submit} aria-label={props.label ?? "New comment"}>
      <textarea
        ref={textRef}
        class="input min-h-24 font-mono text-sm"
        value={getBody()}
        onInput={(e) => setBody(e.target.value)}
        onPaste={onPaste}
        onDrop={onDrop}
        placeholder={props.placeholder ?? "Write a comment… (#N links issues, @user mentions)"}
        aria-label="comment body"
        list={listId()}
      />
      <Show when={props.mentionNames}>
        <MentionDatalist id={listId()} names={props.mentionNames} />
      </Show>
      <div class="flex flex-wrap items-center justify-end gap-2">
        <Show when={props.onClose}>
          <Show
            when={props.closeChooser}
            fallback={
              <button type="button" class="btn" disabled={getBusy()} onClick={() => runClose()}>
                {props.closeLabel ?? "Close"}
              </button>
            }
          >
            <SplitCloseMenu
              primaryLabel={props.closeLabel ?? "Close"}
              primaryAria={`Close as completed`}
              menuLabel="Close options"
              alternateVerb="Close as not planned"
              alternateReason={CLOSE_NOT_PLANNED}
              disabled={getBusy()}
              onPrimary={() => runClose(CLOSE_COMPLETED)}
              onPick={(reason) => runClose(reason)}
            />
          </Show>
        </Show>
        <Show when={props.onCommentAndClose}>
          <Show
            when={props.closeChooser}
            fallback={
              <button
                type="button"
                class="btn"
                disabled={getBusy()}
                onClick={(e) => {
                  e.preventDefault();
                  commentAndClose(getBody());
                }}
              >
                {props.commentAndCloseLabel ?? "Comment and Close"}
              </button>
            }
          >
            <SplitCloseMenu
              primaryLabel={props.commentAndCloseLabel ?? "Comment and Close"}
              primaryAria={`Comment and close as completed`}
              menuLabel="Comment and close options"
              alternateVerb="Comment and close as not planned"
              alternateReason={CLOSE_NOT_PLANNED}
              disabled={getBusy()}
              onPrimary={() => commentAndClose(getBody(), CLOSE_COMPLETED)}
              onPick={(reason) => commentAndClose(getBody(), reason)}
            />
          </Show>
        </Show>
        <button type="submit" class="btn primary" disabled={getBusy() || !getBody().trim()}>
          {getBusy() ? "Posting…" : (props.submitLabel ?? "Comment")}
        </button>
      </div>
    </form>
  );
}
