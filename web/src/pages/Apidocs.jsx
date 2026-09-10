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
  ["POST", "/api/v1/repos", "explicit create → placeholder (write; 201 / 200 already:true / 409 with winner URL)"],
  ["GET|PUT|DELETE", "/{o}/{r}/api", "repo summary (SWR, +placeholder projection when empty) · create (write; ?placeholder=true) · delete (admin)"],
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
  ["GET", "/{o}/{r}/api/checks?after=&n=", "paged checks index (read; no-store; n default 50, max 200)"],
  ["GET", "/{o}/{r}/api/checks/{sha}", "combined worst-of state + per-state counts (read; no-store)"],
  ["GET", "/{o}/{r}/api/checks/statuses/{sha}", "per-context statuses for one sha (read; no-store)"],
  ["POST", "/{o}/{r}/api/checks/statuses/{sha}", "report one context result (repo write, or wct_ CI token with checks:write — § Checks CI below)"],
  ["GET|POST", "/{o}/{r}/api/checks/tokens", "CI tokens (admin): list without secrets · mint → 201 {id, token, scopes}, secret shown once"],
  ["DELETE", "/{o}/{r}/api/checks/tokens/{id}", "revoke a CI token (admin; 204, idempotent)"],
  ["GET", "/{o}/{r}/info/refs?service=…", "git smart HTTP v0/v2 advertisement"],
  ["POST", "/{o}/{r}/git-upload-pack", "fetch"],
  ["POST", "/{o}/{r}/git-receive-pack", "push"],
];

const sdkSnippet = `<script type="module">
import repos from "https://<host>/repos.js";
repos.configure({ token: "…" }).repo("demo/hello").tree("main", "").then(console.log);
<\/script>`;

// Worked CI flow for the Checks section: mint a wct_ token as admin, report
// one context from CI, read the combined view (shapes pinned by
// internal/checks http/service tests; routes pinned by TestExposedCoversRoutes).
const ciSnippet = `# 1. mint a CI token (repo admin; secret shown once)
curl -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \\
  -d '{"name":"ci","scopes":["checks:write"]}' \\
  https://<host>/demo/hello/api/checks/tokens
# -> {"id":"abcd1234","token":"wct_abcd1234.<secret>","scopes":["checks:write"]}
# 2. post a result from CI
curl -H "Authorization: Bearer wct_abcd1234.<secret>" -H "Content-Type: application/json" \\
  -d '{"context":"ci/build","state":"success","target_url":"https://ci.example/r/1","description":"build #1"}' \\
  https://<host>/demo/hello/api/checks/statuses/<sha>
# -> 200 {"sha":"…","context":"ci/build","state":"success",…}
# 3. read the combined view (read access; anonymous on public repos)
curl https://<host>/demo/hello/api/checks/<sha>
# -> {"sha":"…","state":"success","total_counts":{…},"statuses":[…]}`;

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
              <table class="data-table">
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
        {/* Issue #275: path codes scroll in place, the page never pans. */}
        <div class="overflow-x-auto">
        <table class="data-table">
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
        </div>
      </section>

      <section id="checks-ci" class="card mb-4 p-4">
        <h3 class="mb-2 font-semibold">Checks — reporting from external CI</h3>
        <div class="space-y-2 text-sm">
          <p class="muted">
            External CI posts per-commit results without a user credential: a repo
            admin mints a scoped <code class="font-mono text-xs">wct_</code> token,
            CI reports with it, anyone with read access views the combined state
            (the <code class="font-mono text-xs">/checks</code> page and the PR
            merge gate consume the same views). All checks GETs are{" "}
            <code class="font-mono text-xs">no-store</code> — sha-addressed does
            NOT mean immutable here; never cache them long. Errors are plain
            text: anonymous-denied reads get a real{" "}
            <code class="font-mono text-xs">401</code> with{" "}
            <code class="font-mono text-xs">WWW-Authenticate: Bearer</code>,
            authenticated-but-insufficient gets{" "}
            <code class="font-mono text-xs">403</code>.
          </p>
          <ul class="list-disc space-y-1 pl-6">
            <li>
              <strong>Auth.</strong> Reads need read access.{" "}
              <code class="font-mono text-xs">POST …/checks/statuses/{"{sha}"}</code>{" "}
              needs the repo <em>write</em> role <em>or</em> a{" "}
              <code class="font-mono text-xs">wct_&lt;id&gt;.&lt;secret&gt;</code>{" "}
              token with <code class="font-mono text-xs">checks:write</code> for
              this repo (send as{" "}
              <code class="font-mono text-xs">Authorization: Bearer</code>, Basic
              password, or{" "}
              <code class="font-mono text-xs">X-Walgit-Authorization</code>).
              Token mint/list/revoke need <em>admin</em>.
            </li>
            <li>
              <strong>Report shape.</strong>{" "}
              <code class="font-mono text-xs">POST …/checks/statuses/{"{sha}"}</code>{" "}
              takes{" "}
              <code class="font-mono text-xs">{"{context, state, target_url?, description?, started_at?, completed_at?}"}</code>{" "}
              (strict: unknown keys are{" "}
              <code class="font-mono text-xs">400</code>;{" "}
              <code class="font-mono text-xs">state</code> is one of{" "}
              <code class="font-mono text-xs">pending|success|failure|error</code>,
              anything else is <code class="font-mono text-xs">409</code>;
              unknown sha is <code class="font-mono text-xs">404</code>) and
              answers <code class="font-mono text-xs">200</code> with the full
              status record{" "}
              <code class="font-mono text-xs">{"{sha, context, state, target_url?, description?, started_at?, completed_at?, creator, created_at, updated_at, version}"}</code>.
            </li>
            <li>
              <strong>Read shapes.</strong>{" "}
              <code class="font-mono text-xs">GET …/checks/{"{sha}"}</code> →{" "}
              <code class="font-mono text-xs">{"{sha, state, total_counts, statuses}"}</code>{" "}
              (worst-of: error {" > "} failure {" > "} pending {" > "} success;
              zero contexts ⇒ <code class="font-mono text-xs">pending</code>).{" "}
              <code class="font-mono text-xs">GET …/checks/statuses/{"{sha}"}</code>{" "}
              → <code class="font-mono text-xs">{"{sha, statuses}"}</code>{" "}
              (context-sorted).{" "}
              <code class="font-mono text-xs">GET …/checks</code> →{" "}
              <code class="font-mono text-xs">{"{checks, more}"}</code>.
            </li>
          </ul>
          <p class="muted">Worked example (create token → post a status → read combined):</p>
          <pre class="code-view overflow-x-auto p-3">{ciSnippet}</pre>
          <p class="muted">
            The shipped SDK wraps all of it:{" "}
            <code class="font-mono text-xs">repo.checks.list/combined/statuses/report</code>{" "}
            and <code class="font-mono text-xs">repo.ciTokens.create/list/revoke</code>{" "}
            (same wire surface, both lanes).
          </p>
        </div>
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
