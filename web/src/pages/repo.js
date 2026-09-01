// web/src/pages/repo.js — repo chrome (§2.6): header, clone menu (recipes from
// GET /services/setup.json — the sanctioned plain-fetch exception), tabs computed
// from the pathname, branch/tag picker streaming refs over SSE (50/page, 150 ms
// debounce, abort per keystroke), tasks overlay, and the per-tab page mount.

import repos from "repos";
import { createRoot, createEffect, createSignal, onCleanup, el } from "lib/reactive.js";
import { useData, REPO_TTL, reportError, trackPending } from "lib/data.js";
import { navigate } from "../main.js"; // static: main.js loads this module only via import()

import { mountStream } from "lib/sse.js";

export const BUSY_MS = 1500; // poll cadence while something runs
export const IDLE_MS = 15000; // poll cadence when idle
export const LINGER_MS = 20000; // finished tasks stay listed this long

export function fmtDate(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? String(iso) : d.toISOString().replace("T", " ").slice(0, 16) + "Z";
}

export function fmtBytes(n) {
  if (n === undefined || n === null || n < 0) return "—";
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let v = n;
  let u = -1;
  while (v >= 1024 && u < units.length - 1) { v /= 1024; u++; }
  return `${v >= 100 ? Math.round(v) : v.toFixed(1)} ${units[u]}`;
}

