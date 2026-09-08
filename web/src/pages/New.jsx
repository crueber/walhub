// web/src/pages/New.jsx — route "/new" (Forgejo #210): explicit create-repo
// placeholder form → 201 navigates to /{o}/{r} (placeholder view).
// Solid signals only (D-WEB-6); every call through the SDK (dogfood rule);
// dark + light via dark: variants; 409-exists and validation 400s render
// inline (expected control flow ≠ reportError — the Ticket-1
// tolerateMissing discipline extended).

import { createSignal, Show, onCleanup } from "solid-js";
import { A, useNavigate, useSearchParams } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { validateRepoName, isUiRouteCollision } from "../../sdk/src/create.js";
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
  const [getErr, setErr] = createSignal("");
  const [getWinner, setWinner] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);
  const [getMe, setMe] = createSignal(null);
  let ctrl = null;
  onCleanup(() => {
    if (ctrl) ctrl.abort();
  });

  repos
    .me()
    .then((me) => {
      setMe(me);
      if (!getOwner() && me?.principal && !me.anonymous) setOwner(me.principal);
    })
    .catch(() => setMe({ anonymous: true }));
  const anonymous = () => getMe()?.anonymous !== false && !getMe()?.principal;
  const noWrite = () => getMe() != null && getMe().write === false;

  const fieldError = () => validateRepoName(getOwner(), getName()).error ?? "";

  const submit = async (e) => {
    e.preventDefault();
    if (getBusy()) return;
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
        <div class="grid grid-cols-2 gap-3">
          <label class="grid gap-1">
            <span class="text-sm font-medium">Owner</span>
            <input
              class="input font-mono"
              value={getOwner()}
              onInput={(e) => setOwner(e.currentTarget.value.trim())}
              placeholder="acme"
              autocomplete="off"
              spellcheck={false}
              aria-label="Owner"
            />
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
        <Show when={fieldError() && getName()}>
          <p class="text-xs text-red-700 dark:text-red-400">{fieldError()}</p>
        </Show>
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
              <option value="public">public</option>
              <option value="private">private</option>
            </select>
          </label>
        </div>
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
            disabled={getBusy() || !!fieldError() || !getOwner() || !getName()}
          >
            {getBusy() ? "creating…" : "create repository"}
          </button>
        </div>
      </form>
    </div>
  );
}
