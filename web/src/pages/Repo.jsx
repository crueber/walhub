// web/src/pages/Repo.jsx — the repository chrome (§2.6): header (title, head
// pill, clone menu, ref picker, tasks overlay) + tabs + nested route content.
// The repo context (owner/name/repoClient) flows to tab pages via context so
// every tab shares one client and one summary fetch.

import repos from "../../sdk/src/index.js";
import { createContext, useContext, createSignal, createEffect, onCleanup, For, Show, Switch, Match } from "solid-js";
import { useParams, A, useLocation, useNavigate } from "@solidjs/router";
import { useData, reportError, REPO_TTL } from "../lib/data.js";
import { activeTab } from "../lib/tabs.js";
import { mountStream } from "../lib/sse.js";

export const BUSY_MS = 1500; // poll cadence while something runs
export const IDLE_MS = 15000; // poll cadence when idle
export const LINGER_MS = 20000; // finished tasks stay listed this long

const RepoCtx = createContext();
export function useRepo() {
  return useContext(RepoCtx);
}

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

async function fetchRecipes() {
  const res = await fetch("/services/setup.json", { credentials: "same-origin" });
  if (!res.ok) throw new Error(`setup recipes: ${res.status}`);
  return res.json();
}

function CloneMenu(props) {
  // Dogfood exception (§2.6): the recipe payload is server-rendered JSON.
  const [getRecipes, setRecipes] = createSignal(null);
  const [getOpen, setOpen] = createSignal(false);
  const load = () => {
    if (getRecipes()) return;
    fetchRecipes()
      .then(setRecipes)
      .catch((e) => { setRecipes([]); reportError(e, "recipes"); });
  };
  const clone = () => props.summary?.clone_url ?? `${location.origin}/${props.full}.git`;
  return (
    <details class="clone-menu relative" onToggle={(e) => { setOpen(e.target.open); if (e.target.open) load(); }}>
      <summary class="pill cursor-pointer select-none">Clone</summary>
      <div class="clone-body card absolute right-0 z-30 mt-2 w-96 space-y-2 p-3">
        <Show when={getRecipes()} fallback={<p class="muted">loading recipes…</p>}>
          <For each={getRecipes() ?? []}>
            {(r) => (
              <div class="clone-recipe">
                <strong>{r.name ?? r.kind ?? "clone"}</strong>
                <Show when={r.doc}>
                  <p class="muted text-xs">{r.doc}</p>
                </Show>
                <code class="clone-cmd block overflow-x-auto rounded bg-zinc-100 px-2 py-1 font-mono text-xs dark:bg-zinc-800">
                  {String(r.command ?? r.cmd ?? "").replaceAll("{url}", clone()).replaceAll("URL", clone())}
                </code>
              </div>
            )}
          </For>
          <div class="clone-recipe">
            <strong>git</strong>
            <code class="clone-cmd block overflow-x-auto rounded bg-zinc-100 px-2 py-1 font-mono text-xs dark:bg-zinc-800">
              {`git clone ${clone()}`}
            </code>
          </div>
        </Show>
      </div>
    </details>
  );
}

// --- tabs -----------------------------------------------------------------------

const TABS = [
  { id: "code", label: "Code", href: (full) => `/${full}` },
  { id: "commits", label: "Commits", href: (full) => `/${full}/commits` },
  { id: "issues", label: "Issues", href: (full) => `/${full}/issues` },
  { id: "pulls", label: "Pulls", href: (full) => `/${full}/pulls` },
  { id: "checks", label: "Checks", href: (full) => `/${full}/checks` },
  { id: "releases", label: "Releases", href: (full) => `/${full}/releases` },
  { id: "wal", label: "WAL", href: (full) => `/${full}/wal` },
  { id: "settings", label: "Settings", href: (full) => `/${full}/settings` },
];

// The tab matcher lives in lib/tabs.js (pure, unit-tested): the active tab
// derives from the first path segment after /:owner/:name, so blob/tree
// paths with tab-word filenames (checks.go, …) still highlight Code.

