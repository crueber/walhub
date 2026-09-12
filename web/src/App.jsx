// web/src/App.jsx — the router root (D-WEB-6): site chrome (header, nav, theme
// toggle), the top progress bar fed by the data layer's pending counter, and
// the global error tray. Route content renders through props.children.

import { createEffect, createSignal, Show, For } from "solid-js";
import { A, useLocation } from "@solidjs/router";
import repos from "../sdk/src/index.js";
import { usePending, useData, trayErrors, dismissError } from "./lib/data.js";
import { theme, toggleTheme } from "./lib/store.js";
import { navModel } from "./lib/identity.js";
import { refreshUnread, unreadCount } from "./pages/Notifications.jsx";
import NotificationTray from "./components/NotificationTray.jsx";
import IdentityMenu from "./components/IdentityMenu.jsx";

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

  // Forgejo #371: navbar identity surface. me() rides the shared "me"
  // cache key pages already fetch (zero new requests); discovery (AuthOpen)
  // carries the auth mode + browser-login advertisement. Both swallow
  // auth failures to null (signed out / offline → legacy nav, never a
  // spinner or tray entry).
  const [getMe] = useData("me", () => repos.me().catch(() => null));
  const [getDiscovery] = useData("discovery", () => repos.discovery().catch(() => null));
  const nav = () => navModel({ me: getMe(), discovery: getDiscovery() }, location.pathname + location.search);

  return (
    <div class="flex min-h-screen flex-col">
      <div class="progress" classList={{ hidden: !barVisible() }} aria-hidden="true">
        <div class="progress-bar" style={{ width: barWidth() }} />
      </div>

      <header class="site-header sticky top-0 z-40">
        {/* Narrow widths (issue #273): the row never grows past the viewport.
            The brand + right cluster are pinned (shrink-0); the site-nav takes
            the leftover (min-w-0 flex-1) and scrolls internally
            (overflow-x-auto) instead of pushing the page sideways. Gaps relax
            at sm: so desktop spacing is unchanged. No page-level horizontal
            scroll from the header at 390px. */}
        <div class="mx-auto flex max-w-6xl items-center gap-3 px-4 py-2.5 sm:gap-6">
          <A href="/" class="brand shrink-0 text-lg">
            walhub
          </A>
          <nav aria-label="Site" class="site-nav flex min-w-0 flex-1 items-center gap-3 overflow-x-auto whitespace-nowrap sm:gap-4">
            <A href="/explore">explore</A>
            <A href="/import">import</A>
            {/* Forgejo #371: signed-in users (outside none mode) find keys
                in the identity menu — the primary nav keeps it for
                signed-out visitors. */}
            <Show when={nav().showKeysInNav}>
              <A href="/keys">keys</A>
            </Show>
            {/* Forgejo #362: the invitee inbox. Gated on the shared unread
                signal — non-null means the authenticated unread_count probe
                succeeded, so this adds no request of its own (law 6);
                anonymous visitors never see the link and the page itself
                explains sign-in. Forgejo #371: signed-in users find it in
                the identity menu instead. */}
            <Show when={nav().showInvitationsInNav && unreadCount() !== null}>
              <A href="/invitations">invitations</A>
            </Show>
            {/* Forgejo #371: setup stays in the primary nav for signed-out
                visitors and in none mode (first-run discoverability);
                signed-in users find it in the identity menu (admin-only). */}
            <Show when={nav().showSetupInNav}>
              <A href="/setup">setup</A>
            </Show>
          </nav>
          <div class="ml-auto flex shrink-0 items-center gap-2">
            <A href="/api" class="nav-link">API</A>
            {/* Forgejo #371: signed-out + browser login available → Login
                through the OIDC pathway, returning to the current page. */}
            <Show when={nav().showLogin}>
              <a href={nav().loginHref} class="btn primary px-2 py-1">Login</a>
            </Show>
            {/* Forgejo #371: signed-in → avatar-or-username identity menu.
                Forgejo #376: the avatar URL rides me().avatar_url (stable,
                cache-busted) — "" renders the initials fallback.
                Forgejo #390: the identity control is the far-right
                element (after the tray and theme toggle); the bare
                circle freed the btn-box width, so the cluster still
                fits at 390px (#273). */}
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
            <Show when={nav().showIdentity}>
              <IdentityMenu username={nav().username} avatarUrl={nav().avatarUrl} items={nav().menuItems} />
            </Show>
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
