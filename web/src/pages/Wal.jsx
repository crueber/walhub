// web/src/pages/Wal.jsx — WAL tab (§2.9): health box (status, served-by, issues,
// deep fsck, suggestions each running its op), ops box (single-flight run buttons,
// boolean / strategy / text params, live SSE log, recent history), manifest +
// local-copy + packs, bundle chain tree, bundle slot plan, compactions, WAL
// segments (newest 5 + "all"). Solid port of pages/wal.js (D-WEB-6).

import { For, Show, createSignal, onCleanup } from "solid-js";
import { useDataRefetchable, DEFAULT_TTL, invalidate, reportError } from "../lib/data.js";
import { mountStream } from "../lib/sse.js";
import { useRepo, fmtDate, fmtBytes } from "./Repo.jsx";

// --- status chips -------------------------------------------------------------

const CHIP_TONES = {
  ok: "bg-emerald-100 text-emerald-800 dark:bg-emerald-900/60 dark:text-emerald-300",
  built: "bg-emerald-100 text-emerald-800 dark:bg-emerald-900/60 dark:text-emerald-300",
  error: "bg-red-100 text-red-800 dark:bg-red-900/60 dark:text-red-300",
  failed: "bg-red-100 text-red-800 dark:bg-red-900/60 dark:text-red-300",
  unavailable: "bg-red-100 text-red-800 dark:bg-red-900/60 dark:text-red-300",
  degraded: "bg-amber-100 text-amber-800 dark:bg-amber-900/60 dark:text-amber-300",
  skipped: "bg-amber-100 text-amber-800 dark:bg-amber-900/60 dark:text-amber-300",
  too_small: "bg-amber-100 text-amber-800 dark:bg-amber-900/60 dark:text-amber-300",
  "too-small": "bg-amber-100 text-amber-800 dark:bg-amber-900/60 dark:text-amber-300",
};

function StatusChip(props) {
  return <span class={`chip ${CHIP_TONES[props.status] ?? CHIP_TONES.unavailable}`}>{props.status ?? "ok"}</span>;
}

// --- small shared pieces -------------------------------------------------------

function KV(props) {
  return (
    <div class="overflow-x-auto"><table class="data-table kv">
      <tbody>
        <For each={props.rows}>{([label, value]) => <tr><th class="w-40 align-top">{label}</th><td>{value}</td></tr>}</For>
      </tbody>
    </table></div>
  );
}

function Card(props) {
  return (
    <section class="card p-4" id={props.id}>
      <h2 class="mb-2 flex items-center gap-2 font-semibold">
        {props.title}
        {props.children}
      </h2>
      {props.body}
    </section>
  );
}

// --- health suggestions (one run button per suggestion) ------------------------

function Suggestion(props) {
  const [getBusy, setBusy] = createSignal(false);
  const run = async () => {
    setBusy(true);
    try {
      await props.repo.ops.run(props.s.op, props.s.params ?? {}, () => {});
      props.refresh();
    } catch (e) {
      reportError(e, `op ${props.s.op}`);
    } finally {
      setBusy(false);
    }
  };
  return (
    <div class="suggestion flex flex-wrap items-center gap-2 py-0.5 text-sm">
      <code class="font-mono text-xs">{props.s.op}</code>
      <Show when={props.s.reason}>
        <span class="muted"> — {props.s.reason}</span>
      </Show>
      <Show when={props.s.auto}>
        <span class="chip">auto</span>
      </Show>
      <button type="button" class="pill cursor-pointer" disabled={getBusy()} onClick={() => void run()}>
        run {props.s.op}
      </button>
    </div>
  );
}

// --- ops box (single-flight per op, live SSE frames into the shared log) --------