// --- watch toggle (06 §7): optimistic flip, reconcile on error -------------

function WatchToggle(props) {
  const [getWatch, setWatch] = createSignal(null);
  const load = async () => {
    try {
      const res = await props.repo.watch.get();
      setWatch(res);
    } catch {
      setWatch(null); // anonymous or unreadable: hide the toggle
    }
  };
  load();
  const flip = async () => {
    const cur = getWatch();
    if (!cur) return;
    setWatch({ watching: !cur.watching, watchers: cur.watchers }); // optimistic
    try {
      const res = await props.repo.watch.set(!cur.watching);
      setWatch(res);
    } catch (e) {
      setWatch(cur); // reconcile on error
      reportError(e, "watch");
    }
  };
  return (
    <Show when={getWatch()}>
      {(w) => (
        <button
          type="button"
          class="btn px-2 py-1 text-sm"
          classList={{ primary: w().watching }}
          onClick={flip}
          title={w().watching ? "Unwatch this repo" : "Watch this repo"}
          aria-pressed={w().watching}
        >
          {w().watching ? "👁 watching" : "👁 watch"} · {w().watchers ?? 0}
        </button>
      )}
    </Show>
  );
}

// --- ref picker (SSE stream, one live stream per picker) -------------------------

function RefPicker(props) {
  const repo = props.repo;
  const navigate = useNavigate();
  const [getRefs, setRefs] = createSignal([]);
  const [getKind, setKind] = createSignal("branches");
  const [getQuery, setQuery] = createSignal("");
  const [getOpen, setOpen] = createSignal(false);
  let root;

  const stream = mountStream(
    (signal, emit) => repo.refStream(getKind(), { q: getQuery(), n: 50 }, emit, { signal }),
    (ref) => setRefs((list) => [...list.filter((r) => r.name !== ref.name), ref]), // keyed by name
  );

  let debounce = 0;
  const search = () => {
    clearTimeout(debounce);
    debounce = setTimeout(() => { setRefs([]); stream.run(); }, 150);
  };
  onCleanup(() => { clearTimeout(debounce); stream.cancel(); document.removeEventListener("click", close); });

  const close = (e) => { if (root && !root.contains(e.target)) { setOpen(false); stream.cancel(); } };
  document.addEventListener("click", close);

  const pick = (r) => {
    setOpen(false);
    stream.cancel();
    const kind = r.name.startsWith("refs/tags/") ? "tag" : "branch";
    navigate(`/${props.full}/tree/${kind === "tag" ? r.name : shortRef(r.name)}`);
  };

  return (
    <div class="ref-picker relative" ref={root}>
      <button
        type="button"
        class="pill cursor-pointer select-none"
        onClick={() => {
          const next = !getOpen();
          setOpen(next);
          if (next && getRefs().length === 0) stream.run();
          if (!next) stream.cancel();
        }}
      >
        refs ▾
      </button>
      <Show when={getOpen()}>
        <div class="ref-drop card absolute right-0 z-30 mt-2 w-80 p-2">
          <div class="ref-controls mb-2 flex gap-2">
            <select
              class="input"
              value={getKind()}
              onChange={(e) => { setKind(e.currentTarget.value); setRefs([]); stream.run(); }}
            >
              <option value="branches">branches</option>
              <option value="tags">tags</option>
            </select>
            <input
              class="input"
              type="search"
              placeholder="filter refs…"
              autocomplete="off"
              onInput={(e) => { setQuery(e.currentTarget.value.trim()); search(); }}
            />
          </div>
          <div class="ref-list max-h-72 space-y-0.5 overflow-y-auto" classList={{ "stream-error": stream.state() === "error" }}>
            <For each={getRefs()}>
              {(r) => (
                <button
                  type="button"
                  class="ref-row flex w-full items-center justify-between rounded px-2 py-1 text-left text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800"
                  onClick={() => pick(r)}
                >
                  <span class="ref-name font-mono">{shortRef(r.name)}</span>
                  <span class="ref-sha muted font-mono text-xs">{String(r.sha ?? "").slice(0, 10)}</span>
                </button>
              )}
            </For>
          </div>
        </div>
      </Show>
    </div>
  );
}

