// web/src/pages/PullNew.jsx — route "/:owner/:name/pulls/new" (08 §1):
// open a pull request (write+). Each side (From = head, To = base) pairs a
// repo dropdown with a branch dropdown (Forgejo #328): the branch pickers
// stream refs over SSE via the side's repo facade (the §2.6 picker pattern:
// 150 ms debounce, abort the in-flight stream on every keystroke, rows
// keyed by name), re-keyed whenever the side's repo changes (stale options
// are dropped and the stream restarts against the new repo).
//
// The pickers drive a LIVE comparison preview (issue #34): 300 ms after
// the last pick the page fetches both histories (`commits?ref=` — the From
// history from the From repo's client, so cross-repo previews read the
// fork — n = PREVIEW_WINDOW, in parallel) and intersects them client-side
// (lib/compare.js) — ahead/behind counts plus the From-only commit list,
// title/body prefilled from the From tip until the user edits. `?base=`
// / `?head=` search params prefill the To/From refs, so branch pages and
// the pulls list can link straight into a prefilled composer.
//
// The open call targets the To repo's client with the frozen wire keys
// (`base_ref`/`head_ref`); a From repo different from the To repo adds
// `fork: {repo}` (the OpenInput.ForkInfo path). Direction + endpoint rules
// live in the headless-testable lib/pr-composer.js — this page keeps only
// fetch + render. Repo dropdowns list the current owner's repos filtered
// by the viewer's resolved role (To = write+, From = any resolved role);
// the current repo is always present. Cross-owner repos are out of scope
// for v1 (no listing endpoint serves them; same-owner forks are the
// default fork target).
//
// ### Concurrency
// Hazard: picks stack preview fetches; a slow earlier pair must never
// overwrite a newer one, and dead runs must not leak requests.
// Avoidance: the preview effect owns exactly one AbortController per
// run — onCleanup clears the debounce timer AND aborts the in-flight
// pair before the next run starts, and completions are dropped unless
// their monotonic run id is still current. No shared mutable state.

import { createEffect, createSignal, For, Show, onCleanup } from "solid-js";
import { A, useNavigate, useSearchParams } from "@solidjs/router";
import repos from "../../sdk/src/index.js";
import { useRepo } from "./Repo.jsx";
import { reportError } from "../lib/data.js";
import { mountStream } from "../lib/sse.js";
import { roleAtLeast } from "../components/perms.jsx";
import { useRole } from "../components/perms.jsx";
import { shortSha } from "../lib/sha.jsx";
import { PREVIEW_WINDOW, compareHistories, tipSubject, fmtBounded, toShortRef } from "../lib/compare.js";
import {
  FROM_LABEL,
  TO_LABEL,
  buildOpenCall,
  sameEndpoint,
  buildRepoOptions,
  withCurrent,
  filterReposByRole,
  applyEndpointAction,
  fmtEndpoint,
  openErrorMessage,
  previewErrorMessage,
} from "../lib/pr-composer.js";
import DateTime from "../components/DateTime.jsx";

/**
 * RefDropdown: a true single-select listbox of one repo's branches — no
 * free-form entry (the value only ever changes via props.onPick with a
 * streamed refname). The filter input narrows the streamed list; it never
 * sets the value. props.repo is a stable facade whose refStream delegates
 * to the side's currently selected repo, so a repo change re-keys the
 * stream (options cleared, filter reset, in-flight run aborted).
 */
