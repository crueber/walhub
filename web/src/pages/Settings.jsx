// web/src/pages/Settings.jsx — repo settings, five sub-tabs (§2.9 + 05 §9):
// 1. Scheduled tasks — strategy table + placement/host facts + upstream follow status.
// 2. Push policy — textarea editor, 400 ms debounced validate, dry-run vs last N
//    pushes, save/discard/copy.
// 3. Effective config & history — TOML editor with debounced validate, publish with
//    a message, clear, per-revision history with "Revert to this" + line diff.
// 4. Access — visibility + role bindings (Access.jsx).
// 5. CI tokens — wct_ token mint/list/revoke (05 §3, admin-only).

import { createSignal, createEffect, onCleanup, For, Show } from "solid-js";
import { useData, reportError, asList } from "../lib/data.js";
import { useRepo, fmtDate, fmtBytes } from "./Repo.jsx";
import AccessTab from "./Access.jsx";

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

const TABS = ["Scheduled tasks", "Push policy", "Effective config & history", "Access", "CI tokens", "Webhooks"];

// --- tab 1: scheduled tasks ------------------------------------------------------

function ScheduledTab(props) {
  const full = props.ctx.full;   // captured once — no props reads inside the effect
  const repo = props.repo;
  const [getDesc] = useData(`settings-describe:${full}`, () => repo.settings.describe(), 5000);
  const strategies = (d) => d?.sections?.strategies ?? d?.strategies ?? [];
  const host = (d) => d?.maintenance?.this_host ?? {};

  return (
    <Show when={getDesc()} fallback={<p class="muted">loading…</p>}>
      {(d) => (
        <>
          <section class="card p-4">
            <h3 class="mb-2 font-semibold">Bundle strategies</h3>
            <div class="overflow-x-auto">
            <table class="data-table">
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
                      <td>{asList(s.refs).join(", ") || "—"}</td>
                    </tr>
                  )}
                </For>
              </tbody>
            </table>
            </div>
          </section>
          <section class="card mt-4 p-4">
            <h3 class="mb-2 font-semibold">Placement &amp; host facts</h3>
            <table class="data-table kv">
              <tbody>
                <tr><th class="w-48 align-top">host</th><td>{host(d()).name ?? d().maintenance?.host ?? "—"}</td></tr>
                <tr><th class="w-48 align-top">serves</th><td>{host(d()).serves === true ? "yes" : host(d()).serves === false ? "no" : "—"}</td></tr>
                <tr><th class="w-48 align-top">maintains</th><td>{host(d()).maintains === true ? "yes" : host(d()).maintains === false ? "no" : "—"}</td></tr>
                <tr><th class="w-48 align-top">disk</th><td>{host(d()).disk ?? "—"}</td></tr>
                <tr><th class="w-48 align-top">max pack bytes</th><td>{host(d()).max_pack_bytes ? fmtBytes(host(d()).max_pack_bytes) : "—"}</td></tr>
                <tr><th class="w-48 align-top">cache budget</th><td>{host(d()).cache_budget_bytes ? fmtBytes(host(d()).cache_budget_bytes) : "—"}</td></tr>
                <tr><th class="w-48 align-top">roles</th><td>{asList(host(d()).roles).join(", ") || "—"}</td></tr>
              </tbody>
            </table>
          </section>
          <section class="card mt-4 p-4">
            <h3 class="mb-2 font-semibold">Upstream follow</h3>
            <p>
              {d().upstream?.git ? `${d().upstream.git}` : "no upstream configured"}{" "}
              {d().upstream?.git && d().upstream?.follow
                ? <span class="chip">{`following every ${d().upstream.follow_interval_secs ?? "?"}s`}</span>
                : <span class="chip !bg-amber-100 !text-amber-800 dark:!bg-amber-900/60 dark:!text-amber-300">not following</span>}
              <Show when={d().upstream?.last_round}>
                <span class="muted">{` · last round ${fmtDate(d().upstream.last_round)}`}</span>
              </Show>
            </p>
          </section>
          <section class="card mt-4 p-4">
            <h3 class="mb-2 font-semibold">Effective values</h3>
            <table class="data-table kv">
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
            <table class="data-table">
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

// --- tab 5: CI tokens (05 §9, admin-only) ---------------------------------------------

function CITokensTab(props) {
  const key = () => `ci-tokens:${props.ctx.full}`;
  const [getTokens, setTokens] = createSignal(null);
  const [getName, setName] = createSignal("");
  const [getSecret, setSecret] = createSignal("");
  const [getNote, setNote] = createSignal("");

  const load = async () => {
    try {
      const res = await props.repo.ciTokens.list();
      setTokens(res.tokens ?? []);
      setNote("");
    } catch (e) { setNote(String(e.message ?? e)); }
  };
  load();

  const create = async (e) => {
    e.preventDefault();
    const name = getName().trim();
    if (!name) return;
    try {
      const res = await props.repo.ciTokens.create({ name });
      setSecret(res.token ?? "");
      setName("");
      setNote(`created ${res.id} — copy the secret now, it is shown once`);
      load();
    } catch (err) { setNote(String(err.message ?? err)); }
  };

  const revoke = async (id) => {
    try {
      await props.repo.ciTokens.revoke(id);
      if (getSecret()) setSecret("");
      load();
    } catch (err) { setNote(String(err.message ?? err)); }
  };

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(getSecret());
      setNote("secret copied");
    } catch {
      setNote("copy failed — select the secret manually");
    }
  };

  return (
    <>
      <section class="card p-4">
        <h3 class="mb-2 font-semibold">CI tokens</h3>
        <p class="muted mb-3 text-sm">
          External CI reports statuses with a <code class="font-mono">wct_</code> token.
          Tokens grant <code class="font-mono">checks:write</code> on this repo only —
          never push, merge, or admin.
        </p>
        <form class="flex flex-wrap items-center gap-2" onSubmit={create}>
          <input
            class="input w-64"
            placeholder="token name (e.g. woodpecker)"
            value={getName()}
            onInput={(e) => setName(e.target.value)}
            aria-label="Token name"
          />
          <button type="submit" class="btn btn-primary px-3 py-1">
            create token
          </button>
        </form>
        <Show when={getSecret()}>
          <div class="mt-3 rounded border border-amber-300 bg-amber-50 p-3 dark:border-amber-700 dark:bg-amber-950">
            <p class="mb-1 text-sm font-semibold text-amber-800 dark:text-amber-200">Copy now — shown once:</p>
            <div class="flex flex-wrap items-center gap-2">
              <code class="break-all font-mono text-xs">{getSecret()}</code>
              <button type="button" class="pill cursor-pointer select-none" onClick={copy}>
                copy
              </button>
            </div>
          </div>
        </Show>
        <Show when={getNote()}>
          <p class="mt-2 text-sm text-zinc-600 dark:text-zinc-300">{getNote()}</p>
        </Show>
      </section>
      <section class="card mt-4 p-4">
        <h3 class="mb-2 font-semibold">Tokens</h3>
        <Show when={getTokens()} fallback={<p class="muted">loading…</p>}>
          <table class="data-table">
            <thead>
              <tr><th>id</th><th>name</th><th>scopes</th><th>created by</th><th>created</th><th>status</th><th></th></tr>
            </thead>
            <tbody>
              <For each={getTokens() ?? []} fallback={<tr><td colspan={7} class="muted">no tokens</td></tr>}>
                {(t) => (
                  <tr>
                    <td class="font-mono text-xs">{t.id}</td>
                    <td>{t.name}</td>
                    <td class="text-xs">{(t.scopes ?? []).join(", ")}</td>
                    <td class="text-xs">{t.created_by}</td>
                    <td class="text-xs">{fmtDate(t.created_at)}</td>
                    <td class="text-xs">{t.revoked_at ? `revoked ${fmtDate(t.revoked_at)}` : "active"}</td>
                    <td>
                      <Show when={!t.revoked_at}>
                        <button type="button" class="link text-xs" onClick={() => revoke(t.id)}>
                          revoke
                        </button>
                      </Show>
                    </td>
                  </tr>
                )}
              </For>
            </tbody>
          </table>
        </Show>
      </section>
    </>
  );
}

