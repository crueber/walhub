// web/src/pages/Repos.jsx — route "/:owner": the owner's profile + repositories.
// The profile header (Forgejo #234) shows the display name (owner slug when
// unset), location, IANA timezone, and the markdown bio rendered through the
// shared pipeline (renderBody: marked GFM + DOMPurify — the same gate as
// README tabs and thread bodies). The "Edit profile" affordance renders only
// when the server says so (`can_edit`: host admin, name-matched principal,
// or org-owner role — the client never decides); the form (display name /
// location / timezone picker from Intl.supportedValuesOf, never free text /
// markdown bio with live preview) saves through the SDK and invalidates the
// `profile:{owner}` cache entry so the page reflects the update without a
// full reload (the Access-tab save→invalidate shape).
//
// Rows arrive pre-ordered by most recent commit (Forgejo #247: the detailed
// listing, `sort=activity&order=desc`, stabilized by orderByActivity — the
// same ordering as `/explore`, shared cache key). Star counts ride the shared
// `social:{o}/{r}` cache entries (<StarCount>, lib/stars.js) and last-active
// stamps render from the listing rows (<ActivityStamp at/empty props>, no
// per-row fetch), so rows fetched on `/` are reused here. The shared
// <RepoRow> (link + mirror badge + star count + last-active stamp) renders in a responsive
// two-column grid, one column on narrow widths — the owners page builds on
// the same component.

import repos from "../../sdk/src/index.js";
import { createSignal, For, Show } from "solid-js";
import { useParams, A } from "@solidjs/router";
import { useData, invalidate, reportError } from "../lib/data.js";
import { orderByActivity } from "../lib/owners.js";
import { timeZones } from "../lib/timezone.js";
import {
  emptyProfile,
  normalizeProfile,
  profileSaveBody,
} from "../lib/profile.js";
import { renderBody } from "../lib/render-md.js";
import { OrgAvatar } from "./Org.jsx";
import { mirrorRowBadge } from "../lib/mirror.js";
import { visibilityBadge } from "../lib/visibility.js";
import StarCount from "../components/StarCount.jsx";
import ActivityStamp from "../components/ActivityStamp.jsx";

/** One repo row: link + visibility/mirror badges + star count + last-active stamp. Shared with `/`.
 *  Issue #235, explicitly descoped: rows ride the detailed owners listing,
 *  so there is no per-repo summary in hand — showing descriptions here would
 *  cost one summary fetch per row (N round trips for N repos). The repo
 *  header remains the description surface. `at`/`empty` carry the listing's
 *  activity so the stamp renders without a fetch (Forgejo #247).
 *  `mirror`/`mirrorUpstream` carry the listing's mirror flag (Forgejo #281:
 *  same payload, no extra fetch) so mirror rows render the badge with an
 *  accessible label naming the upstream when known.
 *  `visibility` carries the listing row's visibility (Forgejo #345: same
 *  payload, no extra fetch); unknown renders no badge. */
export function RepoRow(props) {
  const full = () => `${props.owner}/${props.name}`;
  const badge = () => mirrorRowBadge({ mirror: props.mirror, upstream: props.mirrorUpstream });
  const vis = () => visibilityBadge({ visibility: props.visibility });
  return (
    <li class="flex flex-wrap items-baseline gap-x-1.5">
      <A
        class="text-emerald-700 hover:underline dark:text-emerald-400"
        href={`/${full()}`}
      >
        {full()}
      </A>
      <Show when={vis().show}>
        <span class="pill visibility-badge" role="img" title={vis().title} aria-label={vis().title}>
          {vis().label}
        </span>
      </Show>
      <Show when={badge().show}>
        <span class="pill mirror-badge" role="img" title={badge().title} aria-label={badge().title}>
          {badge().label}
        </span>
      </Show>
      <StarCount full={full()} />
      <ActivityStamp full={full()} at={props.at} empty={props.empty} />
    </li>
  );
}

/** Profile edit form: seeded from the fetched doc when opened, saved through
 *  the SDK. props: owner, doc (server profile), onDone(savedDoc|null). */
