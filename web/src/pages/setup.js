// web/src/pages/setup.js — §2.10 Setup UI (route /setup): schema-grouped cards with
// effective values, client validation mirroring the server (lib/setup.js, debounced
// per-keystroke hints), Validate → POST /api/v1/setup/test, Save → PUT /api/v1/setup,
// restart-required hints, setup-only-mode error banner.
// Dogfood exception: plain fetch for the three /api/v1/setup* endpoints (§6) —
// the SDK surface does not include them.

import { createRoot, createEffect, createSignal, el } from "lib/reactive.js";
import { reportError } from "lib/data.js";
import { validateSetup, normalizeSetup, isRestartLikely, FIELDS } from "lib/setup.js";

const FIELD_BY_KEY = new Map(FIELDS.map((f) => [f.key, f]));

async function getSetup(token) {
  const res = await fetch(`/api/v1/setup${token ? `?token=${encodeURIComponent(token)}` : ""}`, { credentials: "same-origin" });
  if (res.status === 403) {
    const body = await res.text();
    return { access: "denied", detail: body };
  }
  if (!res.ok) throw new Error(`GET /api/v1/setup: ${res.status} ${await res.text()}`);
  return res.json();
}

async function postSetup(method, payload, token) {
  const res = await fetch(`/api/v1/setup${method === "PUT" ? "" : "/test"}${token ? `?token=${encodeURIComponent(token)}` : ""}`, {
    method,
    credentials: "same-origin",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(payload),
  });
  const body = res.headers.get("content-type")?.includes("json") ? await res.json() : { errors: [{ key: "", message: await res.text() }] };
  return { status: res.status, body };
}

function inputFor(field, key, getValue, onInput) {
  const type = field?.type ?? "string";
  if (type === "bool") {
    return el("input", { type: "checkbox", checked: /^(true|1|on|yes)$/i.test(getValue(key)) ? "" : undefined, onchange: (e) => onInput(key, e.target.checked ? "true" : "false") });
  }
  if (type === "enum") {
    return el("select", { onchange: (e) => onInput(key, e.target.value) },
      el("option", { value: "" }, "(default)"),
      ...field.enum.map((v) => el("option", { value: v, selected: getValue(key) === v ? "" : undefined }, v)));
  }
  if (type === "toml") {
    return el("textarea", { rows: "4", spellcheck: "false", placeholder: "TOML only — validated server-side",
      oninput: (e) => onInput(key, e.target.value) }, getValue(key) ? undefined : null);
  }
  if (type === "list" || type === "globs") {
    return el("input", { type: "text", value: Array.isArray(getValue(key)) ? getValue(key).join(", ") : (getValue(key) ?? ""),
      oninput: (e) => onInput(key, e.target.value) });
  }
  if (type === "int" || type === "float") return el("input", { type: "number", step: "any", value: getValue(key) ?? "", oninput: (e) => onInput(key, e.target.value) });
  if (type === "duration") return el("input", { type: "text", placeholder: "e.g. 5m, 1h, 0", value: getValue(key) ?? "", oninput: (e) => onInput(key, e.target.value) });
  if (type === "size") return el("input", { type: "text", placeholder: "e.g. 64MiB, 0B", value: getValue(key) ?? "", oninput: (e) => onInput(key, e.target.value) });
  return el("input", { type: field?.secret ? "password" : "text", value: getValue(key) ?? "", oninput: (e) => onInput(key, e.target.value) });
}

