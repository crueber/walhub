// web/src/pages/Access.jsx — repo Access tab (features/01 §9): visibility
// toggle, role-binding table, add-binding form, full-document PUT with the
// CAS version in the footer; a 409 renders "changed under you, reload".

import { createSignal, For, Show } from "solid-js";
import { useData, invalidate, reportError } from "../lib/data.js";
import { TTL } from "../lib/collab.js";

const ROLES = ["read", "triage", "write", "maintain", "admin"];

export default function AccessTab(props) {
  const full = props.ctx.full;
  const repo = props.repo;
  const key = () => `access:${full}`;
  const [getDoc] = useData(key(), () => repo.access.get(), 5000);
  // Baseline: the last server doc (dirty() compares the form against this,
  // not against the cached getDoc which invalidate() does not refresh).
  const [getBase, setBase] = createSignal(null);
  const [getVis, setVis] = createSignal("public");
  const [getRows, setRows] = createSignal([]);
  const [getNote, setNote] = createSignal("");
  const [getSub, setSub] = createSignal("");
  const [getRole, setRole] = createSignal("read");
  const [getSaving, setSaving] = createSignal(false);

  const reset = (doc) => {
    setBase(doc);
    setVis(doc.visibility ?? "public");
    setRows((doc.role_bindings ?? []).map((b) => ({ subject: b.subject, role: b.role })));
    setNote("");
  };

  const load = async (keepNote = "") => {
    setNote("");
    try {
      const doc = await repo.access.get();
      invalidate(key());
      reset(doc);
      if (keepNote) setNote(keepNote);
    } catch (err) {
      reportError(err, key());
      setNote(friendly(err));
    }
  };

  const save = async () => {
    const base = getBase();
    setSaving(true);
    setNote("");
    try {
      const next = await repo.access.put({
        version: base?.version ?? 0,
        visibility: getVis(),
        role_bindings: getRows().map((r) => ({ subject: r.subject, role: r.role })),
      });
      await load(`saved (version ${next.version})`);
    } catch (err) {
      reportError(err, key());
      setNote(friendly(err));
      if (err?.status === 409) await load();
    } finally {
      setSaving(false);
    }
  };

  const addRow = () => {
    const sub = getSub().trim();
    if (!sub) return;
    if (getRows().some((r) => r.subject.toLowerCase() === sub.toLowerCase())) {
      setNote("that subject already has a binding");
      return;
    }
    setRows([...getRows(), { subject: sub, role: getRole() }]);
    setSub("");
  };

  const dirty = () => {
    const base = getBase();
    if (!base) return false;
    return (
      getVis() !== (base.visibility ?? "public") ||
      JSON.stringify(getRows()) !== JSON.stringify((base.role_bindings ?? []).map((b) => ({ subject: b.subject, role: b.role })))
    );
  };

  // Seed the form on first load.
  const seed = (doc) => {
    if (!getBase() && doc) reset(doc);
    return null;
  };

  return (
    <div>
      <section class="card mb-4 p-4" aria-label="Effective access">
        <CollaboratorsBlock full={full} repo={repo} />
      </section>
      <Show when={getDoc()} fallback={<p class="muted">loading…</p>}>
        {(doc) => (
          <>
            {seed(doc())}
            <section class="card p-4">
              <h3 class="mb-2 font-semibold">Visibility</h3>
              <label class="flex items-center gap-2 text-sm">
                <span class="muted">Anonymous readers:</span>
                <select
                  class="input"
                  value={getVis()}
                  onChange={(e) => setVis(e.currentTarget.value)}
                >
                  <option value="public">public — anyone may read</option>
                  <option value="private">private — members only</option>
                </select>
              </label>
            </section>

            <section class="card mt-4 p-4">
              <h3 class="mb-2 font-semibold">Role bindings</h3>
              <Show when={getRows().length > 0} fallback={<p class="muted text-sm">no bindings — org owners and host admins still apply.</p>}>
                <div class="overflow-x-auto">
                  <table class="data-table">
                    <thead>
                      <tr><th>subject</th><th>role</th><th><span class="sr-only">actions</span></th></tr>
                    </thead>
                    <tbody>
                      <For each={getRows()}>
                        {(row, i) => (
                          <tr>
                            <td><code class="font-mono text-xs">{row.subject}</code></td>
                            <td>
                              <select
                                class="input"
                                value={row.role}
                                onChange={(e) => {
                                  const next = [...getRows()];
                                  next[i()] = { ...next[i()], role: e.currentTarget.value };
                                  setRows(next);
                                }}
                              >
                                <For each={ROLES}>{(r) => <option value={r}>{r}</option>}</For>
                              </select>
                            </td>
                            <td>
                              <button type="button" class="btn px-2 py-1" onClick={() => setRows(getRows().filter((_, j) => j !== i()))}>
                                remove
                              </button>
                            </td>
                          </tr>
                        )}
                      </For>
                    </tbody>
                  </table>
                </div>
              </Show>
              <div class="mt-3 grid grid-cols-1 gap-2 sm:grid-cols-[minmax(0,1fr)_auto_auto] sm:items-end">
                <label class="min-w-0 text-sm">
                  <span class="muted block text-xs">subject (user:email or team:org/slug)</span>
                  <input
                    class="input font-mono text-xs"
                    size={32}
                    placeholder="user:jane@example.com"
                    value={getSub()}
                    onInput={(e) => setSub(e.currentTarget.value)}
                  />
                </label>
                <label class="text-sm">
                  <span class="muted block text-xs">role</span>
                  <select class="input" value={getRole()} onChange={(e) => setRole(e.currentTarget.value)}>
                    <For each={ROLES}>{(r) => <option value={r}>{r}</option>}</For>
                  </select>
                </label>
                <button type="button" class="btn justify-self-start px-3 py-1 sm:justify-self-auto" onClick={addRow}>add</button>
              </div>
            </section>

            <div class="mt-3 flex flex-wrap items-center gap-2">
              <button type="button" class="btn px-3 py-1" disabled={!dirty() || getSaving()} onClick={save}>
                {getSaving() ? "saving…" : "save bindings"}
              </button>
              <button type="button" class="btn px-3 py-1" onClick={load}>reload</button>
              <span class="muted text-xs">version {getBase()?.version ?? 0} · full-document PUT · 409 means someone else saved first</span>
            </div>
            <Show when={getNote()}>
              <p class="mt-2 text-sm text-amber-700 dark:text-amber-300">{getNote()}</p>
            </Show>
            <Show when={!getBase()}>
              <p class="muted mt-2 text-sm">triage role or higher required to view access.</p>
            </Show>
          </>
        )}
      </Show>
    </div>
  );
}

