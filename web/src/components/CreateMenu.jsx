// web/src/components/CreateMenu.jsx — Forgejo #466: the navbar create
// control (upper right, immediately left of the identity menu): a compact
// plus-icon button opening a dropdown with New repository (/new), Import
// repository (/import), and New organization (/orgs/new).
//
// Props: { items } — items are navModel createItems ({kind, label, href});
// all three targets are in-app routes, so every entry renders as a router
// link that navigates and closes the menu. The button renders for signed-in
// users only (App.jsx gates on nav().showCreate — the same condition as the
// identity menu): no permission probe here (law 6 — no new requests), the
// target pages enforce their own gates server-side and render their own
// errors. The glyph is the shared Icon "plus" (lib/icons.jsx — a minimal
// inline stroke, since #465 shipped no plus).
//
// Dismissal clones IdentityMenu.jsx (the #255/#311 popover family):
// Escape closes and refocuses the trigger, arrow keys walk the items,
// Tab-out dismisses, any outside click dismisses (the trigger lives inside
// the root so it never fights the handler). Entries are native links (role
// menu/menuitem). The panel carries max-w-[calc(100vw-1rem)] per the
// mobile-popover rule so it never clips at 390px. No new deps.

import { createSignal, For, Show, onCleanup } from "solid-js";
import { A } from "@solidjs/router";
import Icon from "../lib/icons.jsx";

export default function CreateMenu(props) {
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
    const opening = !getOpen();
    setOpen(opening);
    if (opening) {
      // Focus the first item once the menu renders (Forgejo #477: read the
      // pre-toggle value — getOpen() after setOpen already reflects the new
      // state, so the old `if (!getOpen())` check never fired on open).
      queueMicrotask(() => menuRef?.querySelector("[role=menuitem]")?.focus());
    }
  };

  const onDocClick = (e) => {
    if (getOpen() && root && !root.contains(e.target)) close(false);
  };
  document.addEventListener("click", onDocClick);
  onCleanup(() => document.removeEventListener("click", onDocClick));

  const onMenuKey = (e) => {
    const items = [...(menuRef?.querySelectorAll("[role=menuitem]") ?? [])];
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

  return (
    <div
      class="relative"
      ref={root}
      onKeyDown={(e) => e.key === "Escape" && getOpen() && (e.preventDefault(), close(true))}
    >
      <button
        ref={toggleRef}
        type="button"
        class="btn px-2 py-1"
        title="Create new"
        aria-label="Create new"
        aria-haspopup="menu"
        aria-expanded={getOpen() ? "true" : "false"}
        onClick={toggle}
      >
        <Icon name="plus" />
      </button>
      <Show when={getOpen()}>
        <div
          ref={menuRef}
          role="menu"
          aria-label="Create new"
          class="card absolute right-0 z-50 mt-1 grid max-h-96 w-52 max-w-[calc(100vw-1rem)] gap-0.5 overflow-y-auto p-1"
          onKeyDown={onMenuKey}
        >
          <For each={props.items ?? []}>
            {(item) => (
              <A
                href={item.href}
                role="menuitem"
                class="rounded-md px-3 py-1.5 text-left text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800"
                onClick={() => setOpen(false)}
              >
                {item.label}
              </A>
            )}
          </For>
        </div>
      </Show>
    </div>
  );
}
