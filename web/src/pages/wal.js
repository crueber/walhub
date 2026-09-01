// web/src/pages/wal.js — WAL page (§2.9): health box (issues, deep fsck, suggestions
// each dispatching its op as a task), ops box (single-flight buttons, boolean /
// strategy params, live SSE log, grouped recent history), manifest + local-copy
// boxes, packs/checkpoints, bundle chain tree, bundle slot plan, compactions,
// WAL segments (newest 5 + "all").

import { createRoot, createEffect, createSignal, onCleanup, el } from "lib/reactive.js";
import { useData, DEFAULT_TTL } from "lib/data.js";
import { mountStream } from "lib/sse.js";
import { fmtBytes, fmtDate, shortRef } from "./repo.js";

function statusChip(status) {
  return el("span", { class: `chip status-${status ?? "ok"}` }, status ?? "ok");
}

function suggestionRow(repo, s, refresh) {
  const [busy, setBusy] = createSignal(false);
  const btn = el("button", { class: "pill action", type: "button", onclick: runIt }, `run ${s.op}`);
  async function runIt() {
    setBusy(true);
    try {
      await repo.ops.run(s.op, s.params ?? {}, () => {});
      refresh();
    } finally { setBusy(false); }
  }
  createEffect(() => { btn.disabled = busy(); });
  return el("div", { class: "suggestion" },
    el("code", {}, s.op), s.reason ? el("span", { class: "muted" }, ` — ${s.reason}`) : null,
    s.auto ? el("span", { class: "chip" }, "auto") : null, " ", btn);
}

function opsBox(ctx, logSink) {
  const repo = ctx.repoClient;
  const [getOps] = useData(`ops:${ctx.full}`, () => repo.ops.list(), DEFAULT_TTL);
  const running = new Set(); // single-flight per op
  const live = new Set(); // attached op streams — cancelled on unmount
  onCleanup(() => { for (const s of live) s.cancel(); live.clear(); });
  const box = el("section", { class: "card" }, el("h2", {}, "Ops"), el("p", { class: "muted" }, "loading ops…"));
  createEffect(() => {
    const data = getOps();
    if (!data) return;
    const strategies = data.bundle_strategies ?? [];
    const rows = (data.available ?? []).map((op) => {
      const paramsRow = el("div", { class: "op-params" });
      const collect = {};
      for (const p of op.params ?? []) {
        if (p.kind === "bool") {
          collect[p.name] = el("input", { type: "checkbox" });
          paramsRow.append(el("label", { class: "op-param" }, collect[p.name], " ", p.name));
        } else if (p.kind === "strategy") {
          collect[p.name] = el("select", {}, ...strategies.map((s, i) => el("option", { value: String(s.name ?? i) }, s.name ?? `#${i}`)));
          paramsRow.append(el("label", { class: "op-param" }, p.name, ": ", collect[p.name]));
        } else {
          collect[p.name] = el("input", { type: "text", placeholder: p.name });
          paramsRow.append(el("label", { class: "op-param" }, p.name, ": ", collect[p.name]));
        }
      }
      const [getBusy, setBusy] = createSignal(false);
      const runBtn = el("button", { class: "pill action", type: "button", onclick: runOp }, `run ${op.op}`);
      createEffect(() => { runBtn.disabled = getBusy() || running.has(op.op); });
      async function runOp() {
        if (running.has(op.op)) return; // single-flight
        running.add(op.op);
        setBusy(true);
        logSink(`→ ${op.op}`);
        const params = Object.fromEntries(Object.entries(collect).map(([k, input]) => [k, input.type === "checkbox" ? input.checked : input.value]).filter(([, v]) => v !== "" && v !== false));
        const release = () => { running.delete(op.op); setBusy(false); live.delete(stream); };
        const stream = mountStream(
          // attach for the task's whole lifetime; the SDK resolves when the task ends
          (signal, emit) => Promise.resolve(repo.ops.run(op.op, params, emit, { signal })).finally(release),
          (frame) => logSink(`${op.op}: ${typeof frame === "string" ? frame : JSON.stringify(frame)}`));
        live.add(stream);
        stream.run();
      }
      return el("div", { class: "op-row" },
        el("div", { class: "op-head" }, el("code", {}, op.op), op.doc ? el("span", { class: "muted" }, ` — ${op.doc}`) : null, " ", runBtn),
        (op.params ?? []).length ? paramsRow : null);
    });
    box.replaceChildren(
      el("h2", {}, "Ops"),
      el("p", { class: "muted" }, `${(data.available ?? []).length} available · ${(data.recent ?? []).length} recent`),
      ...rows,
      (data.recent ?? []).length ? el("details", {}, el("summary", { class: "pill" }, "recent ops"),
        el("ul", { class: "recent-ops" }, ...(data.recent ?? []).map((t) => el("li", {}, `${t.kind ?? t.op} — ${t.summary ?? t.state ?? ""} · ${fmtDate(t.finished ?? t.at)}`)))) : null);
  });
  return box;
}

