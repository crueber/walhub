// web/src/pages/tree.js — Code tab: tree at ref/path via the §9.2 resolve → sha
// chain (§2.4), breadcrumbs, entry listing, README markdown-lite preview.

import { createRoot, createEffect, el } from "lib/reactive.js";
import { useResolved } from "lib/data.js";
import { renderMarkdown } from "lib/markdown.js";
import { sanitize } from "lib/sanitize.js";
import { shortRef } from "./repo.js";

function breadcrumb(full, rest, head) {
  const parts = rest ? rest.split("/") : [];
  const crumbs = [el("a", { href: `/${full}` }, head ?? "root")];
  parts.forEach((part, i) => {
    const sub = parts.slice(0, i + 1).join("/");
    crumbs.push(" / ", i === parts.length - 1 ? el("strong", {}, part) : el("a", { href: `/${full}/tree/${sub}` }, part));
  });
  return el("nav", { class: "crumbs" }, ...crumbs);
}

export function mount(container, ctx) {
  return createRoot((dispose) => {
    const { full, rest } = ctx;
    const [getTree] = useResolved(ctx.owner, ctx.name, rest || "", "tree");
    const root = el("div", { class: "tree-page" });
    container.append(root);

    createEffect(() => {
      const t = getTree();
      if (!t) {
        root.replaceChildren(el("p", { class: "muted" }, "loading tree…"));
        return;
      }
      const treeRest = (t.ref ? `${shortRef(t.ref)}` : "") + (t.path ? `/${t.path}` : "");
      const rows = (t.entries ?? []).map((e) =>
        el("tr", {},
          el("td", { class: "entry-icon" }, e.type === "tree" ? "📁" : e.type === "commit" ? " submodule" : "📄"),
          el("td", { class: "entry-name" },
            e.type === "blob"
              ? el("a", { href: `/${full}/blob/${treeRest}/${e.name}` }, e.name)
              : el("a", { href: `/${full}/tree/${treeRest}/${e.name}` }, e.name)),
          el("td", { class: "entry-mode muted" }, e.mode ?? ""),
          el("td", { class: "entry-size muted tabular" }, e.type === "blob" ? String(e.size ?? "-") : "")));

      const table = el("table", { class: "grid tree-table" },
        el("thead", {}, el("tr", {}, el("th", {}, ""), el("th", {}, "name"), el("th", {}, "mode"), el("th", {}, "size"))),
        el("tbody", {}, ...rows));
      root.replaceChildren(
        breadcrumb(full, t.path ?? "", shortRef(t.ref)),
        el("p", { class: "muted" }, `${String(t.sha).slice(0, 12)} · ${(t.entries ?? []).length} entries`),
        table);

      const readme = t.readme;
      if (readme?.contents) {
        const mdBody = el("div", { class: "markdown-body" });
        root.append(el("section", { class: "readme card" }, el("h2", {}, readme.name), mdBody));
        mdBody.innerHTML = sanitize(renderMarkdown(readme.contents)); // the sanitizer is the innerHTML gate (§2.2)
      }
    });
    return dispose;
  });
}