export function mount(container) {
  return createRoot((dispose) => {
    const root = el("div", { class: "setup-page" });
    container.append(root);
    const token = new URLSearchParams(location.search).get("token") ?? ""; // WALHUB_SETUP_TOKEN hosts: query param, never stored

    const [getData, setData] = createSignal(null);
    const [getValues, setValues] = createSignal({});
    const [getErrors, setErrors] = createSignal([]);
    const [getResult, setResult] = createSignal(null); // {kind: "test"|"save", status, body}
    let debounce = 0;

    const onInput = (key, value) => {
      setValues((v) => ({ ...v, [key]: value }));
      clearTimeout(debounce);
      debounce = setTimeout(() => setErrors(validateSetup(getValues())), 250); // per-keystroke inline hints (debounced 250 ms)
    };

    const validateBtn = el("button", { class: "pill action", type: "button", onclick: doValidate }, "Validate");
    const saveBtn = el("button", { class: "pill action primary", type: "button", onclick: doSave }, "Save");
    const resultBox = el("pre", { class: "op-log" });
    createEffect(() => {
      const r = getResult();
      resultBox.textContent = r ? JSON.stringify(r, null, 2) : "";
    });

    async function doValidate() {
      const errors = validateSetup(getValues()).filter((e) => e.severity === "error");
      setErrors(validateSetup(getValues()));
      if (errors.length) {
        setResult({ kind: "test", status: "client", body: { errors } });
        return;
      }
      try {
        const { status, body } = await postSetup("POST", normalizeSetup(getValues()), token);
        setResult({ kind: "test", status, body });
      } catch (e) { reportError(e, "setup/test"); }
    }
    async function doSave() {
      const errors = validateSetup(getValues()).filter((e) => e.severity === "error");
      setErrors(validateSetup(getValues()));
      if (errors.length) { setResult({ kind: "save", status: "client", body: { errors } }); return; }
      try {
        const test = await postSetup("POST", normalizeSetup(getValues()), token);
        if (test.status !== 200) { setResult({ kind: "save", status: test.status, body: test.body }); return; } // gate: validate, then write
        const { status, body } = await postSetup("PUT", normalizeSetup(getValues()), token);
        setResult({ kind: "save", status, body });
        if (status === 200) {
          const changed = Object.keys(normalizeSetup(getValues()).overrides);
          const hints = (body.requires_restart ?? []).filter(isRestartLikely);
          setResult({ kind: "save", status, body, hint: hints.length
            ? `written to <data-dir>/walhub.toml — restart required for: ${changed.filter((k) => isRestartLikely(k)).join(", ")} (server list authoritative)`
            : `written to <data-dir>/walhub.toml` });
        }
      } catch (e) { reportError(e, "setup save"); }
    }

    createEffect(() => {
      const data = getData();
      if (!data) {
        root.replaceChildren(el("h2", {}, "Setup"), el("p", { class: "muted" }, "loading configuration schema…"));
        return;
      }

      const head = [el("h2", {}, "Setup")];

      if (data.access === "denied" || data.access === "admin_required" || data.access === "token_required") {
        root.replaceChildren(
          ...head,
          el("section", { class: "card" },
            el("h3", {}, "Access restricted"),
            el("p", {}, "This host requires an admin principal or a setup token to view or change configuration."),
            data.detail ? el("pre", { class: "op-log" }, data.detail) : null,
            el("p", { class: "muted" }, "Authenticate, then reload this page. Hosts with WALHUB_SETUP_TOKEN take the token as a query parameter (?token=…) on the GET/POST/PUT — it is never stored.")));
        return;
      }

      // setup-only mode banner: config invalid → only /setup, /healthz, /readyz answer
      if (data.file_state === "invalid") {
        head.push(el("div", { class: "banner banner-error" },
          el("strong", {}, "Setup-only mode — the config file is invalid."),
          el("p", {}, "Until a fixed config is saved AND the server restarted, only /setup, /healthz and /readyz answer; everything else returns 503. Fix the errors below, Validate, Save, restart.")));
      }

      if (data.errors?.length) {
        head.push(el("section", { class: "card errors-card" },
          el("h3", {}, "Current validation errors (from the server)"),
          el("table", { class: "grid" },
            el("thead", {}, el("tr", {}, el("th", {}, "section"), el("th", {}, "key"), el("th", {}, "message"), el("th", {}, "value"))),
            el("tbody", {}, ...data.errors.map((e) => el("tr", {},
              el("td", {}, (e.key ?? "").split(".")[0] ?? ""),
              el("td", {}, el("code", {}, e.key ?? "")),
              el("td", {}, e.message ?? ""),
              el("td", {}, el("code", {}, JSON.stringify(e.value ?? "")))))))),
          el("p", { class: "muted" }, "Nothing is disabled — fix the values and re-validate."));
      }

      // schema-grouped cards
      const cards = (data.groups ?? []).map((g) => {
        const section = g.section ?? "config";
        const rows = (g.keys ?? []).map((k) => {
          const field = FIELD_BY_KEY.get(k.key) ?? { type: k.type ?? "string" };
          const fromFile = k.value !== k.default;
          const row = el("div", { class: "setup-key", "data-key": k.key },
            el("label", { class: "setup-label" },
              el("code", {}, k.key),
              fromFile ? el("span", { class: "chip" }, "file") : el("span", { class: "muted" }, "default"),
              k.doc ? el("span", { class: "muted doc" }, k.doc) : null),
            inputFor(field, k.key, (key) => getValues()[key] ?? (k.value === null || k.value === undefined ? "" : Array.isArray(k.value) ? k.value.join(", ") : String(k.value)), onInput),
            el("div", { class: "key-errors" }));
          return row;
        });
        return el("section", { class: "card setup-section", "data-section": section }, el("h3", {}, section), ...rows);
      });

      root.replaceChildren(
        ...head,
        el("div", { class: "btn-row setup-actions" }, validateBtn, saveBtn),
        resultBox,
        ...cards);

      // inline error hints under each key (client rules mirror the server's)
      createEffect(() => {
        const errs = getErrors();
        for (const row of root.querySelectorAll(".setup-key")) {
          const key = row.getAttribute("data-key");
          const mine = errs.filter((e) => e.key === key);
          const slot = row.querySelector(".key-errors");
          slot.replaceChildren(...mine.map((e) =>
            el("p", { class: e.severity === "warn" ? "warn-line" : "err-line" }, e.message)));
        }
      });
    });

    getSetup(token)
      .then((data) => setData(data))
      .catch((e) => { reportError(e, "setup"); setData({ access: "error", detail: String(e.message ?? e) }); });

    return dispose;
  });
}
