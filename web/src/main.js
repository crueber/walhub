// web/src/main.js — the hand-rolled router (§2 preamble, ~60 lines) + global chrome
// wiring: top progress bar, error tray. Route inventory per §2.3. Pages are plain
// mount/unmount functions; lazy pages are native dynamic import() cached in a map.

import repos from "repos";
import { createEffect, el } from "lib/reactive.js";
import { initData, usePending, trayErrors, dismissError, trackPending, reportError } from "lib/data.js";

initData(repos); // the dogfood client, one instance

// --- router -------------------------------------------------------------------

function route(pattern) {
  const keys = [];
  let src = "";
  for (let i = 0; i < pattern.length; i++) {
    const ch = pattern[i];
    if (ch === ":") {
      let j = i + 1;
      while (j < pattern.length && /[a-zA-Z]/.test(pattern[j])) j++;
      keys.push(pattern.slice(i + 1, j));
      src += "([^/]+)";
      i = j - 1;
    } else if (ch === "*") {
      keys.push("rest");
      src += "(.*)";
    } else {
      src += /[a-zA-Z0-9/_-]/.test(ch) ? ch : `\\${ch}`;
    }
  }
  const rx = new RegExp(`^${src}$`);
  return {
    match(pathname) {
      const hit = rx.exec(pathname);
      if (!hit) return null;
      const params = {};
      keys.forEach((k, idx) => (params[k] = decodeURIComponent(hit[idx + 1])));
      return params;
    },
  };
}

// repo routes share the chrome; REPO(tab) lazy-loads the shell with a tab hint
const REPO = (tab) => async (container, params) => {
  const mod = await import("./pages/repo.js");
  return mod.mount(container, params, tab);
};

const routes = [
  ["/", async (c, p) => (await import("./pages/owners.js")).mount(c, p)],
  ["/setup", async (c, p) => (await import("./pages/setup.js")).mount(c, p)],
  ["/api", async (c, p) => (await import("./pages/apidocs.js")).mount(c, p)],
  ["/:owner", async (c, p) => (await import("./pages/repos.js")).mount(c, p)],
  ["/:owner/:repo", REPO("code")],
  ["/:owner/:repo/tree/*", REPO("code")],
  ["/:owner/:repo/blob/*", REPO("code")],
  ["/:owner/:repo/commits", REPO("commits")],
  ["/:owner/:repo/commit/:sha", REPO("commits")],
  ["/:owner/:repo/wal", REPO("wal")],
  ["/:owner/:repo/settings", REPO("settings")],
].map(([pattern, load]) => ({ matcher: route(pattern), load }));

let unmount = null;
let currentPath = location.pathname + location.search;

function resolvePage(pathname) {
  for (const r of routes) {
    const params = r.matcher.match(pathname);
    if (params) return { load: r.load, params };
  }
  return null;
}

export function navigate(path, { replace = false } = {}) {
  history[replace ? "replaceState" : "pushState"]({}, "", path);
  render();
}

function render() {
  currentPath = location.pathname + location.search;
  unmount?.();
  unmount = null;
  const app = document.getElementById("app");
  app.replaceChildren();
  const found = resolvePage(location.pathname);
  if (!found) {
    app.append(
      el("section", { class: "notfound" },
        el("h1", {}, "404"),
        el("p", {}, "No page at ", el("code", {}, location.pathname), "."),
        el("p", {}, el("a", { href: "/" }, "Back to owners")))
    );
    return;
  }
  const at = currentPath; // a navigation during the load must not own the slot
  trackPending(Promise.resolve(found.load(app, found.params))
    .then((u) => { if (currentPath === at) unmount = typeof u === "function" ? u : null; else if (typeof u === "function") u(); })
    .catch((e) => reportError(e, "page")));
}

addEventListener("popstate", render);

// same-origin link interception keeps navigation in the SPA
document.addEventListener("click", (e) => {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  const a = e.target.closest("a[href]");
  if (!a || a.target || a.hasAttribute("download")) return;
  const url = new URL(a.href, location.href);
  if (url.origin !== location.origin) return;
  e.preventDefault();
  navigate(url.pathname + url.search);
});

// --- global chrome: top progress bar + error tray -------------------------------

const bar = document.querySelector("#progress .progress-bar");
const progressWrap = document.getElementById("progress");
createEffect(() => {
  const pending = usePending()();
  progressWrap.classList.toggle("active", pending > 0);
  bar.style.width = `${Math.min(95, 6 + 89 * (1 - 1 / (pending + 1)))}%`;
});

const tray = document.getElementById("tray");
createEffect(() => {
  const entries = trayErrors();
  tray.replaceChildren(...entries.map((entry) =>
    el("div", { class: "tray-item", role: "status" },
      el("span", { class: "tray-key" }, entry.key || "error"),
      el("span", { class: "tray-msg" }, entry.message),
      el("button", { class: "tray-x", onclick: () => dismissError(entry), "aria-label": "dismiss" }, "×"))
  ));
});

// --- go -------------------------------------------------------------------------

render();
