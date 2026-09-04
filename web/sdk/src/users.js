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
  };
}
