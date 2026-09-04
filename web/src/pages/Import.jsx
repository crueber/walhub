// web/src/pages/Import.jsx — route "/import" (docs/features/10 §5):
// import form → running (progress bars + log tail from the task SSE
// stream) → done/error. Solid signals/stores + context only (D-WEB-6);
// every call through the SDK (dogfood rule); dark + light via dark:
// variants on every surface; permission gating disables the form AND
// honors server 401/403 (never client-only enforcement).

import { createSignal, For, Show, onCleanup } from "solid-js";
import { A, useSearchParams } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { normalizeSource } from "../../sdk/src/import.js";
import { reportError } from "../lib/data.js";

export default function Import() {
  const [search] = useSearchParams();
  const [getUrl, setUrl] = createSignal("");
  const [getOwner, setOwner] = createSignal(search.owner ?? "");
  const [getName, setName] = createSignal("");
  const [getToken, setToken] = createSignal("");
  const [getBranchOnly, setBranchOnly] = createSignal(false);
  const [getPullHeads, setPullHeads] = createSignal(false);
  const [getNotes, setNotes] = createSignal(false);
  const [getFormat, setFormat] = createSignal("");
  const [getPhase, setPhase] = createSignal("form"); // form | running | done | error
  const [getBars, setBars] = createSignal({}); // label → {done, total, unit, percent}
  const [getLog, setLog] = createSignal([]);
  const [getOutcome, setOutcome] = createSignal(null);
  const [getErr, setErr] = createSignal("");
  const [getTaskId, setTaskId] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);
  const [getMe, setMe] = createSignal(null);
  let ctrl = null;
  onCleanup(() => {
    if (ctrl) ctrl.abort();
  });

  repos
    .me()
    .then(setMe)
    .catch(() => setMe({ anonymous: true }));
  const anonymous = () => getMe()?.anonymous !== false && !getMe()?.principal;

  const suggestion = () => normalizeSource(getUrl());

  const pushLog = (text) => {
    setLog((log) => [...log.slice(-59), text]);
  };

  const onEvent = (p) => {
    if (!p) return;
    if (p.event === "notice") pushLog(p.text ?? "");
    else if (p.event === "progress") {
      setBars((bars) => ({ ...bars, [p.label ?? "clone"]: p }));
    } else if (p.event === "task" && p.log_tail) {
      setLog(p.log_tail.slice(-60));
    }
  };

  const start = async (e) => {
    e.preventDefault();
    if (getBusy()) return;
    setBusy(true);
    setErr("");
    setBars({});
    setLog([]);
    setOutcome(null);
    setPhase("running");
    ctrl?.abort();
    ctrl = new AbortController();
    const signal = ctrl.signal;
    try {
      const payload = {
        source_url: getUrl().trim(),
        owner: getOwner().trim(),
        name: getName().trim(),
        refs: [],
        default_branch_only: getBranchOnly(),
        include_pull_heads: getPullHeads(),
        include_notes: getNotes(),
        format: getFormat() || undefined,
      };
      if (getToken()) payload.token = getToken();
      const started = await repos.imports.start(payload, { signal });
      if (started?.repo && started?.import) {
        // Idempotent no-op (200): the source already landed.
        setOutcome({ ...started.import, repo: started.repo, noop: true });
        setPhase("done");
        return;
      }
      const id = started?.task?.id;
      if (!id) throw new Error("import did not return a task");
      setTaskId(id);
      const result = await repos.imports.attach(id, onEvent, { signal });
      setOutcome(result);
      setPhase("done");
    } catch (err) {
      if (err?.status === 499 || signal.aborted) return; // navigated away
      const msg = String(err?.message ?? err ?? "import failed");
      setErr(msg);
      pushLog(`error: ${msg}`);
      setPhase("error");
      reportError(err, "import");
    } finally {
      setBusy(false);
    }
  };

  const retry = () => {
    setPhase("form");
    setErr("");
  };

  const heads = () => Object.entries(getOutcome()?.head_shas ?? {});

  return (
    <div class="import-page grid max-w-2xl gap-4">
      <h2 class="text-xl font-semibold">Import repository</h2>
      <Show when={getPhase() === "form" || getPhase() === "error"}>
        <form class="card grid gap-3 p-4" onSubmit={start} aria-label="Import repository">
          <label class="grid gap-1">
            <span class="text-sm font-medium">Source URL (owner/repo, GitHub URL, or any git URL)</span>
            <input
              class="input font-mono"
              value={getUrl()}
              onInput={(e) => {
                setUrl(e.currentTarget.value);
                const s = normalizeSource(e.currentTarget.value);
                if (s.owner && !getOwner()) setOwner(s.owner);
                if (s.name && !getName()) setName(s.name);
              }}
              placeholder="acme/monorepo or https://github.com/acme/monorepo.git"
              autocomplete="off"
              spellcheck={false}
            />
            <Show when={suggestion().url && getUrl()}>
              <span class="muted text-xs">
                canonical: <code>{suggestion().url}</code>
              </span>
            </Show>
            <Show when={suggestion().error && getUrl()}>
              <span class="text-xs text-amber-700 dark:text-amber-400">{suggestion().error}</span>
            </Show>
          </label>
          <div class="grid grid-cols-2 gap-3">
            <label class="grid gap-1">
              <span class="text-sm font-medium">Owner</span>
              <input
                class="input font-mono"
                value={getOwner()}
                onInput={(e) => setOwner(e.currentTarget.value.trim())}
                placeholder="acme"
                autocomplete="off"
                spellcheck={false}
              />
            </label>
            <label class="grid gap-1">
              <span class="text-sm font-medium">Name</span>
              <input
                class="input font-mono"
                value={getName()}
                onInput={(e) => setName(e.currentTarget.value.trim())}
                placeholder="monorepo"
                autocomplete="off"
                spellcheck={false}
              />
            </label>
          </div>
          <label class="grid gap-1">
            <span class="text-sm font-medium">
              Token <span class="muted">(private sources only — never stored, never logged)</span>
            </span>
            <input
              class="input font-mono"
              type="password"
              value={getToken()}
              onInput={(e) => setToken(e.currentTarget.value)}
              placeholder="contents:read token for a private source"
              autocomplete="off"
            />
          </label>
          <div class="flex flex-wrap gap-4">
            <label class="flex items-center gap-1 text-sm">
              <input type="checkbox" checked={getBranchOnly()} onChange={(e) => setBranchOnly(e.currentTarget.checked)} />
              default branch only
            </label>
            <label class="flex items-center gap-1 text-sm" title="refs/pull/N/head only — never /merge">
              <input type="checkbox" checked={getPullHeads()} onChange={(e) => setPullHeads(e.currentTarget.checked)} />
              include PR heads
            </label>
            <label class="flex items-center gap-1 text-sm">
              <input type="checkbox" checked={getNotes()} onChange={(e) => setNotes(e.currentTarget.checked)} />
              include notes
            </label>
            <label class="flex items-center gap-1 text-sm">
              <span>format</span>
              <select class="input w-auto" value={getFormat()} onChange={(e) => setFormat(e.currentTarget.value)}>
                <option value="">follow source</option>
                <option value="sha1">sha1</option>
                <option value="sha256">sha256</option>
              </select>
            </label>
          </div>
          <p class="muted text-xs">
            LFS-tracked files import as pointer blobs (never smudged). Server-side ssh is not
            supported in v1 — use https with a token for private sources.
          </p>
          <Show when={anonymous()}>
            <p class="text-xs text-amber-700 dark:text-amber-400">
              You are not signed in — the server will refuse the import (401). Sign in first.
            </p>
          </Show>
          <Show when={getPhase() === "error"}>
            <div class="rounded-lg border border-red-300 bg-red-50 p-3 text-sm text-red-800 dark:border-red-900 dark:bg-red-950/60 dark:text-red-200">
              {getErr()}
            </div>
          </Show>
          <div class="flex gap-2">
            <Show when={getPhase() === "error"}>
              <button type="button" class="btn px-3 py-1" onClick={retry}>
                back to form
              </button>
            </Show>
            <button type="submit" class="btn primary px-3 py-1" disabled={getBusy() || anonymous() || !getUrl() || !getOwner() || !getName()}>
              {getBusy() ? "starting…" : "start import"}
            </button>
          </div>
        </form>
      </Show>
      <Show when={getPhase() === "running"}>
        <div class="card grid gap-3 p-4" aria-label="Import progress" aria-live="polite">
          <h3 class="font-semibold">
            importing {getOwner()}/{getName()}
            <Show when={getTaskId()}>
              <span class="muted font-mono text-xs"> ({getTaskId()})</span>
            </Show>
          </h3>
          <For each={Object.entries(getBars())}>
            {([label, bar]) => (
              <div>
                <div class="mb-1 flex justify-between text-xs">
                  <span class="font-medium">{label}</span>
                  <span class="muted font-mono">
                    {bar.done}
                    {bar.total ? `/${bar.total}` : ""} {bar.unit ?? ""}
                    {bar.percent != null ? ` (${bar.percent.toFixed(0)}%)` : ""}
                  </span>
                </div>
                <div class="h-2 overflow-hidden rounded bg-zinc-200 dark:bg-zinc-800">
                  <div
                    class="h-full bg-emerald-500 transition-all"
                    style={{ width: `${bar.percent != null ? Math.min(100, bar.percent) : 100}%` }}
                  />
                </div>
              </div>
            )}
          </For>
          <pre class="max-h-64 overflow-y-auto rounded bg-zinc-100 p-2 font-mono text-xs text-zinc-800 dark:bg-zinc-900 dark:text-zinc-200">
            <For each={getLog()}>{(line) => <>{line}{"\n"}</>}</For>
          </pre>
          <p class="muted text-xs">The import keeps running if you navigate away — re-open this page via the API task id.</p>
        </div>
      </Show>
      <Show when={getPhase() === "done" && getOutcome()}>
        <div class="card grid gap-3 p-4" aria-label="Import done">
          <div class="rounded-lg border border-emerald-300 bg-emerald-50 p-3 text-sm text-emerald-800 dark:border-emerald-900 dark:bg-emerald-950/60 dark:text-emerald-200">
            <Show when={getOutcome().noop} fallback={<>imported {getOutcome().repo}</>}>
              already imported from this source — no-op
            </Show>{" "}
            from <code class="font-mono">{getOutcome().source_url}</code>
          </div>
          <A class="text-emerald-700 hover:underline dark:text-emerald-400" href={`/${getOutcome().repo}`}>
            open {getOutcome().repo}
          </A>
          <details>
            <summary class="cursor-pointer text-sm">head SHAs ({heads().length})</summary>
            <ul class="mt-1 space-y-0.5 font-mono text-xs">
              <For each={heads()}>
                {([ref, sha]) => (
                  <li>
                    <span class="text-zinc-600 dark:text-zinc-400">{ref}</span> {sha}
                  </li>
                )}
              </For>
            </ul>
          </details>
        </div>
      </Show>
    </div>
  );
}