function OpRow(props) {
  const bools = {}; // param name → [get, set]
  const texts = {}; // param name → [get, set]
  const [getStrategy, setStrategy] = createSignal("");
  for (const p of props.op.params ?? []) {
    if (p.kind === "bool") bools[p.name] = createSignal(false);
    else if (p.kind !== "strategy") texts[p.name] = createSignal("");
  }

  // params snapshot taken when the button is clicked (stream.run() happens then)
  function collect() {
    const params = {};
    for (const p of props.op.params ?? []) {
      if (p.kind === "bool") {
        if (bools[p.name][0]()) params[p.name] = true;
      } else if (p.kind === "strategy") {
        if (getStrategy() !== "") params[p.name] = getStrategy();
      } else {
        const v = texts[p.name][0]();
        if (v !== "") params[p.name] = v;
      }
    }
    return params;
  }

  // one stream per row: run() aborts the previous attempt before opening a new one
  const stream = mountStream(
    (signal, emit) =>
      Promise.resolve(props.repo.ops.run(props.op.op, collect(), emit, { signal })).finally(() => {
        props.release(props.op.op);
        props.unregister(stream);
      }),
    (frame) => props.onLog(`${props.op.op}: ${typeof frame === "string" ? frame : JSON.stringify(frame)}`),
  );
  onCleanup(() => stream.cancel());

  const runIt = () => {
    if (props.busy()) return; // single-flight: (repo, kind)
    props.onLog(`→ ${props.op.op}`);
    props.acquire(props.op.op);
    props.register(stream);
    stream.run();
  };

  return (
    <div class="op-row py-2">
      <div class="op-head flex flex-wrap items-center gap-2">
        <code class="font-mono text-xs">{props.op.op}</code>
        <Show when={props.op.doc}>
          <span class="muted text-sm"> — {props.op.doc}</span>
        </Show>
        <button type="button" class="pill action cursor-pointer" disabled={props.busy()} onClick={runIt}>
          {props.busy() ? "running…" : `run ${props.op.op}`}
        </button>
      </div>
      <Show when={(props.op.params ?? []).length > 0}>
        <div class="op-params mt-1 flex flex-wrap items-center gap-3 text-sm">
          <For each={props.op.params ?? []}>
            {(p) => (
              <Show
                when={p.kind === "strategy"}
                fallback={
                  <Show when={p.kind === "bool"} fallback={
                    <label class="muted">
                      {p.name}:{" "}
                      <input class="input inline-block w-40" type="text" placeholder={p.name} value={texts[p.name][0]()} onInput={(e) => texts[p.name][1](e.currentTarget.value)} />
                    </label>
                  }>
                    <label class="muted flex items-center gap-1">
                      <input type="checkbox" checked={bools[p.name][0]()} onChange={(e) => bools[p.name][1](e.currentTarget.checked)} /> {p.name}
                    </label>
                  </Show>
                }
              >
                <label class="muted">
                  {p.name}:{" "}
                  <select class="input inline-block w-44" value={getStrategy()} onChange={(e) => setStrategy(e.currentTarget.value)}>
                    <For each={props.strategies()}>
                      {(s, i) => <option value={typeof s === "string" ? s : String(s.name ?? i())}>{typeof s === "string" ? s : s.name ?? `#${i()}`}</option>}
                    </For>
                  </select>
                </label>
              </Show>
            )}
          </For>
        </div>
      </Show>
    </div>
  );
}

function OpsBox(props) {
  const repo = props.ctx.repoClient;
  const [getOps] = useDataRefetchable(`ops:${props.ctx.full}`, () => repo.ops.list(), DEFAULT_TTL);
  const [getRunning, setRunning] = createSignal(new Set()); // single-flight per op
  const live = new Set(); // attached op streams — cancelled on unmount
  onCleanup(() => { for (const s of live) s.cancel(); live.clear(); });
  const acquire = (op) => setRunning((s) => new Set(s).add(op));
  const release = (op) => setRunning((s) => { const n = new Set(s); n.delete(op); return n; });
  const register = (stream) => live.add(stream);

  return (
    <Show when={getOps()} fallback={<Card title="Ops" body={<p class="muted">loading ops…</p>} />}>
      {(data) => {
        const available = () => data().available ?? [];
        const recent = () => data().recent ?? [];
        const strategies = () => data().bundle_strategies ?? [];
        return (
          <Card
            title="Ops"
            body={
              <>
                <p class="muted mb-1 text-sm">{available().length} available · {recent().length} recent</p>
                <For each={available()}>
                  {(op) => (
                    <OpRow
                      op={op}
                      repo={repo}
                      strategies={strategies}
                      busy={() => getRunning().has(op.op)}
                      acquire={acquire}
                      release={release}
                      register={register}
                      unregister={(stream) => live.delete(stream)}
                      onLog={props.onLog}
                    />
                  )}
                </For>
                <Show when={recent().length > 0}>
                  <details class="mt-2">
                    <summary class="pill cursor-pointer select-none">recent ops</summary>
                    <ul class="recent-ops mt-2 space-y-1 text-sm">
                      <For each={recent()}>
                        {(t) => (
                          <li>
                            {t.kind ?? t.op} — {t.summary ?? t.state ?? ""} · {fmtDate(t.finished ?? t.at)}
                          </li>
                        )}
                      </For>
                    </ul>
                  </details>
                </Show>
              </>
            }
          />
        );
      }}
    </Show>
  );
}

// --- bundle chain tree ---------------------------------------------------------

