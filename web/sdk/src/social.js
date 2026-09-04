/**
 * Social client group (docs/features/07 §§4–7): stars, the social counters,
 * and starred lists. Repo-scoped calls ride `/{o}/{r}/api/star|social`
 * (both lanes via the browser-lane rewrite); starred lists ride the
 * top-level `/api/v1/me/starred` + `/api/v1/users/{principal}/starred`
 * twins. Watch mutation stays on `repo.watch` (notifications.js, 06 §6) —
 * this module only reads watch state via `social().viewer.watching`. Thin
 * fetch wrappers with the SDK's lane/401 rules.
 */

/**
 * Attach the repo-scoped social surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachSocial(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);

  repo.star = {
    /** Star: `PUT …/star` → `{stars}` (authenticated + repo-visible, idempotent). */
    set: (opts) => client._call(p("/star"), { method: "PUT", ...opts }),
    /** Unstar: `DELETE …/star` → `{stars}` (authenticated, always allowed). */
    remove: (opts) => client._call(p("/star"), { method: "DELETE", ...opts }),
  };

  repo.social = {
    /** Counters + viewer flags: `GET …/social` → `{stars, watchers, forks, viewer}` (SWR+ETag). */
    get: (opts) => client._call(p("/social"), { method: "GET", ...opts }),
  };
}

/**
 * Attach the top-level starred twins onto the client.
 * @param {import("./core.js").ReposClient} client default client to extend
 */
export function attachSocialTop(client) {
  client.social = {
    /** Own stars: `GET /api/v1/me/starred?n=&after=` (authenticated, newest first). */
    myStarred: (query = {}, opts) =>
      client._call(`/api/v1/me/starred${qs(query)}`, { method: "GET", ...opts }),
    /** One principal's stars: `GET /api/v1/users/{principal}/starred?n=&after=` (public). */
    userStarred: (principal, query = {}, opts) =>
      client._call(`/api/v1/users/${encodeURIComponent(principal)}/starred${qs(query)}`, {
        method: "GET",
        ...opts,
      }),
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
