// web/src/pages/commits.js — Commits tab list: §9.2 resolve → commits?ref={sha}
// (immutable, §2.4), pagination via ?skip=, path-limited history via ?path=.

import { createRoot, createEffect, el } from "lib/reactive.js";
import { useResolved } from "lib/data.js";
import { fmtDate, shortRef } from "./repo.js";

export function commitRow(full, c) {
  return el("div", { class: "commit-row" },
    el("div", { class: "commit-main" },
      el("a", { class: "commit-subject", href: `/${full}/commit/${c.sha}` }, c.subject ?? "(no message)"),
      el("div", { class: "muted commit-meta" },
        el("span", {}, c.author ?? ""),
        c.author_email ? el("span", {}, ` <${c.author_email}>`) : null,
        el("span", {}, ` · ${fmtDate(c.author_date)}`),
        c.parents?.length ? el("span", {}, ` · parent ${c.parents[0].slice(0, 10)}`) : null,
        (c.trailers?.length ?? 0) > 0 ? el("span", { class: "pill" }, `${c.trailers.length} trailers`) : null)),
    el("code", { class: "sha" }, String(c.sha).slice(0, 12)));
}

export function mount(container, ctx) {
  return createRoot((dispose) => {
    const { full } = ctx;
    const query = new URLSearchParams(location.search);
    const skip = Number(query.get("skip") ?? 0) || 0;
    const path = query.get("path") ?? "";
    const rest = ctx.rest || query.get("ref") || "";

    const [getHistory] = useResolved(ctx.owner, ctx.name, rest, "commits");
    const root = el("div", { class: "commits-page" });
    container.append(root);

    createEffect(() => {
      const h = getHistory();
      if (!h) {
        root.replaceChildren(el("p", { class: "muted" }, "loading history…"));
        return;
      }
      const commits = h.commits ?? [];
      const rows = commits.map((c) => commitRow(full, c));
      const more = h.more
        ? el("a", { class: "pill", href: `/${full}/commits?${new URLSearchParams({ ...(path ? { path } : {}), skip: String(skip + commits.length) })}` }, "older →")
        : null;
      root.replaceChildren(
        el("div", { class: "crumbs" },
          el("a", { href: `/${full}` }, shortRef(h.ref)),
          path ? el("span", {}, ` · history of ${path}`) : null),
        el("div", { class: "commit-list" }, ...rows),
        more ? el("div", { class: "pager" }, more) : null);
    });
    return dispose;
  });
}
