// web/src/pages/Fork.jsx — route "/:owner/:name/fork" (issue #424):
// fork-creation form → 202 polls the child summary, then navigates to
// /{targetOwner}/{name}. Sibling New.jsx is the reference implementation
// for field layout, the visibility control, and the submit pattern.
// Solid signals only (D-WEB-6); every call through the SDK (dogfood
// rule); taken-name conflicts and validation 400s render inline
// (expected control flow ≠ reportError).

import { createSignal, Show, For, onCleanup } from "solid-js";
import { A, useParams, useNavigate } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { validateRepoName } from "../../sdk/src/create.js";
import { allowedOwners } from "../lib/orgs.js";
import { forkDefaultName, forkBranchShort } from "../lib/fork.js";
import { invalidate } from "../lib/data.js";

const POLL_MS = 1000;
const POLL_TIMEOUT_MS = 60000;

function winnerUrl(msg) {
  const m = String(msg ?? "").match(/https?:\/\/[^\s"']+/);
  return m ? m[0] : "";
}

export default function Fork() {
  const params = useParams();
  const navigate = useNavigate();
  const full = () => `${params.owner}/${params.name}`;
  const repoClient = repos.repo(full());

  const [getOwner, setOwner] = createSignal("");
  const [getName, setName] = createSignal(forkDefaultName(params.name));
  const [getVisibility, setVisibility] = createSignal("public");
  const [getBranch, setBranch] = createSignal(""); // full ref; "" = parent HEAD
  const [getBranches, setBranches] = createSignal(null); // null = loading
  const [getDescription, setDescription] = createSignal("");
  const [getErr, setErr] = createSignal("");
  const [getWinner, setWinner] = createSignal("");
  const [getBusy, setBusy] = createSignal(false);
  const [getStatus, setStatus] = createSignal("");
  const [getOwners, setOwners] = createSignal(null);
  let alive = true;
  onCleanup(() => {
    alive = false;
  });

  // Owners: self + member orgs (the server admits exactly that set —
  // the #346 gate; admins needing a foreign namespace use the API).
  repos
    .me()
    .then(async (me) => {
      if (!alive) return;
      const self = me?.principal && !me.anonymous ? me.principal : "";
      if (self) setOwner(self);
      if (!self) {
        setOwners([]);
        return;
      }
      const opts = await allowedOwners(repos, self).catch(() => [self]);
      if (!alive) return;
      setOwners(opts);
      if (!opts.includes(getOwner())) setOwner(self);
    })
    .catch(() => {
      if (alive) setOwners([]);
    });

  // Branches: the starting-branch options (default = parent HEAD).
  repoClient
    .branches({ n: 100 })
    .then((page) => {
      if (!alive) return;
      setBranches(page?.refs ?? []);
    })
    .catch(() => {
      if (alive) setBranches([]);
    });
  repoClient
    .get()
    .then((summary) => {
      if (!alive) return;
      if (summary?.head?.name && !getBranch()) setBranch(summary.head.name);
    })
    .catch(() => {});

  const fieldError = () => validateRepoName(getOwner(), getName()).error ?? "";
  const branchOptions = () => {
    const refs = getBranches() ?? [];
    const head = getBranch();
    // The parent HEAD leads even when the page is truncated (n=100).
    const names = refs.map((r) => r.name);
    if (head && !names.includes(head)) return [head, ...names];
    return names;
  };

  const failInline = (err) => {
    const msg = String(err?.message ?? err ?? "fork failed");
    setErr(msg);
    try {
      const u = winnerUrl(msg);
      setWinner(u ? new URL(u).pathname : "");
    } catch {
      setWinner("");
    }
  };

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
    setStatus("");
    try {
      // Pre-flight the taken name inline (the server fail-fasts 409
      // too — this only avoids the task round trip for the common
      // case; the server stays authoritative).
      try {
        await repos.repo(`${v.owner}/${v.name}`).get();
        setErr(`repository already exists: ${v.owner}/${v.name}`);
        setWinner(`/${v.owner}/${v.name}`);
        return;
      } catch (pre) {
        if (pre?.status !== 404) {
          // A non-404 probe failure is not "free" — submit anyway and
          // let the server decide (its 409 names the winner).
        }
      }
      const res = await repoClient.forks.create({
        target_owner: v.owner,
        name: v.name,
        visibility: getVisibility(),
        branch: getBranch() || undefined,
        description: getDescription().trim() || undefined,
      });
      const child = res?.repo ?? `${v.owner}/${v.name}`;
      // The 202 only queued the task: poll the child summary until the
      // shared manifest lands (servable), then enter the fork.
      setStatus(`forking ${full()} → ${child}…`);
      const deadline = Date.now() + POLL_TIMEOUT_MS;
      for (;;) {
        if (!alive) return;
        try {
          await repos.repo(child).get();
          break;
        } catch (pe) {
          if (Date.now() > deadline) {
            throw new Error(`fork is taking longer than expected — check ${child} shortly`);
          }
          await new Promise((r) => setTimeout(r, POLL_MS));
        }
      }
      invalidate("owners");
      invalidate(`repos:${v.owner}`);
      navigate(`/${child}`);
    } catch (err) {
      if (err?.status === 409) {
        failInline(err);
        return;
      }
      setErr(String(err?.message ?? err ?? "fork failed"));
      setWinner("");
    } finally {
      if (alive) {
        setBusy(false);
        setStatus("");
      }
    }
  };

  return (
    <div class="fork-page grid max-w-2xl gap-4">
      <h2 class="text-xl font-semibold">
        Fork <A class="hover:underline" href={`/${full()}`}>{full()}</A>
      </h2>
      <p class="muted text-sm">
        A fork shares the parent's objects and starts from its refs — pushes
        to either side stay independent. Issues and pull requests start fresh.
      </p>
      <form class="card grid gap-3 p-4" onSubmit={submit} aria-label="Fork repository">
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
              placeholder={forkDefaultName(params.name)}
              autocomplete="off"
              spellcheck={false}
              aria-label="Name"
            />
          </label>
        </div>
        <Show when={fieldError() && getName()}>
          <p class="text-xs text-red-700 dark:text-red-400">{fieldError()}</p>
        </Show>
        <div class="grid grid-cols-2 gap-3">
          <label class="grid gap-1">
            <span class="text-sm font-medium">Visibility</span>
            <select
              class="input w-auto"
              value={getVisibility()}
              onChange={(e) => setVisibility(e.currentTarget.value)}
              aria-label="Visibility"
            >
              <option value="public">public — anyone may read</option>
              <option value="authenticated">private — logged in only</option>
              <option value="private">private — owner/org only</option>
            </select>
          </label>
          <label class="grid gap-1">
            <span class="text-sm font-medium">Starting branch</span>
            <Show when={getBranches() !== null} fallback={<select class="input font-mono" disabled aria-label="Starting branch"><option>…</option></select>}>
              <select
                class="input font-mono"
                value={getBranch()}
                onChange={(e) => setBranch(e.currentTarget.value)}
                aria-label="Starting branch"
              >
                <option value="">parent default</option>
                <For each={branchOptions()}>{(b) => <option value={b}>{forkBranchShort(b)}</option>}</For>
              </select>
            </Show>
            <span class="muted text-xs">becomes the fork's default branch</span>
          </label>
        </div>
        <label class="grid gap-1">
          <span class="text-sm font-medium">Description <span class="muted">(optional)</span></span>
          <input
            class="input"
            value={getDescription()}
            onInput={(e) => setDescription(e.currentTarget.value)}
            placeholder="what this fork is for"
            maxlength={1024}
            autocomplete="off"
            aria-label="Description"
          />
        </label>
        <Show when={getStatus()}>
          <p class="muted text-sm" role="status">{getStatus()}</p>
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
            {getBusy() ? "forking…" : "create fork"}
          </button>
          <A class="btn px-3 py-1" href={`/${full()}`}>
            cancel
          </A>
        </div>
      </form>
    </div>
  );
}
