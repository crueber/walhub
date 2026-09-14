// web/src/pages/OrgNew.jsx — route "/orgs/new" (Forgejo #348): create-org
// form → 201 navigates to /:org/settings (the management surface).
// Solid signals only (D-WEB-6); every call through the SDK (dogfood rule);
// dark + light via dark: variants; 409-taken and validation 400s render
// inline (expected control flow ≠ reportError — the New.jsx discipline).
// Name validation mirrors the server (identity.ValidOrg) through the
// shared lib rule so the form never promises what POST /api/v1/orgs
// refuses; the server re-validates.
// Form-page pattern (Forgejo #479 standing rule — reference: ReleaseNew.jsx):
// centered mx-auto max-w-2xl column, one h2 + one muted intro, single card
// form, label.grid.gap-1 fields with id + aria-describedby help, inline
// errors/warnings, primary button with busy swap + cancel to /explore.

import { createSignal, Show, onCleanup } from "solid-js";
import { A, useLocation, useNavigate } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { orgCreateBody, validateOrgName } from "../lib/orgs.js";
import { invalidate, useData } from "../lib/data.js";
import { anonWriteTarget, write401Target } from "../lib/writeGate.js";

export default function OrgNew() {
  const navigate = useNavigate();
  const location = useLocation();
  const [getOrg, setOrg] = createSignal("");
  const [getDisplay, setDisplay] = createSignal("");
  const [getDesc, setDesc] = createSignal("");
  const [getErr, setErr] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);
  const [getMe, setMe] = createSignal(null);
  let ctrl = null;
  onCleanup(() => {
    if (ctrl) ctrl.abort();
  });

  repos
    .me()
    .then((me) => setMe(me))
    .catch(() => setMe({ anonymous: true }));
  const anonymous = () => getMe()?.anonymous !== false && !getMe()?.principal;
  const noWrite = () => getMe() != null && getMe().write === false;
  // Forgejo #502: anonymous viewers route to the log-in interstitial
  // instead of firing the create (discovery rides the shared cache key —
  // zero new requests); the write opts out of popup auth so a
  // stale-identity 401 routes the same way.
  const [getDiscovery] = useData("discovery", () => repos.discovery().catch(() => null));
  const here = () => location.pathname + location.search;
  const writeGateHref = () =>
    anonWriteTarget({ me: getMe(), discovery: getDiscovery() }, here(), "Create a new organization");

  const fieldError = () => validateOrgName(getOrg()).error ?? "";

  const submit = async (e) => {
    e.preventDefault();
    if (getBusy()) return;
    const gateHref = writeGateHref();
    if (gateHref) {
      navigate(gateHref);
      return;
    }
    const v = validateOrgName(getOrg());
    if (v.error) {
      setErr(v.error);
      return;
    }
    setBusy(true);
    setErr("");
    ctrl?.abort();
    ctrl = new AbortController();
    const signal = ctrl.signal;
    try {
      const res = await repos.orgs.create(
        orgCreateBody({ org: v.org, display_name: getDisplay(), description: getDesc() }),
        { signal, noPopupAuth: true },
      );
      const slug = res?.org ?? v.org;
      invalidate("owners");
      navigate(`/${slug}/settings`);
    } catch (err) {
      if (err?.status === 499 || signal.aborted) return; // navigated away
      // 409-taken and validation 400s render inline; a stale-identity 401
      // routes to the interstitial; the form owns the rest — no tray.
      const target = write401Target(err, { me: getMe(), discovery: getDiscovery() }, here(), "Create a new organization");
      if (target) {
        navigate(target);
        return;
      }
      setErr(String(err?.message ?? err ?? "create failed"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="new-page mx-auto grid max-w-2xl gap-4">
      <h2 class="text-xl font-semibold">New organization</h2>
      <p class="muted text-sm">
        Reserve an organization namespace. You become its owner and land in
        its settings, where you can edit the profile and add members.
      </p>
      <form class="card grid gap-3 p-4" onSubmit={submit} aria-label="New organization">
        <label class="grid gap-1" for="orgnew-name">
          <span class="text-sm font-medium">Organization name</span>
          <input
            id="orgnew-name"
            class="input font-mono"
            value={getOrg()}
            onInput={(e) => setOrg(e.currentTarget.value)}
            placeholder="acme"
            autocomplete="off"
            spellcheck={false}
            aria-label="Organization name"
            aria-describedby="orgnew-name-help"
          />
          <span id="orgnew-name-help" class="muted text-xs">Lowercase letters, digits, and hyphens, 1–39 characters.</span>
        </label>
        <Show when={fieldError() && getOrg()}>
          <p class="text-xs text-red-700 dark:text-red-400">{fieldError()}</p>
        </Show>
        <label class="grid gap-1" for="orgnew-display">
          <span class="text-sm font-medium">Display name</span>
          <input
            id="orgnew-display"
            class="input"
            value={getDisplay()}
            onInput={(e) => setDisplay(e.currentTarget.value)}
            placeholder="Acme Corp"
            autocomplete="off"
            spellcheck={false}
            aria-label="Display name"
          />
        </label>
        <label class="grid gap-1" for="orgnew-desc">
          <span class="text-sm font-medium">Description</span>
          <input
            id="orgnew-desc"
            class="input"
            value={getDesc()}
            onInput={(e) => setDesc(e.currentTarget.value)}
            placeholder="What this organization is for…"
            autocomplete="off"
            spellcheck={false}
            aria-label="Description"
          />
        </label>
        <Show when={anonymous()}>
          <p class="text-xs text-amber-700 dark:text-amber-400">
            You are browsing as a guest —{" "}
            <A class="hover:underline" href={writeGateHref() ?? here()}>
              sign in to create an organization
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
            {getErr()}
          </div>
        </Show>
        <div class="flex gap-2">
          <button
            type="submit"
            class="btn primary px-3 py-1"
            disabled={getBusy() || !!fieldError() || !getOrg().trim()}
          >
            {getBusy() ? "creating…" : "create organization"}
          </button>
          <A class="btn px-3 py-1" href="/explore">
            cancel
          </A>
        </div>
      </form>
    </div>
  );
}
