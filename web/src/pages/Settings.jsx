// web/src/pages/Settings.jsx — repo settings, three sub-tabs (§2.9):
// 1. Scheduled tasks — strategy table + placement/host facts + upstream follow status.
// 2. Push policy — textarea editor, 400 ms debounced validate, dry-run vs last N
//    pushes, save/discard/copy.
// 3. Effective config & history — TOML editor with debounced validate, publish with
//    a message, clear, per-revision history with "Revert to this" + line diff.

import { createSignal, createEffect, onCleanup, For, Show } from "solid-js";
import { useData, reportError } from "../lib/data.js";
import { useRepo, fmtDate } from "./Repo.jsx";

// --- tiny line diff (LCS) for the per-revision "line diff" ----------------------

export function lineDiff(aLines, bLines) {
  const n = aLines.length;
  const m = bLines.length;
  const dp = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i][j] = aLines[i] === bLines[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }
  const rows = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (aLines[i] === bLines[j]) { rows.push({ t: " ", text: aLines[i] }); i++; j++; }
    else if (dp[i + 1][j] >= dp[i][j + 1]) { rows.push({ t: "-", text: aLines[i++] }); }
    else { rows.push({ t: "+", text: bLines[j++] }); }
  }
  while (i < n) rows.push({ t: "-", text: aLines[i++] });
  while (j < m) rows.push({ t: "+", text: bLines[j++] });
  return rows;
}

function debounce(fn, ms) {
  let t = 0;
  const d = (...args) => { clearTimeout(t); t = setTimeout(() => fn(...args), ms); };
  d.cancel = () => clearTimeout(t);
  onCleanup(d.cancel);
  return d;
}

const TABS = ["Scheduled tasks", "Push policy", "Effective config & history"];

// --- tab 1: scheduled tasks ------------------------------------------------------

function ScheduledTab(props) {
  const [getDesc] = useData(`settings-describe:${props.ctx.full}`, () => props.repo.settings.describe(), 5000);
  const strategies = (d) => d?.sections?.strategies ?? d?.strategies ?? [];
  const host = (d) => d?.maintenance?.this_host ?? {};

  return (
    <Show when={getDesc()} fallback={<p class="muted">loading…</p>}>
      {(d) => (
        <>
          <section class="card p-4">
            <h3 class="mb-2 font-semibold">Bundle strategies</h3>
            <table class="grid">
              <thead>
                <tr><th>name</th><th>kind</th><th>base</th><th>schedule</th><th>next</th><th>keep</th><th>filter</th><th>refs</th></tr>
              </thead>
              <tbody>
                <For each={strategies(d())}>
                  {(s) => (
                    <tr>
                      <td>{s.name ?? ""}</td>
                      <td>{s.kind ?? ""}</td>
                      <td>{s.base ?? "—"}</td>
                      <td>{`${s.schedule ?? ""}${s.schedule_human ? ` (${s.schedule_human})` : ""}`}</td>
                      <td>{fmtDate(s.next)}</td>
                      <td>{String(s.keep ?? "—")}</td>
                      <td>{s.filter ?? "—"}</td>
                      <td>{(s.refs ?? []).join(", ") || "—"}</td>
                    </tr>
                  )}
                </For>
              </tbody>
            </table>
          </section>
          <section class="card mt-4 p-4">
            <h3 class="mb-2 font-semibold">Placement &amp; host facts</h3>
            <table class="grid kv">
              <tbody>
                <tr><th class="w-48 align-top">host</th><td>{host(d()).name ?? d().maintenance?.host ?? "—"}</td></tr>
                <tr><th class="w-48 align-top">serves</th><td>{(host(d()).serves ?? []).join(", ") || "—"}</td></tr>
                <tr><th class="w-48 align-top">maintains</th><td>{(host(d()).maintains ?? []).join(", ") || "—"}</td></tr>
                <tr><th class="w-48 align-top">disk</th><td>{host(d()).disk ?? "—"}</td></tr>
                <tr><th class="w-48 align-top">max pack bytes</th><td>{String(host(d()).max_pack_bytes ?? "—")}</td></tr>
                <tr><th class="w-48 align-top">cache budget</th><td>{String(host(d()).cache_budget_bytes ?? "—")}</td></tr>
                <tr><th class="w-48 align-top">roles</th><td>{(host(d()).roles ?? []).join(", ") || "—"}</td></tr>
              </tbody>
            </table>
          </section>
          <section class="card mt-4 p-4">
            <h3 class="mb-2 font-semibold">Upstream follow</h3>
            <p>
              {d().upstream?.git ? `${d().upstream.git}` : "no upstream configured"}{" "}
              {d().upstream?.follow
                ? <span class="chip">{`following every ${d().upstream.follow_interval_secs ?? "?"}s`}</span>
                : <span class="chip !bg-amber-100 !text-amber-800 dark:!bg-amber-900/60 dark:!text-amber-300">not following</span>}
              <Show when={d().upstream?.last_round}>
                <span class="muted">{` · last round ${fmtDate(d().upstream.last_round)}`}</span>
              </Show>
            </p>
          </section>
          <section class="card mt-4 p-4">
            <h3 class="mb-2 font-semibold">Effective values</h3>
            <table class="grid kv">
              <tbody>
                <For each={d().fields ?? []}>
                  {(f) => (
                    <tr>
                      <th class="w-64 align-top font-mono text-xs">{f.key}</th>
                      <td>
                        {String(f.value ?? "")}{" "}
                        {f.source === "setting"
                          ? <span class="chip">repo setting</span>
                          : <span class="muted">host</span>}
                      </td>
                    </tr>
                  )}
                </For>
              </tbody>
            </table>
          </section>
        </>
      )}
    </Show>
  );
}

