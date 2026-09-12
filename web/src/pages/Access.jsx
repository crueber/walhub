// web/src/pages/Access.jsx — repo Access tab (features/01 §9): visibility
// toggle, role-binding table, add-binding form, full-document PUT with the
// CAS version in the footer; a 409 renders "changed under you, reload".
// Issue #361: on org-owned repos the add form gains a team dropdown (the
// owner org's roster via client.orgs.teams.list, composing team:org/slug
// into the subject field); free text stays the fallback and the spelling
// validates client-side (lib/access.js) with a friendly note.

import { createSignal, createEffect, For, Show } from "solid-js";
import repos from "../../sdk/src/index.js";
import { useData, invalidate, reportError } from "../lib/data.js";
import { TTL } from "../lib/collab.js";
import {
  validateAccessSubject,
  composeTeamSubject,
  shouldFetchTeams,
  asTeamList,
  teamOptionLabel,
} from "../lib/access.js";
import VisSelect from "../components/VisSelect.jsx";
import { friendlyAccessError } from "../lib/accessSave.js";

const ROLES = ["read", "triage", "write", "maintain", "admin"];

export default function AccessTab(props) {
  const full = props.ctx.full;
  const repo = props.repo;
  const key = () => `access:${full}`;
  const [getDoc] = useData(key(), () => repo.access.get(), 5000);
  // Baseline: the last server doc (dirty() compares the form against this,
  // not against the cached getDoc which invalidate() does not refresh).
  const [getBase, setBase] = createSignal(null);
  // Forgejo #394: null = unseeded (loading) — the select renders a blank
  // disabled control until server truth arrives, never a public-looking
  // default. reset() fills it from a PRESENT doc, where `?? "public"` is
  // the genuine missing-field default.
  const [getVis, setVis] = createSignal(null);
  const [getRows, setRows] = createSignal([]);
  const [getNote, setNote] = createSignal("");
  const [getSub, setSub] = createSignal("");
  const [getRole, setRole] = createSignal("read");
  const [getSaving, setSaving] = createSignal(false);
  // Team-subject picker (issue #361): the owner org's team roster, fetched
  // once for the add-binding form. [] while loading-denied-or-empty — an
  // email (user-owned) owner skips the GET entirely (an org slug can never
  // contain `@`); a 404 (legacy-namespace owner) or 403 (no team
  // visibility) degrades to the free-text subject input, which always
  // stays. One extra GET on an admin settings tab, never on a hot path.
  const owner = () => String(props.ctx?.owner ?? "").trim();
  // Forgejo #374: the visibility options depend on the owner kind
  // (user-owned: owner-only private; org-owned: org-members-only
  // private). The org-vs-user verdict rides the #348 owner-kind marker:
  // orgs.get resolves for orgs and 404s (→ null, never a tray) for
  // users — one extra GET on an admin settings tab, never on a hot
  // path. Email owners skip the probe (an org slug can never contain
  // `@`, same rule as the team fetch below).
  const [getOrg] = useData(`org:${owner().toLowerCase()}`, () => {
    if (!shouldFetchTeams(owner())) return null;
    return repos.orgs.get(owner()).then((doc) => doc ?? null, () => null);
  }, 5000);
  const isOrg = () => getOrg() != null;
  const [getTeamRows] = useData(`org-teams:${owner().toLowerCase()}`, () => {
    if (!shouldFetchTeams(owner())) return [];
    return repos.orgs.teams.list(owner()).then(asTeamList, () => []);
  }, 5000);
  const [getTeamPick, setTeamPick] = createSignal("");

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
      setNote(friendlyAccessError(err));
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
      setNote(friendlyAccessError(err));
      if (err?.status === 409) await load();
    } finally {
      setSaving(false);
    }
  };

  const addRow = () => {
    // The team dropdown composes into the subject field, so validation
    // reads the one free-text value — typed or composed alike get the
    // friendly note, never a silent no-op.
    const checked = validateAccessSubject(getSub());
    if (checked.error) {
      setNote(checked.error);
      return;
    }
    const sub = checked.subject;
    if (getRows().some((r) => r.subject.toLowerCase() === sub.toLowerCase())) {
      setNote("that subject already has a binding");
      return;
    }
    setRows([...getRows(), { subject: sub, role: getRole() }]);
    setSub("");
    setTeamPick("");
  };

  // Picking a team fills the subject field with the composed spelling
  // (editable after — free text stays the fallback for users and for
  // non-org owners, where the dropdown never renders).
  const pickTeam = (slug) => {
    setTeamPick(slug);
    if (slug) setSub(composeTeamSubject(owner(), slug));
  };

  const dirty = () => {
    const base = getBase();
    if (!base) return false;
    return (
      getVis() !== (base.visibility ?? "public") ||
      JSON.stringify(getRows()) !== JSON.stringify((base.role_bindings ?? []).map((b) => ({ subject: b.subject, role: b.role })))
    );
  };

  // Forgejo #394: follow the shared entry while the form is clean —
  // a Settings visibility save, an explicit reload, or poll revalidation
  // delivering a newer doc reseeds the untouched form; user edits are
  // never clobbered (same rule as Settings, lib/visibilityReseed.js).
  // The baseline rebases ONLY while clean, so a dirty form keeps its
  // original CAS version and a concurrent save still 409s into the
  // "changed under you, reload" path instead of silently
  // last-writer-winning over the remote edit. The doc-identity guard
  // keeps this settled: reset() baselines this exact doc, so the
  // post-reset rerun skips.
  createEffect(() => {
    const doc = getDoc();
    if (!doc) return;
    if (doc !== getBase() && (!getBase() || !dirty())) reset(doc);
  });

  return (
    <div>
      <section class="card mb-4 p-4" aria-label="Effective access">
        <CollaboratorsBlock full={full} repo={repo} />
      </section>
      <Show when={getDoc()} fallback={<p class="muted">loading…</p>}>
        {(doc) => (
          <>
            <section class="card p-4">
              <h3 class="mb-2 font-semibold">Visibility</h3>
              <label class="flex items-center gap-2 text-sm">
                <span class="muted">Anonymous readers:</span>
                {/* Forgejo #410: the shared VisSelect (real <For> render path
                    over identity-stable options). The default
                    aria-label="Visibility" is new here — the inline select
                    it replaces had none. */}
                <VisSelect
                  value={getVis()}
                  disabled={getVis() === null}
                  onChange={setVis}
                  isOrg={isOrg()}
                />
              </label>
              <Show when={getVis() === null}>
                <p class="muted mt-1 text-xs">loading current visibility…</p>
              </Show>
            </section>

            <section class="card mt-4 p-4">
              <h3 class="mb-2 font-semibold">Role bindings</h3>
              <Show when={getRows().length > 0} fallback={<p class="muted text-sm">no bindings — org members, org owners, and host admins still apply.</p>}>
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
              <div class="mt-3 grid grid-cols-1 gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto_auto] sm:items-end">
                <label class="min-w-0 text-sm">
                  <span class="muted block text-xs">subject (user:email or team:org/slug)</span>
                  <input
                    class="input font-mono text-xs"
                    size={32}
                    placeholder="user:jane@example.com"
                    value={getSub()}
                    onInput={(e) => { setSub(e.currentTarget.value); setTeamPick(""); }}
                  />
                </label>
                <Show when={(getTeamRows() ?? []).length > 0}>
                  <label class="min-w-0 text-sm">
                    <span class="muted block text-xs">team (org {owner()}) — fills the subject</span>
                    <select
                      class="input min-w-0"
                      value={getTeamPick()}
                      aria-label={`teams in ${owner()}`}
                      onChange={(e) => pickTeam(e.currentTarget.value)}
                    >
                      <option value="">pick a team…</option>
                      <For each={getTeamRows()}>{(t) => <option value={t.slug}>{teamOptionLabel(t)}</option>}</For>
                    </select>
                  </label>
                </Show>
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
            <Show when={dirty()}>
              <p class="warn-line mt-2 !text-sm">unsaved changes</p>
            </Show>
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

// Failure wording lives in lib/accessSave.js (friendlyAccessError) — the
// Settings visibility save shares it so both tabs word the same failure
// the same way (Forgejo #391).

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
