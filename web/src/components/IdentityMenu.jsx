// web/src/components/IdentityMenu.jsx — Forgejo #371: the navbar identity
// control (upper right, left of the tray): an avatar-or-username button
// opening a dropdown with profile, keys, invitations, setup (admin-only),
// and log out.
//
// Props: { username, avatarUrl?, items } — items are navModel menuItems
// ({kind, label, href}); logout renders as a plain anchor (GET
// /_auth/logout clears the session cookie server-side and 302s home),
// everything else as router links. avatarUrl is optional: no avatar
// storage exists yet (#349 candidate), so the username renders until
// avatars land and adding them later is a prop change.
//
// Dismissal mirrors the #255/#311 popover family (CommentComposer
// SplitCloseMenu): Escape closes and refocuses the toggle, arrow keys walk
// the items, Tab-out dismisses, any outside click dismisses (the toggle
// lives inside the root so it never fights the handler). Menus are native
// buttons/links (role menu/menuitem). No new deps.

import { createSignal, For, Show, onCleanup } from "solid-js";
import { A } from "@solidjs/router";

export default function IdentityMenu(props) {
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
      // Focus the first item once the menu renders.
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
        class="btn max-w-32 truncate px-2 py-1"
        title={props.username}
        aria-label={`Account: ${props.username}`}
        aria-haspopup="menu"
        aria-expanded={getOpen() ? "true" : "false"}
        onClick={toggle}
      >
        <Show
          when={props.avatarUrl}
          fallback={<span class="font-medium">{props.username}</span>}
        >
          <img src={props.avatarUrl} alt="" class="inline h-5 w-5 rounded-full" />
        </Show>
      </button>
      <Show when={getOpen()}>
        <div
          ref={menuRef}
          role="menu"
          aria-label="Account"
          class="card absolute right-0 z-50 mt-1 grid max-h-96 w-52 max-w-[calc(100vw-2rem)] gap-0.5 overflow-y-auto p-1"
          onKeyDown={onMenuKey}
        >
          <For each={props.items ?? []}>
            {(item) => (
              <Show
                when={item.kind !== "logout"}
                fallback={
                  <a
                    href={item.href}
                    role="menuitem"
                    class="rounded-md px-3 py-1.5 text-left text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800"
                  >
                    {item.label}
                  </a>
                }
              >
                <A
                  href={item.href}
                  role="menuitem"
                  class="rounded-md px-3 py-1.5 text-left text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800"
                  onClick={() => setOpen(false)}
                >
                  {item.label}
                </A>
              </Show>
            )}
          </For>
        </div>
      </Show>
    </div>
  );
}
