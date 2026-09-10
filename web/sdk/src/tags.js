/**
 * Tags client group (Forgejo #253): create a lightweight tag at a commit,
 * server-side via the WAL ref-update path. Repo-scoped call rides
 * `/{o}/{r}/api/tags` (both lanes via the browser-lane rewrite). Thin fetch
 * wrapper with the SDK's lane/401 rules; the tags ref stream
 * (`repo.tags()` in repo.js) shows the new tag without a reload.
 */

/**
 * Attach the tags surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachTags(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);
  const json = (doc) => ({
    body: JSON.stringify(doc ?? {}),
    headers: { "Content-Type": "application/json" },
  });

  repo.tagsApi = {
    /**
     * Create a lightweight tag at a commit: `POST …/tags` with
     * `{name, sha, message?}` → 201 `{name, sha, ref}` (write; unknown sha
     * 404, existing tag 409, bad name 400, annotated/message 422).
     */
    create: (fields = {}, opts) =>
      client._call(p("/tags"), {
        method: "POST",
        ...json(fields),
        ...opts,
      }),
  };
}
