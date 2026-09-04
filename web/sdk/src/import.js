/**
 * Imports client group (docs/features/10 §5): start/get/attach for the
 * top-level `POST /api/v1/repos/imports` twins (lane twin
 * `/api-browser/v1/…`, the forks.create pattern in pulls.js §7).
 * Thin fetch wrappers with the SDK's lane/401 rules; the attach stream
 * rides the shared SSE envelope (`_envelope` in core.js — one parser,
 * 05 precedent): notice/progress/task surface through onProgress and the
 * terminal result resolves (error throws ReposError).
 */

/**
 * Client-side source normalization (the server re-validates everything;
 * this only prefills owner/name and shows the canonical URL).
 * Never throws: `{url}` is always set; `{error}` names a client-side
 * problem; owner/name are suggestions when derivable.
 *
 * @param {string} raw pasted URL or `owner/repo` shorthand
 * @returns {{url: string, kind: string, owner?: string, name?: string, error?: string}}
 */
export function normalizeSource(raw) {
  const s = (raw ?? "").trim();
  if (!s) return { url: "", kind: "generic", error: "paste a git URL or owner/repo" };
  const short = s.match(/^([A-Za-z0-9._-]{1,100})\/([A-Za-z0-9._-]{1,100}(?:\.git)?)$/);
  if (short && !s.includes("://")) {
    const owner = short[1];
    const name = short[2].replace(/\.git$/, "");
    if (!owner.startsWith(".") && !name.startsWith(".") && owner !== ".." && name !== "..") {
      return { url: `https://github.com/${owner}/${name}.git`, kind: "github", owner, name };
    }
  }
  const gh = s.match(/^https?:\/\/github\.com\/([A-Za-z0-9._-]+)\/([A-Za-z0-9._-]+?)(?:\.git)?\/?$/i);
  if (gh) {
    const owner = gh[1];
    const name = gh[2];
    return { url: `https://github.com/${owner}/${name}.git`, kind: "github", owner, name };
  }
  if (/^[^:@\s]+@[^:\s]+:.+$/.test(s)) {
    return { url: s, kind: "generic", error: "server-side ssh is not supported in v1 — use https with a token" };
  }
  return { url: s, kind: s.startsWith("file://") ? "file" : "generic" };
}

/**
 * Attach the imports surface onto the client instance (top-level —
 * the target repo does not exist yet, so this is not repo-scoped).
 * @param {import("./core.js").ReposClient} client client to extend
 */
export function attachImports(client) {
  const lanePath = (suffix = "") => {
    const lane = client.lane === "browser" ? "api-browser" : "api";
    return `/${lane}/v1/repos/imports${suffix}`;
  };
  const json = (doc) => ({
    body: JSON.stringify(doc ?? {}),
    headers: { "Content-Type": "application/json" },
  });

  client.imports = {
    /**
     * Start (or join, or no-op): `POST …/repos/imports` →
     * `202 {task, target}` (+ `joined:true` on a params match) or
     * `200 {repo, import}` when this source already landed.
     */
    start: (payload = {}, opts) =>
      client._call(lanePath(), { method: "POST", ...json(payload), ...opts }),
    /** Task record: `GET …/repos/imports/{id}` (JSON). */
    get: (id, opts) =>
      client._call(lanePath(`/${encodeURIComponent(id)}`), { method: "GET", ...opts }),
    /**
     * Attach the narrated stream: `GET …/repos/imports/{id}` (SSE).
     * Calls `onEvent({event, ...})` per notice/progress/task frame and
     * resolves the terminal `{repo, head_shas, format, imported_at}`
     * result (a terminal error throws ReposError). Cancel via
     * `opts.signal` — aborting stops listening, never the import
     * (the server runs it detached, like every other task).
     */
    attach: (id, onEvent, opts = {}) =>
      client._call(lanePath(`/${encodeURIComponent(id)}`), {
        method: "GET",
        headers: { Accept: "text/event-stream" },
        onProgress: onEvent,
        sse: false,
        signal: opts.signal,
        ...opts,
      }),
  };
}
