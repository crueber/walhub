// web/src/pages/New.jsx — route "/new" (Forgejo #210): explicit create-repo
// placeholder form → 201 navigates to /{o}/{r} (placeholder view).
// Solid signals only (D-WEB-6); every call through the SDK (dogfood rule);
// dark + light via dark: variants; 409-exists and validation 400s render
// inline (expected control flow ≠ reportError — the Ticket-1
// tolerateMissing discipline extended).

import { createSignal, Show, For, onCleanup } from "solid-js";
import { A, useNavigate, useSearchParams } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { validateRepoName, isUiRouteCollision } from "../../sdk/src/create.js";
import { MIRROR_PRESETS, DEFAULT_MIRROR_PRESET, validateMirrorCreate } from "../lib/mirror.js";
import { allowedOwners } from "../lib/orgs.js";
import { invalidate } from "../lib/data.js";

function winnerUrl(msg) {
  const m = String(msg ?? "").match(/https?:\/\/[^\s"']+/);
  return m ? m[0] : "";
}

export default function New() {
  const navigate = useNavigate();
  const [search] = useSearchParams();
  const [getOwner, setOwner] = createSignal(search.owner ?? "");
  const [getName, setName] = createSignal("");
  const [getFormat, setFormat] = createSignal("sha1");
  const [getVisibility, setVisibility] = createSignal("public");
  // Forgejo #240: mirror-from-URL mode (same owner/name fields, plus the
  // upstream source + schedule preset → POST /api/v1/repos/mirrors).
  const [getMode, setMode] = createSignal("empty"); // empty | mirror
  const [getSource, setSource] = createSignal("");
  const [getSchedule, setSchedule] = createSignal(DEFAULT_MIRROR_PRESET);
  const [getErr, setErr] = createSignal("");
  const [getWinner, setWinner] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);
  const [getMe, setMe] = createSignal(null);
  // Forgejo #346: the owner is bounded to self + member orgs (the server
  // admits exactly that set, plus host admins via the API — the UI cannot
  // see the admin flag on me, so admins needing a foreign namespace use
  // the API directly). null = still resolving.
  const [getOwners, setOwners] = createSignal(null);
  let ctrl = null;
  onCleanup(() => {
    if (ctrl) ctrl.abort();
  });

  repos
    .me()
    .then(async (me) => {
      setMe(me);
      const self = me?.principal && !me.anonymous ? me.principal : "";
      if (!getOwner() && self) setOwner(self);
      if (!self) {
        setOwners([]);
        return;
      }
      const opts = await allowedOwners(repos, self).catch(() => [self]);
      setOwners(opts);
      // Clamp a ?owner= preset (or stale value) to the admitted set — the
      // server would 403 anything else with the allowed owners named.
      if (!opts.includes(getOwner())) setOwner(self);
    })
    .catch(() => {
      setMe({ anonymous: true });
      setOwners([]);
    });
  const anonymous = () => getMe()?.anonymous !== false && !getMe()?.principal;
  const noWrite = () => getMe() != null && getMe().write === false;

  const fieldError = () => validateRepoName(getOwner(), getName()).error ?? "";

  // Mirror-mode validation rides the shared lib rule (server re-validates).
  const mirrorError = () =>
    getMode() === "mirror"
      ? validateMirrorCreate({ sourceUrl: getSource(), owner: getOwner(), name: getName(), schedule: getSchedule() }).error ?? ""
      : "";

  const submit = async (e) => {
    e.preventDefault();
    if (getBusy()) return;
    const v = validateRepoName(getOwner(), getName());
    if (v.error) {
      setErr(v.error);
      setWinner("");
      return;
    }
    if (getMode() === "mirror") {
      const mv = validateMirrorCreate({ sourceUrl: getSource(), owner: getOwner(), name: getName(), schedule: getSchedule() });
      if (mv.error) {
        setErr(mv.error);
        setWinner("");
        return;
      }
    }
    setBusy(true);
    setErr("");
    setWinner("");
    ctrl?.abort();
    ctrl = new AbortController();
    const signal = ctrl.signal;
    try {
      if (getMode() === "mirror") {
        // Pull-only mirror: the first sync fires async server-side.
        const res = await repos.mirrors.create(
          {
            source_url: getSource().trim(),
            owner: v.owner,
            name: v.name,
            schedule: getSchedule(),
          },
          { signal },
        );
        const full = res?.target ?? `${v.owner}/${v.name}`;
        invalidate("owners");
        invalidate(`repos:${v.owner}`);
        navigate(`/${full}`);
        return;
      }
      const res = await repos.repos.create(
        {
          owner: v.owner,
          name: v.name,
          object_format: getFormat() || undefined,
          visibility: getVisibility(),
        },
        { signal },
      );
      const full = res?.full_name ?? `${v.owner}/${v.name}`;
      invalidate("owners");
      invalidate(`repos:${v.owner}`);
      navigate(`/${full}`);
    } catch (err) {
      if (err?.status === 499 || signal.aborted) return; // navigated away
      if (err?.status === 409) {
        // 409-exists: plain-text body carries the winner URL (B3) — link
        // the squatter inline, never a tray.
        const msg = String(err?.message ?? "repository already exists");
        setErr(msg);
        const u = winnerUrl(msg);
        try {
          const path = u ? new URL(u).pathname : "";
          setWinner(path || "");
        } catch {
          setWinner("");
        }
        return;
      }
      // Validation 400s render inline; anything else stays inline too (the
      // form owns its errors — no tray on expected outcomes).
      setErr(String(err?.message ?? err ?? "create failed"));
      setWinner("");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="new-page grid max-w-2xl gap-4">
      <h2 class="text-xl font-semibold">New repository</h2>
      <p class="muted text-sm">
        Reserve a name and get push instructions. The first push adopts the
        placeholder — nothing to approve, never a conflict.
      </p>
      <form class="card grid gap-3 p-4" onSubmit={submit} aria-label="New repository">
        <div class="flex flex-wrap gap-4" role="radiogroup" aria-label="Repository kind">
          <label class="flex items-center gap-1 text-sm">
            <input type="radio" name="kind" checked={getMode() === "empty"} onChange={() => setMode("empty")} />
            empty repository
          </label>
          <label class="flex items-center gap-1 text-sm">
            <input type="radio" name="kind" checked={getMode() === "mirror"} onChange={() => setMode("mirror")} />
            mirror from URL <span class="muted">(pull-only, scheduled syncs)</span>
          </label>
        </div>
        <div class="grid grid-cols-2 gap-3">
          <label class="grid gap-1">
            <span class="text-sm font-medium">Owner</span>
            <Show
              when={getOwners() !== null}
              fallback={
                <select class="input font-mono" disabled aria-label="Owner">
                  <option>{getOwner() || "…"}</option>
                </select>
              }
            >
              <select
                class="input font-mono"
                value={getOwner()}
                onChange={(e) => setOwner(e.currentTarget.value)}
                aria-label="Owner"
              >
                <For each={getOwners() ?? []}>{(o) => <option value={o}>{o}</option>}</For>
              </select>
            </Show>
            <span class="muted text-xs">you and your orgs only</span>
          </label>
          <label class="grid gap-1">
            <span class="text-sm font-medium">Name</span>
            <input
              class="input font-mono"
              value={getName()}
              onInput={(e) => setName(e.currentTarget.value.trim())}
              placeholder="newthing"
              autocomplete="off"
              spellcheck={false}
              aria-label="Name"
            />
          </label>
        </div>
        <Show when={getMode() === "mirror"}>
          <label class="grid gap-1">
            <span class="text-sm font-medium">Source URL</span>
            <input
              class="input font-mono"
              value={getSource()}
              onInput={(e) => setSource(e.currentTarget.value)}
              placeholder="https://github.com/acme/upstream.git"
              autocomplete="off"
              spellcheck={false}
              aria-label="Source URL"
            />
          </label>
          <label class="grid gap-1">
            <span class="text-sm font-medium">Sync schedule</span>
            <select class="input w-auto" value={getSchedule()} onChange={(e) => setSchedule(e.currentTarget.value)} aria-label="Sync schedule">
              <For each={MIRROR_PRESETS}>{(p) => <option value={p.id}>{p.label}</option>}</For>
            </select>
          </label>
          <p class="muted text-xs">
            Mirrors are pull-only: pushes are rejected for everyone, and the upstream
            syncs on the schedule. The first sync starts immediately.
          </p>
        </Show>
        <Show when={fieldError() && getName()}>
          <p class="text-xs text-red-700 dark:text-red-400">{fieldError()}</p>
        </Show>
        <Show when={getMode() === "mirror" && mirrorError()}>
          <p class="text-xs text-red-700 dark:text-red-400">{mirrorError()}</p>
        </Show>
        <Show when={!fieldError() && isUiRouteCollision(getOwner()) && getOwner()}>
          <p class="text-xs text-amber-700 dark:text-amber-400">
            warning: owner name collides with a UI route — the /:owner page will misroute (git/API unaffected)
          </p>
        </Show>
        <Show when={getMode() === "empty"}>
          <div class="flex flex-wrap items-center gap-4">
            <label class="flex items-center gap-1 text-sm">
              <span>format</span>
              <select class="input w-auto" value={getFormat()} onChange={(e) => setFormat(e.currentTarget.value)} aria-label="Object format">
                <option value="sha1">sha1</option>
                <option value="sha256">sha256</option>
              </select>
            </label>
            <label class="flex items-center gap-1 text-sm">
              <span>visibility</span>
              <select class="input w-auto" value={getVisibility()} onChange={(e) => setVisibility(e.currentTarget.value)} aria-label="Visibility">
                <option value="public">public — anyone may read</option>
                <option value="authenticated">private — logged in only</option>
                <option value="private">private — owner/org only</option>
              </select>
            </label>
          </div>
        </Show>
        <Show when={anonymous() || noWrite()}>
          <p class="text-xs text-amber-700 dark:text-amber-400">
            You are not signed in as a writer — the server will refuse the create (401/403). Sign in first.
          </p>
        </Show>
        <Show when={getErr()}>
          <div class="rounded-lg border border-red-300 bg-red-50 p-3 text-sm text-red-800 dark:border-red-900 dark:bg-red-950/60 dark:text-red-200">
            {getErr()}{" "}
            <Show when={getWinner()}>
              <A class="hover:underline" href={getWinner()}>
                take me there →
              </A>
            </Show>
          </div>
        </Show>
        <div class="flex gap-2">
          <button
            type="submit"
            class="btn primary px-3 py-1"
            disabled={getBusy() || !!fieldError() || !!mirrorError() || !getOwner() || !getName() || (getMode() === "mirror" && !getSource().trim())}
          >
            {getBusy() ? "creating…" : getMode() === "mirror" ? "create mirror" : "create repository"}
          </button>
        </div>
      </form>
    </div>
  );
}
