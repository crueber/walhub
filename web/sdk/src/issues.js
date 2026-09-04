/**
 * Issues client group (docs/features/02 §7/§11): threads, comments,
 * reactions, labels, milestones over `/{o}/{r}/api/issues…` (both lanes
 * via the browser-lane rewrite). Thin fetch wrappers with the SDK's
 * lane/401 rules; thread pages use `mountStream` (lib/sse.js) to follow
 * `issue`/`issue_event` on the repo's existing SSE stream.
 */

/**
 * Attach the issues surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachIssues(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);
  const json = (doc) => ({
    body: JSON.stringify(doc ?? {}),
    headers: { "Content-Type": "application/json" },
  });

  repo.issues = {
    /** Paged cards: `GET …/issues?state=&labels=&assignee=&milestone=&since=&after=&n=`. */
    list: (query = {}, opts) =>
      client._call(p(`/issues${qs(query)}`), { method: "GET", ...opts }),
    /** Create: `POST …/issues` → `201 {thread, events:[opened]}`. */
    create: ({ title, body } = {}, opts) =>
      client._call(p("/issues"), { method: "POST", ...json({ title, body }), ...opts }),
    /** Thread: `GET …/issues/{num}?after_seq=&n=` (ETag `v<version>`, SWR). */
    get: (num, query = {}, opts) =>
      client._call(p(`/issues/${num}${qs(query)}`), { method: "GET", ...opts }),
    /** Patch: `PATCH …/issues/{num}` (unknown keys 400). */
    patch: (num, fields = {}, opts) =>
      client._call(p(`/issues/${num}`), { method: "PATCH", ...json(fields), ...opts }),
    /** Comment: `POST …/issues/{num}/comments` → `201 {event}`. */
    comment: (num, body, opts) =>
      client._call(p(`/issues/${num}/comments`), { method: "POST", ...json({ body }), ...opts }),
    /** Seq-window: `GET …/issues/{num}/events?after_seq=&n=` (newest-last). */
    events: (num, query = {}, opts) =>
      client._call(p(`/issues/${num}/events${qs(query)}`), { method: "GET", ...opts }),
    reactions: {
      /** React: `POST …/issues/{num}/reactions` (dup → 200 `{summary}`). */
      add: (num, { target_event_seq, content } = {}, opts) =>
        client._call(p(`/issues/${num}/reactions`), {
          method: "POST",
          ...json({ target_event_seq, content }),
          ...opts,
        }),
      /** Unreact: `DELETE …/issues/{num}/reactions/{seq}/{content}` (own only). */
      remove: (num, target_event_seq, content, opts) =>
        client._call(
          p(`/issues/${num}/reactions/${encodeURIComponent(String(target_event_seq))}/${encodeURIComponent(String(content))}`),
          { method: "DELETE", ...opts }
        ),
    },
  };

  repo.labels = {
    /** `GET …/labels` → `{labels:[]}`. */
    list: (opts) => client._call(p("/labels"), { method: "GET", ...opts }),
    /** `POST …/labels` (triage) → `201 {label}`. */
    create: ({ name, color, description } = {}, opts) =>
      client._call(p("/labels"), { method: "POST", ...json({ name, color, description }), ...opts }),
    /** `PATCH …/labels/{name}` (triage; names immutable). */
    update: (name, fields = {}, opts) =>
      client._call(p(`/labels/${encodeURIComponent(name)}`), { method: "PATCH", ...json(fields), ...opts }),
    /** `DELETE …/labels/{name}` → `200 {threads_affected}`. */
    delete: (name, opts) =>
      client._call(p(`/labels/${encodeURIComponent(name)}`), { method: "DELETE", ...opts }),
  };

  repo.milestones = {
    /** `GET …/milestones?state=` (derived `percent` per row). */
    list: (query = {}, opts) =>
      client._call(p(`/milestones${qs(query)}`), { method: "GET", ...opts }),
    /** `GET …/milestones/{id}`. */
    get: (id, opts) => client._call(p(`/milestones/${encodeURIComponent(id)}`), { method: "GET", ...opts }),
    /** `POST …/milestones` (triage) → `201 {milestone}`. */
    create: ({ title, description, due_on } = {}, opts) =>
      client._call(p("/milestones"), { method: "POST", ...json({ title, description, due_on }), ...opts }),
    /** `PATCH …/milestones/{id}` (triage). */
    update: (id, fields = {}, opts) =>
      client._call(p(`/milestones/${encodeURIComponent(id)}`), { method: "PATCH", ...json(fields), ...opts }),
    /** `DELETE …/milestones/{id}` (409 while open issues reference it). */
    delete: (id, opts) =>
      client._call(p(`/milestones/${encodeURIComponent(id)}`), { method: "DELETE", ...opts }),
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
