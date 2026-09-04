// web/src/pages/Tree.jsx — Code tab: tree at ref/path via the §9.2 resolve →
// sha chain, breadcrumbs, entry listing, README markdown-lite preview.

import { For, Show } from "solid-js";
import { A } from "@solidjs/router";
import { useResolved } from "../lib/data.js";
import { renderMarkdown } from "../lib/markdown.js";
import { sanitize } from "../lib/sanitize.js";
import { fmtSize } from "../lib/format.js";
import { useRepo, shortRef } from "./Repo.jsx";

function Breadcrumb(props) {
  const parts = () => (props.path ? props.path.split("/") : []);
  return (
    <nav class="crumbs mb-2 text-sm">
      <A class="text-emerald-700 hover:underline dark:text-emerald-400" href={`/${props.full}`}>
        {props.head ?? "root"}
      </A>
      <For each={parts()}>
        {(part, i) => {
          const sub = () => parts().slice(0, i() + 1).join("/");
          return (
            <>
              {" / "}
              <Show when={i() === parts().length - 1} fallback={
                <A class="text-emerald-700 hover:underline dark:text-emerald-400" href={`/${props.full}/tree/${sub()}`}>{part}</A>
              }>
                <strong>{part}</strong>
              </Show>
            </>
          );
        }}
      </For>
    </nav>
  );
}

export default function Tree() {
  const ctx = useRepo();
  const [getTree] = useResolved(() => ctx.owner, () => ctx.name, () => ctx.rest || "", "tree");

  return (
    <div class="tree-page">
      <Show when={getTree()} fallback={<p class="muted">loading tree…</p>}>
        {(t) => {
          const treeRest = () => (t().ref ? `${shortRef(t().ref)}` : "") + (t().path ? `/${t().path}` : "");
          return (
            <>
              <Breadcrumb full={ctx.full} path={t().path ?? ""} head={shortRef(t().ref)} />
              <p class="muted mb-2 text-xs">{String(t().sha).slice(0, 12)} · {(t().entries ?? []).length} entries</p>
              <table class="data-table tree-table">
                <thead>
                  <tr><th class="w-8" /><th>name</th><th class="w-24">mode</th><th class="w-24">size</th></tr>
                </thead>
                <tbody>
                  <For each={t().entries ?? []}>
                    {(e) => (
                      <tr>
                        <td class="entry-icon">{e.type === "tree" ? "📁" : e.type === "commit" ? "↗" : "📄"}</td>
                        <td class="entry-name">
                          <Show
                            when={e.type === "blob"}
                            fallback={<A class="text-emerald-700 hover:underline dark:text-emerald-400" href={`/${ctx.full}/tree/${treeRest()}/${e.name}`}>{e.name}</A>}
                          >
                            <A class="text-emerald-700 hover:underline dark:text-emerald-400" href={`/${ctx.full}/blob/${treeRest()}/${e.name}`}>{e.name}</A>
                          </Show>
                        </td>
                        <td class="entry-mode muted font-mono text-xs">{e.mode ?? ""}</td>
                        <td class="entry-size muted tabular text-xs" title={e.type === "blob" && e.size != null ? `${e.size} bytes` : undefined}>{e.type === "blob" ? (e.size == null ? "-" : fmtSize(e.size)) : ""}</td>
                      </tr>
                    )}
                  </For>
                </tbody>
              </table>
              <Show when={t().readme?.contents}>
                <section class="readme card mt-4 p-4">
                  <h2 class="mb-2 font-semibold">{t().readme.name}</h2>
                  {/* the sanitizer is the innerHTML gate (§2.2) */}
                  <div class="markdown-body" innerHTML={sanitize(renderMarkdown(t().readme.contents))} />
                </section>
              </Show>
            </>
          );
        }}
      </Show>
    </div>
  );
}