// --- tab 2: push policy ------------------------------------------------------------

function PolicyTab(props) {
  const [getText, setText] = createSignal("");
  const [getSaved, setSaved] = createSignal("");
  const [getNote, setNote] = createSignal("");
  const [getResult, setResult] = createSignal(null); // validate result
  const [getDry, setDry] = createSignal(null);
  const [getDryN, setDryN] = createSignal("10");

  const validateDebounced = debounce(async () => {
    try {
      setResult(await props.repo.policy.validate(getText()));
    } catch (e) {
      setResult({ ok: false, errors: [String(e.message ?? e)] });
    }
  }, 400);

  const dirty = () => getText() !== getSaved();

  async function reload() {
    try {
      const p = await props.repo.policy.get();
      const text = typeof p === "string" ? p : JSON.stringify(p, null, 2);
      setText(text);
      setSaved(text);
    } catch (e) { reportError(e, "policy"); }
  }
  async function save() {
    try {
      await props.repo.policy.put(getText());
      setSaved(getText());
      setNote("policy saved");
    } catch (e) { reportError(e, "policy save"); }
  }
  async function copy() {
    try { await navigator.clipboard.writeText(getText()); } catch { /* clipboard unavailable */ }
  }
  async function dryRun() {
    try { setDry(await props.repo.policy.dryRun(Number(getDryN()) || 10)); }
    catch (e) { reportError(e, "policy dry-run"); }
  }

  void reload();

  return (
    <>
      <section class="card p-4">
        <h3 class="mb-2 font-semibold">Push policy</h3>
        <p class="muted mb-2 text-sm">validated client-side-free: the server is the gate (400 with reasons, fail closed on the next push)</p>
        <textarea
          class="input font-mono text-xs leading-5"
          rows="14"
          spellcheck="false"
          placeholder="policy JSON — empty means allow-all"
          value={getText()}
          onInput={(e) => { setText(e.currentTarget.value); validateDebounced(); }}
        />
        <div class="mt-2 flex flex-wrap items-center gap-2">
          <button class="pill !border-emerald-500 cursor-pointer select-none" type="button" onClick={save}>Save</button>
          <button class="pill cursor-pointer select-none" type="button" onClick={reload}>Discard</button>
          <button class="pill cursor-pointer select-none" type="button" onClick={copy}>Copy</button>
          <input
            class="input w-20 tabular"
            type="number"
            min="1"
            max="100"
            value={getDryN()}
            onInput={(e) => setDryN(e.currentTarget.value)}
          />
          <button class="pill cursor-pointer select-none" type="button" onClick={dryRun}>Dry-run last N pushes</button>
        </div>
        <div class="mt-2 space-y-1 text-sm">
          <p class="muted">{getNote()}</p>
          <Show when={getResult()}>
            {(r) => (
              <p class={r().ok === false ? "err-line !mt-0 !text-sm" : "!mt-0 text-sm text-emerald-700 dark:text-emerald-400"}>
                {r().ok === false ? `invalid: ${(r().errors ?? []).join("; ")}` : "valid"}
              </p>
            )}
          </Show>
          <Show when={dirty()}>
            <p class="warn-line !mt-0 !text-sm">unsaved changes</p>
          </Show>
        </div>
      </section>
      <Show when={getDry()}>
        {(d) => (
          <section class="card mt-4 p-4">
            <h3 class="mb-2 font-semibold">{`Dry run: ${d().allowed ?? 0} allowed / ${d().denied ?? 0} denied of ${d().pushes?.length ?? 0}`}</h3>
            <table class="grid">
              <thead>
                <tr><th>seq</th><th>principal</th><th>atomic</th><th>refs</th></tr>
              </thead>
              <tbody>
                <For each={d().pushes ?? []}>
                  {(p) => (
                    <tr>
                      <td class="tabular">{String(p.seq ?? "")}</td>
                      <td>{p.principal ?? ""}</td>
                      <td class="tabular">{String(p.atomic ?? "")}</td>
                      <td>{(p.refs ?? []).map((r) => `${r.name}${r.ok ? "" : ` ✗ ${r.reason ?? ""}`}`).join(", ") || "—"}</td>
                    </tr>
                  )}
                </For>
              </tbody>
            </table>
          </section>
        )}
      </Show>
    </>
  );
}

