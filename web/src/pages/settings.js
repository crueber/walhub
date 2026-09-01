// web/src/pages/settings.js — repo settings, three sub-tabs (§2.9):
// 1. Scheduled tasks — strategy table + placement/host facts + upstream follow status.
// 2. Push policy — textarea editor, 400 ms debounced validate, dry-run vs last N
//    pushes, save/discard/copy.
// 3. Effective config & history — TOML editor with debounced validate + live fields
//    preview, publish with a message, clear, per-revision history with
//    "Revert to this" + line diff.

import { createRoot, createEffect, createSignal, onCleanup, el } from "lib/reactive.js";
import { useData, reportError } from "lib/data.js";
import { fmtDate } from "./repo.js";

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

function tabsBar(active, pick) {
  const bar = el("nav", { class: "subtabs" });
  for (const t of ["Scheduled tasks", "Push policy", "Effective config & history"]) {
    bar.append(el("button", { type: "button", class: `pill ${t === active() ? "active" : ""}`,
      onclick: () => pick(t) }, t));
  }
  return bar;
}

export function mount(container, ctx) {
  return createRoot((dispose) => {
    const repo = ctx.repoClient;
    const [getTab, setTab] = createSignal("Scheduled tasks");
    const root = el("div", { class: "settings-page" });
    container.append(el("h2", { class: "page-title" }, "Settings"), root);

    createEffect(() => {
      const tab = getTab();
      const holder = el("div");
      root.replaceChildren(tabsBar(getTab, setTab), holder);
      const sub = tab === "Scheduled tasks" ? scheduledTab
        : tab === "Push policy" ? policyTab : configTab;
      sub(holder, ctx, repo);
    });
    return dispose;
  });
}

// --- tab 1: scheduled tasks ------------------------------------------------------

function scheduledTab(holder, ctx, repo) {
  const [getDesc] = useData(`settings-describe:${ctx.full}`, () => repo.settings.describe(), 5000);
  createEffect(() => {
    const d = getDesc();
    if (!d) { holder.replaceChildren(el("p", { class: "muted" }, "loading…")); return; }
    const host = d.maintenance?.this_host ?? {};
    holder.replaceChildren(
      el("section", { class: "card" },
        el("h3", {}, "Bundle strategies"),
        el("table", { class: "grid" },
          el("thead", {}, el("tr", {}, el("th", {}, "name"), el("th", {}, "kind"), el("th", {}, "base"), el("th", {}, "schedule"), el("th", {}, "next"), el("th", {}, "keep"), el("th", {}, "filter"), el("th", {}, "refs"))),
          el("tbody", {}, ...(d.sections?.strategies ?? d.strategies ?? []).map((s) => el("tr", {},
            el("td", {}, s.name ?? ""), el("td", {}, s.kind ?? ""), el("td", {}, s.base ?? "—"),
            el("td", {}, `${s.schedule ?? ""}${s.schedule_human ? ` (${s.schedule_human})` : ""}`),
            el("td", {}, fmtDate(s.next)), el("td", {}, String(s.keep ?? "—")),
            el("td", {}, s.filter ?? "—"), el("td", {}, (s.refs ?? []).join(", ") || "—"))))))),
      el("section", { class: "card" },
        el("h3", {}, "Placement & host facts"),
        el("table", { class: "grid kv" }, el("tbody", {},
          el("tr", {}, el("th", {}, "host"), el("td", {}, host.name ?? d.maintenance?.host ?? "—")),
          el("tr", {}, el("th", {}, "serves"), el("td", {}, (host.serves ?? []).join(", ") || "—")),
          el("tr", {}, el("th", {}, "maintains"), el("td", {}, (host.maintains ?? []).join(", ") || "—")),
          el("tr", {}, el("th", {}, "disk"), el("td", {}, host.disk ?? "—")),
          el("tr", {}, el("th", {}, "max pack bytes"), el("td", {}, String(host.max_pack_bytes ?? "—"))),
          el("tr", {}, el("th", {}, "cache budget"), el("td", {}, String(host.cache_budget_bytes ?? "—"))),
          el("tr", {}, el("th", {}, "roles"), el("td", {}, (host.roles ?? []).join(", ") || "—"))))),
      el("section", { class: "card" },
        el("h3", {}, "Upstream follow"),
        el("p", {}, d.upstream?.git ? `${d.upstream.git}` : "no upstream configured",
          d.upstream?.follow ? el("span", { class: "chip" }, `following every ${d.upstream.follow_interval_secs ?? "?"}s`) : el("span", { class: "chip warn" }, "not following"),
          d.upstream?.last_round ? el("span", { class: "muted" }, ` · last round ${fmtDate(d.upstream.last_round)}`) : null)),
      el("section", { class: "card" },
        el("h3", {}, "Effective values"),
        el("table", { class: "grid kv" }, el("tbody", {}, ...(d.fields ?? []).map((f) => el("tr", {},
          el("th", {}, f.key), el("td", {}, String(f.value ?? ""),
            f.source === "setting" ? el("span", { class: "chip" }, "repo setting") : el("span", { class: "muted" }, " host")))))));
  });
}

