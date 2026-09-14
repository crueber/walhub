// web/src/pages/New.jsx — route "/new" (Forgejo #210): explicit create-repo
// placeholder form → 201 navigates to /{o}/{r} (placeholder view).
// Forgejo #487: reserve/push-only — the mirror-from-URL mode is gone from
// this page (Import is the sole mirror path); this form creates an empty
// placeholder and nothing else.
// Solid signals only (D-WEB-6); every call through the SDK (dogfood rule);
// dark + light via dark: variants; 409-exists and validation 400s render
// inline (expected control flow ≠ reportError — the Ticket-1
// tolerateMissing discipline extended).
// Form-page pattern (Forgejo #479 standing rule — reference: ReleaseNew.jsx):
// centered mx-auto max-w-2xl column, one h2 + one muted intro, single card
// form, label.grid.gap-1 fields with id + aria-describedby help, collapsing
// collapsing
// grid-cols-1 sm:… rows (never a bare grid-cols-2), inline
// errors/warnings, primary button with busy swap + cancel to /explore.
// Forgejo #486: the Name field carries live charset validation (shared
// lib/repo-name.js rule) in a reserved-height slot — no always-on helper.
// Forgejo #497: the Owner/Name pair is the shared OwnerNameRow component
// (matched h-9 heights, items-start top alignment, asymmetric 1fr/2fr
// split, error slot below the grid) — identical on Import.jsx.

import { createSignal, Show, onCleanup } from "solid-js";
import { A, useLocation, useNavigate, useSearchParams } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { validateRepoName, isUiRouteCollision } from "../../sdk/src/create.js";
import { allowedOwners } from "../lib/orgs.js";
import { validateRepoChars } from "../lib/repo-name.js";
import OwnerNameRow from "../components/OwnerNameRow.jsx";
import { invalidate, useData } from "../lib/data.js";
import { anonWriteTarget, write401Target } from "../lib/writeGate.js";

function winnerUrl(msg) {
  const m = String(msg ?? "").match(/https?:\/\/[^\s"']+/);
  return m ? m[0] : "";
}

export default function New() {
  const navigate = useNavigate();
  const location = useLocation();
  const [search] = useSearchParams();
  const [getOwner, setOwner] = createSignal(search.owner ?? "");
  const [getName, setName] = createSignal("");
  const [getFormat, setFormat] = createSignal("sha1");
  const [getVisibility, setVisibility] = createSignal("public");
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
  // Forgejo #502: anonymous viewers route to the log-in interstitial
  // instead of firing the create (discovery rides the shared cache key —
  // zero new requests); the write opts out of popup auth so a
  // stale-identity 401 routes the same way.
  const [getDiscovery] = useData("discovery", () => repos.discovery().catch(() => null));
  const here = () => location.pathname + location.search;
  const writeGateHref = () =>
    anonWriteTarget({ me: getMe(), discovery: getDiscovery() }, here(), "Create a new repository");

  const fieldError = () => validateRepoName(getOwner(), getName()).error ?? "";

  // Forgejo #486: live name-charset error only (empty → "", required rides
  // the disabled submit), rendered into the shared OwnerNameRow's
  // reserved-height slot below the grid — never a conditionally-mounted
  // block, so the rows below never shift while typing.
  const nameCharsError = () => validateRepoChars(getName());

  const submit = async (e) => {
    e.preventDefault();
    if (getBusy()) return;
    const gateHref = writeGateHref();
    if (gateHref) {
      navigate(gateHref);
      return;
    }
    const v = validateRepoName(getOwner(), getName());
    if (v.error) {
      setErr(v.error);
      setWinner("");
      return;
    }
    setBusy(true);
    setErr("");
    setWinner("");
    ctrl?.abort();
    ctrl = new AbortController();
    const signal = ctrl.signal;
    try {
      const res = await repos.repos.create(
        {
          owner: v.owner,
          name: v.name,
          object_format: getFormat() || undefined,
          visibility: getVisibility(),
        },
        { signal, noPopupAuth: true },
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
      // Validation 400s render inline; a stale-identity 401 routes to the
      // interstitial; anything else stays inline too (the form owns its
      // errors — no tray on expected outcomes).
      const target = write401Target(err, { me: getMe(), discovery: getDiscovery() }, here(), "Create a new repository");
      if (target) {
        navigate(target);
        return;
      }
      setErr(String(err?.message ?? err ?? "create failed"));
      setWinner("");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="new-page mx-auto grid max-w-2xl gap-4">
      <h2 class="text-xl font-semibold">New repository</h2>
      <p class="muted text-sm">
        Reserve a name and get push instructions. The first push adopts the
        placeholder — nothing to approve, never a conflict. Mirroring an
        existing repository from a URL instead? <A class="hover:underline" href="/import">Import it</A> —
        Import is the mirror path.
      </p>
      <form class="card grid gap-3 p-4" onSubmit={submit} aria-label="New repository">
        <OwnerNameRow
          prefix="new"
          getOwner={getOwner}
          setOwner={setOwner}
          getOwners={getOwners}
          getName={getName}
          setName={setName}
          namePlaceholder="newthing"
          nameCharsError={nameCharsError}
        />
        <Show when={!fieldError() && isUiRouteCollision(getOwner()) && getOwner()}>
          <p class="text-xs text-amber-700 dark:text-amber-400">
            warning: owner name collides with a UI route — the /:owner page will misroute (git/API unaffected)
          </p>
        </Show>
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
        <Show when={anonymous()}>
          <p class="text-xs text-amber-700 dark:text-amber-400">
            You are browsing as a guest —{" "}
            <A class="hover:underline" href={writeGateHref() ?? here()}>
              sign in to create a repository
            </A>
            .
          </p>
        </Show>
        <Show when={!anonymous() && noWrite()}>
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
            disabled={getBusy() || !!fieldError() || !getOwner() || !getName()}
          >
            {getBusy() ? "creating…" : "create repository"}
          </button>
          <A class="btn px-3 py-1" href="/explore">
            cancel
          </A>
        </div>
      </form>
    </div>
  );
}
