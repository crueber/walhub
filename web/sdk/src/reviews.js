/**
 * Reviews client group (docs/features/04 §7/§8): immutable reviews,
 * line-anchored threads, review requests, and review-suggest. Repo-scoped
 * calls ride `/{o}/{r}/api/pulls/{num}/…` (both lanes via the browser-lane
 * rewrite). Thin fetch wrappers with the SDK's lane/401 rules; thread
 * pages follow `review`/`thread` frames on the repo's collaboration SSE
 * stream once 06 lands it (the server already publishes them — see
 * internal/review). Diff rendering reuses `lib/diff.js` unchanged
 * (dogfood rule: every call goes through the SDK).
 *
 * @typedef {import("./types.js").Review} Review
 * @typedef {import("./types.js").ThreadAnchor} ThreadAnchor
 * @typedef {import("./types.js").ThreadHeader} ThreadHeader
 */

/**
 * Attach the reviews surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachReviews(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);
  const json = (doc) => ({
    body: JSON.stringify(doc ?? {}),
    headers: { "Content-Type": "application/json" },
  });

  repo.pulls.reviews = {
    /** Paged review events: `GET …/pulls/{num}/reviews?n=&after=` (newest-first). */
    list: (num, query = {}, opts) =>
      client._call(p(`/pulls/${num}/reviews${qs(query)}`), { method: "GET", ...opts }),
    /**
     * Submit: `POST …/pulls/{num}/reviews` → `201 {review, threads, summary}`
     * (422 author self-approve, 409 stale commit_sha).
     */
    submit: (num, { state, body, commit_sha, threads } = {}, opts) =>
      client._call(p(`/pulls/${num}/reviews`), {
        method: "POST",
        ...json({ state, body, commit_sha, threads }),
        ...opts,
      }),
    /** One review event: `GET …/pulls/{num}/reviews/{seq}` (404 unknown). */
    get: (num, seq, opts) =>
      client._call(p(`/pulls/${num}/reviews/${seq}`), { method: "GET", ...opts }),
    /** Dismiss: `POST …/pulls/{num}/reviews/{seq}/dismiss` (maintain). */
    dismiss: (num, seq, { reason } = {}, opts) =>
      client._call(p(`/pulls/${num}/reviews/${seq}/dismiss`), {
        method: "POST",
        ...json({ reason }),
        ...opts,
      }),
  };

  repo.pulls.threads = {
    /** Paged thread headers: `GET …/pulls/{num}/threads?resolved=&after=&n=`. */
    list: (num, query = {}, opts) =>
      client._call(p(`/pulls/${num}/threads${qs(query)}`), { method: "GET", ...opts }),
    /** Open: `POST …/pulls/{num}/threads` → `201 {thread}`. */
    create: (num, { anchor, body } = {}, opts) =>
      client._call(p(`/pulls/${num}/threads`), {
        method: "POST",
        ...json({ anchor, body }),
        ...opts,
      }),
    /** Thread + comments: `GET …/pulls/{num}/threads/{tid}`. */
    get: (num, tid, query = {}, opts) =>
      client._call(p(`/pulls/${num}/threads/${tid}${qs(query)}`), { method: "GET", ...opts }),
    /** Comment: `POST …/pulls/{num}/threads/{tid}/comments` → `201 {comment}`. */
    comment: (num, tid, body, opts) =>
      client._call(p(`/pulls/${num}/threads/${tid}/comments`), {
        method: "POST",
        ...json({ body }),
        ...opts,
      }),
    /** Resolve: `POST …/pulls/{num}/threads/{tid}/resolve` (opener/participant/triage). */
    resolve: (num, tid, opts) =>
      client._call(p(`/pulls/${num}/threads/${tid}/resolve`), { method: "POST", ...opts }),
    /** Unresolve: `POST …/pulls/{num}/threads/{tid}/unresolve`. */
    unresolve: (num, tid, opts) =>
      client._call(p(`/pulls/${num}/threads/${tid}/unresolve`), { method: "POST", ...opts }),
  };

  repo.pulls.requests = {
    /** Current requested reviewers: `GET …/pulls/{num}/review-requests`. */
    list: (num, opts) =>
      client._call(p(`/pulls/${num}/review-requests`), { method: "GET", ...opts }),
    /** Request: `POST …/pulls/{num}/review-requests` (author/triage+). */
    add: (num, reviewers = [], opts) =>
      client._call(p(`/pulls/${num}/review-requests`), {
        method: "POST",
        ...json({ reviewers }),
        ...opts,
      }),
    /** Remove: `DELETE …/pulls/{num}/review-requests` (author/triage+/self). */
    remove: (num, reviewers = [], opts) =>
      client._call(p(`/pulls/${num}/review-requests`), {
        method: "DELETE",
        ...json({ reviewers }),
        ...opts,
      }),
  };

  /** Reviewer suggestions: `GET …/pulls/{num}/review-suggest?q=` (20/page). */
  repo.pulls.suggest = (num, q = "", opts) =>
    client._call(p(`/pulls/${num}/review-suggest${qs({ q })}`), { method: "GET", ...opts });
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