export function shortRef(name) {
  return String(name ?? "").replace(/^refs\/(?:heads|tags)\//, "");
}

// --- clone menu (recipes from the server, never a local copy) ------------------

export async function fetchRecipes() {
  const res = await fetch("/services/setup.json", { credentials: "same-origin" });
  if (!res.ok) throw new Error(`setup recipes: ${res.status}`);
  return res.json();
}

export function cloneMenu(full, summary) {
  const wrap = el("details", { class: "clone-menu" }, el("summary", { class: "pill" }, "Clone"));
  const body = el("div", { class: "clone-body" }, el("p", { class: "muted" }, "loading recipes…"));
  wrap.append(body);
  fetchRecipes()
    .then((recipes) => {
      body.replaceChildren();
      const clone = summary?.clone_url ?? `${location.origin}/${full}.git`;
      for (const r of recipes.recipes ?? []) {
        body.append(
          el("div", { class: "clone-recipe" },
            el("strong", {}, r.name ?? r.kind ?? "clone"),
            r.doc ? el("p", { class: "muted" }, r.doc) : null,
            el("code", { class: "clone-cmd" }, String(r.command ?? r.cmd ?? "").replaceAll("{url}", clone).replaceAll("URL", clone)),
          )
        );
      }
      body.append(el("div", { class: "clone-recipe" }, el("strong", {}, "git"), el("code", { class: "clone-cmd" }, `git clone ${clone}`)));
    })
    .catch((e) => { body.replaceChildren(el("p", { class: "muted" }, "recipes unavailable")); reportError(e, "recipes"); });
  return wrap;
}

// --- tabs -----------------------------------------------------------------------

export const TABS = [
  { id: "code", label: "Code", href: (full) => `/${full}` },
  { id: "commits", label: "Commits", href: (full) => `/${full}/commits` },
  { id: "wal", label: "WAL", href: (full) => `/${full}/wal` },
  { id: "settings", label: "Settings", href: (full) => `/${full}/settings` },
];

export function activeTab(pathname) {
  if (/\/wal$/.test(pathname)) return "wal";
  if (/\/settings/.test(pathname)) return "settings";
  if (/\/commits|\/commit\//.test(pathname)) return "commits";
  return "code";
}

// --- ref picker (SSE stream, one live stream per picker) -------------------------

export function refPicker(full, onPick) {
  const repo = repos.repo(full);
  const [getRefs, setRefs] = createSignal([]);
  const [getKind, setKind] = createSignal("branches");
  const [getQuery, setQuery] = createSignal("");
  const [getOpen, setOpen] = createSignal(false);

  const stream = mountStream(
    (signal, emit) => repo.refStream(getKind(), { q: getQuery(), n: 50 }, emit, { signal }),
    (ref) => setRefs((list) => [...list.filter((r) => r.name !== ref.name), ref]), // keyed by name
  );

  let debounce = 0;
  const search = () => {
    clearTimeout(debounce);
    debounce = setTimeout(() => { setRefs([]); stream.run(); }, 150);
  };
  onCleanup(() => { clearTimeout(debounce); stream.cancel(); }); // root dispose tears the picker down

  const listEl = el("div", { class: "ref-list" });
  createEffect(() => {
    const refs = getRefs();
    listEl.replaceChildren(...refs.map((r) =>
      el("button", { class: "ref-row", type: "button", onclick: () => { setOpen(false); onPick(r); } },
        el("span", { class: "ref-name" }, shortRef(r.name)),
        el("span", { class: "ref-sha" }, String(r.sha ?? "").slice(0, 10)))));
  });
  createEffect(() => { stream.state() === "error" && listEl.classList.add("stream-error"); });

  const input = el("input", {
    class: "ref-search", type: "search", placeholder: "filter refs…", autocomplete: "off",
    oninput: (e) => { setQuery(e.target.value.trim()); search(); },
  });
  const kindSel = el("select", {
    class: "ref-kind", onchange: (e) => { setKind(e.target.value); setRefs([]); stream.run(); },
  },
    el("option", { value: "branches" }, "branches"),
    el("option", { value: "tags" }, "tags"));

  const wrap = el("div", { class: "ref-picker" },
    el("button", { class: "pill", type: "button", onclick: () => {
      const next = !getOpen();
      setOpen(next);
      if (next && getRefs().length === 0) stream.run();
      if (!next) stream.cancel();
    } }, "refs ▾"),
    el("div", { class: "ref-drop", hidden: true },
      el("div", { class: "ref-controls" }, kindSel, input),
      listEl));

  createEffect(() => {
    wrap.querySelector(".ref-drop").hidden = !getOpen();
  });
  const close = (e) => { if (!wrap.contains(e.target)) { setOpen(false); stream.cancel(); } };
  document.addEventListener("click", close);

  return { el: wrap, dispose: () => { clearTimeout(debounce); stream.cancel(); document.removeEventListener("click", close); } };
}

// --- tasks overlay -----------------------------------------------------------------

function percentOf(task) {
  const p = task.progress;
  if (!p) return undefined;
  if (typeof p.percent === "number") return p.percent;
  if (p.total > 0) return (100 * (p.done ?? 0)) / p.total;
  return undefined;
}

export function tasksOverlay(full) {
  const repo = repos.repo(full);
  const [getRunning, setRunning] = createSignal([]);
  const [getDone, setDone] = createSignal([]);
  const [getOpen, setOpen] = createSignal(false);
  let alive = true;
  let timer = 0;
  let seenHost = "";
  let seen = new Map();
  const wrap = el("div", { class: "tasks-indicator" });
  const disposeRoot = createRoot((dispose) => {
    onCleanup(() => { alive = false; clearTimeout(timer); document.removeEventListener("click", onDoc); });
    const tick = async () => {
      try {
        const t = await repo.tasks();
        if (!alive) return;
        const now = new Map((t.running ?? []).map((r) => [r.id, r]));
        const done = [];
        for (const [id, prev] of seen) {
          if (now.has(id)) continue;
          const rec = (t.recent ?? []).find((r) => r.id === id);
          if (rec) done.push(rec);
          else if (t.hostname === seenHost) done.push({ ...prev, ok: prev.ok ?? true, finished: prev.finished ?? new Date().toISOString(), summary: prev.summary || "done" });
          // a different instance answered: keep waiting for the owner instance
        }
        for (const rec of done) if (rec.ok === false) reportError(new Error(rec.summary ?? "task failed"), `${String(rec.kind).replaceAll(/[-_]/g, " ")} task`);
        seenHost = t.hostname;
        seen = now;
        setRunning(t.running ?? []);
        if (done.length) {
          const ids = new Set(done.map((d) => d.id));
          const finished = done;
          setDone((d) => [...d.filter((x) => !ids.has(x.id)), ...finished].slice(-5));
          setTimeout(() => alive && setDone((d) => d.filter((x) => !ids.has(x.id))), LINGER_MS);
        }
        timer = setTimeout(tick, (t.running ?? []).length ? BUSY_MS : IDLE_MS);
      } catch (e) {
        if (!alive) return;
        if (e?.status !== 404) reportError(e, "tasks");
        timer = setTimeout(tick, IDLE_MS);
      }
    };
    void tick();

    const onDoc = (e) => { if (!wrap.contains(e.target)) setOpen(false); };
    document.addEventListener("click", onDoc);

    createEffect(() => {
      const running = getRunning();
      const done = getDone();
      const open = getOpen() && (running.length || done.length);
      const head = running.find((t) => t.progress) ?? running[0] ?? done[done.length - 1];
      if (!head) { wrap.replaceChildren(); return; }
      const pct = percentOf(head);
      const others = running.length > 1 ? running.length - 1 : 0;
      const failed = done.some((t) => t.ok === false);
      const pill = el("button", { class: `tasks-pill ${running.length ? "busy" : "idle"} ${failed ? "failed" : ""}`, type: "button", onclick: () => setOpen(!getOpen()) },
        running.length ? el("span", { class: "spinner", "aria-hidden": true }) : el("span", { class: `dot ${failed ? "failed" : "ok"}`, "aria-hidden": true }),
        el("span", { class: "task-kind" }, String(head.kind).replaceAll(/[-_]/g, " ")),
        others > 0 ? el("span", { class: "muted" }, `+${others}`) : null,
        pct !== undefined ? el("span", { class: "muted tabular" }, `${pct.toFixed(0)}%`) : null);
      const drop = el("div", { class: "tasks-drop", hidden: !open },
        el("strong", {}, head.hostname ?? ""),
        ...running.map((t) => el("div", { class: "task-row" }, el("code", {}, t.id?.slice(0, 8) ?? ""), " ", String(t.kind).replaceAll(/[-_]/g, " "),
          percentOf(t) !== undefined ? el("span", { class: "muted tabular" }, `${percentOf(t).toFixed(0)}%`) : null,
          t.summary ? el("span", { class: "muted" }, t.summary) : null)),
        ...done.map((t) => el("div", { class: `task-row done ${t.ok === false ? "failed" : ""}` }, el("code", {}, t.id?.slice(0, 8) ?? ""), " ", String(t.kind).replaceAll(/[-_]/g, " "), " — ", t.summary ?? (t.ok === false ? "failed" : "done"))));
      wrap.replaceChildren(pill, drop);
    });
  });
  return { el: wrap, dispose: disposeRoot };
}

// --- shell ---------------------------------------------------------------------

const TAB_PAGES = {
  code: () => import("./tree.js"),
  commits: () => import("./commits.js"),
  wal: () => import("./wal.js"),
  settings: () => import("./settings.js"),
};

/**
 * Repo shell mount. tab is the route hint (code|commits|wal|settings); the exact
 * sub-page (tree vs blob, commits list vs commit detail) is derived from the URL.
 */
export function mount(container, params, tab) {
  const full = `${params.owner}/${params.name}`;
  return createRoot((dispose) => {
    const repoClient = repos.repo(full);
    const [getSummary] = useData(`repo:${full}`, () => repoClient.get(), REPO_TTL);

    const header = el("div", { class: "repo-header" });
    const tabsBar = el("nav", { class: "repo-tabs", "aria-label": "repository sections" });
    const content = el("div", { class: "repo-content" });
    container.append(el("div", { class: "repo-shell" }, header, tabsBar, content));

    onCleanup(() => tasks.dispose()); // overlay runs its own nested root
    const picker = refPicker(full, (r) => {
      const kind = r.name.startsWith("refs/tags/") ? "tag" : "branch";
      navigate(`/${full}/tree/${kind === "tag" ? r.name : shortRef(r.name)}`);
    });
    const tasks = tasksOverlay(full);
    header.append(tasks.el, picker.el);

    createEffect(() => {
      const s = getSummary();
      if (!s) return;
      header.prepend(
        el("div", { class: "repo-title" },
          el("h1", {}, el("a", { href: `/${params.owner}` }, params.owner), " / ", el("a", { href: `/${full}` }, params.name)),
          el("div", { class: "repo-meta" },
            s.head ? el("span", { class: "pill" }, `${shortRef(s.head.name)} @ ${String(s.head.sha).slice(0, 10)}`) : el("span", { class: "pill" }, "empty"),
            el("span", { class: "muted" }, `${s.branches ?? 0} branches · ${s.tags ?? 0} tags`)),
          cloneMenu(full, s)));
    });

    const active = activeTab(location.pathname);
    tabsBar.append(...TABS.map((t) =>
      el("a", { class: `repo-tab ${t.id === active ? "active" : ""}`, href: t.href(full), "aria-current": t.id === active ? "page" : undefined }, t.label)));

    // dispatch the tab page
    let page;
    if (tab === "code" && /\/blob\//.test(location.pathname)) page = import("./blob.js");
    else if (tab === "commits" && /\/commit\//.test(location.pathname)) page = import("./commit.js");
    else page = TAB_PAGES[tab]?.() ?? Promise.resolve({ mount: () => {} });

    const ctx = { owner: params.owner, name: params.name, full, params, rest: params.rest ?? "", sha: params.sha ?? "", repoClient };
    trackPending(Promise.resolve(page).then((m) => m.mount(content, ctx)));
    return dispose;
  });
}
