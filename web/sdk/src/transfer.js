/**
 * Transfer client group (Forgejo #358): direct owner-initiated repo
 * transfer. `repo.transfer({owner, repo?})` rides
 * `POST /{o}/{r}/api/transfer` (both lanes via the browser-lane
 * rewrite) → `{owner, repo}` at the new address. Server-gated: repo
 * admin on the source plus #346 admission on the destination.
 */

/**
 * Attach the transfer surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachTransfer(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);

  /**
   * Move this repo to another owner (`{owner, repo?}` — repo defaults
   * server-side to the current name, for transfer+rename pass both).
   * 201 carries the new address; 403/404/409 surface verbatim.
   */
  repo.transfer = (body, opts) =>
    client._call(p("/transfer"), {
      method: "POST",
      body: JSON.stringify(body ?? {}),
      headers: { "Content-Type": "application/json" },
      ...opts,
    });
}
