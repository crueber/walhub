/**
 * Releases client group (docs/features/07 §§3/7/8): release headers, asset
 * bytes (two-step upload), the latest pointer, and changelog autodraft.
 * Repo-scoped calls ride `/{o}/{r}/api/releases…` (both lanes via the
 * browser-lane rewrite); asset downloads ride the repo-subpath byte route
 * `/{o}/{r}/releases/{tag}/assets/{name}` (static contract — plain links,
 * never the JSON envelope). Thin fetch wrappers with the SDK's lane/401
 * rules; release pages follow `release` frames on the repo's existing SSE
 * stream.
 */

/**
 * Attach the releases surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachReleases(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);
  const json = (doc) => ({
    body: JSON.stringify(doc ?? {}),
    headers: { "Content-Type": "application/json" },
  });

  repo.releases = {
    /** Paged release cards: `GET …/releases?n=&after=` (drafts hidden, newest first). */
    list: (query = {}, opts) =>
      client._call(p(`/releases${qs(query)}`), { method: "GET", ...opts }),
    /** Latest: `GET …/releases/latest[?include_prereleases=1]` (404 when none). */
    latest: (query = {}, opts) =>
      client._call(p(`/releases/latest${qs(query)}`), { method: "GET", ...opts }),
    /** One release: `GET …/releases/{tag}` (drafts included, ETag + SWR). */
    get: (tag, opts) =>
      client._call(p(`/releases/${encodeURIComponent(tag)}`), { method: "GET", ...opts }),
    /** Create-or-update: `PUT …/releases/{tag}` → 201/200 Release (write; unknown tag 404). */
    put: (tag, fields = {}, opts) =>
      client._call(p(`/releases/${encodeURIComponent(tag)}`), {
        method: "PUT",
        ...json(fields),
        ...opts,
      }),
    /** Delete: `DELETE …/releases/{tag}` (maintain; repairs latest). */
    remove: (tag, opts) =>
      client._call(p(`/releases/${encodeURIComponent(tag)}`), { method: "DELETE", ...opts }),
    /** Autodraft: `GET …/releases/autodraft?tag=&since=` → `{tag, since, body, prs, more}`. */
    autodraft: (query = {}, opts) =>
      client._call(p(`/releases/autodraft${qs(query)}`), { method: "GET", ...opts }),
    /**
     * Upload: `POST …/releases/{tag}/assets/{name}` with raw bytes +
     * `X-Walgit-Asset-Sha256` (computed here via `crypto.subtle.digest`
     * unless `sha256` is given) → 201 asset entry (409 on sha clash).
     */
    uploadAsset: async (tag, name, data, { sha256, contentType, ...opts } = {}) => {
      const bytes = data instanceof Uint8Array ? data : new Uint8Array(await toArrayBuffer(data));
      const digest = sha256 ?? (await sha256Hex(bytes));
      return client._call(p(`/releases/${encodeURIComponent(tag)}/assets/${encodeURIComponent(name)}`), {
        method: "POST",
        headers: {
          "Content-Type": contentType ?? "application/octet-stream",
          "X-Walgit-Asset-Sha256": digest,
        },
        body: bytes,
        ...opts,
      });
    },
    /** Delete asset: `DELETE …/releases/{tag}/assets/{name}` (write). */
    deleteAsset: (tag, name, opts) =>
      client._call(p(`/releases/${encodeURIComponent(tag)}/assets/${encodeURIComponent(name)}`), {
        method: "DELETE",
        ...opts,
      }),
  };

  repo.releaseAssetUrl = (tag, name) =>
    `${client.base}/${repo.owner}/${repo.name}/releases/${encodeURIComponent(tag)}/assets/${encodeURIComponent(name)}`;
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

async function toArrayBuffer(data) {
  if (data instanceof ArrayBuffer) return data;
  if (data?.buffer instanceof ArrayBuffer) return data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength);
  if (typeof data === "string") return new TextEncoder().encode(data).buffer;
  if (data?.arrayBuffer instanceof Function) return data.arrayBuffer();
  throw new Error("uploadAsset: data must be bytes, a string, or a Blob/File");
}

/** Lowercase hex sha256 of bytes (WebCrypto; Node 19+ and browsers). */
export async function sha256Hex(bytes) {
  const digest = await globalThis.crypto.subtle.digest("SHA-256", bytes);
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}
