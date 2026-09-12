// web/src/components/IdentityMenu.jsx — Forgejo #371: the navbar identity
// control (upper right, far right of the cluster): a bare-circle
// avatar-or-initials trigger opening a dropdown with profile, keys,
// invitations, setup (admin-only), and log out.
//
// Forgejo #390 (restyle only): the trigger dropped the btn box entirely —
// the avatar is a standalone circle (h-8 w-8) with a light outer ring
// (ring-1 zinc) that emphasizes on hover (ring-2 emerald); a small caret
// beside the circle plus the hover ring keeps the dropdown affordance.
// Without an avatar the username's initial renders in the same circle
// shape. Popover behavior (outside-click, Esc + focus return, arrows,
// menu roles) is untouched.
//
// Props: { username, avatarUrl?, items } — items are navModel menuItems
// ({kind, label, href}); logout renders as a plain anchor (GET
// /_auth/logout clears the session cookie server-side and 302s home),
// everything else as router links. avatarUrl is optional (Forgejo #376
// — me().avatar_url, omitted when the user has none): without one the
// username renders instead.
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
        class="group flex max-w-32 items-center gap-0.5 rounded-full p-0.5 outline-none focus-visible:ring-2 focus-visible:ring-emerald-500/60"
        title={props.username}
        aria-label={`Account: ${props.username}`}
        aria-haspopup="menu"
        aria-expanded={getOpen() ? "true" : "false"}
        onClick={toggle}
      >
        <Show
          when={props.avatarUrl}
          fallback={
            <span
              aria-hidden="true"
              class="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-zinc-200 text-sm font-semibold uppercase text-zinc-600 ring-1 ring-zinc-300 transition group-hover:ring-2 group-hover:ring-emerald-500/50 dark:bg-zinc-700 dark:text-zinc-200 dark:ring-zinc-600"
            >
              {props.username?.slice(0, 1)}
            </span>
          }
        >
          <img
            src={props.avatarUrl}
            alt=""
            class="h-8 w-8 shrink-0 rounded-full ring-1 ring-zinc-300 transition group-hover:ring-2 group-hover:ring-emerald-500/50 dark:ring-zinc-600"
          />
        </Show>
        <span
          aria-hidden="true"
          class="select-none text-[10px] leading-none text-zinc-400 transition group-hover:text-zinc-600 dark:text-zinc-500 dark:group-hover:text-zinc-300"
        >
          ▾
        </span>
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