// --- tab 3: effective config & history ----------------------------------------------

function ConfigTab(props) {
  const [getText, setText] = createSignal("");
  const [getMessage, setMessage] = createSignal("");
  const [getNote, setNote] = createSignal("");
  const [getDiff, setDiff] = createSignal(null); // { rev, rows }

  const [getEffective] = useData(`settings-effective:${props.ctx.full}`, () => props.repo.settings.effective(), 5000);
  const [getHistory] = useData(`settings-history:${props.ctx.full}`, () => props.repo.settings.history(), 5000);

  // prefill once the effective config arrives and the editor is still untouched
  createEffect(() => {
    const eff = getEffective();
    if (eff !== undefined && getText() === "") {
      setText(typeof eff === "string" ? eff : String(eff?.toml ?? ""));
    }
  });

  const validateDebounced = debounce(async () => {
    try {
      const r = await props.repo.settings.validate(getText());
      setNote(r.ok === false ? `errors: ${(r.errors ?? []).join("; ")}` : "valid");
    } catch (e) { setNote(String(e.message ?? e)); }
  }, 400);

  async function publish() {
    try {
      await props.repo.settings.put(getText(), getMessage());
      setNote("published");
      setMessage("");
    } catch (e) { setNote(String(e.message ?? e)); }
  }
  async function clearAll() {
    try {
      await (props.repo.settings.delete?.() ?? props.repo.settings.put("", ""));
      setText("");
      setNote("");
    } catch (e) { setNote(String(e.message ?? e)); }
  }
  function revert(e) {
    setText(e.toml ?? "");
    setDiff(null);
    validateDebounced();
  }
  function showDiff(e) {
    const rows = lineDiff(String(e.toml ?? "").split("\n"), getText().split("\n"));
    setDiff({ rev: e.revision ?? e.seq, rows });
  }

  return (
    <>
      <section class="card p-4">
        <h3 class="mb-2 font-semibold">Effective config (TOML)</h3>
        <textarea
          class="input font-mono text-xs leading-5"
          rows="16"
          spellcheck="false"
          placeholder={"[bundles]\nmain_only = false"}
          value={getText()}
          onInput={(e) => { setText(e.currentTarget.value); validateDebounced(); }}
        />
        <div class="mt-2 flex flex-wrap items-center gap-2">
          <button class="pill !border-emerald-500 cursor-pointer select-none" type="button" onClick={publish}>Publish</button>
          <button class="pill cursor-pointer select-none" type="button" onClick={clearAll}>Clear</button>
          <input
            class="input w-72"
            type="text"
            placeholder="publish message (author = $USER)"
            value={getMessage()}
            onInput={(e) => setMessage(e.currentTarget.value)}
          />
        </div>
        <Show when={getNote()}>
          <p class="mt-2 text-sm text-emerald-700 dark:text-emerald-400">{getNote()}</p>
        </Show>
      </section>
      <section class="card mt-4 p-4">
        <h3 class="mb-2 font-semibold">History</h3>
        <Show
          when={(getHistory()?.entries ?? []).length}
          fallback={<p class="muted">{getHistory() ? "no revisions published" : "loading…"}</p>}
        >
          <div class="space-y-2">
            <For each={getHistory().entries ?? []}>
              {(e) => (
                <div class="rev-row border-b border-zinc-100 pb-2 dark:border-zinc-800/60">
                  <div class="flex flex-wrap items-center gap-2">
                    <strong>{`#${e.revision ?? e.seq}`}</strong>
                    <span class="muted text-sm">{` ${e.author ?? ""} · ${fmtDate(e.at)} · ${e.message ?? ""} `}</span>
                    <button class="pill cursor-pointer select-none" type="button" onClick={() => revert(e)}>Revert to this</button>
                    <button class="pill cursor-pointer select-none" type="button" onClick={() => showDiff(e)}>line diff</button>
                  </div>
                </div>
              )}
            </For>
          </div>
        </Show>
      </section>
      <Show when={getDiff()}>
        {(df) => (
          <section class="card mt-4 p-4">
            <h3 class="mb-2 font-semibold">{`Diff of #${df().rev} vs editor`}</h3>
            <pre class="code-view p-3">{df().rows.map((r) => `${r.t === "-" ? "-" : r.t === "+" ? "+" : " "} ${r.text}`).join("\n")}</pre>
          </section>
        )}
      </Show>
    </>
  );
}

// --- shell ---------------------------------------------------------------------------

export default function Settings() {
  const ctx = useRepo();
  const repo = ctx.repoClient;
  const [getTab, setTab] = createSignal("Scheduled tasks");

  return (
    <div class="settings-page">
      <h2 class="mb-3 text-lg font-semibold">Settings</h2>
      <nav class="subtabs mb-4 flex flex-wrap gap-1.5" aria-label="settings sections">
        <For each={TABS}>
          {(t) => (
            <button
              type="button"
              class="pill cursor-pointer select-none"
              classList={{
                "!border-emerald-500 !font-medium !text-emerald-700 dark:!text-emerald-300": getTab() === t,
              }}
              onClick={() => setTab(t)}
            >
              {t}
            </button>
          )}
        </For>
      </nav>
      <div>
        <Show when={getTab() === "Scheduled tasks"}><ScheduledTab ctx={ctx} repo={repo} /></Show>
        <Show when={getTab() === "Push policy"}><PolicyTab ctx={ctx} repo={repo} /></Show>
        <Show when={getTab() === "Effective config & history"}><ConfigTab ctx={ctx} repo={repo} /></Show>
      </div>
    </div>
  );
}
