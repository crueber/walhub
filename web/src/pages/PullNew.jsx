// web/src/pages/PullNew.jsx — route "/:owner/:name/pulls/new" (08 §1):
// open a pull request (write+). Head/base pickers stream refs over SSE
// via repo.refStream (the §2.6 picker pattern: 150 ms debounce, abort
// the in-flight stream on every keystroke, rows keyed by name).
//
// The pickers drive a LIVE comparison preview (issue #34): 300 ms after
// the last keystroke the page fetches both histories (`commits?ref=`,
// n = PREVIEW_WINDOW, in parallel) and intersects them client-side
// (lib/compare.js) — ahead/behind counts plus the head-only commit list,
// title/body prefilled from the head tip until the user edits. `?base=`
// / `?head=` search params prefill the pickers, so branch pages and the
// pulls list can link straight into a prefilled composer.
//
// ### Concurrency
// Hazard: keystrokes stack preview fetches; a slow earlier pair must
// never overwrite a newer one, and dead runs must not leak requests.
// Avoidance: the preview effect owns exactly one AbortController per
// run — onCleanup clears the debounce timer AND aborts the in-flight
// pair before the next run starts, and completions are dropped unless
// their monotonic run id is still current. No shared mutable state.

import { createEffect, createSignal, For, Show, onCleanup } from "solid-js";
import { A, useNavigate, useSearchParams } from "@solidjs/router";
import { useRepo } from "./Repo.jsx";
import { reportError } from "../lib/data.js";
import { mountStream } from "../lib/sse.js";
import { roleAtLeast } from "../components/perms.jsx";
import { useRole } from "../components/perms.jsx";
import { shortSha } from "../lib/sha.jsx";
import { PREVIEW_WINDOW, compareHistories, tipSubject, fmtBounded, toShortRef } from "../lib/compare.js";
import DateTime from "../components/DateTime.jsx";

function RefSelect(props) {
  const [getQuery, setQuery] = createSignal("");
  const [getOptions, setOptions] = createSignal([]);
  const [getOpen, setOpen] = createSignal(false);
  let debounce = 0;

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
  onCleanup(() => {
    clearTimeout(debounce);
    stream.cancel();
  });

  return (
    <div class="relative">
      <input
        class="input w-full font-mono text-sm"
        placeholder={props.placeholder}
        value={props.value()}
        onInput={(e) => {
          props.onPick(e.target.value);
          search(e.target.value);
          setOpen(true);
        }}
        onFocus={() => {
          setOptions([]);
          stream.run();
          setOpen(true);
        }}
        onBlur={() => setTimeout(() => setOpen(false), 150)}
        onKeyDown={(e) => {
          if (e.key === "Escape") {
            setOpen(false);
            stream.cancel();
          }
        }}
        aria-label={props.label}
      />
      <Show when={getOpen() && getOptions().length > 0}>
        <ul class="ref-list absolute z-10 max-h-48 w-full overflow-y-auto" role="listbox">
          <For each={getOptions()}>
            {(r) => (
              <li>
                <button
                  type="button"
                  class="ref-item w-full text-left font-mono text-xs"
                  onMouseDown={(e) => e.preventDefault()}
                  onClick={() => {
                    props.onPick(r.name);
                    setOpen(false);
                    stream.cancel();
                  }}
                >
                  {r.name}
                </button>
              </li>
            )}
          </For>
        </ul>
      </Show>
    </div>
  );
}

