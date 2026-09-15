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
    /** Orgs the principal belongs to (Forgejo #423): sorted names, [] when
     *  none (the server never 404s — unknown principals answer empty). */
    orgs: (principal, opts) =>
      client._call(`${path(principal)}/orgs`, { method: "GET", ...opts }),
    avatar: {
      /** Avatar image path (`v` = avatar_updated_at cache-busts the
       *  immutable max-age response, the org-avatar shape). Callers
       *  gate on the user profile's avatar_content_type — the client
       *  never probes the bytes. */
      url: (principal, v) => `${path(principal)}/avatar${v ? `?v=${enc(v)}` : ""}`,
      /** Upload raw image bytes (PUT, self or admin): PNG/JPEG/GIF,
       *  2 MiB cap — the server center-crops to a square PNG. WebP is
       *  415-rejected (stdlib cannot crop it); SVG likewise. */
      upload: async (principal, data, { contentType, ...opts } = {}) => {
        const bytes = data instanceof Uint8Array ? data : new Uint8Array(await toArrayBuffer(data));
        return client._call(`${path(principal)}/avatar`, {
          method: "PUT",
          headers: { "Content-Type": contentType ?? "application/octet-stream" },
          body: bytes,
          ...opts,
        });
      },
      /** Regenerate the avatar (POST, self or admin): installs a fresh
       *  deterministic render and clears the delete opt-out (replaces
       *  an upload when one exists — Forgejo #601). */
      regenerate: (principal, opts) =>
        client._call(`${path(principal)}/avatar`, { method: "POST", ...opts }),
      /** Remove the avatar (DELETE, self or admin): opts out of
       *  auto-generation until an explicit regenerate (or re-upload). */
      remove: (principal, opts) =>
        client._call(`${path(principal)}/avatar`, { method: "DELETE", ...opts }),
    },
  };
}

/** Coerce upload input to bytes (File/Blob preferred — carries image data). */
async function toArrayBuffer(data) {
  if (data instanceof ArrayBuffer) return data;
  if (data?.buffer instanceof ArrayBuffer) return data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength);
  if (typeof data === "string") return new TextEncoder().encode(data).buffer;
  if (data?.arrayBuffer instanceof Function) return data.arrayBuffer();
  throw new Error("users.avatar.upload: data must be bytes, a string, or a Blob/File");
}
