// web/src/pages/Blob.jsx — Code tab blob view (§2.7 decision tree):
// too_large → placeholder; binary → "binary file, {human size}"; .md → Preview|Code
// toggle (marked GFM + DOMPurify in preview, tokenizer in code view); else
// line-numbered <pre> tinted by the mini tokenizer. Raw deep link comes from
// the SDK's urls builder (§1.1) — no hand-built API URLs.

import { createSignal, createEffect, onCleanup, untrack, For, Show, Switch, Match } from "solid-js";
import { A } from "@solidjs/router";
import { useResolved } from "../lib/data.js";
import { renderBody } from "../lib/render-md.js";
import { fmtSize } from "../lib/format.js";
import { languageFor, highlight } from "../lib/highlight.js";
import { splitLines, parseLineHash, lineHash, dragRange, sameSelection } from "../lib/blob-lines.js";
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
              <Show when={i() < parts().length - 1} fallback={<strong>{part}</strong>}>
                <A
                  class="text-emerald-700 hover:underline dark:text-emerald-400"
                  href={`/${props.full}/tree/${shortRef(props.rev)}/${sub()}`}
                >
                  {part}
                </A>
              </Show>
            </>
          );
        }}
      </For>
    </nav>
    </Show>
  );
}

// Per-line code table with GitHub-style #L selection (issue #243). One
// <table> keeps each gutter number + code line in the same <tr> so the
// columns can never desync; the scroll container wraps the whole table so a
// long line scrolls both columns together. Code is tokenized PER LINE
// because the mini tokenizer's block-comment spans can cross newlines
// (verified: highlight("/* foo\nbar */") wraps the newline inside one
// <span>) — splitting highlighted HTML mid-span would break tags, and an
// unclosed /* fragment simply renders plain on its line.
// Selection model (Solid signals, no library): mousedown on a gutter number
// sets the anchor, mouseover extends the focus (drag), mouseup ends the
// drag; the click then pushes ONE history entry. During the drag each frame
// uses replaceState (no history spam, URL still shareable mid-drag).
// Shift-click extends from the existing anchor. Code cells carry no
// handlers, so normal text selection in the code area is untouched.
function CodeLines(props) {
  const lines = () => splitLines(props.text);
  const [getSel, setSel] = createSignal(parseLineHash(window.location.hash));
  let anchor = getSel()?.start ?? null;
  let dragging = false;

  const readHash = () => {
    const next = parseLineHash(window.location.hash);
    if (!sameSelection(next, getSel())) {
      setSel(next);
      anchor = next?.start ?? null;
    }
  };

  // Re-highlight + scroll once this file's rows exist. Tracks lines() only:
  // the sel read + scroll MUST stay untracked, or every drag frame would
  // yank the scroll back to the anchor instead of following the pointer.
  // Blob→blob navigations change lines(), so the new file re-reads the hash
  // after its rows render; a hash pointing past EOF matches no row and
  // scrolls nowhere.
  createEffect(() => {
    const ls = lines();
    const next = parseLineHash(window.location.hash);
    untrack(() => {
      if (!sameSelection(next, getSel())) {
        setSel(next);
        anchor = next?.start ?? null;
      }
      if (next && ls.length >= next.start) {
        document.getElementById(`L${next.start}`)?.scrollIntoView({ block: "center" });
      }
    });
  });

  const preview = (a, f) => {
    const r = dragRange(a, f);
    if (!r) return;
    setSel(r);
    window.history.replaceState(null, "", lineHash(r.start, r.end));
  };

  const onNumMouseDown = (n, ev) => {
    if (ev.button !== 0) return;
    anchor = ev.shiftKey && anchor != null ? anchor : n;
    dragging = true;
    preview(anchor, n);
    ev.preventDefault();
  };

  const onNumMouseOver = (n) => {
    if (dragging) preview(anchor, n);
  };

  const endDrag = () => {
    dragging = false;
  };

  const onNumClick = (n, ev) => {
    ev.preventDefault();
    // Keyboard Enter/Space arrives as a click with detail 0 and no drag
    // before it — plain Enter jumps to the focused line, Shift+Enter extends
    // from the anchor. A mouse click lands after the mousedown-drag
    // previewed the range, so push that range as-is.
    const r = ev.detail === 0 ? dragRange(ev.shiftKey ? anchor ?? n : n, n) : getSel();
    if (r) {
      if (ev.detail === 0) {
        anchor = ev.shiftKey ? anchor ?? n : n;
        setSel(r);
      }
      window.location.hash = lineHash(r.start, r.end);
    }
  };

  window.addEventListener("hashchange", readHash);
  window.addEventListener("mouseup", endDrag);
  window.addEventListener("blur", endDrag);
  onCleanup(() => {
    window.removeEventListener("hashchange", readHash);
    window.removeEventListener("mouseup", endDrag);
    window.removeEventListener("blur", endDrag);
  });

  return (
    <div class="blob-cols flex overflow-x-auto">
      <table class="blob-table" aria-label="File content by line">
        <tbody>
          <For each={lines()}>
            {(line, i) => {
              const n = i() + 1;
              // Self-contained per line (see note above); empty lines get a
              // <br> so the row keeps its height without a selectable char.
              const html = highlight(line, props.lang) || "<br />";
              const inSel = () => {
                const s = getSel();
                return !!s && n >= Math.min(s.start, s.end) && n <= Math.max(s.start, s.end);
              };
              return (
                <tr id={`L${n}`} class="blob-row" classList={{ "line-hl": inSel() }}>
                  <td class="blob-num">
                    <a
                      href={`#L${n}`}
                      data-line={n}
                      aria-label={`Line ${n}`}
                      onMouseDown={(ev) => onNumMouseDown(n, ev)}
                      onMouseOver={() => onNumMouseOver(n)}
                      onClick={(ev) => onNumClick(n, ev)}
                    >
                      {n}
                    </a>
                  </td>
                  <td class="blob-code">
                    <code innerHTML={html} />
                  </td>
                </tr>
              );
            }}
          </For>
        </tbody>
      </table>
    </div>
  );
}

