// web/src/pages/Blob.jsx — Code tab blob view (§2.7 decision tree):
// too_large → placeholder; binary → "binary file, {human size}"; .md → Preview|Code
// toggle (markdown-lite + sanitizer in preview, tokenizer in code view); else
// line-numbered <pre> tinted by the mini tokenizer. Raw deep link comes from
// the SDK's urls builder (§1.1) — no hand-built API URLs.

import { createSignal, For, Show, Switch, Match } from "solid-js";
import { A } from "@solidjs/router";
import { useResolved } from "../lib/data.js";
import { renderMarkdown } from "../lib/markdown.js";
import { sanitize } from "../lib/sanitize.js";
import { fmtSize } from "../lib/format.js";
import { languageFor, highlight } from "../lib/highlight.js";
import { useRepo, shortRef } from "./Repo.jsx";

function Breadcrumb(props) {
  const parts = () => (props.path ? props.path.split("/") : []);
  return (
    <Show when={parts().length > 0}>
      <nav class="crumbs mb-2 text-sm">
        <For each={parts()}>
          {(part, i) => {
            const sub = () => parts().slice(0, i() + 1).join("/");
            return (
              <>
                <Show when={i() > 0}>{" / "}</Show>
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

export default function Blob() {
  const ctx = useRepo();
  const [getBlob] = useResolved(ctx.owner, ctx.name, ctx.rest ?? "", "blob");
  const [getView, setView] = createSignal("preview");

  return (
    <div class="blob-page">
      <Show when={getBlob()} fallback={<p class="muted">loading blob…</p>}>
        {(b) => {
          const name = () => b().name ?? (b().path ?? "").split("/").pop() ?? "";
          const lang = () => languageFor(name());
          const isMd = () => /\.(md|markdown)$/i.test(name());
          const rawHref = () =>
            b().ref ? ctx.repoClient.urls.raw(shortRef(b().ref), b().path ?? "") : undefined;
          const lines = () => {
            const text = b().contents ?? "";
            const ls = text.split("\n");
            if (ls.length > 1 && ls[ls.length - 1] === "") ls.pop();
            return ls;
          };

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
                      {/* the sanitizer is the only innerHTML gate (§2.2) */}
                      <div class="markdown-body p-4" innerHTML={sanitize(renderMarkdown(b().contents ?? ""))} />
                    </Show>
                  </Match>
                  <Match when={true}>
                    <div class="blob-cols flex overflow-x-auto">
                      <pre class="blob-gutter tabular select-none border-r border-zinc-200 bg-zinc-100 px-2.5 py-3 text-right font-mono text-xs leading-5 text-zinc-400 dark:border-zinc-800 dark:bg-zinc-900 dark:text-zinc-500">
                        {lines().map((_, i) => i + 1).join("\n")}
                      </pre>
                      <pre class="code-view m-0 rounded-none border-0 px-3.5 py-3 flex-1 min-w-0">
                        <code innerHTML={highlight(b().contents ?? "", lang())} />
                      </pre>
                    </div>
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