function bundleChain(bundles) {
  const list = bundles ?? [];
  const known = new Set(list.map((b) => b.sha));
  // roots = full bundles; incrementals whose base vanished surface as orphan roots (warned)
  const roots = list.filter((b) => !b.base_id || b.kind === "full" || !known.has(b.base_id));
  const byBase = new Map();
  for (const b of list) {
    if (!b.base_id) continue;
    if (!byBase.has(b.base_id)) byBase.set(b.base_id, []);
    byBase.get(b.base_id).push(b);
  }
  const renderNode = (b, depth) => {
    const kids = (byBase.get(b.sha) ?? []).sort((x, y) => String(x.creation_token ?? "").localeCompare(String(y.creation_token ?? "")));
    const orphan = b.kind !== "full" && b.base_id && !known.has(b.base_id);
    return [
      el("li", { class: `bundle-node ${orphan ? "orphan" : ""}`, style: `margin-left:${depth * 16}px` },
        el("code", {}, String(b.sha ?? "").slice(0, 10)),
        ` ${b.strategy ?? ""}/${b.kind ?? ""}`,
        orphan ? el("span", { class: "chip warn" }, "base missing") : null,
        el("span", { class: "muted" }, ` · ${fmtBytes(b.size)} · token ${b.creation_token ?? "—"}`)),
      ...kids.flatMap((k) => renderNode(k, depth + 1)),
    ];
  };
  return el("ul", { class: "bundle-chain" }, ...roots.flatMap((r) => renderNode(r, 0)));
}

