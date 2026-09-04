// web/src/pages/Org.jsx — org settings (features/01 §9, `/:org/settings`):
// sub-tabs profile / members / teams / invitations. Member rows carry an
// inline role <select>; the invite form shows the returned accept link.

import { createSignal, For, Show } from "solid-js";
import { useParams } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { useData, invalidate, reportError } from "../lib/data.js";

const ORG_ROLES = ["owner", "member"];
const TABS = ["Profile", "Members", "Teams", "Invitations"];

function fmtDate(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? String(iso) : d.toISOString().replace("T", " ").slice(0, 16) + "Z";
}

function ProfileTab(props) {
  const org = props.org;
  const key = () => `org:${org}`;
  const [getOrg] = useData(key(), () => repos.orgs.get(org), 5000);
  const [getName, setName] = createSignal("");
  const [getDesc, setDesc] = createSignal("");
  const [getNote, setNote] = createSignal("");
  const [getSeeded, setSeeded] = createSignal(false);

  const seed = (o) => {
    if (o && !getSeeded()) {
      setName(o.display_name ?? "");
      setDesc(o.description ?? "");
      setSeeded(true);
    }
    return null;
  };

  const save = async () => {
    setNote("");
    try {
      await repos.orgs.put(org, { display_name: getName(), description: getDesc() });
      invalidate(key());
      setNote("saved");
    } catch (err) {
      reportError(err, key());
      setNote(err?.status === 403 ? "org owner required" : String(err?.message ?? err));
    }
  };

  return (
    <Show when={getOrg()} fallback={<p class="muted">loading…</p>}>
      {(o) => (
        <>
          {seed(o())}
          <section class="card p-4">
            <h3 class="mb-2 font-semibold">Profile</h3>
            <div class="flex max-w-lg flex-col gap-2">
              <label class="text-sm">
                <span class="muted block text-xs">display name</span>
                <input class="input w-full" value={getName()} onInput={(e) => setName(e.currentTarget.value)} />
              </label>
              <label class="text-sm">
                <span class="muted block text-xs">description</span>
                <input class="input w-full" value={getDesc()} onInput={(e) => setDesc(e.currentTarget.value)} />
              </label>
              <div>
                <button type="button" class="btn px-3 py-1" onClick={save}>save profile</button>
              </div>
              <Show when={getNote()}><p class="text-sm text-amber-700 dark:text-amber-300">{getNote()}</p></Show>
              <p class="muted text-xs">created {fmtDate(o().created_at)} · updated {fmtDate(o().updated_at)}</p>
            </div>
          </section>
        </>
      )}
    </Show>
  );
}

function MembersTab(props) {
  const org = props.org;
  const key = () => `org-members:${org}`;
  const [getRoster] = useData(key(), () => repos.orgs.members.list(org), 5000);
  const [getNew, setNew] = createSignal("");
  const [getNewRole, setNewRole] = createSignal("member");
  const [getNote, setNote] = createSignal("");

  const refresh = () => invalidate(key());
  const fail = (err) => {
    reportError(err, key());
    setNote(err?.status === 403 ? "org owner required" : err?.status === 409 ? String(err?.message ?? err) : String(err?.message ?? err));
  };

  const setRole = async (principal, role) => {
    setNote("");
    try {
      await repos.orgs.members.put(org, principal, role);
      refresh();
    } catch (err) {
      fail(err);
    }
  };

  const remove = async (principal) => {
    setNote("");
    try {
      await repos.orgs.members.delete(org, principal);
      refresh();
    } catch (err) {
      fail(err);
    }
  };

  const add = async () => {
    const email = getNew().trim().toLowerCase();
    if (!email) return;
    setNote("");
    try {
      await repos.orgs.members.put(org, email, getNewRole());
      setNew("");
      refresh();
    } catch (err) {
      fail(err);
    }
  };

  return (
    <section class="card p-4">
      <h3 class="mb-2 font-semibold">Members</h3>
      <Show when={getRoster()} fallback={<p class="muted">loading…</p>}>
        {(m) => (
          <div class="overflow-x-auto">
            <table class="data-table">
              <thead><tr><th>principal</th><th>role</th><th><span class="sr-only">actions</span></th></tr></thead>
              <tbody>
                <For each={m().members ?? []}>
                  {(row) => (
                    <tr>
                      <td><code class="font-mono text-xs">{row.principal}</code></td>
                      <td>
                        <select
                          class="input"
                          value={row.role}
                          onChange={(e) => setRole(row.principal, e.currentTarget.value)}
                        >
                          <For each={ORG_ROLES}>{(r) => <option value={r}>{r}</option>}</For>
                        </select>
                      </td>
                      <td>
                        <button type="button" class="btn px-2 py-1" onClick={() => remove(row.principal)}>remove</button>
                      </td>
                    </tr>
                  )}
                </For>
              </tbody>
            </table>
          </div>
        )}
      </Show>
      <div class="mt-3 flex flex-wrap items-end gap-2">
        <label class="text-sm">
          <span class="muted block text-xs">email</span>
          <input class="input font-mono text-xs" size={30} value={getNew()} onInput={(e) => setNew(e.currentTarget.value)} placeholder="sam@example.com" />
        </label>
        <label class="text-sm">
          <span class="muted block text-xs">role</span>
          <select class="input" value={getNewRole()} onChange={(e) => setNewRole(e.currentTarget.value)}>
            <For each={ORG_ROLES}>{(r) => <option value={r}>{r}</option>}</For>
          </select>
        </label>
        <button type="button" class="btn px-3 py-1" onClick={add}>add member</button>
      </div>
      <Show when={getNote()}><p class="mt-2 text-sm text-amber-700 dark:text-amber-300">{getNote()}</p></Show>
    </section>
  );
}

