// web/src/pages/Apidocs.jsx — route "/api": the API surface rendered from the
// discovery contract (07_api.md §8). The vanilla page was static; the port adds
// the live `GET /api/v1` discovery document via the dogfood SDK (§6), rendered
// beside the static route table, cache classes and SDK snippet.
//
// D-WEB-6: SolidJS port of pages/apidocs.js (same content, Solid + Tailwind).

import { For, Show } from "solid-js";
import repos from "../../sdk/src/index.js";
import { useData } from "../lib/data.js";

// The static route table of §8 (07_api.md) — kept verbatim from apidocs.js,
// then extended per surface (Forgejo #271 checks, #272 everything else).
// It is DERIVED, not independent: every row's path shape matches one
// discovery endpoints[] template (GET /api/v1), and each feature package
// pins that correspondence both ways in TestExposedCoversRoutes — a served
// route missing from discovery fails CI. Both lanes (…/api/… and
// …/api-browser/…) serve every row; the table shows the /api spelling.
const routes = [
  ["GET", "/api/v1", "discovery document (endpoints, capabilities)"],
  ["GET", "/api/v1/owners", "owners on this host, sorted, from the store"],
  ["GET", "/api/v1/owners/{o}/repos", "short repo names of one owner"],
  ["POST", "/api/v1/repos", "explicit create → placeholder (write; 201 / 200 already:true / 409 with winner URL)"],
  ["POST", "/api/v1/repos/imports", "start (or join) a URL import → 202 {task, target} (write; idempotent no-op 200)"],
  ["GET", "/api/v1/repos/imports/{id}", "import status (read)"],
  ["POST", "/api/v1/repos/mirrors", "create a repo from a URL as a pull mirror (admin)"],
  ["GET|PUT|DELETE", "/{o}/{r}/api/mirror", "mirror sidecar: open read · replace/delete (admin)"],
  ["GET", "/{o}/{r}/api/mirror/sync", "last sync status (open read, like GET mirror)"],
  ["POST", "/{o}/{r}/api/mirror/sync", "sync now (admin)"],
  ["GET|PUT", "/api/v1/users/{principal}", "profile (read; self or admin to edit)"],
  ["GET|POST", "/api/v1/orgs", "orgs (read; write to create)"],
  ["GET|PUT|DELETE", "/api/v1/orgs/{org}", "org (read; owner to change; DELETE 204)"],
  ["GET", "/api/v1/orgs/{org}/members", "members (read)"],
  ["GET|PUT|DELETE", "/api/v1/orgs/{org}/members/{principal}", "membership (read; owner to change)"],
  ["GET|POST", "/api/v1/orgs/{org}/teams", "teams (read; owner to create)"],
  ["GET|PUT|DELETE", "/api/v1/orgs/{org}/teams/{slug}", "team (read; owner to change; DELETE 204)"],
  ["PUT|DELETE", "/api/v1/orgs/{org}/teams/{slug}/members/{principal}", "team membership (owner)"],
  ["GET|POST", "/api/v1/orgs/{org}/invitations", "org invites (owner)"],
  ["DELETE", "/api/v1/orgs/{org}/invitations/{id}", "cancel org invite (owner; 204)"],
  ["GET", "/api/v1/invitations", "my invitations (authenticated)"],
  ["GET|DELETE", "/api/v1/invitations/{id}", "preview (?token=) · decline (authenticated; DELETE 204)"],
  ["POST", "/api/v1/invitations/{id}/accept", "accept → {bound} (authenticated)"],
  ["GET|PUT", "/{o}/{r}/api/access", "access doc (triage read; admin replace with version CAS)"],
  ["GET", "/{o}/{r}/api/permissions", "my resolved role {role} (read)"],
  ["GET", "/{o}/{r}/api/collaborators", "effective bindings + source (read)"],
  ["GET", "/{o}/{r}/api/assignables", "mentions autocomplete source (read)"],
  ["GET|POST", "/{o}/{r}/api/invitations", "repo invites (admin)"],
  ["DELETE", "/{o}/{r}/api/invitations/{id}", "cancel repo invite (admin; 204)"],
  ["GET|POST", "/{o}/{r}/api/issues", "issues: list (?state=&labels=&assignee=&milestone=&since=&after=&n=, read) · create (authenticated + read)"],
  ["GET|PATCH", "/{o}/{r}/api/issues/{num}", "issue thread (no-cache + ETag) · patch (author or triage)"],
  ["GET", "/{o}/{r}/api/issues/{num}/events", "event tail (no-store)"],
  ["POST", "/{o}/{r}/api/issues/{num}/comments", "comment (authenticated + read)"],
  ["POST", "/{o}/{r}/api/issues/{num}/reactions", "react {target_event_seq, content} (authenticated + read)"],
  ["DELETE", "/{o}/{r}/api/issues/{num}/reactions/{seq}/{content}", "unreact (204)"],
  ["GET|POST", "/{o}/{r}/api/labels", "labels (read) · create (triage)"],
  ["PATCH|DELETE", "/{o}/{r}/api/labels/{name}", "update · delete → {threads_affected}"],
  ["GET|POST", "/{o}/{r}/api/milestones", "milestones (?state=, read) · create (triage)"],
  ["GET|PATCH|DELETE", "/{o}/{r}/api/milestones/{id}", "milestone, 6-hex id (read · triage · triage)"],
  ["POST", "/{o}/{r}/api/attachments", "upload attachment (authenticated + read; multipart)"],
  ["GET|HEAD", "/{o}/{r}/attachments/{sha}/{name}", "attachment bytes (static contract: ETag, ranges)"],
  ["GET|POST", "/{o}/{r}/api/pulls", "PRs: list (?state=, read) · open (write)"],
  ["GET|PUT", "/{o}/{r}/api/pulls/{num}", "PR detail (read) · update (author or triage)"],
  ["GET", "/{o}/{r}/api/pulls/{num}/diff", "unified diff (read)"],
  ["GET", "/{o}/{r}/api/pulls/{num}/commits", "PR commits (read)"],
  ["POST", "/{o}/{r}/api/pulls/{num}/comments", "timeline comment (authenticated + read)"],
  ["POST", "/{o}/{r}/api/pulls/{num}/merge", "merge (maintain; 202 task — poll …/merge/task)"],
  ["GET", "/{o}/{r}/api/pulls/{num}/merge/task", "merge task record (read; SSE attach like tasks)"],
  ["POST", "/{o}/{r}/api/pulls/{num}/update-branch", "fast-forward head onto base (write; 202 task)"],
  ["DELETE", "/{o}/{r}/api/pulls/{num}/head", "delete the merged head branch (maintain)"],
  ["POST", "/api/v1/repos/{o}/{r}/forks", "fork (write on parent; 202 task)"],
  ["GET|POST", "/{o}/{r}/api/pulls/{num}/reviews", "reviews (read) · submit {state, body, commit_sha?, threads?} (authenticated + read)"],
  ["GET", "/{o}/{r}/api/pulls/{num}/reviews/{seq}", "one review (read)"],
  ["POST", "/{o}/{r}/api/pulls/{num}/reviews/{seq}/dismiss", "dismiss (maintain)"],
  ["GET|POST", "/{o}/{r}/api/pulls/{num}/threads", "threads (?resolved=, read) · open {anchor, body} (authenticated + read)"],
  ["GET", "/{o}/{r}/api/pulls/{num}/threads/{id}", "thread + comments (read)"],
  ["POST", "/{o}/{r}/api/pulls/{num}/threads/{id}/comments", "reply (authenticated + read)"],
  ["POST", "/{o}/{r}/api/pulls/{num}/threads/{id}/resolve", "resolve (author or triage)"],
  ["POST", "/{o}/{r}/api/pulls/{num}/threads/{id}/unresolve", "unresolve (author or triage)"],
  ["GET|POST|DELETE", "/{o}/{r}/api/pulls/{num}/review-requests", "requested reviewers (read; triage to change)"],
  ["GET", "/{o}/{r}/api/pulls/{num}/review-suggest", "reviewer suggestions (?q=, read)"],
  ["GET", "/{o}/{r}/api/releases", "releases (?n=&after=, mutable no-cache + ETag)"],
  ["GET", "/{o}/{r}/api/releases/latest", "latest pointer (?include_prereleases=1)"],
  ["GET", "/{o}/{r}/api/releases/autodraft?tag=", "changelog draft from merged PRs (tag required)"],
  ["GET|PUT|DELETE", "/{o}/{r}/api/releases/{tag}", "release (read) · upsert {name, body, draft?, prerelease?} (write; If-Match) · delete (maintain)"],
  ["POST|DELETE", "/{o}/{r}/api/releases/{tag}/assets/{name}", "upload (write; X-Walgit-Asset-Sha256 required — § Releases) · delete (write)"],
  ["GET|HEAD", "/{o}/{r}/releases/{tag}/assets/{name}", "asset bytes (immutable, ETag, ranges)"],
  ["POST", "/{o}/{r}/api/tags", "create lightweight/annotated tag at a commit (write; 201, 409 when present)"],
  ["PUT|DELETE", "/{o}/{r}/api/star", "star / unstar (authenticated; unstar always works)"],
  ["GET", "/{o}/{r}/api/social", "counts + viewer flags (mutable no-cache + ETag)"],
  ["GET", "/api/v1/me/starred", "my stars (authenticated, no-store)"],
  ["GET", "/api/v1/users/{principal}/starred", "their stars (read)"],
  ["GET|PUT|DELETE", "/{o}/{r}/api/watch", "watch state: get (authenticated) · set/clear (authenticated + read)"],
  ["GET", "/api/v1/notifications", "tray (?state=&after=&n=, authenticated, no-store)"],
  ["GET", "/api/v1/notifications/unread_count", "O(1) unread count (authenticated)"],
  ["POST", "/api/v1/notifications/read_all", "mark the index window read → {updated} (authenticated)"],
  ["GET", "/api/v1/notifications/stream", "per-user SSE notification frames (authenticated; § Streams)"],
  ["POST", "/api/v1/notifications/{id}/read", "mark read (foreign ids 404, authenticated)"],
  ["POST", "/api/v1/notifications/{id}/unread", "mark unread (authenticated)"],
  ["GET|POST", "/{o}/{r}/api/webhooks", "webhooks without secrets (admin) · create (admin, 201)"],
  ["GET|PATCH|DELETE", "/{o}/{r}/api/webhooks/{id}", "webhook (admin; DELETE 204)"],
  ["POST", "/{o}/{r}/api/webhooks/{id}/ping", "test delivery → {delivery} (admin)"],
  ["GET", "/{o}/{r}/api/webhooks/{id}/deliveries", "recent deliveries (admin, no-store)"],
  ["GET", "/{o}/{r}/api/collab/stream", "repo activity SSE (read-gated; § Streams)"],
  ["GET|PUT|DELETE", "/{o}/{r}/api", "repo summary (SWR, +placeholder projection when empty; open_issues/open_pulls tab-badge counts) · create (write; ?placeholder=true) · delete (admin)"],
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

// Worked asset flow: upsert the header, upload bytes with their checksum,
// read the release back (shapes pinned by internal/releases http/service
// tests; routes pinned by TestExposedCoversRoutes).
const releaseSnippet = `# 1. upsert the header (repo write; If-Match optional for CAS)
curl -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \\
  -d '{"name":"v1.2","body":"notes","draft":false}' -X PUT \\
  https://<host>/demo/hello/api/releases/v1.2
# 2. upload bytes (checksum header required; 201 with browser_download_url)
SHA=$(sha256sum dist.zip | cut -d' ' -f1)
curl -H "Authorization: Bearer $TOKEN" -H "X-Walgit-Asset-Sha256: $SHA" \\
  --data-binary @dist.zip \\
  https://<host>/demo/hello/api/releases/v1.2/assets/dist.zip
# 3. bytes download immutable; JSON revalidates every read
curl https://<host>/demo/hello/releases/v1.2/assets/dist.zip`;

// Worked notification flow: read the tray, hold the SSE stream, flip one
// entry (shapes pinned by internal/notify http tests; routes pinned by
// TestExposedCoversRoutes).
const notifySnippet = `# 1. tray (authenticated; newest-first, ?state=unread&n=50)
curl -H "Authorization: Bearer $TOKEN" https://<host>/api/v1/notifications
# -> {"notifications":[…],"more":false}
# 2. hold the per-user stream (named SSE frames until disconnect)
curl -H "Authorization: Bearer $TOKEN" -H "Accept: text/event-stream" \\
  https://<host>/api/v1/notifications/stream
# 3. mark one entry read (foreign ids are 404, never 403)
curl -H "Authorization: Bearer $TOKEN" -X POST \\
  https://<host>/api/v1/notifications/<id>/read`;

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

      <section id="issues" class="card mb-4 p-4">
        <h3 class="mb-2 font-semibold">Issues, labels, milestones</h3>
        <div class="space-y-2 text-sm">
          <p class="muted">
            Threads with an immutable event tail. Reads need read access;
            create/comment/react need an authenticated principal with read
            access; patching honors author-or-triage per key; labels and
            milestones need <code class="font-mono text-xs">triage</code>.
            Threads are <code class="font-mono text-xs">no-cache</code> with a
            version ETag; the events tail is{" "}
            <code class="font-mono text-xs">no-store</code>. Unknown issue
            numbers are <code class="font-mono text-xs">404</code>; anonymous
            on a private repo is <code class="font-mono text-xs">401</code>.
          </p>
          <ul class="list-disc space-y-1 pl-6">
            <li>
              <strong>Shapes.</strong>{" "}
              <code class="font-mono text-xs">GET …/issues</code> →{" "}
              <code class="font-mono text-xs">{"{issues, more}"}</code>;{" "}
              <code class="font-mono text-xs">POST …/issues</code> takes{" "}
              <code class="font-mono text-xs">{"{title, body?}"}</code> →{" "}
              <code class="font-mono text-xs">201 {"{thread, event}"}</code>;{" "}
              <code class="font-mono text-xs">PATCH …/issues/{"{num}"}</code>{" "}
              takes{" "}
              <code class="font-mono text-xs">{"{title?, body?, state?, labels?, assignees?, milestone?}"}</code>.
              Reactions take{" "}
              <code class="font-mono text-xs">{"{target_event_seq, content}"}</code>{" "}
              (duplicate add is <code class="font-mono text-xs">200</code>{" "}
              with the summary, first add is{" "}
              <code class="font-mono text-xs">201</code>). Attachment upload
              is multipart <code class="font-mono text-xs">POST</code>; bytes
              download at{" "}
              <code class="font-mono text-xs">/{`{o}/{r}`}/attachments/{`{sha}/{name}`}</code>{" "}
              (static contract, like release assets).
            </li>
          </ul>
          <p class="muted">
            SDK:{" "}
            <code class="font-mono text-xs">repo.issues.list/create/get/patch/comment/events</code>
            ,{" "}
            <code class="font-mono text-xs">repo.issues.reactions.add/remove</code>
            , <code class="font-mono text-xs">repo.labels.*</code>,{" "}
            <code class="font-mono text-xs">repo.milestones.*</code>.
          </p>
        </div>
      </section>

      <section id="pulls" class="card mb-4 p-4">
        <h3 class="mb-2 font-semibold">Pulls and review</h3>
        <div class="space-y-2 text-sm">
          <p class="muted">
            PRs ride the same numbering/thread/index family as issues. Reads
            need read access; opening, forking, and commenting need{" "}
            <code class="font-mono text-xs">write</code>; PR update honors
            author-or-triage; merge and head-delete need{" "}
            <code class="font-mono text-xs">maintain</code> (the merge gate
            also consults required reviews + checks). Merge, update-branch,
            and fork answer <code class="font-mono text-xs">202</code> with a
            task — poll <code class="font-mono text-xs">…/merge/task</code>{" "}
            (same SSE-attach envelope as{" "}
            <code class="font-mono text-xs">…/api/tasks/{"{id}"}</code>).
            Review reads need read access; submitting needs authenticated +
            read; review-request changes need{" "}
            <code class="font-mono text-xs">triage</code>; dismiss needs{" "}
            <code class="font-mono text-xs">maintain</code>; resolve honors
            author-or-triage.
          </p>
          <ul class="list-disc space-y-1 pl-6">
            <li>
              <strong>Shapes.</strong>{" "}
              <code class="font-mono text-xs">POST …/pulls</code> takes{" "}
              <code class="font-mono text-xs">{"{title, base_ref, head_ref, body?, fork?}"}</code>;{" "}
              <code class="font-mono text-xs">POST …/merge</code> takes{" "}
              <code class="font-mono text-xs">{"{strategy, commit_title?, commit_message?, delete_head?}"}</code>;{" "}
              <code class="font-mono text-xs">POST …/reviews</code> takes{" "}
              <code class="font-mono text-xs">{"{state, body?, commit_sha?, threads?}"}</code>{" "}
              with <code class="font-mono text-xs">state</code> one of{" "}
              <code class="font-mono text-xs">approve|request-changes|comment</code>;
              thread anchors are{" "}
              <code class="font-mono text-xs">{"{path, side, old_start?, old_lines?, new_start?, new_lines?, commit_sha?}"}</code>.
            </li>
          </ul>
          <p class="muted">
            SDK: <code class="font-mono text-xs">repo.pulls.*</code> (open,
            merge, mergeTask, updateBranch, deleteHead, fork) and{" "}
            <code class="font-mono text-xs">repo.reviews.*</code> (reviews,
            threads, requests, suggest).
          </p>
        </div>
      </section>

      <section id="releases" class="card mb-4 p-4">
        <h3 class="mb-2 font-semibold">Releases and assets</h3>
        <div class="space-y-2 text-sm">
          <p class="muted">
            Release headers mutate by direct user action, so every
            version-keyed GET revalidates (
            <code class="font-mono text-xs">no-cache</code> + store-version
            ETag — never a stale window). Reads need read access; upsert and
            asset upload/delete need{" "}
            <code class="font-mono text-xs">write</code>; release delete
            needs <code class="font-mono text-xs">maintain</code>. Asset
            bytes are immutable and content-addressed (
            <code class="font-mono text-xs">public, immutable</code>, ranges,
            304). Tags literally named{" "}
            <code class="font-mono text-xs">latest</code> or{" "}
            <code class="font-mono text-xs">autodraft</code> stay visible in
            the list but their single-GET is shadowed by the pointer.
          </p>
          <p class="muted">Worked example (upsert → upload with checksum → read):</p>
          <pre class="code-view overflow-x-auto p-3">{releaseSnippet}</pre>
          <p class="muted">
            SDK: <code class="font-mono text-xs">repo.releases.*</code>{" "}
            (list, latest, autodraft, upsert, remove, uploadAsset,
            deleteAsset) plus{" "}
            <code class="font-mono text-xs">sha256Hex</code> for the upload
            header.
          </p>
        </div>
      </section>

      <section id="social" class="card mb-4 p-4">
        <h3 class="mb-2 font-semibold">Social: stars, watchers, forks</h3>
        <div class="space-y-2 text-sm">
          <p class="muted">
            Counters mutate on every star/watch/fork, so{" "}
            <code class="font-mono text-xs">GET …/social</code> revalidates
            every read (<code class="font-mono text-xs">no-cache</code> +
            counter ETag). Starring needs an authenticated principal with
            repo visibility; unstarring always works for authenticated
            callers. Watch mutation lives here too: get needs auth, set/clear
            need auth + read. Forks are counted through the pulls fork task.
          </p>
          <p class="muted">
            SDK: <code class="font-mono text-xs">repo.social.*</code> (star,
            unstar, counts) and{" "}
            <code class="font-mono text-xs">client.socialTop.*</code> (my /
            their starred lists).
          </p>
        </div>
      </section>

      <section id="streams" class="card mb-4 p-4">
        <h3 class="mb-2 font-semibold">Streams: notifications, collab, tasks</h3>
        <div class="space-y-2 text-sm">
          <p class="muted">
            Three SSE surfaces share one envelope: send{" "}
            <code class="font-mono text-xs">Accept: text/event-stream</code>{" "}
            and the server holds the stream with named frames until the client
            disconnects. The tray and unread count are{" "}
            <code class="font-mono text-xs">no-store</code> (per-user private
            reads, authenticated; foreign notification ids are{" "}
            <code class="font-mono text-xs">404</code>, never 403). Webhooks
            are repo-admin only and never return secrets (
            <code class="font-mono text-xs">secret_set</code> instead). Task
            records (<code class="font-mono text-xs">…/api/tasks/{"{id}"}</code>{" "}
            and <code class="font-mono text-xs">…/pulls/{"{num}"}/merge/task</code>)
            answer JSON by default and attach as SSE under the same Accept
            header.
          </p>
          <p class="muted">Worked example (tray → stream → mark read):</p>
          <pre class="code-view overflow-x-auto p-3">{notifySnippet}</pre>
          <p class="muted">
            SDK: <code class="font-mono text-xs">client.notifications.*</code>{" "}
            (tray, unreadCount, flip, readAll, stream via{" "}
            <code class="font-mono text-xs">readSse</code>),{" "}
            <code class="font-mono text-xs">repo.notifyRepo.*</code> (watch,
            webhooks, deliveries, ping),{" "}
            <code class="font-mono text-xs">repo.collab.stream</code>.
          </p>
        </div>
      </section>

      <section id="identity" class="card mb-4 p-4">
        <h3 class="mb-2 font-semibold">Identity: profiles, orgs, access</h3>
        <div class="space-y-2 text-sm">
          <p class="muted">
            Profiles are self-or-admin writes; org reads admit anonymous on
            public hosts, member/team/invite writes need org owner, repo
            invites need repo admin,{" "}
            <code class="font-mono text-xs">…/access</code> reads need{" "}
            <code class="font-mono text-xs">triage</code> and replaces need{" "}
            <code class="font-mono text-xs">admin</code> with a version CAS
            (409 <code class="font-mono text-xs">access.json changed under
            you</code> on conflict). Invitations are token-bound: preview
            takes <code class="font-mono text-xs">?token=</code>, accept binds
            to the authenticated subject. Roles resolve server-side — the UI
            reads one <code class="font-mono text-xs">{"{role}"}</code> from{" "}
            <code class="font-mono text-xs">…/permissions</code> instead of
            re-implementing the order.
          </p>
          <p class="muted">
            SDK: <code class="font-mono text-xs">client.users.*</code>,{" "}
            <code class="font-mono text-xs">client.orgs.*</code>,{" "}
            <code class="font-mono text-xs">client.invites.*</code>,{" "}
            <code class="font-mono text-xs">repo.repoInvites.*</code>,{" "}
            <code class="font-mono text-xs">repo.access.*</code>.
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
