// web/src/pages/owners.js — route "/": every owner on the host (store-backed list).

import repos from "repos";
import { createRoot, createEffect, el } from "lib/reactive.js";
import { useData } from "lib/data.js";

export function mount(container) {
  return createRoot((dispose) => {
    const [getOwners] = useData("owners", () => repos.owners.list());
    const root = el("div", { class: "owners-page" }, el("h2", {}, "Owners"));
    container.append(root);
    createEffect(() => {
      const owners = getOwners();
      if (!owners) {
        root.append(el("p", { class: "muted" }, "loading…"));
        return;
      }
      root.replaceChildren(
        el("h2", {}, "Owners"),
        ...(owners ?? []).length
          ? [el("ul", { class: "owner-list" }, ...owners.map((o) =>
              el("li", {}, el("a", { class: "owner-link", href: `/${o}` }, o))))]
          : [el("p", { class: "muted" }, "no repositories yet — push one, or use the API to create it")]);
    });
    return dispose;
  });
}