/** The live base…head preview: status line + head-only commits. */
function ComparePreview(props) {
  const pv = () => props.preview;
  return (
    <div class="rounded-lg border border-zinc-200 bg-zinc-50/60 p-3 dark:border-zinc-800 dark:bg-zinc-900/40" aria-live="polite">
      <Show when={pv().status === "idle"}>
        <p class="muted text-sm">Pick a base and a head branch to preview the comparison.</p>
      </Show>
      <Show when={pv().status === "same-names"}>
        <p class="muted text-sm">Base and head are the same ref — pick two different branches.</p>
      </Show>
      <Show when={pv().status === "loading"}>
        <p class="muted text-sm">Comparing {props.base} … {props.head}…</p>
      </Show>
      <Show when={pv().status === "error"}>
        <p class="err-line text-sm" role="alert">{pv().error}</p>
      </Show>
      <Show when={pv().status === "same"}>
        <p class="muted text-sm">
          <code class="font-mono">{shortSha(pv().headSha)}</code> — both refs point at the same commit, nothing to merge.
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
            {" "}· base <code class="font-mono">{shortSha(pv().baseSha)}</code> · head{" "}
            <code class="font-mono">{shortSha(pv().headSha)}</code>
          </span>
        </p>
        <Show when={pv().truncatedHead || pv().truncatedBase}>
          <p class="warn-line">History past the {PREVIEW_WINDOW}-commit window — counts are lower bounds.</p>
        </Show>
        <Show
          when={pv().unique.length > 0}
          fallback={<p class="muted mt-2 text-sm">Head is already merged into base — opening would be a no-op.</p>}
        >
          <ul class="mt-2 grid gap-1.5">
            <For each={pv().unique.slice(0, 10)}>
              {(c) => (
                <li class="flex min-w-0 items-baseline gap-2 text-sm">
                  <A
                    class="shrink-0 font-mono text-xs text-emerald-700 hover:underline dark:text-emerald-400"
                    href={`/${props.full}/commit/${c.sha}`}
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
  const [getBase, setBase] = createSignal(search.base || "refs/heads/main");
  const [getHead, setHead] = createSignal(search.head || "");
  const [getBusy, setBusy] = createSignal(false);
  const [getPreview, setPreview] = createSignal({ status: "idle" });

  // Live preview: debounce keystrokes, fetch both histories in parallel,
  // split the head at the base SHA. One AbortController per run, torn
  // down in onCleanup; stale completions are dropped by run id.
  let run = 0;
  createEffect(() => {
    const base = getBase().trim();
    const head = getHead().trim();
    if (!base || !head) {
      setPreview({ status: "idle" });
      return;
    }
    if (base === head) {
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
        const [headPage, basePage] = await Promise.all([
          ctx.repoClient.commits({ ref: toShortRef(head), n: PREVIEW_WINDOW }, { signal: ctl.signal }),
          ctx.repoClient.commits({ ref: toShortRef(base), n: PREVIEW_WINDOW }, { signal: ctl.signal }),
        ]);
        if (id !== run || ctl.signal.aborted) return;
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
        // Prefill from the head branch until the user edits: title from
        // the tip subject, body from the head-only subject list.
        const tip = tipSubject(headPage?.commits?.[0]);
        if (tip && !getTitleTouched()) setTitle(tip);
        if (!getBodyTouched() && cmp.unique.length > 0) {
          setBody(cmp.unique.slice(0, 20).map((cc) => `- ${tipSubject(cc) || "(no message)"}`).join("\n"));
        }
      } catch (err) {
        if (id !== run || ctl.signal.aborted) return;
        const msg = err?.status === 404 ? "no such ref — check the branch names" : String(err?.message ?? err);
        setPreview({ status: "error", error: `Couldn't compare: ${msg}` });
      }
    }, 300);
    onCleanup(() => {
      clearTimeout(timer);
      ctl.abort();
    });
  });

  const swap = () => {
    const b = getBase();
    setBase(getHead());
    setHead(b);
  };

  const open = async (e) => {
    e.preventDefault();
    if (getBusy() || !getTitle().trim() || !getHead().trim()) return;
    setBusy(true);
    try {
      const res = await ctx.repoClient.pulls.open({
        title: getTitle().trim(),
        body: getBody().trim() || undefined,
        base_ref: getBase().trim(),
        head_ref: getHead().trim(),
      });
      const num = res.thread?.num ?? res.pr?.num;
      navigate(`/${ctx.full}/pull/${num}`);
    } catch (err) {
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
                <label class="field">
                  <span>Base ref</span>
                  <RefSelect repo={ctx.repoClient} value={getBase} onPick={setBase} label="base ref" placeholder="refs/heads/main" />
                </label>
                <button
                  type="button"
                  class="btn px-2 py-1"
                  onClick={swap}
                  title="Swap base and head"
                  aria-label="Swap base and head"
                >
                  ⇄
                </button>
                <label class="field">
                  <span>Head ref</span>
                  <RefSelect repo={ctx.repoClient} value={getHead} onPick={setHead} label="head ref" placeholder="refs/heads/feature" />
                </label>
              </div>
              <ComparePreview preview={getPreview()} base={getBase().trim()} head={getHead().trim()} full={ctx.full} />
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
            <div>
              <button type="submit" class="btn primary" disabled={getBusy() || !getTitle().trim() || !getHead().trim()}>
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
