// web/src/pages/Apidocs.jsx — route "/api": the API surface rendered from the
// discovery contract (07_api.md §8). The vanilla page was static; the port adds
// the live `GET /api/v1` discovery document via the dogfood SDK (§6), rendered
// beside the static route table, cache classes and SDK snippet.
//
// D-WEB-6: SolidJS port of pages/apidocs.js (same content, Solid + Tailwind).

import { For, Show } from "solid-js";
import repos from "../../sdk/src/index.js";
import { useData } from "../lib/data.js";

// The static route table of §8 (07_api.md) — kept verbatim from apidocs.js.
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

const sdkSnippet = `<script type="module">
import repos from "https://<host>/repos.js";
repos.configure({ token: "…" }).repo("demo/hello").tree("main", "").then(console.log);
<\/script>`;

export default function Apidocs() {
  // Live discovery document: GET /api/v1 through the default SDK client.
  const [getDiscovery] = useData("discovery", () => repos.discovery());

  return (
    <div class="apidocs-page">
      <h2 class="mb-1 text-xl font-semibold">API</h2>
      <p class="muted mb-4">
        JSON API with the SSE envelope; every GET accepts application/json,
        text/event-stream. Errors are plain text. Null-safe: empty arrays, never
        null (except a repo's head).
      </p>

      <section class="card mb-4 p-4">
        <h3 class="mb-2 font-semibold">Discovery (GET /api/v1)</h3>
        <Show when={getDiscovery()} fallback={<p class="muted">loading discovery…</p>}>
          {(d) => (
            <div class="space-y-2 text-sm">
              <p>
                <strong>{d().name}</strong> · API version{" "}
                <span class="pill">{d().version}</span>
              </p>
              <table class="grid">
                <tbody>
                  <tr>
                    <td class="w-32">base</td>
                    <td><code class="font-mono text-xs">{d().base}</code></td>
                  </tr>
                  <tr>
                    <td>sdk</td>
                    <td><code class="font-mono text-xs">{d().sdk}</code></td>
                  </tr>
                  <tr>
                    <td>bearer</td>
                    <td class="muted"><code class="font-mono text-xs">{d().auth?.bearer}</code></td>
                  </tr>
                  <tr>
                    <td>setup recipes</td>
                    <td><code class="font-mono text-xs">{d().auth?.setup}</code></td>
                  </tr>
                  <tr>
                    <td>browser lane</td>
                    <td class="muted"><code class="font-mono text-xs">{d().auth?.browser}</code></td>
                  </tr>
                </tbody>
              </table>
              <Show when={(d().endpoints ?? []).length > 0}>
                <ul class="flex flex-wrap gap-1">
                  <For each={d().endpoints}>
                    {(e) => <li class="chip font-mono">{e}</li>}
                  </For>
                </ul>
              </Show>
            </div>
          )}
        </Show>
      </section>

      <section class="card mb-4 p-4">
        <h3 class="mb-2 font-semibold">Cache classes (§9.2)</h3>
        <ul class="list-disc space-y-1 pl-6 text-sm">
          <li>
            <strong>sha-addressed</strong> — full 40/64-hex in {"{rev}"}: private,
            immutable, cache forever (the UI caches them with ttl = Infinity).
          </li>
          <li>
            <strong>ref-dependent</strong> — owners/refs/resolve and name-addressed
            reads: max-age=0 + stale-while-revalidate + ETag = resolved sha.
          </li>
        </ul>
      </section>

      <section class="card mb-4 p-4">
        <h3 class="mb-2 font-semibold">Routes</h3>
        <table class="grid">
          <thead>
            <tr><th class="w-40">method</th><th>path</th><th>what</th></tr>
          </thead>
          <tbody>
            <For each={routes}>
              {([m, p, desc]) => (
                <tr>
                  <td class="font-mono text-xs">{m}</td>
                  <td><code class="font-mono text-xs">{p}</code></td>
                  <td class="muted">{desc}</td>
                </tr>
              )}
            </For>
          </tbody>
        </table>
      </section>

      <section class="card p-4">
        <h3 class="mb-2 font-semibold">SDK</h3>
        <p class="mb-2 text-sm">
          Third-party integrations import <code>/repos.js</code> as an ES module —
          the same wire surface this UI dogfoods:
        </p>
        <pre class="code-view p-3">{sdkSnippet}</pre>
      </section>
    </div>
  );
}
