// web/src/pages/Repos.jsx — routes "/:owner" (profile),
// "/:owner/repositories" (the repositories tab, Forgejo #422), and
// "/:owner/organizations" (the organizations tab, Forgejo #430): the owner's
// profile + repositories + org memberships. All three views share one
// two-column profile layout: main content left (identity on the profile
// view, the listing on the repositories tab, the membership list on the
// organizations tab), the right sidebar carrying the vertical OwnerTabs
// first (Forgejo #437, above the avatar) then the gated avatar asides.
// The profile view is the dedicated identity view (no repository listing,
// no membership list — those live on their tabs).
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
import { createSignal, createEffect, For, Show } from "solid-js";
import { useParams, useLocation, A } from "@solidjs/router";
import { useData, invalidate, reportError } from "../lib/data.js";
import { orderByActivity } from "../lib/owners.js";
import { normalizeMemberOrgs } from "../lib/orgs.js";
import { timeZones } from "../lib/timezone.js";
import {
  emptyProfile,
  normalizeProfile,
  profileSaveBody,
} from "../lib/profile.js";
import { renderBody } from "../lib/render-md.js";
import { initAutogrow, growTextarea } from "../lib/autogrow.js";
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

/** Owner tab list (Forgejo #437; supersedes the #435 top strip): Profile ⇄
 *  Repositories ⇄ Organizations as a vertical tab list at the top of the
 *  profile-layout right sidebar, above the avatar. The list is a bordered
 *  box in the sidebar zinc language (`border-zinc-200
 *  dark:border-zinc-700`, the existing text/hover treatment); the active
 *  tab fills its row with the container's selected background (zinc-100 /
 *  dark zinc-800) so it reads connected to the box — a true-tab look, not
 *  a floating underline. Stacked full-width, no scrolling, no wrapping
 *  (the 390px rules are the column's `min-w-0`, not internal scroll).
 *  The list renders outside every gate so all viewers on both variants
 *  reach every owner route. Keep: the active-tab derivation from the
 *  pathname, `aria-current="page"`, the tab-badge repository count on the
 *  Repositories tab (the shared `repos:{owner}` payload — no new fetch),
 *  and the isOrg gating (Organizations hidden on org profiles: member
 *  principals are email spellings, not routable owner slugs per #370).
 *  props: owner, count (number|null), isOrg (bool). */
