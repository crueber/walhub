/**
 * Access client group (features/01 §8–§9 + 08 §§3.6/5): repo role
 * bindings + visibility, the resolved-role gating read, and the
 * collaborator/assignable listings (mentions autocomplete source).
 * `repo.access.get/put` and the 08 reads ride `/{o}/{r}/api/…` (both
 * lanes via the browser-lane rewrite).
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

  /**
   * Resolved P6 role for the caller: `GET …/api/permissions` →
   * `{role}` (`{role: null}` when the caller holds no role; anonymous
   * resolves `read` when anonymous_read admits them). Read-gated; the
   * server is authoritative (08 §5 — client gating is cosmetic).
   */
  repo.permissions = (opts) =>
    client._call(p("/permissions"), { method: "GET", ...opts });

  repo.collaborators = {
    /**
     * Effective bindings: `GET …/api/collaborators` →
     * `{collaborators: [{principal, role, source}]}` (read).
     */
    list: (opts) =>
      client._call(p("/collaborators"), { method: "GET", ...opts }),
  };

  /**
   * Mentions source: `GET …/api/assignables` →
   * `{assignables: [{principal, display}]}` (read; 300 s client TTL).
   */
  repo.assignables = (opts) =>
    client._call(p("/assignables"), { method: "GET", ...opts });
}
