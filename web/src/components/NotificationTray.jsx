// web/src/components/NotificationTray.jsx — 08 §2 NotificationTray.
//
// The global chrome bell + dropdown (all pages): unread count as a
// signal, recent unread rows, "mark all read", and a link to the full
// /notifications tray page. Source: the per-user stream (app-lifetime,
// started once — never re-opened per navigation) plus the
// `notifications:me` cache. Focus returns to the bell on Esc/close;
// the badge is an aria-live region.

import { createSignal, For, Show, onCleanup, onMount } from "solid-js";
import { A } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { useData, invalidate, reportError } from "../lib/data.js";
import { TTL } from "../lib/collab.js";
import { unreadCount, refreshUnread } from "../pages/Notifications.jsx";

export default function NotificationTray() {
  const [getOpen, setOpen] = createSignal(false);
  const [getItems] = useData(
    "notifications:me",
    () => repos.notifications.list({ state: "unread", n: 10 }),
    TTL.notifications,
  );
  let bellRef = null;
  let cancelStream = null;

  onMount(() => {
    // App-lifetime per-user stream: one connection, never re-opened per
    // navigation; frames bump the badge and invalidate the tray cache.
    let alive = true;
    repos.notifications
      .stream(() => {
        if (!alive) return;
        refreshUnread();
        invalidate("notifications:me");
      })
      .then((c) => {
        cancelStream = c;
        if (!alive && cancelStream) cancelStream();
      })
      .catch(() => {});
  });
  onCleanup(() => {
    if (cancelStream) cancelStream();
  });

  const close = () => {
    setOpen(false);
    bellRef?.focus();
  };
  const onKey = (e) => {
    if (e.key === "Escape") close();
  };

  const markAll = async () => {
    try {
      await repos.notifications.markAllRead();
      invalidate("notifications:me");
      refreshUnread();
    } catch (err) {
      reportError(err, "notifications-read-all");
    }
  };

  return (
    <div class="relative" onKeyDown={onKey}>
      <button
        ref={bellRef}
        type="button"
        class="btn relative px-2 py-1"
        title="Notifications"
        aria-label={unreadCount() ? `${unreadCount()} unread notifications` : "Notifications"}
        aria-expanded={getOpen()}
        aria-haspopup="true"
        onClick={() => (getOpen() ? close() : setOpen(true))}
      >
        <span aria-hidden="true">🔔</span>
        <Show when={(unreadCount() ?? 0) > 0}>
          <span
            aria-live="polite"
            class="absolute -right-1 -top-1 min-w-5 rounded-full bg-emerald-500 px-1 text-center text-[11px] font-semibold leading-5 text-white"
          >
            {(unreadCount() ?? 0) > 99 ? "99+" : unreadCount()}
          </span>
        </Show>
      </button>
      <Show when={getOpen()}>
        <div
          class="notif-drop scroll-slim card absolute right-0 z-50 mt-1 max-h-96 w-80 overflow-y-auto p-2"
          role="menu"
          aria-label="Unread notifications"
        >
          <div class="mb-1 flex items-center justify-between px-1">
            <span class="text-xs font-semibold">Unread</span>
            <div class="flex gap-2">
              <button type="button" class="link text-xs" onClick={markAll}>
                mark all read
              </button>
              <A href="/notifications" class="link text-xs" onClick={() => setOpen(false)}>
                view all
              </A>
            </div>
          </div>
          <For each={getItems()?.notifications ?? []} fallback={<p class="px-1 text-xs text-zinc-500 dark:text-zinc-400">all caught up</p>}>
            {(n) => (
              <A
                href={n.num > 0 ? (n.kind === "pull" ? `/${n.repo}/pull/${n.num}` : `/${n.repo}/issues/${n.num}`) : `/${n.repo}`}
                class="block rounded px-1 py-1 text-xs hover:bg-zinc-100 dark:hover:bg-zinc-800"
                role="menuitem"
                onClick={() => setOpen(false)}
              >
                <span class="font-medium">{n.repo}#{n.num}</span> · {n.reason}
              </A>
            )}
          </For>
        </div>
      </Show>
    </div>
  );
}
