/**
 * Push-mirror client group (docs/features/13_push_mirror.md): the
 * repo-scoped config + Sync-now + keygen surface. Thin fetch wrappers
 * with the SDK's lane/401 rules.
 *
 * Secrets are write-only: passwords, tokens, and private keys ride the
 * PUT body on save and are never echoed back (the view carries
 * presence + last-4 hint only). The generated private key is never
 * returned either — keygen answers with the public key for upstream
 * install.
 */

/**
 * Attach the push-mirror surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachPushMirror(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);
  const json = (doc) => ({
    body: JSON.stringify(doc ?? {}),
    headers: { "Content-Type": "application/json" },
  });

  repo.pushmirror = {
    /** Config + computed next fire: `GET …/pushmirror` → the view (404 when none). */
    get: (opts) =>
      client._call(p("/pushmirror"), { method: "GET", ...opts }),
    /**
     * Configure-or-update: `PUT …/pushmirror` → `201` view on create,
     * `200` view on update. An upstream_url change is 409
     * (delete-and-recreate). Admin-only.
     */
    put: (payload = {}, opts) =>
      client._call(p("/pushmirror"), { method: "PUT", ...json(payload), ...opts }),
    /** Remove: `DELETE …/pushmirror` → `204` (stops fan-out; admin-only). */
    remove: (opts) =>
      client._call(p("/pushmirror"), { method: "DELETE", ...opts }),
    /**
     * Manual Sync now: `POST …/pushmirror/sync` → `202 {task: {id},
     * target}`. The body carries only `{force}` — credentials come
     * from the stored secret sidecar. Admin-only.
     */
    syncNow: ({ force } = {}, opts) =>
      client._call(p("/pushmirror/sync"), {
        method: "POST",
        ...json({ ...(force ? { force: true } : {}) }),
        ...opts,
      }),
    /**
     * Resolve a Sync-now 202: `GET …/pushmirror/sync?id=` → `{done,
     * task?, error?}`; without id → `{active, recent}`.
     */
    syncStatus: (id, opts) =>
      client._call(p(`/pushmirror/sync${id ? `?id=${encodeURIComponent(id)}` : ""}`), {
        method: "GET",
        ...opts,
      }),
    /**
     * Generate a deploy keypair: `POST …/pushmirror/keygen` →
     * `{public_key, key_fingerprint, has_secret, mirror}`. The private
     * key stays server-side (never returned). Admin-only.
     */
    keygen: ({ known_hosts } = {}, opts) =>
      client._call(p("/pushmirror/keygen"), {
        method: "POST",
        ...json({ ...(known_hosts ? { known_hosts } : {}) }),
        ...opts,
      }),
  };
}