function friendly(err) {
  if (!err) return "";
  if (err.status === 409) return "changed under you — reloaded the latest version";
  if (err.status === 403) return "admin role required to change access";
  if (err.status === 401) return "sign in to view access";
  return String(err.message ?? err);
}

/** Effective access (08 §§3.6/5): your resolved role plus the effective
 *  collaborator list with resolution sources. Read-gated; anonymous on a
 *  private repo sees the 401 note instead of the table. */
function CollaboratorsBlock(props) {
  const [getRole] = useData(`perms:${props.full}`, () => props.repo.permissions().catch(() => ({ role: null })), TTL.perms);
  const [getCollabs] = useData(`collaborators:${props.full}`, () => props.repo.collaborators.list().catch(() => ({ collaborators: [] })), TTL.perms);
  return (
    <>
      <h3 class="mb-2 font-semibold">Effective access</h3>
      <p class="mb-2 text-sm">
        your role: <code class="font-mono text-xs">{getRole()?.role ?? "none"}</code>
      </p>
      <Show when={(getCollabs()?.collaborators ?? []).length > 0} fallback={<p class="muted text-sm">no effective collaborators</p>}>
        <div class="overflow-x-auto">
          <table class="data-table">
            <thead>
              <tr><th>principal</th><th>role</th><th>source</th></tr>
            </thead>
            <tbody>
              <For each={getCollabs().collaborators}>
                {(c) => (
                  <tr>
                    <td><code class="font-mono text-xs">{c.principal}</code></td>
                    <td>{c.role}</td>
                    <td><code class="font-mono text-xs">{c.source}</code></td>
                  </tr>
                )}
              </For>
            </tbody>
          </table>
        </div>
      </Show>
    </>
  );
}
