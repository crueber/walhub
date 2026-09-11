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
  const profile = () => getProfile() ?? emptyProfile(owner());
  const displayName = () => profile().display_name || owner();
  return (
    <div class="repos-page">
      <div class="mb-1 flex items-center justify-between">
        <h2 class="text-xl font-semibold">{displayName()}</h2>
        <Show when={canWrite()}>
          <A class="btn primary px-3 py-1" href={`/new?owner=${encodeURIComponent(owner())}`}>
            New repository
          </A>
        </Show>
      </div>
      <Show when={profile().display_name}>
        <p class="muted text-sm">@{owner()}</p>
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
      <Show when={getProfile()?.can_edit && !getEditing()}>
        <p class="mt-3">
          <button class="btn px-3 py-1" type="button" onClick={() => setEditing(true)}>
            Edit profile
          </button>
        </p>
      </Show>
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
      <h3 class="mb-2 mt-6 text-base font-semibold">Repositories</h3>
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
        {' · '}
        <A class="hover:underline" href={`/${owner()}/settings`}>organization settings</A>
      </p>
    </div>
  );
}