function ChainNode(props) {
  const children = () =>
    (props.byBase.get(props.b.sha) ?? []).sort((x, y) =>
      String(x.creation_token ?? "").localeCompare(String(y.creation_token ?? "")),
    );
  const orphan = () => props.b.kind !== "full" && props.b.base_id && !props.known.has(props.b.base_id);
  return (
    <li class="bundle-node py-0.5" classList={{ orphan: orphan() }}>
      <code class="font-mono text-xs">{String(props.b.sha ?? "").slice(0, 10)}</code>{" "}
      {`${props.b.strategy ?? ""}/${props.b.kind ?? ""}`}
      <Show when={orphan()}>
        <span class="chip bg-amber-100 text-amber-800 dark:bg-amber-900/60 dark:text-amber-300">base missing</span>
      </Show>
      <span class="muted"> · {fmtBytes(props.b.size)} · token {props.b.creation_token ?? "—"}</span>
      <Show when={children().length > 0}>
        <ul class="bundle-chain ml-4 border-l border-zinc-200 pl-3 text-sm dark:border-zinc-800">
          <For each={children()}>{(c) => <ChainNode b={c} byBase={props.byBase} known={props.known} />}</For>
        </ul>
      </Show>
    </li>
  );
}

function BundleChain(props) {
  const list = () => props.bundles ?? [];
  const known = () => new Set(list().map((b) => b.sha));
  // roots = full bundles; incrementals whose base vanished surface as orphan roots (warned)
  const roots = () => list().filter((b) => !b.base_id || b.kind === "full" || !known().has(b.base_id));
  const byBase = () => {
    const m = new Map();
    for (const b of list()) {
      if (!b.base_id) continue;
      const kids = m.get(b.base_id) ?? [];
      kids.push(b);
      m.set(b.base_id, kids);
    }
    return m;
  };
  return (
    <ul class="bundle-chain text-sm">
      <For each={roots()}>{(b) => <ChainNode b={b} byBase={byBase()} known={known()} />}</For>
    </ul>
  );
}

// --- the page ------------------------------------------------------------------

