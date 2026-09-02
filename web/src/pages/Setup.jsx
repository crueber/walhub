// web/src/pages/Setup.jsx — §2.10 Setup UI (route /setup): schema-grouped cards
// with effective values, client validation mirroring the server (lib/setup.js,
// debounced per-keystroke hints), Validate → POST /api/v1/setup/test,
// Save → PUT /api/v1/setup, restart-required hints, setup-only-mode error banner.
// Dogfood exception (§6): plain fetch for the three /api/v1/setup* endpoints —
// the SDK surface does not include them. WALHUB_SETUP_TOKEN hosts take the
// token as a query param (?token=…) — read once, never stored.

import { createSignal, createMemo, onCleanup, For, Show, Switch, Match } from "solid-js";
import { reportError } from "../lib/data.js";
import { validateSetup, normalizeSetup, isRestartLikely, FIELDS, fmtSpecDuration, fmtSpecSize, tomlFragment, fieldAppliesToMode } from "../lib/setup.js";

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

// The server sends typed values (bool, arrays); the form edits strings.
// Durations and sizes surface in the spec spelling ("1h", "64GiB") so the
// editable value matches the example under the label — both spellings save.
function initialText(row, field) {
  const v = row.value;
  if (v === null || v === undefined) return "";
  if (Array.isArray(v)) {
    if (field?.type === "toml") return tomlFragment(v, field.key.split(".").pop(), field.tomlKeys ?? []);
    return v.join(", ");
  }
  if (field?.type === "duration") return fmtSpecDuration(v);
  if (field?.type === "size") return fmtSpecSize(v);
  return String(v);
}

function FieldInput(props) {
  const type = () => props.field?.type ?? "string";
  const set = (v) => props.onInput(props.k, v);
  const id = () => props.id;
  return (
    <Switch>
      <Match when={type() === "bool"}>
        <button
          type="button"
          id={id()}
          role="switch"
          aria-labelledby={props.labelId}
          aria-checked={/^(true|1|on|yes)$/i.test(props.value() ?? "")}
          class="relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors
                 bg-zinc-300 dark:bg-zinc-700"
          classList={{ "!bg-emerald-600": /^(true|1|on|yes)$/i.test(props.value() ?? "") }}
          onClick={() => set(/^(true|1|on|yes)$/i.test(props.value() ?? "") ? "false" : "true")}
        >
          <span
            class="inline-block h-4 w-4 transform rounded-full bg-white shadow transition-transform"
            classList={{ "translate-x-6": /^(true|1|on|yes)$/i.test(props.value() ?? ""), "translate-x-1": !/^(true|1|on|yes)$/i.test(props.value() ?? "") }}
          />
        </button>
      </Match>
      <Match when={type() === "enum"}>
        <select id={id()} class="input md:w-72" onChange={(e) => set(e.currentTarget.value)}>
          <option value="" selected={!props.value()}>(default)</option>
          <For each={props.field?.enum ?? []}>
            {(v) => <option value={v} selected={props.value() === v}>{v}</option>}
          </For>
        </select>
      </Match>
      <Match when={type() === "toml"}>
        <textarea
          id={id()}
          class="input font-mono text-xs"
          rows="4"
          spellcheck={false}
          value={props.value() ?? ""}
          onInput={(e) => set(e.currentTarget.value)}
        />
      </Match>
      <Match when={type() === "list" || type() === "globs"}>
        <input
          id={id()}
          class="input md:max-w-md"
          type="text"
          value={Array.isArray(props.value()) ? props.value().join(", ") : (props.value() ?? "")}
          onInput={(e) => set(e.currentTarget.value)}
        />
      </Match>
      <Match when={type() === "int" || type() === "float"}>
        <input id={id()} class="input md:max-w-md" type="number" step="any" value={props.value() ?? ""} onInput={(e) => set(e.currentTarget.value)} />
      </Match>
      <Match when={type() === "duration"}>
        <input id={id()} class="input md:max-w-md" type="text" value={props.value() ?? ""} onInput={(e) => set(e.currentTarget.value)} />
      </Match>
      <Match when={type() === "size"}>
        <input id={id()} class="input md:max-w-md" type="text" value={props.value() ?? ""} onInput={(e) => set(e.currentTarget.value)} />
      </Match>
      <Match when={true}>
        <input id={id()} class="input md:max-w-md" type={props.field?.secret ? "password" : "text"} value={props.value() ?? ""} onInput={(e) => set(e.currentTarget.value)} />
      </Match>
    </Switch>
  );
}

