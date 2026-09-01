// web/src/pages/repos.js — route "/:owner": the owner's repositories.

import repos from "repos";
import { createRoot, createEffect, el } from "lib/reactive.js";
import { useData } from "lib/data.js";

export function mount(container, params) {
  return createRoot((dispose) => {
    const owner = params.owner;
    const [getRepos] = useData(`repos:${owner}`, () => repos.owners.repos(owner));
    const root = el("div", { class: "repos-page" });
    container.append(root);
    createEffect(() => {
      const names = getRepos();
      if (!names) {
        root.replaceChildren(el("h2", {}, owner), el("p", { class: "muted" }, "loading…"));
        return;
      }
      root.replaceChildren(
        el("h2", {}, owner),
        el("p", { class: "muted" }, `${(names ?? []).length} repositor${(names ?? []).length === 1 ? "y" : "ies"}`),
        ...(names ?? []).length
          ? [el("ul", { class: "repo-list" }, ...names.map((n) =>
              el("li", {}, el("a", { class: "repo-link", href: `/${owner}/${n}` }, `${owner}/${n}`))))]
          : [el("p", { class: "muted" }, `nothing under ${owner} yet`)]);
    });
    return dispose;
  });
}