export default function Blob() {
  const ctx = useRepo();
  // Getters, not setup-time values: @solidjs/router reuses this component on
  // blob→blob navigations, and useResolved captures plain values at setup —
  // static args would freeze the view on the first path (same class as #38;
  // Tree.jsx already passes getters).
  const [getBlob] = useResolved(() => ctx.owner, () => ctx.name, () => ctx.rest ?? "", "blob");
  // Issue #252: same viewed-ref publication as Tree.jsx (resolve reuse, sha
  // views publish an empty name → the pill shows the short sha).
  createEffect(() => {
    const b = getBlob();
    if (b && b.sha && !b.empty && !b.degraded) ctx.setViewed({ name: b.ref ?? "", sha: b.sha });
  });
  onCleanup(() => ctx.setViewed(null));
  const [getView, setView] = createSignal("preview");

  return (
    <div class="blob-page">
      <Show when={getBlob()} fallback={<p class="muted">loading blob…</p>}>
        {(b) => {
          // Empty repo → guide (issue #209); degraded + missing → inline notice.
          if (b().empty) return <EmptyRepoGuide full={ctx.full} summary={ctx.summary?.()} />;
          if (b().degraded) return <DegradedNotice full={ctx.full} cacheKey={`sha:${b().sha}:blob:${b().path ?? ""}`} />;
          const name = () => b().name ?? (b().path ?? "").split("/").pop() ?? "";
          const lang = () => languageFor(name());
          const isMd = () => /\.(md|markdown)$/i.test(name());
          const rawHref = () =>
            b().ref ? ctx.repoClient.urls.raw(shortRef(b().ref), b().path ?? "") : undefined;

          return (
            <>
              <Breadcrumb full={ctx.full} path={b().path ?? ""} rev={b().ref} />
              <div class="blob-head mb-2 flex flex-wrap items-baseline gap-2">
                <h2 class="font-semibold">{name()}</h2>
                <Show when={lang()}>
                  <span class="chip">{lang()}</span>
                </Show>
                <span class="muted tabular text-xs" title={b().size != null ? `${b().size} bytes` : undefined}>{b().size == null ? "?" : fmtSize(b().size)} · {String(b().sha).slice(0, 12)}</span>
                <Show when={rawHref()}>
                  <a class="pill cursor-pointer hover:no-underline ml-auto" href={rawHref()} target="_blank" rel="noopener">
                    raw
                  </a>
                </Show>
              </div>

              <div class="blob-body card overflow-hidden">
                <Switch>
                  <Match when={b().too_large}>
                    <p class="muted italic p-6 text-center">
                      This file is too large to render (<span title={b().size != null ? `${b().size} bytes` : undefined}>{b().size == null ? "?" : fmtSize(b().size)}</span>; the render cap is 2 MiB).
                      Fetch it raw from the API.
                    </p>
                  </Match>
                  <Match when={b().binary}>
                    <p class="muted italic p-6 text-center">binary file, <span title={b().size != null ? `${b().size} bytes` : undefined}>{b().size == null ? "?" : fmtSize(b().size)}</span></p>
                  </Match>
                  <Match when={isMd()}>
                    <div class="seg flex items-center gap-1.5 border-b border-zinc-200 px-3 py-2 dark:border-zinc-800">
                      <button
                        type="button"
                        class="pill cursor-pointer"
                        classList={{ "!border-emerald-500 !text-emerald-600 dark:!text-emerald-400": getView() === "preview" }}
                        onClick={() => setView("preview")}
                      >
                        Preview
                      </button>
                      <button
                        type="button"
                        class="pill cursor-pointer"
                        classList={{ "!border-emerald-500 !text-emerald-600 dark:!text-emerald-400": getView() === "code" }}
                        onClick={() => setView("code")}
                      >
                        Code
                      </button>
                    </div>
                    <Show
                      when={getView() === "preview"}
                      fallback={
                        <pre class="code-view m-0 rounded-none border-0 flex-1 min-w-0">
                          <code innerHTML={highlight(b().contents ?? "", lang())} />
                        </pre>
                      }
                    >
                      {/* the sanitizer is the only innerHTML gate (§2.2); the md
                          context (issue #182) resolves relative images/links
                          against this file's own repo + display ref + dir */}
                      <div class="markdown-body p-4" innerHTML={renderBody(b().contents ?? "", {
                        owner: ctx.owner,
                        repo: ctx.name,
                        ref: shortRef(b().ref),
                        dir: String(b().path ?? "").split("/").slice(0, -1).join("/"),
                      })} />
                    </Show>
                  </Match>
                  <Match when={true}>
                    <CodeLines text={b().contents ?? ""} lang={lang()} />
                  </Match>
                </Switch>
              </div>
            </>
          );
        }}
      </Show>
    </div>
  );
}
