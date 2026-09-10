// web/src/pages/Tree.jsx — Code tab: tree at ref/path via the §9.2 resolve →
// sha chain, breadcrumbs, entry listing, directory-docs markdown tabs.

import { createSignal, createEffect, onCleanup, For, Show, Switch, Match } from "solid-js";
import { A } from "@solidjs/router";
import { useResolved, useData } from "../lib/data.js";
import { renderBody } from "../lib/render-md.js";
import { docCandidates, defaultDocFile, docFetchArgs, docSlug, docFromHash } from "../lib/doctabs.js";
import { fmtSize, fmtMode, fmtSizeParts } from "../lib/format.js";
import DateTime from "../components/DateTime.jsx";
import { useRepo, shortRef } from "./Repo.jsx";
import { EmptyRepoGuide, DegradedNotice } from "../components/EmptyRepoGuide.jsx";

function Breadcrumb(props) {
  const parts = () => (props.path ? props.path.split("/") : []);
  return (
    <Show when={parts().length > 0}>
    <nav class="crumbs mb-2 text-sm">
      <A class="text-emerald-700 hover:underline dark:text-emerald-400" href={`/${props.full}`}>root</A>
      <For each={parts()}>
        {(part, i) => {
          const sub = () => parts().slice(0, i() + 1).join("/");
          return (
            <>
              {" / "}
              <Show when={i() === parts().length - 1} fallback={
                <A class="text-emerald-700 hover:underline dark:text-emerald-400" href={`/${props.full}/tree/${shortRef(props.rev)}/${sub()}`}>{part}</A>
              }>
                <strong>{part}</strong>
              </Show>
            </>
          );
        }}
      </For>
    </nav>
    </Show>
  );
}