export default function Wal() {
  const ctx = useRepo();
  const repo = ctx.repoClient;
  const overviewKey = `overview:${ctx.full}`;
  const refresh = () => { invalidate(overviewKey); invalidate(`ops:${ctx.full}`); };
  const [getOverview] = useDataRefetchable(overviewKey, () => repo.overview(), DEFAULT_TTL);

  // shared live op log: bounded buffer, newest at the bottom
  const [getLog, setLog] = createSignal([]);
  const onLog = (line) => setLog((l) => (l.length >= 200 ? [...l.slice(1), line] : [...l, line]));

  return (
    <div class="wal-page space-y-4">
      <Show when={getOverview()} fallback={<p class="muted">loading overview…</p>}>
        {(o) => {
          const health = () => o().health ?? {};
          const manifest = () => o().manifest ?? {};
          const local = () => o().local ?? {};
          const packs = () => o().packs ?? {};
          const plan = () => o().bundle_plan ?? { slots: [] };
          const byStatus = () => {
            const m = {};
            for (const s of plan().slots ?? []) (m[s.status ?? "other"] ??= []).push(s);
            return m;
          };
          const segments = () => manifest().segments ?? [];
          const [getShowAll, setShowAll] = createSignal(false);
          const segList = () => (getShowAll() ? segments() : segments().slice(-5));

          return (
            <>
              <Card
                title={<>Health <StatusChip status={health().status} /></>}
                body={
                  <>
                    <p class="mb-2 text-sm">
                      served by {o().hostname ?? "?"} · clone {o().clone_url ?? "—"}
                    </p>
                    <Show
                      when={(health().issues ?? []).length > 0}
                      fallback={<p class="muted text-sm">no issues</p>}
                    >
                      <ul class="issues mb-2 list-disc pl-5 text-sm">
                        <For each={health().issues}>{(i) => <li>{i}</li>}</For>
                      </ul>
                    </Show>
                    <Show when={health().deep}>
                      <p class="text-sm">
                        deep fsck: <StatusChip status={health().deep.ok === false ? "error" : "ok"} />
                        <Show when={health().deep.detail}>
                          <span class="muted"> {health().deep.detail}</span>
                        </Show>
                      </p>
                    </Show>
                    <div class="suggestions mt-2">
                      <Show when={(health().suggestions ?? []).length > 0}>
                        <h3 class="mb-1 text-sm font-semibold">Suggestions</h3>
                      </Show>
                      <For each={health().suggestions ?? []}>
                        {(s) => <Suggestion s={s} repo={repo} refresh={refresh} />}
                      </For>
                    </div>
                  </>
                }
              />

              <OpsBox ctx={ctx} onLog={onLog} />

              <div class="data-table gap-4 md:grid-cols-2">
                <Card
                  title="Manifest"
                  body={
                    <KV
                      rows={[
                        ["version", String(manifest().version ?? "—")],
                        ["next_seq", String(manifest().next_seq ?? "—")],
                        ["min_seq", String(manifest().min_seq ?? "—")],
                        ["entries", String(manifest().entries ?? "—")],
                        ["tail entries", String(manifest().tail_entries ?? "—")],
                        ["checkpoint", manifest().checkpoint ? `@${manifest().checkpoint.seq ?? manifest().checkpoint}` : "—"],
                        ["packset", String(manifest().packset?.count ?? manifest().packset?.packs?.length ?? "—")],
                        ["last push", fmtDate(manifest().last_push) || "—"],
                      ]}
                    />
                  }
                />
                <Card
                  title="Local copy"
                  body={
                    <KV
                      rows={[
                        ["version", String(local().version ?? "—")],
                        ["next_seq", String(local().next_seq ?? "—")],
                        ["bootstrapped", String(local().bootstrap ?? "—")],
                        ["reconciled", String(local().reconciled ?? "—")],
                        ["size", fmtBytes(local().size_bytes)],
                      ]}
                    />
                  }
                />
              </div>

              <Card
                title="Packs"
                body={<p class="text-sm">{packs().live ?? "—"} live packs · {fmtBytes(packs().live_bytes)} · {packs().pushes ?? "—"} pushes recorded</p>}
              />

              <Card
                title="Bundle chain"
                body={
                  <>
                    <p class="muted mb-2 text-sm">roots = full bundles; children under base_id, sorted by creation_token</p>
                    <Show when={(o().bundles ?? []).length > 0} fallback={<p class="muted text-sm">no bundles yet</p>}>
                      <BundleChain bundles={o().bundles} />
                    </Show>
                  </>
                }
              />

              <Card
                title="Bundle slot plan"
                body={
                  <>
                    <p class="muted mb-2 text-sm">
                      built {byStatus().built?.length ?? 0} · skipped {byStatus().skipped?.length ?? 0} · too-small {byStatus().too_small?.length ?? 0} · unavailable {byStatus().unavailable?.length ?? 0}
                    </p>
                    <div class="overflow-x-auto"><table class="data-table plan-table">
                      <thead>
                        <tr><th>strategy</th><th>slot</th><th>status</th><th>detail</th><th>bundle</th></tr>
                      </thead>
                      <tbody>
                        <For each={plan().slots ?? []}>
                          {(s) => (
                            <tr>
                              <td>{s.strategy ?? ""}</td>
                              <td>{String(s.slot ?? "")}</td>
                              <td><StatusChip status={s.status} /></td>
                              <td class="muted">{s.detail ?? ""}</td>
                              <td><code class="font-mono text-xs">{String(s.bundle_id ?? "").slice(0, 10)}</code></td>
                            </tr>
                          )}
                        </For>
                      </tbody>
                    </table></div>
                  </>
                }
              />

              <Card
                title="Compactions"
                body={
                  <Show
                    when={(o().compactions ?? []).length > 0}
                    fallback={<p class="muted text-sm">no compactions recorded</p>}
                  >
                    <div class="overflow-x-auto"><table class="data-table">
                      <thead>
                        <tr><th>at_seq</th><th>at</th><th>tier</th><th>packs</th></tr>
                      </thead>
                      <tbody>
                        <For each={o().compactions}>
                          {(c) => (
                            <tr>
                              <td>{String(c.at_seq ?? "")}</td>
                              <td>{fmtDate(c.at)}</td>
                              <td>{String(c.tier ?? "")}</td>
                              <td>{String(c.packs ?? c.pack_count ?? "")}</td>
                            </tr>
                          )}
                        </For>
                      </tbody>
                    </table></div>
                  </Show>
                }
              />

              <Card
                title="WAL segments"
                body={
                  <>
                    <div class="overflow-x-auto"><table class="data-table segments-table">
                      <thead>
                        <tr><th>seq</th><th>pack</th><th>entries</th></tr>
                      </thead>
                      <tbody>
                        <For each={segList()}>
                          {(sg) => (
                            <tr>
                              <td>{String(sg.seq ?? sg.min_seq ?? "")}</td>
                              <td><code class="font-mono text-xs">{String(sg.checksum ?? sg.pack ?? "").slice(0, 12)}</code></td>
                              <td>{String(sg.entries ?? "")}</td>
                            </tr>
                          )}
                        </For>
                      </tbody>
                    </table></div>
                    <Show when={segments().length > 5}>
                      <button type="button" class="pill mt-2 cursor-pointer" onClick={() => setShowAll(!getShowAll())}>
                        {getShowAll() ? "newest 5" : "all"}
                      </button>
                    </Show>
                  </>
                }
              />
            </>
          );
        }}
      </Show>

      <Card title="Live op log" body={<pre class="code-view max-h-80 overflow-y-auto p-3">{getLog().join("\n")}</pre>} />
    </div>
  );
}