// --- star toggle (07 §8): optimistic flip, reconcile on error -------------

function StarToggle(props) {
  const [getSocial, setSocial] = createSignal(null);
  const load = async () => {
    try {
      const res = await props.repo.social.get();
      setSocial(res);
    } catch {
      setSocial(null); // anonymous or unreadable: hide the toggle
    }
  };
  load();
  const flip = async () => {
    const cur = getSocial();
    if (!cur) return;
    const starred = !cur.viewer?.starred;
    setSocial({ ...cur, stars: (cur.stars ?? 0) + (starred ? 1 : -1), viewer: { ...cur.viewer, starred } }); // optimistic
    try {
      const res = starred ? await props.repo.star.set() : await props.repo.star.remove();
      setSocial({ ...cur, stars: res.stars ?? cur.stars, viewer: { ...cur.viewer, starred } });
    } catch (e) {
      setSocial(cur); // reconcile on error
      reportError(e, "star");
    }
  };
  return (
    <Show when={getSocial()}>
      {(s) => (
        <button
          type="button"
          class="btn px-2 py-1 text-sm"
          classList={{ primary: s().viewer?.starred }}
          onClick={flip}
          title={s().viewer?.starred ? "Unstar this repo" : "Star this repo"}
          aria-pressed={s().viewer?.starred}
        >
          ★ {s().viewer?.starred ? "starred" : "star"} · {s().stars ?? 0}
        </button>
      )}
    </Show>
  );
}

// --- tasks overlay -----------------------------------------------------------------

function percentOf(task) {
  const p = task.progress;
  if (!p) return undefined;
  if (typeof p.percent === "number") return p.percent;
  if (p.total > 0) return (100 * (p.done ?? 0)) / p.total;
  return undefined;
}

function TasksOverlay(props) {
  const repo = props.repo;
  const [getRunning, setRunning] = createSignal([]);
  const [getDone, setDone] = createSignal([]);
  const [getOpen, setOpen] = createSignal(false);
  let alive = true;
  let timer = 0;
  let seenHost = "";
  let seen = new Map();
  let root;

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
        setDone((d) => [...d.filter((x) => !ids.has(x.id)), ...done].slice(-5));
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
  onCleanup(() => { alive = false; clearTimeout(timer); document.removeEventListener("click", onDoc); });

  const onDoc = (e) => { if (root && !root.contains(e.target)) setOpen(false); };
  document.addEventListener("click", onDoc);

  const kind = (s) => String(s ?? "").replaceAll(/[-_]/g, " ");
  return (
    <div class="tasks-indicator relative" ref={root}>
      <Show when={getRunning().length || getDone().length}>
        {(list) => {
          const head = getRunning().find((t) => t.progress) ?? getRunning()[0] ?? getDone()[getDone().length - 1];
          const pct = percentOf(head);
          const others = getRunning().length > 1 ? getRunning().length - 1 : 0;
          const failed = getDone().some((t) => t.ok === false);
          return (
            <>
              <button type="button" class="pill cursor-pointer" classList={{ "!border-amber-500": getRunning().length > 0, "!border-red-500": failed }} onClick={() => setOpen(!getOpen())}>
                <Show when={getRunning().length > 0} fallback={<span class={`inline-block h-1.5 w-1.5 rounded-full ${failed ? "bg-red-500" : "bg-emerald-500"}`} aria-hidden="true" />}>
                  <span class="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-amber-500" aria-hidden="true" />
                </Show>
                <span>{kind(head.kind)}</span>
                <Show when={others > 0}>
                  <span class="muted">+{others}</span>
                </Show>
                <Show when={pct !== undefined}>
                  <span class="muted tabular">{pct.toFixed(0)}%</span>
                </Show>
              </button>
              <Show when={getOpen()}>
                <div class="tasks-drop card absolute right-0 z-30 mt-2 w-96 space-y-1 p-3 text-sm">
                  <strong>{head.hostname ?? ""}</strong>
                  <For each={getRunning()}>
                    {(t) => (
                      <div class="task-row flex items-center gap-2">
                        <code class="font-mono text-xs">{t.id?.slice(0, 8) ?? ""}</code>
                        <span>{kind(t.kind)}</span>
                        <Show when={percentOf(t) !== undefined}>
                          <span class="muted tabular">{percentOf(t).toFixed(0)}%</span>
                        </Show>
                        <Show when={t.summary}>
                          <span class="muted">{t.summary}</span>
                        </Show>
                      </div>
                    )}
                  </For>
                  <For each={getDone()}>
                    {(t) => (
                      <div class="task-row flex items-center gap-2" classList={{ "text-red-600 dark:text-red-400": t.ok === false }}>
                        <code class="font-mono text-xs">{t.id?.slice(0, 8) ?? ""}</code>
                        <span>{kind(t.kind)}</span>
                        <span class="muted"> — {t.summary ?? (t.ok === false ? "failed" : "done")}</span>
                      </div>
                    )}
                  </For>
                </div>
              </Show>
            </>
          );
        }}
      </Show>
    </div>
  );
}

