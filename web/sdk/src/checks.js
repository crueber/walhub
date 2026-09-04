/**
 * Checks client group (docs/features/05 §4/§9): commit statuses, the
 * combined worst-of view, the paged index, CI reports (external CI with a
 * `wct_` token), and CI-token management (admin). Repo-scoped calls ride
 * `/{o}/{r}/api/checks…` (both lanes via the browser-lane rewrite).
 * Combined/status GETs are no-store server-side (sha-addressed but
 * mutable) — callers must not cache them under SHA_TTL. Live rows follow
 * `check` frames on the repo's existing SSE stream.
 */

/**
 * Attach the checks surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachChecks(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);
  const json = (doc) => ({
    body: JSON.stringify(doc ?? {}),
    headers: { "Content-Type": "application/json" },
  });

  repo.checks = {
    /** Combined view: `GET …/checks/{sha}` → `{sha, state, total_counts, statuses}` (no-store). */
    combined: (sha, opts) =>
      client._call(p(`/checks/${sha}`), { method: "GET", ...opts }),
    /** Per-context statuses: `GET …/checks/statuses/{sha}` → `{sha, statuses}` (no-store). */
    statuses: (sha, opts) =>
      client._call(p(`/checks/statuses/${sha}`), { method: "GET", ...opts }),
    /** Paged index: `GET …/checks?after=&n=` → `{checks, more}` (n default 50, max 200). */
    list: (query = {}, opts) =>
      client._call(p(`/checks${qs(query)}`), { method: "GET", ...opts }),
    /**
     * Report: `POST …/checks/statuses/{sha}` → `200` full status record.
     * Used by external CI with a `wct_` token (or by repo writers).
     */
    report: (sha, { context, state, target_url, description, started_at, completed_at } = {}, opts) =>
      client._call(p(`/checks/statuses/${sha}`), {
        method: "POST",
        ...json({ context, state, target_url, description, started_at, completed_at }),
        ...opts,
      }),
  };

  repo.ciTokens = {
    /** Mint: `POST …/checks/tokens` → `201 {id, token, scopes}` (secret shown once; admin). */
    create: ({ name, scopes } = {}, opts) =>
      client._call(p("/checks/tokens"), {
        method: "POST",
        ...json({ name, scopes }),
        ...opts,
      }),
    /** List: `GET …/checks/tokens` → `{tokens}` without secrets (admin). */
    list: (opts) =>
      client._call(p("/checks/tokens"), { method: "GET", ...opts }),
    /** Revoke: `DELETE …/checks/tokens/{id}` → `204` (admin). */
    revoke: (id, opts) =>
      client._call(p(`/checks/tokens/${id}`), { method: "DELETE", ...opts }),
  };
}

/** Query-string helper: skips null/undefined/empty; encodes key and value. */
function qs(params) {
  const parts = [];
  for (const [k, v] of Object.entries(params ?? {})) {
    if (v === undefined || v === null || v === "") continue;
    parts.push(`${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`);
  }
  return parts.length ? `?${parts.join("&")}` : "";
}
