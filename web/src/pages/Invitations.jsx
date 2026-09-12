// web/src/pages/Invitations.jsx — route "/invitations" (Forgejo #362):
// the invitee inbox. Lists my pending invites (GET /api/v1/invitations —
// org- and repo-kind alike, expiry served on the row since #362), with
// per-row preview (subject-authorized, no token needed), accept (disabled
// once expired — the server still 409s "invitation expired" fail-closed),
// and decline. A shared accept link deep-links here as
// ?id=&token= and previews that invite on top.

import { createSignal, For, Show } from "solid-js";
import { A, useSearchParams } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { reportError } from "../lib/data.js";
import DateTime from "../components/DateTime.jsx";
import { isInviteExpired, inviteKind, inviteScope } from "../lib/invites.js";

function asList(res) {
  if (Array.isArray(res)) return res;
  if (Array.isArray(res?.entries)) return res.entries;
  if (Array.isArray(res?.invitations)) return res.invitations;
  return [];
}

function friendly(err) {
  if (!err) return "";
  if (err.status === 401) return "sign in to view your invitations";
  if (err.status === 409 && /expired/i.test(String(err.message ?? ""))) return "invitation expired";
  if (err.status === 409) return "invitation no longer pending";
  if (err.status === 403) return "not your invitation";
  return String(err.message ?? err);
}

function ExpiryCell(props) {
  const inv = () => props.inv;
  return (
    <Show when={inv().expires_at} fallback={<span class="muted">—</span>}>
      <Show when={isInviteExpired(inv())} fallback={<DateTime value={inv().expires_at} />}>
        <span class="chip-closed">expired</span>
      </Show>
    </Show>
  );
}