export function mount(container, ctx) {
  return createRoot((dispose) => {
    const repo = ctx.repoClient;
    const [getOverview] = useData(`overview:${ctx.full}`, () => repo.overview(), DEFAULT_TTL);
    const root = el("div", { class: "wal-page" });
    const logLines = [];
    const log = el("pre", { class: "op-log" });
    const logSink = (line) => {
      logLines.push(line);
      if (logLines.length > 200) logLines.shift(); // bounded buffer
      log.textContent = logLines.join("\n");
    };
    container.append(root, el("section", { class: "card" }, el("h2", {}, "Live op log"), log));
    root.append(opsBox(ctx, logSink));

    createEffect(() => {
      const o = getOverview();
      if (!o) {
        root.prepend(el("p", { class: "muted" }, "loading overview…"));
        return;
      }
      const health = o.health ?? {};
      const manifest = o.manifest ?? {};
      const local = o.local ?? {};
      const packs = o.packs ?? {};
      const plan = o.bundle_plan ?? { slots: [] };
      const byStatus = {};
      for (const s of plan.slots ?? []) (byStatus[s.status ?? "other"] ??= []).push(s);

      const healthBox = el("section", { class: "card" },
        el("h2", {}, "Health ", statusChip(health.status)),
        el("p", {}, `served by ${o.hostname ?? "?"} · clone ${o.clone_url ?? "—"}`),
        (health.issues ?? []).length ? el("ul", { class: "issues" }, ...health.issues.map((i) => el("li", {}, i))) : el("p", { class: "muted" }, "no issues"),
        health.deep ? el("p", {}, "deep fsck: ", statusChip(health.deep.ok === false ? "error" : "ok"), health.deep.detail ? el("span", { class: "muted" }, ` ${health.deep.detail}`) : null) : null,
        el("div", { class: "suggestions" },
          (health.suggestions ?? []).length ? el("h3", {}, "Suggestions") : null,
          ...(health.suggestions ?? []).map((s) => suggestionRow(repo, s, () => useData(`overview:${ctx.full}`, () => repo.overview(), DEFAULT_TTL)))));

      const manifestBox = el("section", { class: "card" },
        el("h2", {}, "Manifest"),
        el("table", { class: "grid kv" },
          el("tbody", {},
            el("tr", {}, el("th", {}, "version"), el("td", {}, String(manifest.version ?? "—"))),
            el("tr", {}, el("th", {}, "next_seq"), el("td", {}, String(manifest.next_seq ?? "—"))),
            el("tr", {}, el("th", {}, "min_seq"), el("td", {}, String(manifest.min_seq ?? "—"))),
            el("tr", {}, el("th", {}, "entries"), el("td", {}, String(manifest.entries ?? "—"))),
            el("tr", {}, el("th", {}, "tail entries"), el("td", {}, String(manifest.tail_entries ?? "—"))),
            el("tr", {}, el("th", {}, "checkpoint"), el("td", {}, manifest.checkpoint ? `@${manifest.checkpoint.seq ?? manifest.checkpoint}` : "—")),
            el("tr", {}, el("th", {}, "packset"), el("td", {}, String(manifest.packset?.count ?? manifest.packset?.packs?.length ?? "—"))),
            el("tr", {}, el("th", {}, "last push"), el("td", {}, fmtDate(manifest.last_push))))));

      const localBox = el("section", { class: "card" },
        el("h2", {}, "Local copy"),
        el("table", { class: "grid kv" },
          el("tbody", {},
            el("tr", {}, el("th", {}, "version"), el("td", {}, String(local.version ?? "—"))),
            el("tr", {}, el("th", {}, "next_seq"), el("td", {}, String(local.next_seq ?? "—"))),
            el("tr", {}, el("th", {}, "bootstrapped"), el("td", {}, String(local.bootstrap ?? "—"))),
            el("tr", {}, el("th", {}, "reconciled"), el("td", {}, String(local.reconciled ?? "—"))),
            el("tr", {}, el("th", {}, "size"), el("td", {}, fmtBytes(local.size_bytes))))));

      const packsBox = el("section", { class: "card" },
        el("h2", {}, "Packs"),
        el("p", {}, `${packs.live ?? "—"} live packs · ${fmtBytes(packs.live_bytes)} · ${packs.pushes ?? "—"} pushes recorded`));

      const planBox = el("section", { class: "card" },
        el("h2", {}, "Bundle slot plan"),
        el("p", { class: "muted" }, `built ${byStatus.built?.length ?? 0} · skipped ${byStatus.skipped?.length ?? 0} · too-small ${byStatus.too_small?.length ?? 0} · unavailable ${byStatus.unavailable?.length ?? 0}`),
        el("table", { class: "grid plan-table" },
          el("thead", {}, el("tr", {}, el("th", {}, "strategy"), el("th", {}, "slot"), el("th", {}, "status"), el("th", {}, "detail"), el("th", {}, "bundle"))),
          el("tbody", {}, ...(plan.slots ?? []).map((s) => el("tr", {},
            el("td", {}, s.strategy ?? ""), el("td", {}, String(s.slot ?? "")),
            el("td", {}, el("span", { class: `chip status-${s.status}` }, s.status ?? "")),
            el("td", { class: "muted" }, s.detail ?? ""),
            el("td", {}, el("code", {}, String(s.bundle_id ?? "").slice(0, 10))))))));

      const segments = manifest.segments ?? [];
      const showAll = createSignal(false);
      const segRows = () => {
        const list = showAll[0]() ? segments : segments.slice(-5);
        return list.map((sg) => el("tr", {},
          el("td", {}, String(sg.seq ?? sg.min_seq ?? "")),
          el("td", {}, el("code", {}, String(sg.checksum ?? sg.pack ?? "").slice(0, 12))),
          el("td", {}, String(sg.entries ?? ""))));
      };
      const segTable = el("table", { class: "grid segments-table" },
        el("thead", {}, el("tr", {}, el("th", {}, "seq"), el("th", {}, "pack"), el("th", {}, "entries"))),
        el("tbody", {}, ...segRows()));
      const segBox = el("section", { class: "card" },
        el("h2", {}, "WAL segments"),
        segTable,
        segments.length > 5 ? el("button", { class: "pill", type: "button", onclick: () => {
          showAll[1](!showAll[0]());
          segTable.tBodies[0].replaceChildren(...segRows());
        } }, showAll[0]() ? "newest 5" : "all") : null);

      const compBox = el("section", { class: "card" },
        el("h2", {}, "Compactions"),
        (o.compactions ?? []).length
          ? el("table", { class: "grid" },
              el("thead", {}, el("tr", {}, el("th", {}, "at_seq"), el("th", {}, "at"), el("th", {}, "tier"), el("th", {}, "packs"))),
              el("tbody", {}, ...o.compactions.map((c) => el("tr", {},
                el("td", {}, String(c.at_seq ?? "")), el("td", {}, fmtDate(c.at)),
                el("td", {}, String(c.tier ?? "")), el("td", {}, String(c.packs ?? c.pack_count ?? ""))))))
          : el("p", { class: "muted" }, "no compactions recorded"));

      const chainBox = el("section", { class: "card" },
        el("h2", {}, "Bundle chain"),
        el("p", { class: "muted" }, "roots = full bundles; children under base_id, sorted by creation_token"),
        (o.bundles ?? []).length ? bundleChain(o.bundles) : el("p", { class: "muted" }, "no bundles yet"));

      // keep opsBox (appended earlier) as the first card
      const existingOps = root.querySelector("section.card");
      root.replaceChildren();
      root.append(healthBox, existingOps, manifestBox, localBox, packsBox, chainBox, planBox, compBox, segBox);
    });
    return dispose;
  });
}
