// web/src/pages/commit.js — commit detail (§2.8): sha-addressed (immutable) fetch,
// hand-rolled unified-diff parser, per-file unified/split toggle, file anchors by
// stats[].path, linkified body, grouped trailers.

import repos from "repos";
import { createRoot, createEffect, createSignal, el } from "lib/reactive.js";
import { useData, SHA_TTL } from "lib/data.js";
import { parsePatchFiles, splitRows, linkifyBody, groupTrailers, trailerValue } from "lib/diff.js";
import { fmtDate } from "./repo.js";

function fileAnchor(path) {
  return `f-${path.replace(/[^a-zA-Z0-9_-]/g, "-")}`;
}

function diffTable(file, mode) {
  const table = el("table", { class: `diff ${mode}` });
  const tbody = el("tbody");
  table.append(tbody);
  if (file.isBinary) {
    tbody.append(el("tr", {}, el("td", { colspan: mode === "split" ? "2" : "1", class: "placeholder" }, "Binary file not shown")));
    return table;
  }
  if (mode === "split") {
    for (const row of splitRows([].concat(...file.hunks.map((h) => h.lines)))) {
      tbody.append(el("tr", {},
        row.left ? el("td", { class: `d-${row.left.t}` }, row.left.text || " ") : el("td", { class: "d-blank" }),
        row.right ? el("td", { class: `d-${row.right.t}` }, row.right.text || " ") : el("td", { class: "d-blank" })));
    }
  } else {
    for (const h of file.hunks) {
      tbody.append(el("tr", { class: "hunk-header" }, el("td", {}, `@@ -${h.oldStart},${h.oldLines} +${h.newStart},${h.newLines} @@ ${h.context ?? ""}`)));
      for (const l of h.lines) tbody.append(el("tr", {}, el("td", { class: `d-${l.t}` }, l.text || " ")));
    }
  }
  return table;
}

function trailerTable(full, groups) {
  return el("details", { class: "trailers" },
    el("summary", { class: "pill", title: "Commit trailers (machine-readable footer lines)" },
      `${groups.reduce((n, g) => n + g.trailers.length, 0)} trailers`),
    el("table", { class: "grid trailers-table" },
      ...groups.flatMap(({ group, trailers }) => [
        el("tr", { class: "trailers-group" }, el("td", { colspan: "2" }, el("strong", {}, group))),
        ...trailers.map((t) => {
          const v = trailerValue(t.value);
          return el("tr", {},
            el("td", { class: "trailer-key" }, t.key),
            el("td", { class: "trailer-value" },
              v.sha ? el("a", { class: "sha", href: `/${full}/commit/${v.sha}`, title: "May not exist here (a CI boundary commit)" }, v.sha.slice(0, 12)) : null,
              v.email ? el("span", {}, `${v.name ?? ""} `, el("a", { href: `mailto:${v.email}` }, `<${v.email}>`)) : null,
              v.text ?? ""));
        }),
      ])));
}

export function mount(container, ctx) {
  return createRoot((dispose) => {
    const { full, sha } = ctx;
    const [getCommit] = useData(`commit:${full}:${sha}`, () => repos.repo(full).commit(sha), SHA_TTL);
    const root = el("div", { class: "commit-page" });
    container.append(root);

    createEffect(() => {
      const data = getCommit();
      if (!data) {
        root.replaceChildren(el("p", { class: "muted" }, "loading commit…"));
        return;
      }
      const c = data.commit ?? {};
      const stats = data.stats ?? [];
      const { files } = parsePatchFiles(data.patch ?? "", c.sha);
      const totalAdd = stats.reduce((n, s) => n + (s.additions ?? 0), 0);
      const totalDel = stats.reduce((n, s) => n + (s.deletions ?? 0), 0);
      const groups = groupTrailers(c.trailers);

      const fileNav = el("nav", { class: "file-nav" },
        el("span", { class: "muted" }, "files: "),
        ...files.map((f) => el("a", { href: `#${fileAnchor(f.path)}`, class: "pill" }, f.path)));

      const filesEl = el("div", { class: "commit-files" });
      for (const f of files) {
        const [getMode, setMode] = createSignal("unified");
        const headerFlags = [
          f.added ? "added" : null, f.deleted ? "deleted" : null,
          f.isBinary ? "binary" : null, f.oldPath && !f.added && !f.deleted ? `renamed from ${f.oldPath}` : null,
        ].filter(Boolean).join(", ");
        const card = el("section", { class: "card diff-file", id: fileAnchor(f.path) },
          el("div", { class: "diff-file-head" },
            el("h3", {}, f.path, headerFlags ? el("span", { class: "muted" }, ` (${headerFlags})`) : null),
            el("span", { class: "diffstat" },
              el("span", { class: "add" }, `+${f.additions ?? 0}`), " ",
              el("span", { class: "del" }, `−${f.deletions ?? 0}`)),
            el("div", { class: "seg" },
              el("button", { type: "button", class: "pill", onclick: () => setMode("unified") }, "Unified"),
              el("button", { type: "button", class: "pill", onclick: () => setMode("split") }, "Split"))),
          (() => {
            const holder = el("div", { class: "diff-holder" });
            createEffect(() => {
              holder.replaceChildren(diffTable(f, getMode()));
            });
            return holder;
          })());
        filesEl.append(card);
      }

      root.replaceChildren(
        el("div", { class: "crumbs" }, el("a", { href: `/${full}/commits` }, "Commits"), " / ", el("code", { class: "sha" }, String(c.sha ?? sha).slice(0, 12))),
        el("section", { class: "card commit-head" },
          el("h2", {}, c.subject ?? "(no message)"),
          el("div", { class: "muted commit-meta" },
            el("span", {}, c.author ?? ""),
            c.author_email ? el("span", {}, ` <${c.author_email}>`) : null,
            el("span", {}, ` · authored ${fmtDate(c.author_date)} · committed ${fmtDate(c.committer_date ?? c.commit_date)}`),
            c.parents?.length ? el("span", {}, " · parent ", ...c.parents.map((p) => el("a", { class: "sha", href: `/${full}/commit/${p}` }, p.slice(0, 12)))) : null),
          c.body ? el("div", { class: "commit-body" }) : null,
          groups.length ? trailerTable(full, groups) : null,
          el("p", { class: "muted diffstat" }, el("span", { class: "add" }, `+${totalAdd}`), " ", el("span", { class: "del" }, `−${totalDel}`), ` across ${stats.length} file${stats.length === 1 ? "" : "s"} (server stats)`)),
        files.length ? fileNav : null,
        filesEl);

      // linkified body (escape-first linkification from lib/diff.js)
      if (c.body) root.querySelector(".commit-body").innerHTML = linkifyBody(c.body, `/${full}`);
    });
    return dispose;
  });
}
