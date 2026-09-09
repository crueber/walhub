/**
 * Mirrors client group (docs/features/11_mirror.md §5): the
 * create-from-URL top-level twin plus the repo-scoped config + Sync-now
 * surface. Thin fetch wrappers with the SDK's lane/401 rules.
 *
 * Scheduled syncs fetch public upstreams anonymously (no stored
 * secrets, v1). A token rides only the creation first-sync and the
 * manual Sync-now body — memory-only, never persisted, never logged.
 */

/**
 * Attach the mirrors surface onto the client instance (top-level —
 * the target repo does not exist yet, so this is not repo-scoped).
 * @param {import("./core.js").ReposClient} client client to extend
 */
export function attachMirrors(client) {
  const lanePath = (suffix = "") => {
    const lane = client.lane === "browser" ? "api-browser" : "api";
    return `/${lane}/v1/repos/mirrors${suffix}`;
  };
  const json = (doc) => ({
    body: JSON.stringify(doc ?? {}),
    headers: { "Content-Type": "application/json" },
  });

  client.mirrors = {
    /**
     * Create from URL: `POST …/repos/mirrors` →
     * `202 {task: {id}, target}` (the first sync fires async; poll
     * `repo(full).mirror.syncStatus(id)`). The token (when given) is
     * memory-only for the first sync.
     */
    create: (payload = {}, opts) =>
      client._call(lanePath(), { method: "POST", ...json(payload), ...opts }),
  };
}

/**
 * Attach the mirror surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachMirror(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);
  const json = (doc) => ({
    body: JSON.stringify(doc ?? {}),
    headers: { "Content-Type": "application/json" },
  });

  repo.mirror = {
    /** Config + computed next fire: `GET …/mirror` → the mirror view (404 when not a mirror). */
    get: (opts) =>
      client._call(p("/mirror"), { method: "GET", ...opts }),
    /**
     * Create-or-reschedule: `PUT …/mirror` → `202 {task, target,
     * mirror}` on create (first sync fires async), `200` view on a
     * schedule change. An upstream_url change is 409
     * (delete-and-recreate). Admin-only.
     */
    put: (payload = {}, opts) =>
      client._call(p("/mirror"), { method: "PUT", ...json(payload), ...opts }),
    /** Remove: `DELETE …/mirror` → `204` (stops the loop; admin-only). */
    remove: (opts) =>
      client._call(p("/mirror"), { method: "DELETE", ...opts }),
    /**
     * Manual Sync now: `POST …/mirror/sync` → `202 {task: {id},
     * target}`. The body may carry a memory-only `{token}` and the
     * `{force}` rewind escape hatch. Admin-only.
     */
    syncNow: ({ token, force } = {}, opts) =>
      client._call(p("/mirror/sync"), {
        method: "POST",
        ...json({ ...(token ? { token } : {}), ...(force ? { force: true } : {}) }),
        ...opts,
      }),
    /**
     * Resolve a Sync-now 202: `GET …/mirror/sync?id=` → `{done,
     * task?, error?}`; without id → `{active, recent}`.
     */
    syncStatus: (id, opts) =>
      client._call(p(`/mirror/sync${id ? `?id=${encodeURIComponent(id)}` : ""}`), {
        method: "GET",
        ...opts,
      }),
  };
}