// DocTabs (issue #170): the current directory's *.md/*.markdown files as
// client-side tabs below the file list — README (case-insensitive) first and
// default-selected, rest alphabetical (lib/doctabs.js, unit-tested). Tab
// bodies render through the blob MD pipeline (renderBody: marked + DOMPurify);
// non-selected bodies fetch lazily through the existing blob endpoint keyed
// on the commit sha (immutable → shared with the blob page's cache entry),
// so the pre-filled probed readme costs zero round trips and each newly
// opened tab costs one after. The tree payload's probed readme pre-fills its
// tab with no fetch. Blobs over the JSON cap answer too_large and render
// the same cap note as Blob.jsx.
function DocTabs(props) {
  // props: entries, readme, dirPath, rev (commit sha), repoClient.
  const list = () => docCandidates(props.entries, props.readme);
  const head = () => defaultDocFile(list());
  const [getSel, setSel] = createSignal(null); // explicit choice; null = head
  let lastDir = null;
  createEffect(() => {
    const key = `${props.rev ?? ""}@${props.dirPath ?? ""}`;
    const files = list();
    if (key !== lastDir) {
      // New directory: honor a deep-link #anchor when it names a tab here,
      // else fall back to the README-first head.
      lastDir = key;
      const hash = typeof location !== "undefined" ? location.hash : "";
      setSel(docFromHash(hash, files) ?? head());
    } else if (getSel() != null && !files.includes(getSel())) {
      setSel(head()); // refetch renamed the file out from under us
    }
  });
  const sel = () => getSel() ?? head();

  const select = (name) => {
    setSel(name);
    try {
      history.replaceState(null, "", `#${docSlug(name)}`);
    } catch {
      /* non-browser (SSR/tests): selection still applies */
    }
  };

  // Roving tabindex: arrows move selection and focus, Home/End jump.
  const onKeys = (e) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key)) return;
    e.preventDefault();
    const files = list();
    if (!files.length) return;
    let i = files.indexOf(sel());
    if (e.key === "ArrowRight") i = (i + 1) % files.length;
    else if (e.key === "ArrowLeft") i = (i - 1 + files.length) % files.length;
    else if (e.key === "Home") i = 0;
    else i = files.length - 1;
    select(files[i]);
    e.currentTarget.querySelector(`[data-index="${i}"]`)?.focus();
  };

  const selPath = () => ((props.dirPath ? `${props.dirPath}/` : "") + (sel() ?? ""));
  // Rendered-tab link context (issue #182): the file's own coordinates, so
  // relative images/links resolve against the repo at the page's display
  // ref — the same ref string the tree links above use (sha fallback).
  // props: owner, repo, docRef (display short-ref; named to dodge Solid's
  // reserved `ref` prop, which the compiler drops on components),
  // dirPath (the file's dir).
  const mdCtx = () => ({ owner: props.owner, repo: props.repo, ref: props.docRef, dir: props.dirPath ?? "" });
  // The tab body's blob coordinates (issue #172): the ONLY acceptable
  // revision is the tree payload's resolved commit sha — never a UI display
  // string (ref names split the blob route's {rev} segment and 404). Null
  // means "do not fetch": loading renders with no doomed request, no tray.
  const selArgs = () => (sel() ? docFetchArgs(props.rev, props.dirPath ?? "", sel()) : null);
  const [getDoc] = useData(
    () => (selArgs() ? `sha:${selArgs().rev}:blob:${selPath()}` : "doctabs:none"),
    () => {
      if (!sel()) return Promise.resolve(null);
      // The tree payload already carries the probed readme: no fetch for it.
      const pre = props.readme?.name === sel() ? props.readme.contents : undefined;
      if (pre != null) {
        return Promise.resolve({ name: sel(), path: selPath(), size: pre.length, contents: pre });
      }
      const args = selArgs();
      if (!args) return Promise.resolve(null); // unresolved rev: never fetch a display string
      return props.repoClient.blob(args.rev, args.path);
    },
    Infinity, // sha-addressed payloads are immutable
  );

  return (
    <Show when={list().length > 0}>
      <section class="doctabs card mt-4" aria-label="directory documentation">
        <div
          class="seg flex flex-wrap items-center gap-1.5 border-b border-zinc-200 px-3 py-2 dark:border-zinc-800"
          role="tablist"
          aria-label="markdown files in this directory"
          onKeyDown={onKeys}
        >
          <For each={list()}>
            {(name, i) => (
              <button
                type="button"
                role="tab"
                id={`doctab-${i()}`}
                data-index={i()}
                aria-selected={sel() === name}
                aria-controls="doctab-panel"
                tabindex={sel() === name ? "0" : "-1"}
                title={name}
                class="pill cursor-pointer"
                classList={{ "!border-emerald-500 !text-emerald-600 dark:!text-emerald-400": sel() === name }}
                onClick={() => select(name)}
              >
                {name}
              </button>
            )}
          </For>
        </div>
        <Show when={sel()} fallback={<p class="muted p-4">loading…</p>}>
          <Show when={getDoc() != null} fallback={<p class="muted p-4">loading…</p>}>
            <Switch>
              <Match when={getDoc()?.too_large}>
                <p class="muted italic p-6 text-center">
                  This file is too large to render (<span title={getDoc()?.size != null ? `${getDoc().size} bytes` : undefined}>{getDoc()?.size == null ? "?" : fmtSize(getDoc().size)}</span>; the render cap is 2 MiB).
                  Fetch it raw from the API.
                </p>
              </Match>
              <Match when={getDoc()?.binary}>
                <p class="muted italic p-6 text-center">binary file, <span title={getDoc()?.size != null ? `${getDoc().size} bytes` : undefined}>{getDoc()?.size == null ? "?" : fmtSize(getDoc().size)}</span></p>
              </Match>
              <Match when={true}>
                {/* the sanitizer is the innerHTML gate (§2.2) */}
                <div
                  class="markdown-body p-4"
                  role="tabpanel"
                  id="doctab-panel"
                  aria-label={sel()}
                  innerHTML={renderBody(getDoc()?.contents ?? "", mdCtx())}
                />
              </Match>
            </Switch>
          </Show>
        </Show>
      </section>
    </Show>
  );
}