export default function Setup() {
  // WALHUB_SETUP_TOKEN hosts: query param on the three calls, never stored.
  const token = new URLSearchParams(window.location.search).get("token") ?? "";

  const [getData, setData] = createSignal(null);
  const [getValues, setValues] = createSignal({});
  const [getErrors, setErrors] = createSignal([]);
  const [getResult, setResult] = createSignal(null); // {kind: "test"|"save", status, body, hint?}
  let debounce = 0;

  onCleanup(() => clearTimeout(debounce));

  const onInput = (key, value) => {
    setValues((v) => ({ ...v, [key]: value }));
    clearTimeout(debounce);
    debounce = setTimeout(() => setErrors(validateSetup(getValues())), 250); // per-keystroke inline hints (debounced 250 ms)
  };

  const validateErrors = () => {
    const all = validateSetup(getValues());
    setErrors(all);
    return all.filter((e) => e.severity === "error");
  };

  async function doValidate() {
    const errors = validateErrors();
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
    const errors = validateErrors();
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

  getSetup(token)
    .then(setData)
    .catch((e) => { reportError(e, "setup"); setData({ access: "error", detail: String(e.message ?? e) }); });

  // Unedited keys fall back to the server's effective value for the row.
  const rowFor = (key) => {
    for (const g of getData()?.groups ?? []) {
      const row = (g.keys ?? []).find((k) => k.key === key);
      if (row) return row;
    }
    return null;
  };
  const getValue = (key) => {
    const v = getValues()[key];
    if (v !== undefined) return v;
    const row = rowFor(key);
    return row ? initialText(row, FIELD_BY_KEY.get(key)) : "";
  };

  const restricted = () => {
    const access = getData()?.access;
    return access === "denied" || access === "admin_required" || access === "token_required" || access === "error";
  };
  // The auth card: effective mode from the FORM (falls back to the row value,
  // then "none"); redirect URI derived from the form's public_url when set.
  const authMode = () => {
    const v = getValues()["server.auth.mode"];
    if (v !== undefined && String(v).trim() !== "") return String(v).trim();
    const row = rowFor("server.auth.mode");
    const eff = row ? String(row.value ?? "").trim() : "";
    return eff || "none";
  };
  const redirectURI = () => {
    const pub = String(getValue("server.public_url") ?? "").trim().replace(/\/+$/, "");
    return `${pub || window.location.origin}/_auth/callback`;
  };
  // Memo so the auth-card callout re-renders as the operator edits public_url;
  // a plain call inside <Show> children is evaluated at creation only.
  const redirect = createMemo(redirectURI);

  return (
    <div class="setup-page space-y-4">
      <h2 class="text-xl font-semibold">Setup</h2>
      <Show when={getData()} fallback={<p class="muted">loading configuration schema…</p>}>
        <Show
          when={!restricted()}
          fallback={
            <section class="card space-y-2 p-4">
              <h3 class="font-semibold">Access restricted</h3>
              <p class="text-sm">This host requires an admin principal or a setup token to view or change configuration.</p>
              <Show when={getData()?.detail}>
                <pre class="code-view whitespace-pre-wrap p-3">{getData().detail}</pre>
              </Show>
              <p class="muted text-sm">
                Authenticate, then reload this page. Hosts with WALHUB_SETUP_TOKEN take the token as a query
                parameter (?token=…) on the GET/POST/PUT — it is never stored.
              </p>
            </section>
          }
        >
          {/* setup-only mode banner: config invalid → only /setup, /healthz, /readyz answer */}
          <Show when={getData().file_state === "invalid"}>
            <div class="rounded-lg border border-red-300 bg-red-50 p-3 text-sm text-red-800 dark:border-red-900 dark:bg-red-950/60 dark:text-red-200">
              <strong class="font-semibold">Setup-only mode — the config file is invalid.</strong>
              <p class="mt-1">
                Until a fixed config is saved AND the server restarted, only /setup, /healthz and /readyz answer;
                everything else returns 503. Fix the errors below, Validate, Save, restart.
              </p>
            </div>
          </Show>

          <Show when={(getData().errors ?? []).length > 0}>
            <section class="card errors-card p-4">
              <h3 class="mb-2 font-semibold">Current validation errors (from the server)</h3>
              <table class="data-table">
                <thead>
                  <tr><th>section</th><th>key</th><th>message</th><th>value</th></tr>
                </thead>
                <tbody>
                  <For each={getData().errors}>
                    {(e) => (
                      <tr>
                        <td class="muted">{(e.key ?? "").split(".")[0] ?? ""}</td>
                        <td><code class="font-mono text-xs">{e.key ?? ""}</code></td>
                        <td>{e.message ?? ""}</td>
                        <td><code class="font-mono text-xs">{JSON.stringify(e.value ?? "")}</code></td>
                      </tr>
                    )}
                  </For>
                </tbody>
              </table>
              <p class="muted mt-2 text-sm">Nothing is disabled — fix the values and re-validate.</p>
            </section>
          </Show>

          <div class="btn-row setup-actions flex gap-2">
            <button type="button" class="btn" onClick={doValidate}>Validate</button>
            <button type="button" class="btn primary" onClick={doSave}>Save</button>
          </div>

          <Show when={getResult()}>
            {(r) => (
              <>
                <pre class="code-view p-3">{JSON.stringify(r(), null, 2)}</pre>
                <Show when={r().hint}>
                  <p class="warn-line">{r().hint}</p>
                </Show>
              </>
            )}
          </Show>

          {/* schema-grouped cards; the auth card carries the OIDC redirect
              callout and a discovery test button (§8.6) */}
          <For each={getData().groups ?? []}>
            {(g) => {
              const section = g.section ?? "config";
              const isAuth = section === "auth";
              const [authTest, setAuthTest] = createSignal(null); // null | {phase: "testing"} | {phase: "done", status, body}
              let copied = 0;
              const [getCopied, setCopied] = createSignal(false);
              const runAuthTest = async () => {
                setAuthTest({ phase: "testing" });
                try {
                  const res = await fetch(`/api/v1/setup/auth/test${token ? `?token=${encodeURIComponent(token)}` : ""}`, {
                    method: "POST",
                    credentials: "same-origin",
                    headers: { "content-type": "application/json" },
                    body: JSON.stringify({
                      issuer: getValue("server.auth.issuer"),
                      client_id: getValue("server.auth.oauth_client_id"),
                      redirect_uri: redirect(),
                    }),
                  });
                  const body = await res.json().catch(async () => ({ errors: [{ message: await res.text() }] }));
                  setAuthTest({ phase: "done", status: res.status, body });
                } catch (e) {
                  setAuthTest({ phase: "done", status: 0, body: { errors: [{ message: String(e.message ?? e) }] } });
                }
              };
              const copyRedirect = async () => {
                try {
                  await navigator.clipboard.writeText(redirect());
                  setCopied(true);
                  clearTimeout(copied);
                  copied = setTimeout(() => setCopied(false), 1500);
                } catch { /* clipboard unavailable — the code stays selectable */ }
              };
              return (
                <section class="card setup-section p-4" data-section={section}>
                  <h3 class="mb-2 font-semibold">{section}</h3>
                  {/* OIDC redirect callout: what to register at the issuer */}
                  <Show when={isAuth && authMode() === "oidc"}>
                    <div class="setup-callout">
                      <p class="text-sm">
                        Register this <strong>redirect URI</strong> with your OIDC issuer (exact match, no
                        trailing slash) before sign-in will work:
                      </p>
                      <p class="mt-1 flex flex-wrap items-center gap-2">
                        <code class="code-view inline-block px-2 py-1">{redirect()}</code>
                        <button type="button" class="btn" onClick={copyRedirect}>{getCopied() ? "copied" : "copy"}</button>
                      </p>
                    </div>
                  </Show>
                  <For each={g.keys ?? []}>
                    {(k) => {
                      const field = FIELD_BY_KEY.get(k.key) ?? { type: k.type ?? "string" };
                      const fromFile = k.value !== k.default;
                      const inId = `setup-in-${k.key.replaceAll(".", "--")}`;
                      const lbId = `setup-lb-${k.key.replaceAll(".", "--")}`;
                      return (
                        <Show when={fieldAppliesToMode(field, authMode())}>
                          <div class="setup-row" data-key={k.key}>
                            {/* left: the key, its provenance chip, and a working
                                example that stays visible while typing */}
                            <div class="setup-label-col">
                              <label id={lbId} class="setup-label" for={inId}>
                                <code class="font-mono text-sm">{k.key}</code>
                                <Show when={fromFile} fallback={<span class="muted text-xs">default</span>}>
                                  <span class="chip">file</span>
                                </Show>
                              </label>
                              <Show when={field.ex}>
                                <p class="setup-examples">
                                  <span class="muted">e.g.</span> <span class="setup-ex">{field.ex}</span>
                                  <Show when={field.note}>
                                    <span class="setup-note">{field.note}</span>
                                  </Show>
                                </p>
                              </Show>
                            </div>
                            {/* right: the control, with the client validator's
                                inline hints directly under it */}
                            <div class="setup-input-col">
                              <FieldInput
                                field={field}
                                k={k.key}
                                id={inId}
                                labelId={lbId}
                                value={() => getValue(k.key)}
                                onInput={onInput}
                              />
                              <div class="key-errors">
                                <For each={getErrors().filter((e) => e.key === k.key)}>
                                  {(e) => <p class={e.severity === "warn" ? "warn-line" : "err-line"}>{e.message}</p>}
                                </For>
                              </div>
                            </div>
                          </div>
                        </Show>
                      );
                    }}
                  </For>
                  {/* OIDC discovery test: validates the issuer typed in the form */}
                  <Show when={isAuth && authMode() === "oidc"}>
                    <div class="mt-3 flex flex-wrap items-center gap-2">
                      <button type="button" class="btn" disabled={authTest()?.phase === "testing"} onClick={runAuthTest}>
                        {authTest()?.phase === "testing" ? "testing…" : "Test OIDC setup"}
                      </button>
                      <span class="muted text-xs">fetches &lt;issuer&gt;/.well-known/openid-configuration</span>
                    </div>
                    <Show when={authTest()?.phase === "done" ? authTest() : undefined}>
                      {(r) => (
                        <div class="mt-2">
                          <Show
                            when={r().status === 200}
                            fallback={
                              <For each={r().body?.errors ?? []}>
                                {(e) => <p class="err-line">{e.message}</p>}
                              </For>
                            }
                          >
                            <div class="setup-callout">
                              <p class="text-sm">
                                <strong>Discovery OK</strong> — issuer <code class="font-mono">{r().body?.issuer}</code>
                                <Show when={r().body?.client_id_present} fallback={<span class="muted"> · no client_id in the form yet</span>}>
                                  <span class="muted"> · client_id set</span>
                                </Show>
                              </p>
                              <ul class="muted mt-1 space-y-0.5 font-mono text-xs">
                                <li>authorization: {r().body?.authorization_endpoint}</li>
                                <li>token: {r().body?.token_endpoint}</li>
                                <li>jwks: {r().body?.jwks_uri}</li>
                              </ul>
                            </div>
                          </Show>
                        </div>
                      )}
                    </Show>
                  </Show>
                </section>
              );
            }}
          </For>
        </Show>
      </Show>
    </div>
  );
}
