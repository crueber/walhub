// web/src/pages/OrgNew.jsx — route "/orgs/new" (Forgejo #348): create-org
// form → 201 navigates to /:org/settings (the management surface).
// Solid signals only (D-WEB-6); every call through the SDK (dogfood rule);
// dark + light via dark: variants; 409-taken and validation 400s render
// inline (expected control flow ≠ reportError — the New.jsx discipline).
// Name validation mirrors the server (identity.ValidOrg) through the
// shared lib rule so the form never promises what POST /api/v1/orgs
// refuses; the server re-validates.

import { createSignal, Show, onCleanup } from "solid-js";
import { A, useNavigate } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { orgCreateBody, validateOrgName } from "../lib/orgs.js";
import { invalidate } from "../lib/data.js";

export default function OrgNew() {
  const navigate = useNavigate();
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

  const fieldError = () => validateOrgName(getOrg()).error ?? "";

  const submit = async (e) => {
    e.preventDefault();
    if (getBusy()) return;
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
        { signal },
      );
      const slug = res?.org ?? v.org;
      invalidate("owners");
      navigate(`/${slug}/settings`);
    } catch (err) {
      if (err?.status === 499 || signal.aborted) return; // navigated away
      // 409-taken and validation 400s render inline; the form owns its
      // errors — no tray on expected outcomes.
      setErr(String(err?.message ?? err ?? "create failed"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="new-page grid max-w-2xl gap-4">
      <h2 class="text-xl font-semibold">New organization</h2>
      <p class="muted text-sm">
        Reserve an organization namespace. You become its owner and land in
        its settings, where you can edit the profile and add members.
      </p>
      <form class="card grid gap-3 p-4" onSubmit={submit} aria-label="New organization">
        <label class="grid gap-1">
          <span class="text-sm font-medium">Organization name</span>
          <input
            class="input font-mono"
            value={getOrg()}
            onInput={(e) => setOrg(e.currentTarget.value)}
            placeholder="acme"
            autocomplete="off"
            spellcheck={false}
            aria-label="Organization name"
          />
        </label>
        <Show when={fieldError() && getOrg()}>
          <p class="text-xs text-red-700 dark:text-red-400">{fieldError()}</p>
        </Show>
        <label class="grid gap-1">
          <span class="text-sm font-medium">Display name</span>
          <input
            class="input"
            value={getDisplay()}
            onInput={(e) => setDisplay(e.currentTarget.value)}
            placeholder="Acme Corp"
            autocomplete="off"
            spellcheck={false}
            aria-label="Display name"
          />
        </label>
        <label class="grid gap-1">
          <span class="text-sm font-medium">Description</span>
          <input
            class="input"
            value={getDesc()}
            onInput={(e) => setDesc(e.currentTarget.value)}
            placeholder="What this organization is for…"
            autocomplete="off"
            spellcheck={false}
            aria-label="Description"
          />
        </label>
        <Show when={anonymous() || noWrite()}>
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
