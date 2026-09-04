// web/src/pages/Notifications.jsx — route "/notifications" (06 §7): the
// per-user tray (paged, all/unread tabs), row click navigates to the
// thread and marks read, and the per-user SSE stream prepends live
// items + bumps the chrome badge (via the shared unread signal here).

import { createSignal, createEffect, onCleanup, For, Show } from "solid-js";
import { A, useSearchParams, useNavigate } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { reportError } from "../lib/data.js";

/** Shared unread badge count (App chrome reads this; the tray writes it). */
export const [unreadCount, setUnreadCount] = createSignal(null);

export async function refreshUnread() {
  try {
    const res = await repos.notifications.unreadCount();
    setUnreadCount(res.count ?? 0);
  } catch {
    // Anonymous or offline: no badge (never a spinner).
    setUnreadCount(null);
  }
}

function reasonChip(reason) {
  const hot = reason === "mentioned" || reason === "team_mention" || reason === "assigned" || reason === "review_requested";
  return <span class={`chip ${hot ? "chip-open" : ""}`}>{reason}</span>;
}

function threadHref(n) {
  if (n.kind === "pull" && n.num > 0) return `/${n.repo}/pull/${n.num}`;
  if (n.num > 0) return `/${n.repo}/issues/${n.num}`;
  return `/${n.repo}`;
}

export default function Notifications() {
  const [search, setSearch] = useSearchParams();
  const navigate = useNavigate();
  const [getItems, setItems] = createSignal(null);
  const [getMore, setMore] = createSignal(false);
  const [getBusy, setBusy] = createSignal(false);

  const state = () => (search.state === "read" || search.state === "unread" ? search.state : "");

  const load = async (after) => {
    setBusy(true);
    try {
      const res = await repos.notifications.list({ state: state(), after: after || "", n: 50 });
      setItems((prev) => (after ? [...(prev ?? []), ...(res.notifications ?? [])] : (res.notifications ?? [])));
      setMore(!!res.more);
      if (!after) refreshUnread();
    } catch (e) {
      reportError(e, "notifications");
    } finally {
      setBusy(false);
    }
  };

  const reload = () => {
    setItems(null);
    load("");
  };

  createEffect(() => {
    state();
    reload();
  });

  // Live prepend: the per-user stream (cancelled with the page).
  createEffect(() => {
    let cancel = null;
    let alive = true;
    repos.notifications
      .stream((n) => {
        if (!alive) return;
        if (state() === "read") return;
        setItems((prev) => [n, ...(prev ?? [])]);
        refreshUnread();
      })
      .then((c) => {
        cancel = c;
        if (!alive && cancel) cancel();
      })
      .catch(() => {});
    onCleanup(() => {
      alive = false;
      if (cancel) cancel();
    });
  });

  const open = async (n) => {
    if (n.state === "unread") {
      try {
        await repos.notifications.markRead(n.id);
        setItems((prev) => (prev ?? []).map((x) => (x.id === n.id ? { ...x, state: "read" } : x)));
        refreshUnread();
      } catch (e) {
        reportError(e, "notifications-read");
      }
    }
    navigate(threadHref(n));
  };

  const markAll = async () => {
    try {
      await repos.notifications.markAllRead();
      reload();
    } catch (e) {
      reportError(e, "notifications-read-all");
    }
  };

  return (
    <div class="notifications-page mx-auto max-w-3xl">
      <div class="mb-3 flex flex-wrap items-center gap-2">
        <h2 class="text-lg font-semibold">Notifications</h2>
        <div class="ml-auto flex gap-2">
          <button type="button" class={`btn px-2 py-1 text-sm ${state() === "" ? "primary" : ""}`} onClick={() => setSearch({ state: undefined })}>
            all
          </button>
          <button type="button" class={`btn px-2 py-1 text-sm ${state() === "unread" ? "primary" : ""}`} onClick={() => setSearch({ state: "unread" })}>
            unread
          </button>
          <button type="button" class="btn px-2 py-1 text-sm" onClick={markAll} disabled={getBusy()}>
            mark all read
          </button>
        </div>
      </div>
      <Show when={getItems()} fallback={<p class="muted">loading…</p>}>
        <Show when={getItems().length > 0} fallback={<p class="muted">nothing here — watching a repo or being mentioned lands notifications in this tray.</p>}>
          <ol class="grid gap-2">
            <For each={getItems()}>
              {(n) => (
                <li>
                  <button
                    type="button"
                    onClick={() => open(n)}
                    class={`card flex w-full items-start gap-3 p-3 text-left hover:border-emerald-400 ${n.state === "unread" ? "border-l-4 border-l-emerald-500" : "opacity-75"}`}
                  >
                    <div class="min-w-0 flex-1">
                      <div class="flex flex-wrap items-center gap-2 text-sm">
                        {reasonChip(n.reason)}
                        <span class="muted font-mono text-xs">{n.repo}#{n.num}</span>
                        <span class="muted text-xs">{n.actor}</span>
                      </div>
                      <div class="mt-0.5 truncate font-medium">{n.title || "(no title)"}</div>
                    </div>
                    <Show when={n.state === "unread"}>
                      <span class="mt-1 inline-block h-2 w-2 shrink-0 rounded-full bg-emerald-500" aria-label="unread" />
                    </Show>
                  </button>
                </li>
              )}
            </For>
          </ol>
        </Show>
        <Show when={getMore()}>
          <button
            type="button"
            class="btn mt-3"
            disabled={getBusy()}
            onClick={() => {
              const items = getItems() ?? [];
              load(items.length ? items[items.length - 1].id : "");
            }}
          >
            {getBusy() ? "Loading…" : "Older"}
          </button>
        </Show>
      </Show>
    </div>
  );
}