// --- tab 2: push policy ------------------------------------------------------------

function policyTab(holder, ctx, repo) {
  const [getText, setText] = createSignal("");
  const [getSaved, setSaved] = createSignal("");
  const [getNote, setNote] = createSignal("");
  const [getResult, setResult] = createSignal(null); // validate result
  const [getDry, setDry] = createSignal(null);
  const editor = el("textarea", { class: "editor", rows: "14", spellcheck: "false",
    placeholder: "policy JSON — empty means allow-all" });
  const resultBox = el("pre", { class: "op-log" });

  const validateDebounced = debounce(async () => {
    try {
      setResult(await repo.policy.validate(getText()));
    } catch (e) {
      setResult({ ok: false, errors: [String(e.message ?? e)] });
    }
  }, 400);
  editor.addEventListener("input", () => { setText(editor.value); validateDebounced(); });

  const saveBtn = el("button", { class: "pill action", type: "button", onclick: save }, "Save");
  const discardBtn = el("button", { class: "pill", type: "button", onclick: reload }, "Discard");
  const copyBtn = el("button", { class: "pill", type: "button", onclick: copy }, "Copy");
  const dryInput = el("input", { type: "number", min: "1", max: "100", value: "10", class: "num" });
  const dryBtn = el("button", { class: "pill", type: "button", onclick: dryRun }, "Dry-run last N pushes");

  async function reload() {
    try {
      const p = await repo.policy.get();
      setText(typeof p === "string" ? p : JSON.stringify(p, null, 2));
      setSaved(getText());
      editor.value = getText();
    } catch (e) { reportError(e, "policy"); }
  }
  async function save() {
    try {
      await repo.policy.put(getText());
      setSaved(getText());
      setNote("policy saved");
    } catch (e) { reportError(e, "policy save"); }
  }
  async function copy() {
    try { await navigator.clipboard.writeText(getText()); } catch { /* clipboard unavailable */ }
  }
  async function dryRun() {
    try { setDry(await repo.policy.dryRun(Number(dryInput.value) || 10)); }
    catch (e) { reportError(e, "policy dry-run"); }
  }

  void reload();
  holder.replaceChildren(
    el("section", { class: "card" },
      el("h3", {}, "Push policy"),
      el("p", { class: "muted" }, "validated client-side-free: the server is the gate (400 with reasons, fail closed on the next push)"),
      editor,
      el("div", { class: "btn-row" }, saveBtn, discardBtn, copyBtn, " ", dryInput, " ", dryBtn),
      el("div", { class: "policy-results" },
        (() => {
          const box = el("div");
          createEffect(() => { box.replaceChildren(el("span", { class: "muted" }, getNote())); });
          return box;
        })(),
        (() => {
          const box = el("div");
          createEffect(() => {
            const r = getResult();
            box.replaceChildren(r
              ? el("p", { class: r.ok === false ? "bad" : "good" }, r.ok === false ? `invalid: ${(r.errors ?? []).join("; ")}` : "valid")
              : el("span", { class: "muted" }, ""));
          });
          return box;
        })(),
        resultBox),
      (() => {
        const dryBox = el("section", { class: "card" });
        createEffect(() => {
          const d = getDry();
          if (!d) { dryBox.replaceChildren(); return; }
          dryBox.replaceChildren(
            el("h3", {}, `Dry run: ${d.allowed ?? 0} allowed / ${d.denied ?? 0} denied of ${d.pushes?.length ?? 0}`),
            el("table", { class: "grid" }, el("thead", {}, el("tr", {}, el("th", {}, "seq"), el("th", {}, "principal"), el("th", {}, "atomic"), el("th", {}, "refs"))),
              el("tbody", {}, ...(d.pushes ?? []).map((p) => el("tr", {},
                el("td", {}, String(p.seq ?? "")), el("td", {}, p.principal ?? ""), el("td", {}, String(p.atomic ?? "")),
                el("td", {}, (p.refs ?? []).map((r) => `${r.name}${r.ok ? "" : ` ✗ ${r.reason ?? ""}`}`).join(", ") || "—"))))));
        });
        return dryBox;
      })()));
  createEffect(() => { resultBox.textContent = getText() === getSaved() ? "" : "unsaved changes"; });
}

