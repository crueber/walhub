/**
 * Users client group (features/01 §8–§9): user profiles keyed by principal.
 * `client.users.get/put` over `/api/v1/users/{principal}` (both lanes).
 */

const enc = encodeURIComponent;

/**
 * Attach the user surface onto a ReposClient instance.
 * @param {import("./core.js").ReposClient} client client to extend
 */
export function attachUsers(client) {
  /** @param {string} principal */
  const path = (principal) => `/api/v1/users/${enc(principal.toLowerCase())}`;
  client.users = {
    /** GET profile; null when unknown (404 → null, like repo summaries). */
    get: async (principal, opts) => {
      try {
        return await client._call(path(principal), { method: "GET", ...opts });
      } catch (err) {
        if (err?.status === 404) return null;
        throw err;
      }
    },
    /** PUT profile (self or admin): `{display_name?, bio?}`. */
    put: (principal, body, opts) =>
      client._call(path(principal), {
        method: "PUT",
        body: JSON.stringify(body ?? {}),
        headers: { "Content-Type": "application/json" },
        ...opts,
      }),
    avatar: {
      /** Avatar image path (`v` = avatar_updated_at cache-busts the
       *  immutable max-age response, the org-avatar shape). Callers
       *  gate on the user profile's avatar_content_type — the client
       *  never probes the bytes. */
      url: (principal, v) => `${path(principal)}/avatar${v ? `?v=${enc(v)}` : ""}`,
      /** Regenerate the avatar (POST, self or admin): installs a fresh
       *  deterministic render and clears the delete opt-out. */
      regenerate: (principal, opts) =>
        client._call(`${path(principal)}/avatar`, { method: "POST", ...opts }),
      /** Remove the avatar (DELETE, self or admin): opts out of
       *  auto-generation until an explicit regenerate. */
      remove: (principal, opts) =>
        client._call(`${path(principal)}/avatar`, { method: "DELETE", ...opts }),
    },
  };
}