export default function Tree() {
  const ctx = useRepo();
  const [getTree] = useResolved(() => ctx.owner, () => ctx.name, () => ctx.rest || "", "tree");

  // Issue #252: publish the resolved ref so the header pill follows the
  // viewed ref (the summary Head is ref-blind by design — client
  // composition, zero new fetches: this reuses the resolve step above, and
  // its 5s SWR is what moves the pill when the viewed branch is pushed).
  // Sha-addressed views resolve with an empty ref — published as-is, the
  // pill then shows the short sha honestly. Cleared on unmount so non-ref
  // tabs fall back to the summary head; loading states keep the previous
  // value (no flash back to the default branch mid-navigation).
  createEffect(() => {
    const t = getTree();
    if (t && t.sha && !t.empty && !t.degraded) ctx.setViewed({ name: t.ref ?? "", sha: t.sha });
  });
  onCleanup(() => ctx.setViewed(null));

  // Issue #301 per-entry last-commit column: every row renders its own
  // commit_time from the tree payload (one batched server-side log walk per
  // listing, capped by server.max_tree_log). Entries with no date
  // (submodules, capped walks) show a muted em-dash — never the repo HEAD
  // date as a stand-in (the old treeDate()-for-all-rows proxy). No extra
  // fetch: the dates ride the tree payload itself.

  return (
    <div class="tree-page">
      <Show when={getTree()} fallback={<p class="muted">loading tree…</p>}>
        {(t) => {
          const treeRest = () => (t().ref ? `${shortRef(t().ref)}` : "") + (t().path ? `/${t().path}` : "");
          return (
          <>
            {/* Empty repo: the guided state, not an error (issue #209). The
                resolve + tree fetches never issued — zero toasts by
                construction. Any tree/* path lands on the same guide. */}
            <Show when={t().empty}>
              <EmptyRepoGuide full={ctx.full} summary={ctx.summary?.()} repoClient={ctx.repoClient} />
            </Show>
            {/* Degraded repo, object missing: inline notice, never a toast. */}
            <Show when={t().degraded}>
              <DegradedNotice full={ctx.full} cacheKey={`sha:${t().sha}:tree:${t().path ?? ""}`} />
            </Show>
            <Show when={!t().empty && !t().degraded}>
              <>
                <Breadcrumb full={ctx.full} path={t().path ?? ""} rev={t().ref} />
                {/* Issue #223: no header row — the columns (mode, size,
                    icon + name, last-modified) are self-evident. No type
                    column either (mode lead char + icons carry it). The
                    right-side columns hug their content (see .tree-table in
                    src/ui.css); only the name column takes spare width.
                    Issue #275: the table scrolls inside an overflow-x-auto
                    wrapper so a 390px page never pans; the mode column
                    hides below sm: (icons carry the kind on phones). */}
                <div class="overflow-x-auto">
                <table class="data-table tree-table">
                  <tbody>
                    <For each={t().entries ?? []}>
                      {(e) => {
                        // Split size columns (#211): number + unit cells; dirs
                        // and submodules carry size -1 (no stamp), a null blob
                        // size keeps the old "-" placeholder. Exact bytes stay
                        // in the title tooltip, as before.
                        const parts = () => (e.type === "blob" && e.size != null ? fmtSizeParts(e.size) : null);
                        return (
                        <tr>
                          <td class="entry-mode muted font-mono text-xs hidden sm:table-cell" title={e.mode ?? undefined}>{fmtMode(e.mode, e.type)}</td>
                          <td class="entry-size-num muted tabular text-right text-xs" title={e.type === "blob" && e.size != null ? `${e.size} bytes` : undefined}>{parts() ? parts().num : e.type === "blob" ? "-" : ""}</td>
                          <td class="entry-size-unit muted text-xs">{parts()?.unit ?? ""}</td>
                          <td class="entry-icon">{e.type === "tree" ? "📁" : e.type === "commit" ? "↗" : "📄"}</td>
                          <td class="entry-name">
                            <Show
                              when={e.type === "blob"}
                              fallback={<A class="text-emerald-700 hover:underline dark:text-emerald-400" href={`/${ctx.full}/tree/${treeRest()}/${e.name}`}>{e.name}</A>}
                            >
                              <A class="text-emerald-700 hover:underline dark:text-emerald-400" href={`/${ctx.full}/blob/${treeRest()}/${e.name}`}>{e.name}</A>
                            </Show>
                          </td>
                          <td class="entry-date muted text-right text-xs"><DateTime value={e.commit_time} fallback="—" /></td>
                        </tr>
                        );
                      }}
                    </For>
                  </tbody>
                </table>
                </div>
                <DocTabs
                  entries={t().entries}
                  readme={t().readme}
                  dirPath={t().path ?? ""}
                  rev={t().sha}
                  repoClient={ctx.repoClient}
                  owner={ctx.owner}
                  repo={ctx.name}
                  docRef={shortRef(t().ref) || t().sha}
                />
              </>
            </Show>
          </>
          );
        }}
      </Show>
    </div>
  );
}
