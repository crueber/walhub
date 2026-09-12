// web/src/pages/Org.jsx — org settings (features/01 §9, `/:org/settings`):
// sub-tabs profile / members / teams / invitations. Member rows carry an
// inline role <select>; the invite form shows the returned accept link.

import { createSignal, For, Show } from "solid-js";
import { useParams, useNavigate } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { useData, invalidate, reportError } from "../lib/data.js";
import DateTime from "../components/DateTime.jsx";
import { isInviteExpired, invitePageLink } from "../lib/invites.js";
import { timeZones } from "../lib/timezone.js";
import { normalizeOrgProfile, orgSaveBody } from "../lib/org-profile.js";
import { renderBody } from "../lib/render-md.js";
import { DangerConfirm } from "./Settings.jsx";

const ORG_ROLES = ["owner", "member"];
const TABS = ["Profile", "Members", "Teams", "Invitations"];
// Client-side pre-check mirroring the server's 2 MiB avatar cap (the
// server still enforces it — this only fails fast with a readable note).
const MAX_AVATAR_BYTES = 2 << 20;

/** Current avatar <img> (Forgejo #359): renders only when the org doc
 *  names an avatar (avatar_content_type is the render gate — no byte
 *  probing); hides itself if the bytes 404 (pruned bucket). Shared with
 *  the /:owner org header (the RepoRow precedent: page-level sharing). */
export function OrgAvatar(props) {
  const src = () => {
    const o = props.doc?.();
    if (!o?.avatar_content_type) return null;
    return repos.orgs.avatar.url(props.org, o.avatar_updated_at);
  };
  return (
    <Show when={src()}>
      <img
        src={src()}
        alt=""
        width={props.size ?? 48}
        height={props.size ?? 48}
        class="rounded object-cover"
        style={`width:${props.size ?? 48}px;height:${props.size ?? 48}px`}
        onError={(e) => {
          e.currentTarget.style.display = "none";
        }}
      />
    </Show>
  );
}