function TeamsTab(props) {
  const org = props.org;
  const key = () => `org-teams:${org}`;
  const [getTeams] = useData(key(), () => repos.orgs.teams.list(org), 5000);
  const [getSlug, setSlug] = createSignal("");
  const [getName, setName] = createSignal("");
  const [getNote, setNote] = createSignal("");
  const [getAdds, setAdds] = createSignal({});

  const refresh = () => invalidate(key());
  const fail = (err) => {
    reportError(err, key());
    setNote(err?.status === 403 ? "org owner required" : String(err?.message ?? err));
  };

  const create = async () => {
    const slug = getSlug().trim().toLowerCase();
    if (!slug) return;
    setNote("");
    try {
      await repos.orgs.teams.create(org, { slug, name: getName().trim() || slug });
      setSlug("");
      setName("");
      refresh();
    } catch (err) {
      fail(err);
    }
  };

  const removeTeam = async (slug) => {
    setNote("");
    try {
      await repos.orgs.teams.delete(org, slug);
      refresh();
    } catch (err) {
      fail(err);
    }
  };

  const addMember = async (slug) => {
    const email = (getAdds()[slug] ?? "").trim().toLowerCase();
    if (!email) return;
    setNote("");
    try {
      await repos.orgs.teams.addMember(org, slug, email);
      setAdds({ ...getAdds(), [slug]: "" });
      refresh();
    } catch (err) {
      fail(err);
    }
  };

  const removeMember = async (slug, email) => {
    setNote("");
    try {
      await repos.orgs.teams.removeMember(org, slug, email);
      refresh();
    } catch (err) {
      fail(err);
    }
  };

  return (
    <section class="card p-4">
      <h3 class="mb-2 font-semibold">Teams</h3>
      <Show when={getTeams()} fallback={<p class="muted">loading…</p>}>
        {(list) => (
          <Show when={(list() ?? []).length > 0} fallback={<p class="muted text-sm">no teams yet.</p>}>
            <div class="flex flex-col gap-4">
              <For each={list() ?? []}>
                {(t) => (
                  <div class="rounded border border-zinc-200 p-3 dark:border-zinc-800">
                    <div class="flex flex-wrap items-baseline gap-2">
                      <strong class="font-mono text-sm">{t.slug}</strong>
                      <span class="muted text-xs">{t.name ?? ""}</span>
                      <button type="button" class="btn ml-auto px-2 py-1" onClick={() => removeTeam(t.slug)}>delete team</button>
                    </div>
                    <ul class="mt-2 flex flex-col gap-1">
                      <For each={t.members ?? []}>
                        {(m) => (
                          <li class="flex items-center gap-2 text-sm">
                            <code class="font-mono text-xs">{m}</code>
                            <button type="button" class="btn px-2 py-0.5" onClick={() => removeMember(t.slug, m)}>remove</button>
                          </li>
                        )}
                      </For>
                    </ul>
                    <div class="mt-2 flex flex-wrap items-end gap-2">
                      <input
                        class="input font-mono text-xs"
                        size={28}
                        placeholder="sam@example.com"
                        value={getAdds()[t.slug] ?? ""}
                        onInput={(e) => setAdds({ ...getAdds(), [t.slug]: e.currentTarget.value })}
                      />
                      <button type="button" class="btn px-2 py-1" onClick={() => addMember(t.slug)}>add</button>
                    </div>
                  </div>
                )}
              </For>
            </div>
          </Show>
        )}
      </Show>
      <div class="mt-3 flex flex-wrap items-end gap-2">
        <label class="text-sm">
          <span class="muted block text-xs">slug</span>
          <input class="input font-mono text-xs" size={20} value={getSlug()} onInput={(e) => setSlug(e.currentTarget.value)} placeholder="platform" />
        </label>
        <label class="text-sm">
          <span class="muted block text-xs">name</span>
          <input class="input text-xs" size={20} value={getName()} onInput={(e) => setName(e.currentTarget.value)} placeholder="Platform" />
        </label>
        <button type="button" class="btn px-3 py-1" onClick={create}>create team</button>
      </div>
      <Show when={getNote()}><p class="mt-2 text-sm text-amber-700 dark:text-amber-300">{getNote()}</p></Show>
    </section>
  );
}