// --- shell ---------------------------------------------------------------------

export default function Repo(props) {
  const params = useParams();
  const location = useLocation();
  const full = () => `${params.owner}/${params.name}`;
  const repoClient = repos.repo(full());
  const [getSummary] = useData(() => `repo:${full()}`, () => repoClient.get(), REPO_TTL);

  const ctx = {
    get owner() { return params.owner; },
    get name() { return params.name; },
    get full() { return full(); },
    get rest() { return (params.rest ?? "").replace(/^\//, ""); },
    get sha() { return params.sha ?? ""; },
    repoClient,
    params,
  };

  return (
    <RepoCtx.Provider value={ctx}>
      <div class="repo-shell">
        <div class="repo-header mb-3 flex flex-wrap items-center gap-3">
          <Show when={getSummary()} fallback={<span class="muted">loading…</span>}>
            {(s) => (
              <div class="repo-title">
                <h1 class="text-xl font-semibold">
                  <A class="hover:underline" href={`/${params.owner}`}>{params.owner}</A>
                  {" / "}
                  <A class="hover:underline" href={`/${full()}`}>{params.name}</A>
                </h1>
                <div class="repo-meta mt-1 flex items-center gap-2 text-xs">
                  <Show when={s().head} fallback={<span class="pill">empty</span>}>
                    <span class="pill">{shortRef(s().head.name)} @ {String(s().head.sha).slice(0, 10)}</span>
                  </Show>
                  <span class="muted">{s().branches ?? 0} branches · {s().tags ?? 0} tags</span>
                </div>
              </div>
            )}
          </Show>
          <div class="ml-auto flex items-center gap-2">
            <StarToggle repo={repoClient} />
            <WatchToggle repo={repoClient} />
            <TasksOverlay repo={repoClient} />
            <RefPicker full={full()} repo={repoClient} />
            <Show when={getSummary()}>
              {(s) => <CloneMenu full={full()} summary={s()} />}
            </Show>
          </div>
        </div>

        <nav class="repo-tabs mb-4 flex gap-1 border-b border-zinc-200 dark:border-zinc-800" aria-label="repository sections">
          <For each={TABS}>
            {(t) => (
              <A
                href={t.href(full())}
                end={t.id === "code"}
                class="rounded-t px-3 py-1.5 text-sm text-zinc-500 hover:bg-zinc-100 hover:text-zinc-900 dark:text-zinc-400 dark:hover:bg-zinc-900 dark:hover:text-zinc-100"
                classList={{ "!border-b-2 !border-emerald-500 !font-medium !text-zinc-900 dark:!text-zinc-100": activeTab(location.pathname) === t.id }}
                aria-current={activeTab(location.pathname) === t.id ? "page" : undefined}
              >
                {t.label}
              </A>
            )}
          </For>
        </nav>

        <div class="repo-content">{props.children}</div>
      </div>
    </RepoCtx.Provider>
  );
}