function ProfileTab(props) {
  const org = props.org;
  const key = () => `org:${org}`;
  const [getOrg] = useData(key(), () => repos.orgs.get(org), 5000);
  const [getName, setName] = createSignal("");
  const [getDesc, setDesc] = createSignal("");
  const [getLoc, setLoc] = createSignal("");
  const [getTz, setTz] = createSignal("");
  const [getBio, setBio] = createSignal("");
  const [getNote, setNote] = createSignal("");
  const [getSeeded, setSeeded] = createSignal(false);
  const [getAvatarNote, setAvatarNote] = createSignal("");
  const zones = timeZones();

  const seedAll = (o) => {
    if (o && !getSeeded()) {
      const s = normalizeOrgProfile(o, org);
      setName(s.display_name);
      setDesc(s.description);
      setLoc(s.location);
      setTz(s.timezone);
      setBio(s.bio_markdown);
      setSeeded(true);
    }
    return null;
  };

  const save = async () => {
    setNote("");
    try {
      await repos.orgs.put(
        org,
        orgSaveBody({
          display_name: getName(),
          description: getDesc(),
          location: getLoc(),
          timezone: getTz(),
          bio_markdown: getBio(),
        })
      );
      invalidate(key());
      setNote("saved");
    } catch (err) {
      reportError(err, key());
      setNote(err?.status === 403 ? "org owner required" : String(err?.message ?? err));
    }
  };

  const uploadAvatar = async (file) => {
    setAvatarNote("");
    if (!file) return;
    if (file.size > MAX_AVATAR_BYTES) {
      setAvatarNote("avatar too large (max 2 MiB)");
      return;
    }
    try {
      await repos.orgs.avatar.upload(org, file, { contentType: file.type || undefined });
      invalidate(key());
      invalidate(`profile:${org}`);
    } catch (err) {
      reportError(err, key());
      setAvatarNote(
        err?.status === 413
          ? "avatar too large (max 2 MiB)"
          : err?.status === 415
            ? "only PNG, JPEG, GIF, and WebP avatars are accepted"
            : String(err?.message ?? err)
      );
    }
  };

  const removeAvatar = async () => {
    setAvatarNote("");
    try {
      await repos.orgs.avatar.remove(org);
      invalidate(key());
      invalidate(`profile:${org}`);
    } catch (err) {
      reportError(err, key());
      setAvatarNote(String(err?.message ?? err));
    }
  };

  return (
    <Show when={getOrg()} fallback={<p class="muted">loading…</p>}>
      {(o) => (
        <>
          {seedAll(o())}
          <section class="card p-4">
            <h3 class="mb-2 font-semibold">Profile</h3>
            {/* Forgejo #348: non-owners see the profile read-only (the
                server still 403s mutations — client gating is
                cosmetic-on-top, same canManage as the Manage link). */}
            <Show
              when={props.canManage()}
              fallback={
                <>
                  <p class="muted text-sm">read-only — org owner required to edit the profile.</p>
                  <div class="mt-2 flex items-center gap-3">
                    <OrgAvatar org={org} doc={o} size={48} />
                    <div>
                      <Show when={o().display_name}>
                        <p class="text-sm"><span class="muted">display name:</span> {o().display_name}</p>
                      </Show>
                      <Show when={o().description}>
                        <p class="mt-1 text-sm"><span class="muted">description:</span> {o().description}</p>
                      </Show>
                    </div>
                  </div>
                  <Show when={o().location || o().timezone}>
                    <p class="mt-1 text-sm"><span class="muted">location/timezone:</span> {[o().location, o().timezone].filter(Boolean).join(" · ")}</p>
                  </Show>
                  <Show when={o().bio_markdown}>
                    <div class="markdown-body mt-2" innerHTML={renderBody(o().bio_markdown)} />
                  </Show>
                </>
              }
            >
            <div class="flex max-w-lg flex-col gap-2">
              <div class="flex items-center gap-3">
                <OrgAvatar org={org} doc={o} size={48} />
                <div class="flex flex-col gap-1 text-sm">
                  <label class="text-xs">
                    <span class="muted block">avatar (PNG/JPEG/GIF/WebP, ≤ 2 MiB)</span>
                    <input
                      type="file"
                      accept="image/png,image/jpeg,image/gif,image/webp"
                      onChange={(e) => {
                        uploadAvatar(e.currentTarget.files?.[0]);
                        e.currentTarget.value = "";
                      }}
                    />
                  </label>
                  <Show when={o().avatar_content_type}>
                    <button type="button" class="btn self-start px-2 py-0.5 text-xs" onClick={removeAvatar}>
                      remove avatar
                    </button>
                  </Show>
                  <Show when={getAvatarNote()}><p class="text-sm text-amber-700 dark:text-amber-300">{getAvatarNote()}</p></Show>
                </div>
              </div>
              <label class="text-sm">
                <span class="muted block text-xs">display name</span>
                <input class="input w-full" maxlength="200" value={getName()} onInput={(e) => setName(e.currentTarget.value)} />
              </label>
              <label class="text-sm">
                <span class="muted block text-xs">description</span>
                <input class="input w-full" value={getDesc()} onInput={(e) => setDesc(e.currentTarget.value)} />
              </label>
              <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
                <label class="text-sm">
                  <span class="muted block text-xs">location</span>
                  <input class="input w-full" maxlength="200" value={getLoc()} onInput={(e) => setLoc(e.currentTarget.value)} placeholder="City, Country" />
                </label>
                <label class="text-sm">
                  <span class="muted block text-xs">timezone</span>
                  <select class="input w-full" value={getTz()} onChange={(e) => setTz(e.currentTarget.value)}>
                    <option value="">— unset —</option>
                    <For each={zones}>{(z) => <option value={z}>{z}</option>}</For>
                  </select>
                </label>
              </div>
              <label class="text-sm">
                <span class="muted block text-xs">bio (markdown)</span>
                <textarea class="input w-full font-mono text-sm" rows="5" value={getBio()} onInput={(e) => setBio(e.currentTarget.value)} placeholder="A few lines about this organization…" />
              </label>
              <Show when={getBio()}>
                <p class="muted text-xs">Preview</p>
                <div class="markdown-body card p-3" innerHTML={renderBody(getBio())} />
              </Show>
              <div>
                <button type="button" class="btn px-3 py-1" onClick={save}>save profile</button>
              </div>
              <Show when={getNote()}><p class="text-sm text-amber-700 dark:text-amber-300">{getNote()}</p></Show>
            </div>
            </Show>
            <p class="muted mt-2 text-xs">created <DateTime value={o().created_at} /> · updated <DateTime value={o().updated_at} /></p>
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
      <Show when={!props.canManage()}>
        <p class="muted mb-2 text-sm">read-only — org owner required to add, re-role, or remove members.</p>
      </Show>
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
                        <Show when={props.canManage()} fallback={<span class="text-sm">{row.role}</span>}>
                        <select
                          class="input"
                          value={row.role}
                          onChange={(e) => setRole(row.principal, e.currentTarget.value)}
                        >
                          <For each={ORG_ROLES}>{(r) => <option value={r}>{r}</option>}</For>
                        </select>
                        </Show>
                      </td>
                      <td>
                        <Show when={props.canManage()}>
                        <button type="button" class="btn px-2 py-1" onClick={() => remove(row.principal)}>remove</button>
                        </Show>
                      </td>
                    </tr>
                  )}
                </For>
              </tbody>
            </table>
          </div>
        )}
      </Show>
      <Show when={props.canManage()}>
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
      </Show>
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
      <Show when={!props.canManage()}>
        <p class="muted mb-2 text-sm">read-only — org owner required to create teams or edit membership.</p>
      </Show>
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
                      <Show when={props.canManage()}>
                      <button type="button" class="btn ml-auto px-2 py-1" onClick={() => removeTeam(t.slug)}>delete team</button>
                      </Show>
                    </div>
                    <ul class="mt-2 flex flex-col gap-1">
                      <For each={t.members ?? []}>
                        {(m) => (
                          <li class="flex items-center gap-2 text-sm">
                            <code class="font-mono text-xs">{m}</code>
                            <Show when={props.canManage()}>
                            <button type="button" class="btn px-2 py-0.5" onClick={() => removeMember(t.slug, m)}>remove</button>
                            </Show>
                          </li>
                        )}
                      </For>
                    </ul>
                    <Show when={props.canManage()}>
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
                    </Show>
                  </div>
                )}
              </For>
            </div>
          </Show>
        )}
      </Show>
      <Show when={props.canManage()}>
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
      </Show>
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
      <Show when={!props.canManage()}>
        <p class="muted mb-2 text-sm">read-only — org owner required to invite or cancel.</p>
      </Show>
      <Show when={getInvs()} fallback={<p class="muted">loading…</p>}>
        {(list) => (
          <Show when={(list() ?? []).length > 0} fallback={<p class="muted text-sm">no pending invitations.</p>}>
            <div class="overflow-x-auto">
              <table class="data-table">
                <thead><tr><th>subject</th><th>role</th><th>invited by</th><th>expires</th><th><span class="sr-only">actions</span></th></tr></thead>
                <tbody>
                  <For each={list() ?? []}>
                    {(inv) => (
                      <tr>
                        <td><code class="font-mono text-xs">{inv.subject}</code></td>
                        <td>{inv.role}</td>
                        <td><code class="font-mono text-xs">{inv.invited_by}</code></td>
                        {/* Forgejo #362: expiry served on the row; expired
                            reads as expired before accept fails closed
                            (the invitee sees the chip in /invitations and
                            the accept button disabled). Cancel stays. */}
                        <td>
                          <Show when={inv.expires_at} fallback={<span class="muted">—</span>}>
                            <Show when={isInviteExpired(inv)} fallback={<DateTime value={inv.expires_at} />}>
                              <span class="chip-closed">expired</span>
                            </Show>
                          </Show>
                        </td>
                        <td><Show when={props.canManage()}><button type="button" class="btn px-2 py-1" onClick={() => cancel(inv.id)}>cancel</button></Show></td>
                      </tr>
                    )}
                  </For>
                </tbody>
              </table>
            </div>
          </Show>
        )}
      </Show>
      <Show when={props.canManage()}>
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
      </Show>
      <Show when={getLink()}>
        {/* Forgejo #362: the create endpoints return an API accept path —
            share the inbox deep link (the invitee previews/accepts at
            /invitations) and keep the API path as the fallback. */}
        <p class="mt-2 text-sm">share link: <code class="font-mono text-xs">{invitePageLink(getLink()) || getLink()}</code></p>
      </Show>
      <Show when={getNote()}><p class="mt-2 text-sm text-amber-700 dark:text-amber-300">{getNote()}</p></Show>
    </section>
  );
}