function ProfileForm(props) {
  const seed = normalizeProfile(props.doc, props.owner);
  const [getName, setName] = createSignal(seed.display_name);
  const [getLoc, setLoc] = createSignal(seed.location);
  const [getTz, setTz] = createSignal(seed.timezone);
  const [getBio, setBio] = createSignal(seed.bio_markdown);
  const [getSaving, setSaving] = createSignal(false);
  const [getNote, setNote] = createSignal("");
  const zones = timeZones();

  const save = async () => {
    setSaving(true);
    setNote("");
    try {
      const next = await repos.owners.updateProfile(
        props.owner,
        profileSaveBody({
          display_name: getName(),
          location: getLoc(),
          timezone: getTz(),
          bio_markdown: getBio(),
        })
      );
      invalidate(`profile:${props.owner}`);
      props.onDone(next);
    } catch (err) {
      reportError(err, `profile:${props.owner}`);
      setNote(err?.message ?? "save failed");
    } finally {
      setSaving(false);
    }
  };

  return (
    <form
      class="card mt-3 space-y-3 p-4"
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
    >
      <div>
        <label class="mb-1 block text-sm font-medium" for="profile-name">
          Display name
        </label>
        <input
          id="profile-name"
          class="input w-full"
          type="text"
          maxlength="200"
          value={getName()}
          onInput={(e) => setName(e.currentTarget.value)}
          placeholder={props.owner}
        />
      </div>
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div>
          <label class="mb-1 block text-sm font-medium" for="profile-loc">
            Location
          </label>
          <input
            id="profile-loc"
            class="input w-full"
            type="text"
            maxlength="200"
            value={getLoc()}
            onInput={(e) => setLoc(e.currentTarget.value)}
            placeholder="City, Country"
          />
        </div>
        <div>
          <label class="mb-1 block text-sm font-medium" for="profile-tz">
            Timezone
          </label>
          <select
            id="profile-tz"
            class="input w-full"
            value={getTz()}
            onChange={(e) => setTz(e.currentTarget.value)}
          >
            <option value="">— unset —</option>
            <For each={zones}>{(z) => <option value={z}>{z}</option>}</For>
          </select>
        </div>
      </div>
      <div>
        <label class="mb-1 block text-sm font-medium" for="profile-bio">
          Bio (markdown)
        </label>
        <textarea
          id="profile-bio"
          class="input w-full font-mono text-sm"
          rows="6"
          value={getBio()}
          onInput={(e) => setBio(e.currentTarget.value)}
          placeholder="A few lines about this owner…"
        />
        <Show when={getBio()}>
          <p class="muted mb-1 mt-3 text-xs">Preview</p>
          <div class="markdown-body card p-3" innerHTML={renderBody(getBio())} />
        </Show>
      </div>
      <Show when={getNote()}>
        <p class="text-sm text-red-600 dark:text-red-400">{getNote()}</p>
      </Show>
      <div class="flex gap-2">
        <button class="btn primary px-3 py-1" type="submit" disabled={getSaving()}>
          {getSaving() ? "Saving…" : "Save profile"}
        </button>
        <button
          class="btn px-3 py-1"
          type="button"
          onClick={() => props.onDone(null)}
        >
          Cancel
        </button>
      </div>
    </form>
  );
}

