// web/src/components/ReactionMenu.jsx — issue #113: the per-comment "+"
// add-reaction menu (the thread-page half of the reactions surface; the
// summary chips are the toggle-off half, owned by the page).
//
// Contract: the page passes the ALREADY-FILTERED addable contents
// (reactions.addableReactions: REACTIONS minus whatever the summary shows
// for this seq), a per-content busy predicate, and onAdd. This component
// never fetches and never filters — it renders what it is given, so the
// menu always agrees with the chips. Props: { seq, addable: string[],
// isBusy(content) → bool, onAdd(content) }.
//
// The trigger sits INSIDE the summary row (not in the comment header),
// so a comment with no reactions shows a lone "+" where its chips will
// appear. Keyboard: the trigger and every row are native <button>s
// (Tab/Enter/Space free); ArrowDown on the trigger opens the menu into
// the first row; ArrowUp/Down/Home/End move between rows; Esc closes and
// restores focus to the trigger; a pointer click outside closes it.
// Colors work in dark + light via the shared chip/card classes.
//
// ### Concurrency
// Hazard: double-clicking a row fires two POSTs; the second add is a
// server-side duplicate no-op (02 §8), but the page's per-(seq, content)
// busy key disables the row while its mutation is in flight, so the pair
// cannot happen from one client (same idiom as LabelPicker rows).

import { createSignal, For, Show, onCleanup } from "solid-js";
import { reactionEmoji } from "../lib/reactions.js";

export default function ReactionMenu(props) {
  const [getOpen, setOpen] = createSignal(false);
  let root;
  let trigger;
  let menu;

  const rows = () => Array.from(menu?.querySelectorAll("button:not([disabled])") ?? []);
  const focusRow = (i) => {
    const list = rows();
    if (!list.length) return;
    list[((i % list.length) + list.length) % list.length]?.focus();
  };

  const close = (refocus) => {
    setOpen(false);
    if (refocus) trigger?.focus();
  };

  const onDocClick = (e) => {
    if (getOpen() && root && !root.contains(e.target)) close(false);
  };
  const onDocKey = (e) => {
    if (getOpen() && e.key === "Escape") close(true);
  };
  document.addEventListener("click", onDocClick);
  document.addEventListener("keydown", onDocKey);
  onCleanup(() => {
    document.removeEventListener("click", onDocClick);
    document.removeEventListener("keydown", onDocKey);
  });

  const openInto = () => {
    if (!props.addable?.length) return;
    setOpen(true);
    // The <Show> commits synchronously, but focus after paint so the
    // row is focusable in every browser.
    queueMicrotask(() => focusRow(0));
  };

  const onMenuKey = (e) => {
    const list = rows();
    const i = list.indexOf(document.activeElement);
    if (e.key === "ArrowDown") {
      e.preventDefault();
      focusRow(i < 0 ? 0 : i + 1);
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      focusRow(i < 0 ? -1 : i - 1);
    } else if (e.key === "Home") {
      e.preventDefault();
      focusRow(0);
    } else if (e.key === "End") {
      e.preventDefault();
      focusRow(-1);
    }
  };

  const empty = () => !(props.addable?.length > 0);
  return (
    <div class="reaction-menu relative inline-block" ref={root}>
      <button
        type="button"
        ref={trigger}
        class="chip hover:border-zinc-400 disabled:cursor-not-allowed disabled:opacity-50"
        aria-haspopup="menu"
        aria-expanded={getOpen() ? "true" : "false"}
        aria-label={
          empty() ? `all reactions already on comment ${props.seq}` : `add reaction on comment ${props.seq}`
        }
        title={empty() ? "all reactions already added" : "add reaction"}
        disabled={empty()}
        onClick={() => (getOpen() ? close(false) : openInto())}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown" && !getOpen()) {
            e.preventDefault();
            openInto();
          }
        }}
      >
        <span aria-hidden="true">+</span>
      </button>
      <Show when={getOpen() && !empty()}>
        <div
          class="reaction-drop card absolute left-0 z-30 mt-1 flex gap-1 p-1"
          role="menu"
          aria-label={`Add reaction on comment ${props.seq}`}
          ref={menu}
          onKeyDown={onMenuKey}
        >
          <For each={props.addable ?? []}>
            {(content) => (
              <button
                type="button"
                role="menuitem"
                class="chip hover:border-zinc-400 disabled:cursor-wait disabled:opacity-50"
                aria-label={`react ${content}`}
                title={`react ${content}`}
                disabled={props.isBusy?.(content) ?? false}
                onClick={() => {
                  props.onAdd?.(content);
                  close(true);
                }}
              >
                <span aria-hidden="true">{reactionEmoji(content)}</span>
              </button>
            )}
          </For>
        </div>
      </Show>
    </div>
  );
}