// DangerTab is the owner-only Danger Zone (Forgejo #358): delete-org
// with the typed-confirm idiom from Settings.jsx. Non-owners never see
// the tab (canManage is server-authoritative via can_edit); the server
// still gates (owner-only, 409 while the org owns repos — the message
// names transfer, which now exists, and surfaces verbatim in the
// confirm's error line).
function DangerTab(props) {
  const navigate = useNavigate();
  const org = props.org;

  async function deleteOrg() {
    // No reportError here: the throw lands in DangerConfirm's
    // plain-text error line (tray + inline would surface it twice).
    await repos.orgs.delete(org);
    invalidate(`org:${org}`);
    invalidate(`profile:${org}`);
    invalidate("owners");
    navigate("/");
  }

  return (
    <section class="card border-red-500/60 p-4" aria-label="Danger Zone">
      <h3 class="mb-1 font-semibold text-red-700 dark:text-red-400">Danger Zone</h3>
      <p class="muted mb-3 text-sm">Irreversible actions. Each requires typing the organization name.</p>
      <div class="rounded border border-red-500/40 p-3">
        <h4 class="font-medium text-red-700 dark:text-red-400">Delete Organization</h4>
        <p class="mt-1 text-sm text-zinc-600 dark:text-zinc-300">
          Permanently deletes this organization, its roster, teams, and pending
          invitations. Repos owned by the organization block deletion — transfer
          them to another owner (repo settings → Danger Zone) or delete them first.
          This cannot be undone.
        </p>
        <DangerConfirm
          expected={org}
          confirmLabel="Delete this organization"
          onConfirm={deleteOrg}
        />
      </div>
    </section>
  );
}