export default function Invitations() {
  const [search] = useSearchParams();
  const [getItems, setItems] = createSignal(null);
  const [getErr, setErr] = createSignal("");
  const [getNote, setNote] = createSignal("");
  const [getBusy, setBusy] = createSignal({});
  const [getPreviews, setPreviews] = createSignal({});

  const setRowBusy = (id, on) =>
    setBusy((prev) => {
      const next = { ...prev };
      if (on) next[id] = true;
      else delete next[id];
      return next;
    });

  const load = async () => {
    setErr("");
    try {
      setItems(asList(await repos.invites.mine()));
    } catch (e) {
      if (e?.status === 401) {
        setItems([]);
        setErr("sign in to view your invitations");
      } else {
        reportError(e, "invitations");
        setErr(friendly(e));
        setItems([]);
      }
    }
  };
  load();

  // Deep link (?id=&token=) from a shared accept URL: preview that invite
  // on top even when it is not in my inbox (token-authorized preview).
  const linkId = () => (typeof search.id === "string" && search.id ? search.id : "");
  const linkToken = () => (typeof search.token === "string" ? search.token : "");
  const [getLinkPrev, setLinkPrev] = createSignal(null);
  const [getLinkErr, setLinkErr] = createSignal("");
  const loadLink = async () => {
    const id = linkId();
    if (!id) return;
    setLinkErr("");
    setLinkPrev(null);
    try {
      setLinkPrev(await repos.invites.get(id, linkToken() || undefined));
    } catch (e) {
      setLinkErr(friendly(e));
    }
  };
  loadLink();

  const preview = async (id) => {
    if (getPreviews()[id] !== undefined) {
      setPreviews((prev) => {
        const next = { ...prev };
        delete next[id];
        return next;
      });
      return;
    }
    setRowBusy(id, true);
    try {
      const data = await repos.invites.get(id);
      setPreviews((prev) => ({ ...prev, [id]: { data, error: "" } }));
    } catch (e) {
      setPreviews((prev) => ({ ...prev, [id]: { data: null, error: friendly(e) } }));
    } finally {
      setRowBusy(id, false);
    }
  };

  const accept = async (id) => {
    setNote("");
    setRowBusy(id, true);
    try {
      const res = await repos.invites.accept(id);
      setNote(`accepted — bound to ${res?.bound ?? "membership"}`);
      setPreviews((prev) => {
        const next = { ...prev };
        delete next[id];
        return next;
      });
      if (linkId() === id) {
        setLinkPrev(null);
        setLinkErr("");
      }
      await load();
    } catch (e) {
      reportError(e, "invitations-accept");
      setNote(friendly(e));
    } finally {
      setRowBusy(id, false);
    }
  };

  const decline = async (id) => {
    setNote("");
    setRowBusy(id, true);
    try {
      await repos.invites.cancel(id);
      setNote("invitation declined");
      setPreviews((prev) => {
        const next = { ...prev };
        delete next[id];
        return next;
      });
      if (linkId() === id) {
        setLinkPrev(null);
        setLinkErr("");
      }
      await load();
    } catch (e) {
      reportError(e, "invitations-decline");
      setNote(friendly(e));
    } finally {
      setRowBusy(id, false);
    }
  };

  const row = (inv) => {
    const id = inv.id;
    const expired = () => isInviteExpired(inv);
    const busy = () => !!getBusy()[id];
    const prev = () => getPreviews()[id];
    const kind = inviteKind(inv);
    return (
      <li class="card p-3">
        <div class="flex flex-wrap items-center gap-2 text-sm">
          <span class="chip">{kind}</span>
          <Show when={expired()}><span class="chip-closed">expired</span></Show>
          <strong class="font-mono text-sm">{inviteScope(inv) || id}</strong>
          <span class="muted text-xs">as {inv.role ?? "?"}</span>
          <span class="muted ml-auto text-xs">
            from <code class="font-mono text-xs">{inv.invited_by ?? "?"}</code> · expires <ExpiryCell inv={inv} />
          </span>
        </div>
        <Show when={prev()}>
          <div class="mt-2 rounded border border-zinc-200 p-2 text-sm dark:border-zinc-800">
            <Show when={prev().data} fallback={<p class="text-amber-700 dark:text-amber-300">{prev().error || "no preview"}</p>}>
              <dl class="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-0.5 text-xs">
                <dt class="muted">target</dt><dd class="font-mono">{inviteScope(prev().data) || prev().data.id}</dd>
                <dt class="muted">role</dt><dd>{prev().data.role}</dd>
                <dt class="muted">invited by</dt><dd class="font-mono">{prev().data.invited_by}</dd>
                <dt class="muted">state</dt><dd>{prev().data.state}</dd>
                <dt class="muted">created</dt><dd><DateTime value={prev().data.created_at} /></dd>
                <dt class="muted">expires</dt><dd><ExpiryCell inv={prev().data} /></dd>
              </dl>
            </Show>
          </div>
        </Show>
        <div class="mt-2 flex flex-wrap gap-2">
          <button type="button" class="btn px-2 py-1 text-xs" disabled={busy()} onClick={() => preview(id)}>
            {prev() ? "hide details" : "details"}
          </button>
          <button
            type="button"
            class="btn primary px-2 py-1 text-xs"
            disabled={busy() || expired()}
            title={expired() ? "expired — the server rejects accept with 409" : "accept this invitation"}
            onClick={() => accept(id)}
          >
            {busy() ? "working…" : "accept"}
          </button>
          <button type="button" class="btn px-2 py-1 text-xs" disabled={busy()} onClick={() => decline(id)}>
            decline
          </button>
        </div>
      </li>
    );
  };

  return (
    <div class="invitations-page mx-auto max-w-3xl">
      <h2 class="mb-1 text-lg font-semibold">Invitations</h2>
      <p class="muted mb-3 text-sm">
        Pending organization and repository invitations addressed to you. Expired
        invitations stay listed but cannot be accepted — decline clears them.
      </p>
      <Show when={linkId()}>
        <section class="card mb-4 p-3" aria-label="Shared invitation">
          <h3 class="mb-2 font-semibold">Shared invitation</h3>
          <Show when={getLinkPrev()} fallback={<Show when={getLinkErr()} fallback={<p class="muted text-sm">loading…</p>}><p class="text-sm text-amber-700 dark:text-amber-300">{getLinkErr()}</p></Show>}>
            {(pv) => (
              <>
                <p class="text-sm">
                  <span class="chip mr-1">{inviteKind(pv())}</span>
                  <strong class="font-mono">{inviteScope(pv()) || pv().id}</strong>
                  <span class="muted"> as {pv().role} · from </span>
                  <code class="font-mono text-xs">{pv().invited_by}</code>
                  <span class="muted"> · expires </span>
                  <ExpiryCell inv={pv()} />
                </p>
                <div class="mt-2 flex flex-wrap gap-2">
                  <button
                    type="button"
                    class="btn primary px-2 py-1 text-xs"
                    disabled={!!getBusy()[pv().id] || isInviteExpired(pv())}
                    onClick={() => accept(pv().id)}
                  >
                    accept
                  </button>
                  <button type="button" class="btn px-2 py-1 text-xs" disabled={!!getBusy()[pv().id]} onClick={() => decline(pv().id)}>
                    decline
                  </button>
                </div>
              </>
            )}
          </Show>
        </section>
      </Show>
      <Show when={getErr()}><p class="mb-3 text-sm text-amber-700 dark:text-amber-300">{getErr()}</p></Show>
      <Show when={getItems()} fallback={<p class="muted">loading…</p>}>
        <Show when={getItems().length > 0} fallback={<p class="muted text-sm">no pending invitations — new org or repo invites land here.</p>}>
          <ol class="grid gap-2">
            <For each={getItems()}>{(inv) => row(inv)}</For>
          </ol>
        </Show>
      </Show>
      <Show when={getNote()}><p class="mt-3 text-sm text-amber-700 dark:text-amber-300">{getNote()}</p></Show>
      <p class="muted mt-3 text-xs">
        Managing an organization instead? <A class="link" href="/explore">explore owners</A> to find it.
      </p>
    </div>
  );
}