// --- tab 3: effective config & history ----------------------------------------------

function configTab(holder, ctx, repo) {
  const [getText, setText] = createSignal("");
  const [getMessage, setMessage] = createSignal("");
  const editor = el("textarea", { class: "editor", rows: "16", spellcheck: "false", placeholder: "[bundles]\nmain_only = false" });
  const message = el("input", { type: "text", placeholder: "publish message (author = $USER)" });
  const resultBox = el("pre", { class: "op-log" });

  message.addEventListener("input", () => setMessage(message.value));
  const validateDebounced = debounce(async () => {
    try {
      const r = await repo.settings.validate(editor.value);
      resultBox.textContent = r.ok === false ? `errors: ${(r.errors ?? []).join("; ")}` : "valid";
    } catch (e) { resultBox.textContent = String(e.message ?? e); }
  }, 400);
  editor.addEventListener("input", () => { setText(editor.value); validateDebounced(); });

  const [getEffective] = useData(`settings-effective:${ctx.full}`, () => repo.settings.effective(), 5000);
  const [getHistory] = useData(`settings-history:${ctx.full}`, () => repo.settings.history(), 5000);

  createEffect(() => {
    const eff = getEffective();
    if (eff !== undefined && editor.value === "") {
      const toml = typeof eff === "string" ? eff : String(eff?.toml ?? "");
      setText(toml);
      editor.value = toml;
    }
  });

  holder.replaceChildren(
    el("section", { class: "card" },
      el("h3", {}, "Effective config (TOML)"),
      editor,
      el("div", { class: "btn-row" },
        el("button", { class: "pill action", type: "button", onclick: publish }, "Publish"),
        el("button", { class: "pill", type: "button", onclick: clearAll }, "Clear"),
        " ", message),
      resultBox),
    (() => {
      const hist = el("section", { class: "card" }, el("h3", {}, "History"), el("p", { class: "muted" }, "loading…"));
      createEffect(() => {
        const h = getHistory();
        if (!h) return;
        hist.querySelector("p.muted")?.remove();
        for (const row of hist.querySelectorAll(".rev-row")) row.remove();
        for (const e of h.entries ?? []) {
          hist.append(el("div", { class: "rev-row" },
            el("div", { class: "rev-head" },
              el("strong", {}, `#${e.revision ?? e.seq}`),
              el("span", { class: "muted" }, ` ${e.author ?? ""} · ${fmtDate(e.at)} · ${e.message ?? ""} `),
              el("button", { class: "pill", type: "button", onclick: () => revert(e) }, "Revert to this"),
              el("button", { class: "pill", type: "button", onclick: () => showDiff(e) }, "line diff")),
            el("pre", { class: "op-log hidden" })));
        }
        if (!(h.entries ?? []).length) hist.append(el("p", { class: "muted" }, "no revisions published"));
      });
      return hist;
    })());

  async function publish() {
    try {
      await repo.settings.put(editor.value, getMessage());
      resultBox.textContent = "published";
      setMessage("");
      message.value = "";
    } catch (e) { resultBox.textContent = String(e.message ?? e); }
  }
  async function clearAll() {
    try { await repo.settings.delete?.() ?? await repo.settings.put("", ""); editor.value = ""; setText(""); }
    catch (e) { resultBox.textContent = String(e.message ?? e); }
  }
  function revert(e) {
    editor.value = e.toml ?? "";
    setText(editor.value);
    validateDebounced();
    editor.scrollIntoView({ behavior: "smooth" });
  }
  function showDiff(e) {
    const rows = lineDiff(String(e.toml ?? "").split("\n"), getText().split("\n"));
    const pre = el("pre", { class: "op-log" }, rows.map((r) => `${r.t === "-" ? "-" : r.t === "+" ? "+" : " "} ${r.text}`).join("\n"));
    el; // keep linter quiet
    // append transiently under the history card
    holder.append(el("section", { class: "card" }, el("h3", {}, `Diff of #${e.revision ?? e.seq} vs editor`), pre));
  }
}
