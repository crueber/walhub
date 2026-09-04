/**
 * Access client group (features/01 §8–§9): repo role bindings + visibility.
 * `repo.access.get/put` over `/{o}/{r}/api/access` (both lanes via the
 * browser-lane rewrite).
 */

/**
 * Attach the access surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachAccess(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);

  repo.access = {
    /** `{version, visibility, role_bindings[]}` (triage+). */
    get: (opts) => client._call(p("/access"), { method: "GET", ...opts }),
    /**
     * Full-document replace incl. the read `version` (admin).
     * 409 carries "changed under you, reload".
     */
    put: (doc, opts) =>
      client._call(p("/access"), {
        method: "PUT",
        body: JSON.stringify(doc ?? {}),
        headers: { "Content-Type": "application/json" },
        ...opts,
      }),
  };
}