function InvitesTab(props) {
  const org = props.org;
  const key = () => `org-invites:${org}`;
  const [getInvs] = useData(key(), () => repos.orgs.invites.list(org), 5000);
  const [getEmail, setEmail] = createSignal("");
  const [getRole, setRole] = createSignal("member");
  const [getNote, setNote] = createSignal("");
  const [getLink, setLink] = createSignal("");

  const refresh = () => invalidate(key());
  const fail = (err) => {
    reportError(err, key());
    setNote(err?.status === 403 ? "org owner required" : String(err?.message ?? err));
  };

  const invite = async () => {
    const email = getEmail().trim().toLowerCase();
    if (!email) return;
    setNote("");
    setLink("");
    try {
      const res = await repos.orgs.invites.create(org, { email, role: getRole() });
      setLink(res.accept_url ?? "");
      setEmail("");
      refresh();
    } catch (err) {
      fail(err);
    }
  };

  const cancel = async (id) => {
    setNote("");
    try {
      await repos.orgs.invites.cancel(org, id);
      refresh();
    } catch (err) {
      fail(err);
    }
  };

  return (
    <section class="card p-4">
      <h3 class="mb-2 font-semibold">Invitations</h3>
      <Show when={getInvs()} fallback={<p class="muted">loading…</p>}>
        {(list) => (
          <Show when={(list() ?? []).length > 0} fallback={<p class="muted text-sm">no pending invitations.</p>}>
            <div class="overflow-x-auto">
              <table class="data-table">
                <thead><tr><th>subject</th><th>role</th><th>invited by</th><th><span class="sr-only">actions</span></th></tr></thead>
                <tbody>
                  <For each={list() ?? []}>
                    {(inv) => (
                      <tr>
                        <td><code class="font-mono text-xs">{inv.subject}</code></td>
                        <td>{inv.role}</td>
                        <td><code class="font-mono text-xs">{inv.invited_by}</code></td>
                        <td><button type="button" class="btn px-2 py-1" onClick={() => cancel(inv.id)}>cancel</button></td>
                      </tr>
                    )}
                  </For>
                </tbody>
              </table>
            </div>
          </Show>
        )}
      </Show>
      <div class="mt-3 flex flex-wrap items-end gap-2">
        <label class="text-sm">
          <span class="muted block text-xs">email</span>
          <input class="input font-mono text-xs" size={30} value={getEmail()} onInput={(e) => setEmail(e.currentTarget.value)} placeholder="pat@example.com" />
        </label>
        <label class="text-sm">
          <span class="muted block text-xs">role</span>
          <select class="input" value={getRole()} onChange={(e) => setRole(e.currentTarget.value)}>
            <For each={ORG_ROLES}>{(r) => <option value={r}>{r}</option>}</For>
          </select>
        </label>
        <button type="button" class="btn px-3 py-1" onClick={invite}>invite</button>
      </div>
      <Show when={getLink()}>
        <p class="mt-2 text-sm">accept link: <code class="font-mono text-xs">{getLink()}</code></p>
      </Show>
      <Show when={getNote()}><p class="mt-2 text-sm text-amber-700 dark:text-amber-300">{getNote()}</p></Show>
    </section>
  );
}

export default function Org() {
  const params = useParams();
  const org = () => (params.org ?? "").toLowerCase();
  const [getTab, setTab] = createSignal("Profile");

  return (
    <div class="mx-auto max-w-6xl px-4 py-4">
      <h2 class="mb-1 text-lg font-semibold">
        <span class="muted font-normal">org</span> {org()}
      </h2>
      <nav class="subtabs mb-4 flex flex-wrap gap-1.5" aria-label="organization sections">
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
        <Show when={getTab() === "Profile"}><ProfileTab org={org()} /></Show>
        <Show when={getTab() === "Members"}><MembersTab org={org()} /></Show>
        <Show when={getTab() === "Teams"}><TeamsTab org={org()} /></Show>
        <Show when={getTab() === "Invitations"}><InvitesTab org={org()} /></Show>
      </div>
    </div>
  );
}