function RefSelect(props) {
  const [getQuery, setQuery] = createSignal("");
  const [getOptions, setOptions] = createSignal([]);
  const [getOpen, setOpen] = createSignal(false);
  let debounce = 0;
  let filter;

  const stream = mountStream(
    (signal, emit) => props.repo.refStream("branches", { q: getQuery(), n: 50 }, emit, { signal }),
    (ref) => setOptions((list) => [...list.filter((r) => r.name !== ref.name), ref]),
  );
  const search = (q) => {
    setQuery(q);
    clearTimeout(debounce);
    debounce = setTimeout(() => {
      setOptions([]);
      stream.run();
    }, 150);
  };
  // Re-key on repo change: the branch namespace moved — drop stale rows
  // and restart the stream (when open) so the list never shows the old
  // repo's branches under the new repo's name.
  createEffect(() => {
    props.full();
    setQuery("");
    setOptions([]);
    if (getOpen()) stream.run();
  });
  onCleanup(() => {
    clearTimeout(debounce);
    stream.cancel();
  });

  const open = () => {
    setOptions([]);
    stream.run();
    setOpen(true);
    setTimeout(() => filter?.focus(), 0);
  };
  const close = () => {
    setOpen(false);
    stream.cancel();
  };

  return (
    <div class="relative">
      <button
        type="button"
        class="input w-full text-left font-mono text-sm"
        aria-haspopup="listbox"
        aria-expanded={getOpen()}
        aria-label={props.label}
        onClick={() => (getOpen() ? close() : open())}
        onKeyDown={(e) => {
          if (e.key === "Escape") close();
        }}
      >
        <span class="flex w-full items-center justify-between gap-2">
          <Show when={props.value()} fallback={<span class="muted font-sans">{props.placeholder}</span>}>
            <span class="truncate">{toShortRef(props.value())}</span>
          </Show>
          <span class="muted shrink-0" aria-hidden="true">▾</span>
        </span>
      </button>
      <Show when={getOpen()}>
        <ul class="ref-list ref-drop card scroll-slim absolute z-10 mt-1 max-h-48 w-full overflow-y-auto p-1 shadow-lg" role="listbox" aria-label={props.label}>
          <li class="p-1">
            <input
              ref={filter}
              class="input w-full font-sans text-sm"
              type="search"
              placeholder="filter branches…"
              autocomplete="off"
              aria-label={`Filter ${props.label} branches`}
              value={getQuery()}
              onInput={(e) => search(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Escape") close();
              }}
            />
          </li>
          <For each={getOptions()}>
            {(r) => (
              <li>
                <button
                  type="button"
                  class="ref-item flex w-full items-center rounded px-2 py-1 text-left font-mono text-xs text-zinc-800 hover:bg-zinc-100 dark:text-zinc-200 dark:hover:bg-zinc-800"
                  role="option"
                  aria-selected={props.value() === r.name}
                  onMouseDown={(e) => e.preventDefault()}
                  onClick={() => {
                    props.onPick(r.name);
                    close();
                  }}
                >
                  {toShortRef(r.name)}
                </button>
              </li>
            )}
          </For>
        </ul>
      </Show>
    </div>
  );
}

/** The live From…To preview: status line + From-only commits. */
function ComparePreview(props) {
  const pv = () => props.preview;
  return (
    <div class="rounded-lg border border-zinc-200 bg-zinc-50/60 p-3 dark:border-zinc-800 dark:bg-zinc-900/40" aria-live="polite">
      <Show when={pv().status === "idle"}>
        <p class="muted text-sm">Pick a {FROM_LABEL} and a {TO_LABEL} branch to preview the comparison.</p>
      </Show>
      <Show when={pv().status === "same-names"}>
        <p class="muted text-sm">{FROM_LABEL} and {TO_LABEL} are the same endpoint — pick two different branches.</p>
      </Show>
      <Show when={pv().status === "loading"}>
        <p class="muted text-sm">Comparing {props.from} … {props.to}…</p>
      </Show>
      <Show when={pv().status === "error"}>
        <p class="err-line text-sm" role="alert">{pv().error}</p>
      </Show>
      <Show when={pv().status === "same"}>
        <p class="muted text-sm">
          <code class="font-mono">{shortSha(pv().headSha)}</code> — both endpoints point at the same commit, nothing to merge.
        </p>
      </Show>
      <Show when={pv().status === "ready"}>
        <p class="text-sm">
          <strong>
            {fmtBounded(pv().ahead, pv().truncatedHead)} ahead
          </strong>
          <span class="muted"> · </span>
          <strong>
            {fmtBounded(pv().behind, pv().truncatedBase)} behind
          </strong>
          <span class="muted text-xs">
            {" "}· {TO_LABEL}{" "}
            <code class="font-mono">{shortSha(pv().baseSha)}</code> · {FROM_LABEL}{" "}
            <code class="font-mono">{shortSha(pv().headSha)}</code>
          </span>
        </p>
        <Show when={pv().truncatedHead || pv().truncatedBase}>
          <p class="warn-line">History past the {PREVIEW_WINDOW}-commit window — counts are lower bounds.</p>
        </Show>
        <Show
          when={pv().unique.length > 0}
          fallback={<p class="muted mt-2 text-sm">{FROM_LABEL} is already merged into {TO_LABEL} — opening would be a no-op.</p>}
        >
          <ul class="mt-2 grid gap-1.5">
            <For each={pv().unique.slice(0, 10)}>
              {(c) => (
                <li class="flex min-w-0 items-baseline gap-2 text-sm">
                  <A
                    class="shrink-0 font-mono text-xs text-emerald-700 hover:underline dark:text-emerald-400"
                    href={`/${props.fromFull}/commit/${c.sha}`}
                    title={c.sha}
                  >
                    {shortSha(c.sha)}
                  </A>
                  <span class="truncate" title={tipSubject(c) || "(no message)"}>
                    {tipSubject(c) || "(no message)"}
                  </span>
                  <span class="muted ml-auto shrink-0 text-xs">
                    {c.author ? `${c.author} · ` : ""}{c.author_date ? <DateTime value={c.author_date} /> : ""}
                  </span>
                </li>
              )}
            </For>
          </ul>
          <Show when={pv().unique.length > 10}>
            <p class="muted mt-1 text-xs">…and {pv().unique.length - 10} more.</p>
          </Show>
        </Show>
      </Show>
    </div>
  );
}

