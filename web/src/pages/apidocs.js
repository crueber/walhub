// web/src/pages/apidocs.js — route "/api": the API surface rendered from the
// discovery contract (07_api.md §8). Static, no fetch — the discovery document
// lives at GET /api/v1 for machines.

export function mount(container) {
  const routes = [
    ["GET", "/api/v1", "discovery document (endpoints, capabilities)"],
    ["GET", "/api/v1/owners", "owners on this host, sorted, from the store"],
    ["GET", "/api/v1/owners/{o}/repos", "short repo names of one owner"],
    ["GET|PUT|DELETE", "/{o}/{r}/api", "repo summary (SWR) · create (write) · delete (admin)"],
    ["GET", "/{o}/{r}/api/refs", "O(1) default-branch head"],
    ["GET", "/{o}/{r}/api/refs/{branches|tags}", "paged ref list (prefix/q/after/n) — SSE dialect available"],
    ["GET", "/{o}/{r}/api/resolve[/{rest}]", "ref/path split → {ref, sha, path, kind} (SWR)"],
    ["GET", "/{o}/{r}/api/tree/{rev}[/{path}]", "tree listing (immutable at full sha)"],
    ["GET", "/{o}/{r}/api/blob/{rev}/{path}", "blob (2 MiB cap → too_large; NUL/invalid UTF-8 → binary)"],
    ["GET", "/{o}/{r}/api/commits", "history: ?ref=&path=&skip=&n= (immutable at sha refs)"],
    ["GET", "/{o}/{r}/api/commit/{sha}", "commit detail: {commit, stats[], patch}"],
    ["GET|PUT|DELETE", "/{o}/{r}/api/policy", "push policy (admin; 400 with reasons, fail closed)"],
    ["POST", "/{o}/{r}/api/policy/validate", "validate a policy payload → {ok, errors[]}"],
    ["POST", "/{o}/{r}/api/policy/dry-run?last=N", "replay the last N pushes under the policy"],
    ["GET|PUT|DELETE", "/{o}/{r}/api/settings", "per-repo settings TOML (≤ 16 KiB; 4 allowed sections)"],
    ["GET", "/{o}/{r}/api/settings/effective", "effective [bundles]/[maintenance]/[compaction]/[upstream] as TOML"],
    ["GET", "/{o}/{r}/api/settings/history", "per-revision settings history"],
    ["GET", "/{o}/{r}/api/settings/describe", "strategies, host facts, upstream follow, effective fields"],
    ["GET", "/{o}/{r}/api/overview", "WAL health: manifest, local copy, packs, bundles, plan, compactions"],
    ["GET", "/{o}/{r}/api/ops", "available ops + recent tasks + bundle strategies"],
    ["POST", "/{o}/{r}/api/ops/{op}", "run an op (SSE attach; tasks)"],
    ["GET", "/{o}/{r}/api/tasks", "running + recent tasks (no-store)"],
    ["GET", "/{o}/{r}/api/tasks/{id}", "task record JSON, or SSE attach with Accept: text/event-stream"],
    ["GET", "/{o}/{r}/info/refs?service=…", "git smart HTTP v0/v2 advertisement"],
    ["POST", "/{o}/{r}/git-upload-pack", "fetch"],
    ["POST", "/{o}/{r}/git-receive-pack", "push"],
  ];
  const el = (t, attrs, ...kids) => {
    const n = document.createElement(t);
    for (const [k, v] of Object.entries(attrs ?? {})) if (v != null) n.setAttribute(k, v);
    n.append(...kids.flat().filter(Boolean));
    return n;
  };
  const root = el("div", { class: "apidocs-page" },
    el("h2", {}, "API"),
    el("p", { class: "muted" }, "JSON API with the SSE envelope; every GET accepts application/json, text/event-stream. Errors are plain text. Null-safe: empty arrays, never null (except a repo's head)."),
    el("section", { class: "card" },
      el("h3", {}, "Cache classes (§9.2)"),
      el("ul", {},
        el("li", {}, el("strong", {}, "sha-addressed"), " — full 40/64-hex in {rev}: private, immutable, cache forever (the UI caches them with ttl = Infinity)."),
        el("li", {}, el("strong", {}, "ref-dependent"), " — owners/refs/resolve and name-addressed reads: max-age=0 + stale-while-revalidate + ETag = resolved sha."))),
    el("section", { class: "card" },
      el("h3", {}, "Routes"),
      el("table", { class: "grid" },
        el("thead", {}, el("tr", {}, el("th", {}, "method"), el("th", {}, "path"), el("th", {}, "what"))),
        el("tbody", {}, ...routes.map(([m, p, d]) =>
          el("tr", {}, el("td", {}, m), el("td", {}, el("code", {}, p)), el("td", { class: "muted" }, d)))))),
    el("section", { class: "card" },
      el("h3", {}, "SDK"),
      el("p", {}, "Third-party integrations import ", el("code", {}, "/repos.js"), " as an ES module — the same wire surface this UI dogfoods:"),
      el("pre", { class: "op-log" }, `<script type="module">
import repos from "https://<host>/repos.js";
repos.configure({ token: "…" }).repo("demo/hello").tree("main", "").then(console.log);
<\/script>`)));
  container.append(root);
  return () => root.remove();
}