export default function Repos() {
  const params = useParams();
  const owner = () => params.owner;
  const [getDoc] = useData(
    () => `repos:${owner()}`,
    () => repos.owners.detailed(owner(), { sort: "activity", order: "desc" })
  );
  const [getProfile] = useData(
    () => `profile:${owner()}`,
    () => repos.owners.profile(owner()).catch(() => emptyProfile(owner()))
  );
  // Forgejo #348: the public org profile. orgs.get resolves null for
  // non-org owners (404 → null, never a tray), so this is one extra GET
  // exactly when the owner MIGHT be an org — users render unchanged.
  // Private repos stay hidden by the server (#345 visibility filtering
  // on the detailed listing below), never by the client.
  const [getOrg] = useData(
    () => `org:${owner()}`,
    () => repos.orgs.get(owner()).catch(() => null)
  );
  // Forgejo #376: the user avatar. users.get resolves null for unknown
  // owners and orgs (404 → null, never a tray) — one extra cached GET
  // that doubles as the profile-consumption surface (the navbar is the
  // other, via me().avatar_url). Gated on the user profile's
  // avatar_content_type — the client never probes the bytes.
  const [getUser] = useData(
    () => `user:${owner()}`,
    () => repos.users.get(owner()).catch(() => null)
  );
  const userSrc = () => {
    const u = getUser();
    if (!u?.avatar_content_type) return null;
    return repos.users.avatar.url(owner(), u.avatar_updated_at);
  };
  const [getEditing, setEditing] = createSignal(false);
  // Writers-only New button (mirrors require_write so the button never
  // promises what POST /api/v1/repos refuses): hidden for anonymous
  // without write. One me() fetch, no tray (missing = hidden).
  const [getMe] = useData("me", () => repos.me().catch(() => null));
  const canWrite = () => {
    const me = getMe();
    if (!me) return false;
    if (me.anonymous) return false;
    return me.write !== false;
  };
  // Forgejo #376: avatar self-service — the viewer is the owner (the
  // server re-checks self-or-admin; the client never decides).
  const isSelf = () => {
    const me = getMe();
    return !!me && !me.anonymous && (me.principal ?? "").toLowerCase() === owner().toLowerCase();
  };
  const [getAvatarNote, setAvatarNote] = createSignal("");
  const refreshAvatar = () => {
    invalidate(`user:${owner()}`);
    invalidate("me"); // the navbar renders the same avatar
  };
  const regenerateAvatar = async () => {
    setAvatarNote("");
    try {
      await repos.users.avatar.regenerate(owner());
      refreshAvatar();
    } catch (err) {
      reportError(err, `user:${owner()}`);
      setAvatarNote(String(err?.message ?? err));
    }
  };
  const removeAvatar = async () => {
    setAvatarNote("");
    try {
      await repos.users.avatar.remove(owner());
      refreshAvatar();
    } catch (err) {
      reportError(err, `user:${owner()}`);
      setAvatarNote(String(err?.message ?? err));
    }
  };
  const profile = () => getProfile() ?? emptyProfile(owner());
  const displayName = () => profile().display_name || owner();
  // Forgejo #348: org landing. The org header (badge + org display name
  // + description) renders above the repo list when the owner is an org,
  // mirroring #234's owner-profile shape. "Manage" appears only when the
  // server says the viewer may edit (profile can_edit: org owner or host
  // admin via the OwnerEditor seam — the client never decides).
  const isOrg = () => getOrg() != null;
  const orgName = () => getOrg()?.display_name || owner();
  const canManage = () => isOrg() && !!getProfile()?.can_edit;
  return (
    <div class="repos-page">
      {/* Forgejo #413 (#403 follow-up): the New-repository CTA left both
          header spots — the grouped action row under the bio (user) and
          the org title row (org) — for a Repositories toolbar (heading
          row, CTA right-anchored, flex-wrap at 390px per #273-#278), so
          the header carries only identity/profile actions and the create
          CTA sits with the list it populates. The user action row keeps
          its wrapper with just "Edit profile" (stable wrap rhythm);
          the org title row drops its justify-between wrapper (single
          child); the header divider (pb-6 border-b, #403) stays. All
          gating unchanged (New: canWrite; Edit: server can_edit;
          Regenerate/Remove: self-only; #376 cache invalidation) —
          layout only. */}
      <Show when={!isOrg()}>
        <div class="profile-header flex flex-col-reverse gap-4 border-b border-zinc-200 pb-6 sm:flex-row sm:items-start sm:justify-between dark:border-zinc-700">
          <div class="min-w-0 flex-1">
            <h1 class="text-2xl font-semibold">{displayName()}</h1>
            {/* The org block below already renders @{owner}; the
                user-profile handle renders only for non-org owners
                (no double handle). */}
            <Show when={profile().display_name}>
              <p class="muted mt-0.5 text-sm">@{owner()}</p>
            </Show>
            <Show when={profile().location || profile().timezone}>
              <p class="muted mt-1 text-sm">
                {[profile().location, profile().timezone].filter(Boolean).join(" · ")}
              </p>
            </Show>
            <Show when={profile().bio_markdown}>
              <div
                class="markdown-body mt-3"
                innerHTML={renderBody(profile().bio_markdown)}
              />
            </Show>
            {/* Forgejo #413: the New-repository CTA lives in the
                Repositories toolbar below — this row carries only the
                profile action. */}
            <div class="mt-3 flex flex-wrap gap-2">
              <Show when={getProfile()?.can_edit && !getEditing()}>
                <button class="btn px-3 py-1" type="button" onClick={() => setEditing(true)}>
                  Edit profile
                </button>
              </Show>
            </div>
          </div>
          {/* Forgejo #376: avatar self-service for the owner's own page.
              The column renders when there is an avatar to show OR the
              viewer can act (self without an avatar still gets
              Regenerate to opt back in); the actions sit with the
              avatar they act on. Regenerate installs a fresh
              deterministic render (and opts back in); remove deletes
              the avatar and opts out of auto-generation until the
              next regenerate. Non-org only — org avatars live in org
              settings. */}
          <Show when={userSrc() || isSelf()}>
            <div class="profile-avatar flex shrink-0 flex-row flex-wrap items-center gap-3 sm:flex-col sm:items-center">
              <Show when={userSrc()}>
                <img
                  src={userSrc()}
                  alt=""
                  width={96}
                  height={96}
                  class="h-24 w-24 rounded-full ring-1 ring-zinc-300 dark:ring-zinc-600"
                />
              </Show>
              <Show when={isSelf()}>
                <div class="flex flex-row flex-wrap gap-2 text-sm sm:flex-col sm:items-center">
                  <button class="btn px-3 py-1" type="button" onClick={regenerateAvatar}>
                    Regenerate avatar
                  </button>
                  <Show when={userSrc()}>
                    <button class="btn px-3 py-1" type="button" onClick={removeAvatar}>
                      Remove avatar
                    </button>
                  </Show>
                  <Show when={getAvatarNote()}>
                    <span class="muted">{getAvatarNote()}</span>
                  </Show>
                </div>
              </Show>
            </div>
          </Show>
        </div>
        <Show when={getEditing() && getProfile()?.can_edit}>
          <ProfileForm
            owner={owner()}
            doc={getProfile()}
            onDone={(saved) => {
              setEditing(false);
              if (saved) invalidate(`profile:${owner()}`);
            }}
          />
        </Show>
      </Show>
      {/* Forgejo #359: org landing — the org header (avatar + badge +
          org display name) and the org doc fields (description/location/
          timezone/bio) render from the org doc; the user-profile block
          above never renders for orgs. Forgejo #413: the New-repository
          CTA left the title row for the shared Repositories toolbar
          below, so the row drops its justify-between wrapper. */}
      <Show when={isOrg()}>
      <h2 class="mb-1 flex items-center gap-2 text-xl font-semibold">
          {/* Forgejo #359: the org avatar renders from the org doc's
              pointer (no byte probing; hides itself on 404). */}
          <OrgAvatar org={owner()} doc={getOrg} size={36} />
          {orgName()}
          <span class="pill org-badge ml-2 align-middle text-xs font-normal" role="img" title="organization" aria-label="organization">
            org
          </span>
        </h2>
      <p class="muted text-sm">@{owner()}</p>
      <Show when={getOrg()?.description}>
        <p class="mt-1 text-sm">{getOrg().description}</p>
      </Show>
      {/* Forgejo #359: the org profile fields (location/timezone/bio)
          render from the org doc — the owner-profile block above reads
          the separate owner-slug profile, not the org. */}
      <Show when={getOrg()?.location || getOrg()?.timezone}>
        <p class="muted mt-1 text-sm">
          {[getOrg()?.location, getOrg()?.timezone].filter(Boolean).join(" · ")}
        </p>
      </Show>
      <Show when={getOrg()?.bio_markdown}>
        <div
          class="markdown-body mt-3"
          innerHTML={renderBody(getOrg().bio_markdown)}
        />
      </Show>
      <Show when={canManage()}>
        <p class="mt-3">
          <A class="btn px-3 py-1" href={`/${owner()}/settings`}>
            Manage organization
          </A>
        </p>
      </Show>
      </Show>
      {/* Forgejo #413: the Repositories toolbar — the section heading
          with the New-repository CTA right-anchored (the same flex
          items-center justify-between title-row shape the org header
          used; flex-wrap gap-2 per #273-#278 so 390px wraps without
          overflow). The single surface owning repo creation for both
          user and org variants; gate and href unchanged. */}
      <div class="repos-toolbar mb-2 mt-6 flex flex-wrap items-center justify-between gap-2">
        <h3 class="text-base font-semibold">Repositories</h3>
        <Show when={canWrite()}>
          <A class="btn primary px-3 py-1" href={`/new?owner=${encodeURIComponent(owner())}`}>
            New repository
          </A>
        </Show>
      </div>
      <Show when={getDoc()} fallback={<p class="muted">loading…</p>}>
        {(doc) => {
          const rows = orderByActivity(doc().repos);
          return (
            <>
              <p class="muted mb-4">
                {rows.length} repositor{rows.length === 1 ? "y" : "ies"}
              </p>
              <Show
                when={rows.length > 0}
                fallback={<p class="muted">nothing under {owner()} yet</p>}
              >
                <ul class="grid grid-cols-1 gap-x-6 gap-y-1 sm:grid-cols-2">
                  <For each={rows}>
                    {(row) => <RepoRow owner={owner()} name={row.name} at={row.last_commit_time} empty={row.size_bytes === 0} mirror={row.mirror} mirrorUpstream={row.mirror_upstream} visibility={row.visibility} />}
                  </For>
                </ul>
              </Show>
            </>
          );
        }}
      </Show>
      <p class="muted mt-4 text-xs">
        <A class="hover:underline" href={`/import?owner=${encodeURIComponent(owner())}`}>import into {owner()}</A>
        {/* Forgejo #348: the settings link is an org-owner affordance
            (same canManage gate as the header button above); user
            namespaces never had an org settings page to link to. */}
        <Show when={canManage()}>
          {' · '}
          <A class="hover:underline" href={`/${owner()}/settings`}>organization settings</A>
        </Show>
      </p>
    </div>
  );
}