export default function PullNew() {
  const ctx = useRepo();
  const navigate = useNavigate();
  const [search] = useSearchParams();
  const { role } = useRole(ctx.full, ctx.repoClient);
  const [getTitle, setTitle] = createSignal("");
  const [getTitleTouched, setTitleTouched] = createSignal(false);
  const [getBody, setBody] = createSignal("");
  const [getBodyTouched, setBodyTouched] = createSignal(false);
  // From = head (changes come FROM here), To = base (changes go TO here).
  // `?base=`/`?head=` params keep their wire spellings for link compat.
  const [getFromRepo, setFromRepo] = createSignal(ctx.full);
  const [getFromRef, setFromRef] = createSignal(search.head || "");
  const [getToRepo, setToRepo] = createSignal(ctx.full);
  const [getToRef, setToRef] = createSignal(search.base || "refs/heads/main");
  const [getFromRepos, setFromRepos] = createSignal([ctx.full]);
  const [getToRepos, setToRepos] = createSignal([ctx.full]);
  const [getBusy, setBusy] = createSignal(false);
  const [getOpenError, setOpenError] = createSignal("");
  const [getPreview, setPreview] = createSignal({ status: "idle" });

  // Repo dropdown sources (issue #328): the current owner's repos, split
  // by the viewer's resolved role per repo — To needs write (OpenPR
  // requires base write), From needs any resolved role (read). The
  // current repo is always present on both sides. One cold fan-out on
  // mount; failure degrades to [current] on both sides.
  createEffect(() => {
    let alive = true;
    const owner = ctx.owner;
    const current = ctx.full;
    repos.owners
      .repos(owner)
      .then(async (names) => {
        const all = buildRepoOptions(current, owner, Array.isArray(names) ? names : []);
        const settled = await Promise.all(
          all.map((f) =>
            repos
              .repo(f)
              .permissions()
              .then(
                (p) => ({ repo: f, role: p?.role ?? null }),
                () => ({ repo: f, role: null }),
              ),
          ),
        );
        if (!alive) return;
        setToRepos(withCurrent(filterReposByRole(settled, "write"), current));
        setFromRepos(withCurrent(filterReposByRole(settled, "read"), current));
      })
      .catch(() => {
        if (alive) {
          setToRepos([current]);
          setFromRepos([current]);
        }
      });
    onCleanup(() => {
      alive = false;
    });
  });

  // Stable facades: RefSelect calls props.repo.refStream(...) (the §2.6
  // picker shape), delegating to the side's currently selected repo, so a
  // repo change re-keys the stream without remounting the picker.
  const fromRepoFacade = {
    refStream: (kind, query, emit, opts) => repos.repo(getFromRepo()).refStream(kind, query, emit, opts),
  };
  const toRepoFacade = {
    refStream: (kind, query, emit, opts) => repos.repo(getToRepo()).refStream(kind, query, emit, opts),
  };

  const pickFromRepo = (repo) => {
    const next = applyEndpointAction({ repo: getFromRepo(), ref: getFromRef() }, { type: "select-repo", repo });
    setFromRepo(next.repo);
    setFromRef(next.ref);
  };
  const pickToRepo = (repo) => {
    const next = applyEndpointAction({ repo: getToRepo(), ref: getToRef() }, { type: "select-repo", repo });
    setToRepo(next.repo);
    setToRef(next.ref);
  };

  // Live preview: debounce picks, fetch both histories in parallel — the
  // From history from the From repo's client (cross-repo previews read
  // the fork) — split the From walk at the To SHA. One AbortController
  // per run, torn down in onCleanup; stale completions dropped by run id.
  // allSettled attributes failures per side (a missing From branch names
  // From, not a generic compare error).
  let run = 0;
  createEffect(() => {
    const fromRepo = getFromRepo().trim();
    const from = getFromRef().trim();
    const toRepo = getToRepo().trim();
    const to = getToRef().trim();
    if (!from || !to) {
      setPreview({ status: "idle" });
      return;
    }
    if (sameEndpoint(fromRepo, from, toRepo, to)) {
      setPreview({ status: "same-names" });
      return;
    }
    setPreview({ status: "loading" });
    const id = ++run;
    const ctl = new AbortController();
    const timer = setTimeout(async () => {
      try {
        // The commits resolver takes short branch names — the pickers
        // and the open call use full refnames, so shorten here only.
        const [fromRes, toRes] = await Promise.allSettled([
          repos.repo(fromRepo).commits({ ref: toShortRef(from), n: PREVIEW_WINDOW }, { signal: ctl.signal }),
          repos.repo(toRepo).commits({ ref: toShortRef(to), n: PREVIEW_WINDOW }, { signal: ctl.signal }),
        ]);
        if (id !== run || ctl.signal.aborted) return;
        if (fromRes.status === "rejected") {
          setPreview({ status: "error", error: previewErrorMessage(FROM_LABEL, fromRes.reason) });
          return;
        }
        if (toRes.status === "rejected") {
          setPreview({ status: "error", error: previewErrorMessage(TO_LABEL, toRes.reason) });
          return;
        }
        const headPage = fromRes.value;
        const basePage = toRes.value;
        const headSha = headPage?.sha;
        const baseSha = basePage?.sha;
        if (headSha && headSha === baseSha) {
          setPreview({ status: "same", headSha, baseSha });
          return;
        }
        const cmp = compareHistories(headPage, basePage);
        setPreview({
          status: "ready",
          baseSha,
          headSha,
          ahead: cmp.ahead,
          behind: cmp.behind,
          truncatedHead: cmp.truncatedHead,
          truncatedBase: cmp.truncatedBase,
          unique: cmp.unique,
        });
        // Prefill from the From branch until the user edits: title from
        // the tip subject, body from the From-only subject list.
        const tip = tipSubject(headPage?.commits?.[0]);
        if (tip && !getTitleTouched()) setTitle(tip);
        if (!getBodyTouched() && cmp.unique.length > 0) {
          setBody(cmp.unique.slice(0, 20).map((cc) => `- ${tipSubject(cc) || "(no message)"}`).join("\n"));
        }
      } catch (err) {
        if (id !== run || ctl.signal.aborted) return;
        setPreview({ status: "error", error: previewErrorMessage(`${FROM_LABEL}/${TO_LABEL}`, err) });
      }
    }, 300);
    onCleanup(() => {
      clearTimeout(timer);
      ctl.abort();
    });
  });

  const swap = () => {
    const fr = getFromRepo();
    const f = getFromRef();
    setFromRepo(getToRepo());
    setFromRef(getToRef());
    setToRepo(fr);
    setToRef(f);
  };

  const open = async (e) => {
    e.preventDefault();
    if (getBusy() || !getTitle().trim() || !getFromRef().trim()) return;
    setBusy(true);
    setOpenError("");
    try {
      const { baseRepo, payload } = buildOpenCall({
        fromRepo: getFromRepo(),
        fromRef: getFromRef(),
        toRepo: getToRepo(),
        toRef: getToRef(),
        title: getTitle().trim(),
        body: getBody().trim() || undefined,
      });
      const res = await repos.repo(baseRepo).pulls.open(payload);
      const num = res.thread?.num ?? res.pr?.num;
      navigate(`/${baseRepo}/pull/${num}`);
    } catch (err) {
      setOpenError(openErrorMessage(err));
      reportError(err, "pull-open");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="mx-auto max-w-2xl">
      <h2 class="mb-3 text-lg font-semibold">New pull request</h2>
      <Show when={role() === null} fallback={
        <Show when={roleAtLeast(role(), "write")} fallback={
          <p class="card text-sm">opening pull requests needs the write role — your role: {role() ?? "none"}.</p>
        }>
          <form class="card grid gap-3 p-4" onSubmit={open}>
            <fieldset class="grid gap-3">
              <legend class="text-sm font-medium">Compare changes</legend>
              <div class="grid gap-3 sm:grid-cols-[1fr_auto_1fr] sm:items-end">
                <div class="grid gap-1">
                  <label class="field">
                    <span>{FROM_LABEL}</span>
                    <select
                      class="input w-full text-sm"
                      value={getFromRepo()}
                      onChange={(e) => pickFromRepo(e.target.value)}
                      aria-label={`${FROM_LABEL} repository`}
                    >
                      <For each={withCurrent(getFromRepos(), getFromRepo())}>
                        {(f) => <option value={f}>{f}</option>}
                      </For>
                    </select>
                  </label>
                  <RefSelect repo={fromRepoFacade} full={getFromRepo} value={getFromRef} onPick={setFromRef} label={`${FROM_LABEL} branch`} placeholder="Pick a branch…" />
                </div>
                <button
                  type="button"
                  class="btn px-2 py-1"
                  onClick={swap}
                  title={`Swap ${FROM_LABEL} and ${TO_LABEL}`}
                  aria-label={`Swap ${FROM_LABEL} and ${TO_LABEL}`}
                >
                  ⇄
                </button>
                <div class="grid gap-1">
                  <label class="field">
                    <span>{TO_LABEL}</span>
                    <select
                      class="input w-full text-sm"
                      value={getToRepo()}
                      onChange={(e) => pickToRepo(e.target.value)}
                      aria-label={`${TO_LABEL} repository`}
                    >
                      <For each={withCurrent(getToRepos(), getToRepo())}>
                        {(f) => <option value={f}>{f}</option>}
                      </For>
                    </select>
                  </label>
                  <RefSelect repo={toRepoFacade} full={getToRepo} value={getToRef} onPick={setToRef} label={`${TO_LABEL} branch`} placeholder="Pick a branch…" />
                </div>
              </div>
              <ComparePreview
                preview={getPreview()}
                from={fmtEndpoint(getFromRepo(), getFromRef().trim(), getToRepo())}
                to={fmtEndpoint(getToRepo(), getToRef().trim(), getToRepo())}
                fromFull={getFromRepo().trim()}
              />
            </fieldset>
            <label class="field">
              <span>Title</span>
              <input
                class="input"
                value={getTitle()}
                onInput={(e) => {
                  setTitleTouched(true);
                  setTitle(e.target.value);
                }}
                aria-label="title"
              />
            </label>
            <label class="field">
              <span>Body (optional)</span>
              <textarea
                class="input min-h-24 font-mono text-sm"
                value={getBody()}
                onInput={(e) => {
                  setBodyTouched(true);
                  setBody(e.target.value);
                }}
                aria-label="body"
              />
            </label>
            <Show when={getOpenError()}>
              <p class="err-line text-sm" role="alert">{getOpenError()}</p>
            </Show>
            <div>
              <button type="submit" class="btn primary" disabled={getBusy() || !getTitle().trim() || !getFromRef().trim()}>
                {getBusy() ? "Opening…" : "Open pull request"}
              </button>
            </div>
          </form>
        </Show>
      }>
        <p class="card text-sm">sign in to open a pull request.</p>
      </Show>
    </div>
  );
}