export function OwnerTabs(props) {
  const loc = useLocation();
  // Forgejo #430: three-way derivation — the organizations tab is active
  // exactly on /{owner}/organizations (the same pathname pattern #422
  // established for /repositories); anything else falls back to profile.
  const active = () =>
    loc.pathname === `/${props.owner}/organizations`
      ? "orgs"
      : loc.pathname === `/${props.owner}/repositories`
        ? "repos"
        : "profile";
  const cls =
    "block w-full rounded-md px-3 py-1.5 text-sm text-zinc-500 hover:bg-zinc-100 hover:text-zinc-900 dark:text-zinc-400 dark:hover:bg-zinc-900 dark:hover:text-zinc-100";
  return (
    <nav
      class="owner-tabs flex w-full flex-col gap-1 rounded-lg border border-zinc-200 bg-white p-1 dark:border-zinc-700 dark:bg-zinc-950"
      aria-label="owner sections"
    >
      <A
        href={`/${props.owner}`}
        class={cls}
        classList={{ "!bg-zinc-100 !font-medium !text-zinc-900 dark:!bg-zinc-800 dark:!text-zinc-100": active() === "profile" }}
        aria-current={active() === "profile" ? "page" : undefined}
      >
        Profile
      </A>
      <A
        href={`/${props.owner}/repositories`}
        class={cls}
        classList={{ "!bg-zinc-100 !font-medium !text-zinc-900 dark:!bg-zinc-800 dark:!text-zinc-100": active() === "repos" }}
        aria-current={active() === "repos" ? "page" : undefined}
      >
        Repositories
        <Show when={props.count != null}>
          <span class="tab-badge" aria-label={`${props.count} repositories`}>{props.count}</span>
        </Show>
      </A>
      {/* Forgejo #430: the Organizations tab — same link treatment,
          active background, and aria-current as the first two tabs. Hidden
          for org profiles (isOrg): #370 email-principal rationale. */}
      <Show when={!props.isOrg}>
        <A
          href={`/${props.owner}/organizations`}
          class={cls}
          classList={{ "!bg-zinc-100 !font-medium !text-zinc-900 dark:!bg-zinc-800 dark:!text-zinc-100": active() === "orgs" }}
          aria-current={active() === "orgs" ? "page" : undefined}
        >
          Organizations
        </A>
      </Show>
    </nav>
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
  // Forgejo #419: bio auto-grow (shared lib/autogrow.js — also used by the
  // org profile form in Org.jsx). The ref callback records the rows="6"
  // height as the floor; the effect refits on every bio change (typing,
  // paste, seeded value) so the caret line stays visible, capped at 50vh.
  let bioRef;

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

  // Refit after mount and on every bio change (the onInput grow above
  // covers keystrokes; this covers the seeded value and programmatic
  // sets — refit is idempotent so both firing is harmless).
  createEffect(() => {
    getBio();
    if (bioRef) growTextarea(bioRef);
  });

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
          ref={(el) => {
            bioRef = el;
            initAutogrow(el);
          }}
          class="input w-full font-mono text-sm"
          rows="6"
          value={getBio()}
          onInput={(e) => {
            setBio(e.currentTarget.value);
            growTextarea(e.currentTarget);
          }}
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

/** The owner page in all three views (Forgejo #422; third view Forgejo
 *  #430; sidebar tabs Forgejo #437, superseding the #435 top strip): one
 *  shared profile-layout grid on every view. The main column swaps per
 *  view — `view="profile"` is the `/:owner` identity page (identity header
 *  only: no grid, no toolbar, no teaser, no membership list),
 *  `view="repos"` is the `/:owner/repositories` tab (the toolbar + grid +
 *  import link moved verbatim), `view="orgs"` is the
 *  `/:owner/organizations` tab (the #423 membership list moved verbatim).
 *  The right column is the same sidebar shell on all three views: the
 *  vertical OwnerTabs first (ungated, so navigation stays one click away
 *  on every owner route), then the gated avatar asides (#421) below. One
 *  component so the header, sidebar, gates, fetches, and cache keys stay
 *  shared by construction — the listing and membership markups are never
 *  forked. */
function OwnerPage(props) {
  const view = () => props.view ?? "profile";
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
  // Forgejo #423: the membership rail. users.orgs resolves the sorted org
  // names over GET /api/v1/users/{principal}/orgs (server-side, one rail —
  // never a per-org roster fan-out); unknown principals answer [] so the
  // section below always has something explicit to render. A bio edit
  // invalidates only `profile:{owner}` — this key is untouched and stays
  // fresh by construction (separate route, separate ETag).
  const [getMemberOrgs] = useData(
    () => `memberorgs:${owner()}`,
    () => repos.users.orgs(owner()).catch(() => [])
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
  // Forgejo #422: the repositories-tab count rides the shared `repos:{owner}`
  // listing payload (the same key /explore shares) — null while loading, so
  // the tab renders first and the badge fills in with zero new fetches.
  const repoCount = () => {
    const d = getDoc();
    return d ? orderByActivity(d.repos).length : null;
  };
  return (
    <div class="repos-page">
      {/* Forgejo #437 (supersedes the #435 strip-first placement): the
          two-column profile layout renders on ALL THREE owner routes.
          The main column swaps per view below; the right column is the
          same sidebar shell on every view — the vertical OwnerTabs first
          (ungated, above the avatar) so tab navigation stays one click
          away on every owner route, then the gated #421 avatar asides. */}
      <div class="profile-layout grid grid-cols-1 gap-6 sm:grid-cols-[minmax(0,1fr)_12rem]">
        <div class="profile-main min-w-0">
          <Show when={view() === "profile"}>
      {/* Forgejo #421 (#413/#403/#395 follow-up): the profile view keeps the
          identity block in the main column — display name/handle/location/
          bio (with the #420 hide-while-editing gate intact), the edit form
          opening in place. Below sm: the grid is one column so the sidebar
          stacks below the main column at 390px with no horizontal overflow
          (#273-#278: min-w-0 columns, no fixed widths beside the avatar);
          DOM order stays main-first so the h1 keeps heading order. The
          Repositories toolbar lives on the repositories tab (#413, #422);
          the membership list lives on the organizations tab (#423, #430).
          All gating byte-identical (Edit: server can_edit;
          Regenerate/Remove: self-only; Manage: canManage; #376 cache
          invalidation) — layout only. */}
          <Show when={!isOrg()}>
            <div class="profile-header border-b border-zinc-200 pb-6 dark:border-zinc-700">
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
              {/* Forgejo #420: the rendered bio hides while the edit
                  form is open — the form's own inline preview is the only
                  rendered surface during editing. onDone restores via
                  setEditing(false); non-editors never set editing. */}
              <Show when={profile().bio_markdown && !getEditing()}>
                <div
                  class="markdown-body mt-3"
                  innerHTML={renderBody(profile().bio_markdown)}
                />
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
            {/* Forgejo #430: the profile view is rail-free — the #423
                membership list moved verbatim to the organizations tab
                (the view="orgs" branch below). Planner's call per the
                issue: no compact rail kept here; the tab always exists
                for user profiles so the memberships stay one click away.
                Org profiles (the isOrg() branch below) never had this
                section: member principals are email spellings, not
                routable owner slugs (#370) — the roster is managed at
                organization settings instead. */}
          </Show>
      {/* Forgejo #359: org landing — the org identity (badge + org
          display name + org doc fields: description/location/timezone/
          bio) renders from the org doc in the main column; the user-
          profile block above never renders for orgs. The org avatar and
          the Manage affordance live in the sidebar (Forgejo #421: same
          sidebar treatment as the user profile). Forgejo #413: the
          New-repository CTA left the title row for the shared
          Repositories toolbar below, so the title row is just the h2. */}
      <Show when={isOrg()}>
      <h2 class="mb-1 flex items-center gap-2 text-xl font-semibold">
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
      </Show>
      </Show>
      {/* Forgejo #422 (#437: this branch renders in the shared main column —
          the toolbar + grid + import footer moved verbatim, no identity
          block). Forgejo #437 deleted the profile-view count teaser: the
          Repositories tab owns the listing now, so the identity
          page keeps identity only. */}
      <Show when={view() === "repos"}>
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
            (same canManage gate as the sidebar button below); user
            namespaces never had an org settings page to link to. */}
        <Show when={canManage()}>
          {' · '}
          <A class="hover:underline" href={`/${owner()}/settings`}>organization settings</A>
        </Show>
      </p>
      </Show>
      {/* Forgejo #430 (#437: like the repositories branch above, this renders
          in the shared main column — the #423 membership list moved verbatim:
          links + explicit empty state + loading fallback, same
          memberorgs:{owner} key, same normalizeMemberOrgs shape; no identity
          block, no repos grid). The branch is ungated by owner variant like
          the repos tab above — the tab itself is user-profiles-only (isOrg hides
          it), so org owners never navigate here from the UI. */}
      <Show when={view() === "orgs"}>
        <section class="orgs-rail mt-6" aria-label="Organizations">
          <h3 class="text-base font-semibold">Organizations</h3>
          <Show when={getMemberOrgs()} fallback={<p class="muted mt-1 text-sm">loading…</p>}>
            {(orgs) => {
              const names = () => normalizeMemberOrgs(orgs());
              return (
                <Show
                  when={names().length > 0}
                  fallback={<p class="muted mt-1 text-sm">No organizations</p>}
                >
                  <ul class="mt-1 flex flex-wrap gap-x-4 gap-y-1">
                    <For each={names()}>
                      {(org) => (
                        <li>
                          <A
                            class="text-emerald-700 hover:underline dark:text-emerald-400"
                            href={`/${org}`}
                          >
                            {org}
                          </A>
                        </li>
                      )}
                    </For>
                  </ul>
                </Show>
              );
            }}
          </Show>
        </section>
      </Show>
        </div>
        {/* Forgejo #437: the sidebar column renders on EVERY view and both
            variants — the vertical OwnerTabs first (an ungated sibling above
            the gated avatar asides, so the tabs render for ALL viewers on
            both user and org profiles; widening the aside gates instead
            would render empty asides for anonymous viewers, so the #421
            gates below stay byte-identical). Tabs-first DOM order puts
            navigation above the avatar on every owner route. The org
            variant gets the same treatment (Profile | Repositories tabs via
            the isOrg prop, org aside below). `min-w-0` + `flex-col` keep
            the 390px stack clean (#273-#278: no fixed widths, no overflow). */}
        <div class="profile-sidebar-col flex min-w-0 flex-col gap-4">
          <OwnerTabs owner={owner()} count={repoCount()} isOrg={isOrg()} />
          {/* Forgejo #421: the owner-action sidebar — avatar below the tabs,
              then an <hr> in the header divider colors, then the owner actions
              grouped in one vertical full-width stack (Edit profile when
              profile.can_edit, Regenerate/Remove avatar when self-only).
              The aside renders when there is an avatar to show, the
              viewer can act (self without an avatar still gets Regenerate
              to opt back in), or the viewer may edit (host admin on an
              avatarless page); each action keeps its own byte-identical
              gate below. Regenerate installs a fresh deterministic render
              (and opts back in); remove deletes the avatar and opts out
              of auto-generation until the next regenerate (Forgejo #376:
              the server re-checks self-or-admin; the client never
              decides). Non-org only — org avatars live in org settings. */}
          <Show when={!isOrg() && (userSrc() || isSelf() || getProfile()?.can_edit)}>
            <aside class="profile-sidebar flex min-w-0 flex-col items-center gap-3" aria-label="Profile actions">
              <Show when={userSrc()}>
                <div class="profile-avatar shrink-0">
                  <img
                    src={userSrc()}
                    alt=""
                    width={96}
                    height={96}
                    class="h-24 w-24 rounded-full ring-1 ring-zinc-300 dark:ring-zinc-600"
                  />
                </div>
              </Show>
              {/* Forgejo #421: the divider renders only beneath a shown
                  avatar — an avatarless but actionable sidebar (self opted
                  out via #376 Remove, or an editor without an avatar) must
                  not open on a stray rule. */}
              <Show when={userSrc() && ((getProfile()?.can_edit && !getEditing()) || isSelf())}>
                <hr class="w-full border-zinc-200 dark:border-zinc-700" />
              </Show>
              <Show when={(getProfile()?.can_edit && !getEditing()) || isSelf()}>
                <div class="profile-actions flex w-full flex-col gap-2">
                  <Show when={getProfile()?.can_edit && !getEditing()}>
                    <button class="btn w-full justify-center px-3 py-1" type="button" onClick={() => setEditing(true)}>
                      Edit profile
                    </button>
                  </Show>
                  <Show when={isSelf()}>
                    <button class="btn w-full justify-center px-3 py-1" type="button" onClick={regenerateAvatar}>
                      Regenerate avatar
                    </button>
                    <Show when={userSrc()}>
                      <button class="btn w-full justify-center px-3 py-1" type="button" onClick={removeAvatar}>
                        Remove avatar
                      </button>
                    </Show>
                    <Show when={getAvatarNote()}>
                      <span class="muted text-center text-sm">{getAvatarNote()}</span>
                    </Show>
                  </Show>
                </div>
              </Show>
            </aside>
          </Show>
          {/* Forgejo #421: the org sidebar — the same treatment as the
              user sidebar (avatar, divider, vertical full-width action
              stack). Renders when the org doc names an avatar or the
              viewer may manage; the avatar still renders from the org
              doc's pointer with no byte probing (Forgejo #359: hides
              itself on 404). */}
          <Show when={isOrg() && (getOrg()?.avatar_content_type || canManage())}>
            <aside class="profile-sidebar flex min-w-0 flex-col items-center gap-3" aria-label="Organization actions">
              <Show when={getOrg()?.avatar_content_type}>
                <div class="profile-avatar shrink-0">
                  <OrgAvatar org={owner()} doc={getOrg} size={96} />
                </div>
              </Show>
              {/* Forgejo #421: same no-orphan-rule treatment as the user
                  sidebar — the divider needs the org avatar above it. */}
              <Show when={getOrg()?.avatar_content_type && canManage()}>
                <hr class="w-full border-zinc-200 dark:border-zinc-700" />
              </Show>
              <Show when={canManage()}>
                <div class="profile-actions flex w-full flex-col gap-2">
                  <A class="btn w-full justify-center px-3 py-1" href={`/${owner()}/settings`}>
                    Manage organization
                  </A>
                </div>
              </Show>
            </aside>
          </Show>
        </div>
      </div>
    </div>
  );
}

/** Route "/:owner": the profile view (identity + sidebar tabs, no teaser). */
export default function Repos() {
  return <OwnerPage view="profile" />;
}

/** Route "/:owner/repositories" (Forgejo #422): the repositories tab (the
 *  toolbar/grid/import listing moved verbatim, sidebar tabs shared with the
 *  profile view). No API change —
 *  the listing rides the shared `repos:{owner}` key, and the server already
 *  serves the SPA shell on the two-segment shape (repoPageGated), so this
 *  stays client-only. Reservation note: a repo literally named
 *  "repositories" loses its UI page (the static route wins client-side);
 *  its git/API paths are unaffected — the same class as /orgs/new. */
export function OwnerRepositories() {
  return <OwnerPage view="repos" />;
}

/** Route "/:owner/organizations" (Forgejo #430): the organizations tab (the
 *  #423 membership list moved verbatim from the profile rail, sidebar tabs
 *  shared with the profile view) —
 *  links to /:org, explicit "No organizations" empty state, loading state
 *  preserved). No API change — the list rides the shared `memberorgs:
 *  {owner}` key, and the server already serves the SPA shell on the
 *  two-segment shape (repoPageGated), so this stays client-only.
 *  Reservation note: an org literally named "organizations" loses its UI
 *  page (the static route wins client-side); its git/API paths are
 *  unaffected — the same class as /repositories (#422) and /orgs/new. This
 *  absorbs the remainder of #423 (closed): #423's backend membership
 *  endpoint stays; only its presentation surface moves to this tab. */
export function OwnerOrganizations() {
  return <OwnerPage view="orgs" />;
}
