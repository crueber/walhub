/**
 * Pulls client group (docs/features/03 §8/§9): PR threads, diff/commits,
 * merge/update-branch/head tasks, and forks. Repo-scoped calls ride
 * `/{o}/{r}/api/pulls…` (both lanes via the browser-lane rewrite); forks
 * ride the top-level `/api/v1/repos/{owner}/{repo}/forks` twins. Thin fetch
 * wrappers with the SDK's lane/401 rules; thread pages follow `pull`
 * frames on the repo's existing SSE stream. Diff rendering reuses
 * `lib/diff.js` unchanged (dogfood rule: every call goes through the SDK).
 */

/**
 * Attach the pulls surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachPulls(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);
  const json = (doc) => ({
    body: JSON.stringify(doc ?? {}),
    headers: { "Content-Type": "application/json" },
  });

  repo.pulls = {
    /** Paged PR cards: `GET …/pulls?state=&base=&head=&sort=&n=&after=` (index-first). */
    list: (query = {}, opts) =>
      client._call(p(`/pulls${qs(query)}`), { method: "GET", ...opts }),
    /** Open: `POST …/pulls` → `201 {thread, pr}` (409 paired, 422 unresolvable). */
    open: ({ title, base_ref, head_ref, body, fork } = {}, opts) =>
      client._call(p("/pulls"), {
        method: "POST",
        ...json({ title, base_ref, head_ref, body, fork }),
        ...opts,
      }),
    /** Thread: `GET …/pulls/{num}` (header + pr.json + live mergeable; ETag `<head sha>`, SWR). */
    get: (num, opts) =>
      client._call(p(`/pulls/${num}`), { method: "GET", ...opts }),
    /** Edit: `PUT …/pulls/{num}` (title/body/state; unknown keys 400). */
    update: (num, fields = {}, opts) =>
      client._call(p(`/pulls/${num}`), { method: "PUT", ...json(fields), ...opts }),
    /** Comment: `POST …/pulls/{num}/comments` → `201 {event}`. */
    comment: (num, body, opts) =>
      client._call(p(`/pulls/${num}/comments`), { method: "POST", ...json({ body }), ...opts }),
    /** Diff: `GET …/pulls/{num}/diff` (text/plain unified `base...head` patch). */
    diff: (num, opts) =>
      client._call(p(`/pulls/${num}/diff`), { method: "GET", ...opts }),
    /** Commits: `GET …/pulls/{num}/commits?skip=&n=` → `{commits, more}`. */
    commits: (num, query = {}, opts) =>
      client._call(p(`/pulls/${num}/commits${qs(query)}`), { method: "GET", ...opts }),
    /** Merge: `POST …/pulls/{num}/merge` → `202 {task}` (maintain; SSE-attachable). */
    merge: (num, { strategy, commit_title, commit_message, delete_head } = {}, opts) =>
      client._call(p(`/pulls/${num}/merge`), {
        method: "POST",
        ...json({ strategy, commit_title, commit_message, delete_head }),
        ...opts,
      }),
    /** Merge-task poll: `GET …/pulls/{num}/merge/task` (progress + terminal outcome). */
    mergeTask: (num, opts) =>
      client._call(p(`/pulls/${num}/merge/task`), { method: "GET", ...opts }),
    /** Update-branch: `POST …/pulls/{num}/update-branch` → `202 {task}` (409 if dirty). */
    updateBranch: (num, { expected_head_sha } = {}, opts) =>
      client._call(p(`/pulls/${num}/update-branch`), {
        method: "POST",
        ...json({ expected_head_sha }),
        ...opts,
      }),
    /** Delete-head: `DELETE …/pulls/{num}/head` (maintain, post-merge). */
    deleteHead: (num, opts) =>
      client._call(p(`/pulls/${num}/head`), { method: "DELETE", ...opts }),
  };

  repo.forks = {
    /**
     * Fork: `POST /api/v1/repos/{owner}/{repo}/forks` (lane twin
     * `/api-browser/v1/…`) → `202 {task, repo}`. The fork shares the
     * parent's packs by construction (§7).
     */
    create: (opts = {}, callOpts) => {
      const lane = client.lane === "browser" ? "api-browser" : "api";
      return client._call(`/${lane}/v1/repos/${repo.owner}/${repo.name}/forks`, {
        method: "POST",
        ...json(opts),
        ...callOpts,
      });
    },
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
