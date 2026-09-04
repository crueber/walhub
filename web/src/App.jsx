// web/src/App.jsx — the router root (D-WEB-6): site chrome (header, nav, theme
// toggle), the top progress bar fed by the data layer's pending counter, and
// the global error tray. Route content renders through props.children.

import { createEffect, createSignal, Show, For } from "solid-js";
import { A, useLocation } from "@solidjs/router";
import { usePending, trayErrors, dismissError } from "./lib/data.js";
import { theme, toggleTheme } from "./lib/store.js";
import { refreshUnread } from "./pages/Notifications.jsx";
import NotificationTray from "./components/NotificationTray.jsx";

export default function App(props) {
  const location = useLocation();
  const pending = usePending();
  const errors = trayErrors;

  // Progress bar: width follows the in-flight counter (never fully empty while
  // something runs; fades when idle).
  const [barWidth, setBarWidth] = createSignal("0%");
  createEffect(() => {
    const n = pending();
    setBarWidth(n > 0 ? `${Math.min(20 + n * 15, 90)}%` : "0%");
  });
  const [barVisible, setBarVisible] = createSignal(false);
  createEffect(() => {
    if (pending() > 0) setBarVisible(true);
    else setTimeout(() => setBarVisible(false), 300);
  });

  // Close the mobile nav on navigation.
  createEffect(() => location.pathname);

  // Chrome badge: unread count, refreshed on navigation (the tray page
  // refreshes it live via the per-user SSE stream).
  createEffect(() => {
    location.pathname;
    refreshUnread();
  });

  return (
    <div class="flex min-h-screen flex-col">
      <div class="progress" classList={{ hidden: !barVisible() }} aria-hidden="true">
        <div class="progress-bar" style={{ width: barWidth() }} />
      </div>

      <header class="site-header sticky top-0 z-40">
        <div class="mx-auto flex max-w-6xl items-center gap-6 px-4 py-2.5">
          <A href="/" class="brand text-lg">
            walhub
          </A>
          <nav class="site-nav flex items-center gap-4">
            <A href="/" end>
              owners
            </A>
            <A href="/import">import</A>
            <A href="/api">API</A>
            <A href="/keys">keys</A>
            <A href="/setup">setup</A>
          </nav>
          <div class="ml-auto flex items-center gap-2">
            <NotificationTray />
            <button
              type="button"
              class="btn px-2 py-1"
              title="Toggle dark mode"
              onClick={() => toggleTheme()}
            >
              <Show when={theme() === "dark"} fallback={<span aria-hidden="true">☀</span>}>
                <span aria-hidden="true">☾</span>
              </Show>
            </button>
          </div>
        </div>
      </header>

      <main id="app" class="mx-auto w-full max-w-6xl flex-1 px-4 py-6">
        {props.children}
      </main>

      <footer class="border-t border-zinc-200 py-3 text-center text-xs text-zinc-400 dark:border-zinc-800 dark:text-zinc-600">
        walhub — the bucket is the repository
      </footer>

      <div id="tray" class="tray" aria-live="polite">
        <For each={errors()}>
          {(e) => (
            <div class="tray-entry">
              <div class="min-w-0 flex-1">
                <Show when={e.key}>
                  <span class="chip mr-1">{e.key}</span>
                </Show>
                <span class="break-words">{e.message}</span>
              </div>
              <button
                type="button"
                class="text-zinc-400 hover:text-zinc-700 dark:hover:text-zinc-200"
                aria-label="dismiss"
                onClick={() => dismissError(e)}
              >
                ✕
              </button>
            </div>
          )}
        </For>
      </div>
    </div>
  );
}