// --- shell ---------------------------------------------------------------------------

function WebhooksTab(props) {
  const [getHooks, setHooks] = createSignal(null);
  const [getUrl, setUrl] = createSignal("");
  const [getEvents, setEvents] = createSignal("");
  const [getSecret, setSecret] = createSignal("");
  const [getNote, setNote] = createSignal("");
  const [getDeliveries, setDeliveries] = createSignal({});
  const [getOpen, setOpen] = createSignal(null);

  const load = async () => {
    try {
      const res = await props.repo.webhooks.list();
      setHooks(res.webhooks ?? []);
      setNote("");
    } catch (e) { setNote(String(e.message ?? e)); }
  };
  load();

  const create = async (e) => {
    e.preventDefault();
    const url = getUrl().trim();
    if (!url) return;
    const events = getEvents().split(",").map((s) => s.trim()).filter(Boolean);
    try {
      await props.repo.webhooks.create({ url, events, secret: getSecret().trim() || undefined });
      setUrl("");
      setEvents("");
      setSecret("");
      setNote("webhook created — the secret is never shown again");
      load();
    } catch (err) { setNote(String(err.message ?? err)); }
  };

  const remove = async (id) => {
    try {
      await props.repo.webhooks.remove(id);
      load();
    } catch (err) { setNote(String(err.message ?? err)); }
  };

  const ping = async (id) => {
    try {
      const res = await props.repo.webhooks.ping(id);
      setNote(res.delivery ? `ping ${id} delivered` : `ping ${id} queued (delivery pending)`);
      load();
    } catch (err) { setNote(String(err.message ?? err)); }
  };

  const toggleDeliveries = async (id) => {
    if (getOpen() === id) {
      setOpen(null);
      return;
    }
    try {
      const res = await props.repo.webhooks.deliveries(id);
      setDeliveries((prev) => ({ ...prev, [id]: res.entries ?? [] }));
      setOpen(id);
    } catch (err) { setNote(String(err.message ?? err)); }
  };

  return (
    <>
      <section class="card p-4">
        <h3 class="mb-2 font-semibold">Webhooks</h3>
        <p class="muted mb-3 text-sm">
          One POST per collaboration event (comments, reviews, checks, pings) with{" "}
          <code class="font-mono">X-Walgit-Delivery</code> / <code class="font-mono">X-Walgit-Signature</code> /{" "}
          <code class="font-mono">X-Walgit-Event</code> headers. HTTPS only (HTTP on loopback for dev).
          Empty events = all; <code class="font-mono">*</code> matches everything.
        </p>
        <form class="flex flex-wrap items-center gap-2" onSubmit={create}>
          <input
            class="input w-72"
            placeholder="https://example.com/hook"
            value={getUrl()}
            onInput={(e) => setUrl(e.target.value)}
            aria-label="Webhook URL"
          />
          <input
            class="input w-48"
            placeholder="events, comma-list (empty = all)"
            value={getEvents()}
            onInput={(e) => setEvents(e.target.value)}
            aria-label="Events filter"
          />
          <input
            class="input w-48"
            type="password"
            placeholder="secret (optional, write-only)"
            value={getSecret()}
            onInput={(e) => setSecret(e.target.value)}
            aria-label="Webhook secret"
            autocomplete="new-password"
          />
          <button type="submit" class="btn btn-primary px-3 py-1">
            add webhook
          </button>
        </form>
        <Show when={getNote()}>
          <p class="mt-2 text-sm text-zinc-600 dark:text-zinc-300">{getNote()}</p>
        </Show>
      </section>
      <section class="card mt-4 p-4">
        <h3 class="mb-2 font-semibold">Configured hooks</h3>
        <Show when={getHooks()} fallback={<p class="muted">loading…</p>}>
          <table class="data-table">
            <thead>
              <tr><th>url</th><th>events</th><th>active</th><th>secret</th><th></th></tr>
            </thead>
            <tbody>
              <For each={getHooks() ?? []} fallback={<tr><td colspan={5} class="muted">no webhooks</td></tr>}>
                {(h) => (
                  <>
                    <tr>
                      <td class="break-all font-mono text-xs">{h.url}</td>
                      <td class="text-xs">{(h.events ?? []).join(", ") || "all"}</td>
                      <td class="text-xs">{h.active ? "active" : "paused"}</td>
                      <td class="text-xs">{h.secret_set ? "set" : "—"}</td>
                      <td class="whitespace-nowrap text-xs">
                        <button type="button" class="link mr-2" onClick={() => ping(h.id)}>ping</button>
                        <button type="button" class="link mr-2" onClick={() => toggleDeliveries(h.id)}>
                          {getOpen() === h.id ? "hide" : "deliveries"}
                        </button>
                        <button type="button" class="link" onClick={() => remove(h.id)}>delete</button>
                      </td>
                    </tr>
                    <Show when={getOpen() === h.id}>
                      <tr>
                        <td colspan={5}>
                          <For each={getDeliveries()[h.id] ?? []} fallback={<span class="muted text-xs">no deliveries yet</span>}>
                            {(d) => (
                              <div class="font-mono text-xs text-zinc-600 dark:text-zinc-300">
                                #{d.seq} {d.event} → {d.status}{d.error ? ` (${d.error})` : ""} · {d.at}
                              </div>
                            )}
                          </For>
                        </td>
                      </tr>
                    </Show>
                  </>
                )}
              </For>
            </tbody>
          </table>
        </Show>
      </section>
    </>
  );
}

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
        <Show when={getTab() === "Access"}><AccessTab ctx={ctx} repo={repo} /></Show>
        <Show when={getTab() === "CI tokens"}><CITokensTab ctx={ctx} repo={repo} /></Show>
        <Show when={getTab() === "Webhooks"}><WebhooksTab ctx={ctx} repo={repo} /></Show>
      </div>
    </div>
  );
}