export default function Org() {
  const params = useParams();
  const org = () => (params.org ?? "").toLowerCase();
  const [getTab, setTab] = createSignal("Profile");
  // Forgejo #348: owner-only management, discoverable. canManage keys on
  // the org-slug owner profile's can_edit (server-authoritative: org
  // owner or host admin via the OwnerEditor seam — the same gate as the
  // "Manage organization" link on /:org). Non-owners get read-only tabs
  // instead of 403 forms; the server still 403s mutations.
  const [getProfile] = useData(
    () => `profile:${org()}`,
    () => repos.owners.profile(org()).catch(() => null)
  );
  // Forgejo #359: the settings header shows the org avatar (same
  // `org:{org}` cache entry the ProfileTab edits — one fetch, shared).
  const [getOrgDoc] = useData(
    () => `org:${org()}`,
    () => repos.orgs.get(org()).catch(() => null)
  );
  const canManage = () => !!getProfile()?.can_edit;
  // Forgejo #358: owners get the Danger Zone tab; non-owners see nothing
  // (server still gates the delete).
  const tabs = () => (canManage() ? [...TABS, "Danger"] : TABS);

  return (
    <div class="mx-auto max-w-6xl px-4 py-4">
      <h2 class="mb-1 flex items-center gap-2 text-lg font-semibold">
        <OrgAvatar org={org()} doc={getOrgDoc} size={32} />
        <span><span class="muted font-normal">org</span> {org()}</span>
      </h2>
      <Show when={getProfile() && !canManage()}>
        <p class="muted mb-3 text-sm">read-only — org owner required to make changes.</p>
      </Show>
      <nav class="subtabs mb-4 flex flex-wrap gap-1.5" aria-label="organization sections">
        <For each={tabs()}>
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
        <Show when={getTab() === "Profile"}><ProfileTab org={org()} canManage={canManage} /></Show>
        <Show when={getTab() === "Members"}><MembersTab org={org()} canManage={canManage} /></Show>
        <Show when={getTab() === "Teams"}><TeamsTab org={org()} canManage={canManage} /></Show>
        <Show when={getTab() === "Invitations"}><InvitesTab org={org()} canManage={canManage} /></Show>
        <Show when={getTab() === "Danger"}>
          <Show when={canManage()}><DangerTab org={org()} /></Show>
        </Show>
      </div>
    </div>
  );
}
