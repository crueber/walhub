/**
 * Attachments client group (docs/features/02 §12): paste/drop image upload
 * for issue composers over `/{o}/{r}/api/attachments` (both lanes via the
 * browser-lane rewrite). Thin fetch wrappers with the SDK's lane/401 rules.
 *
 * Wire: raw image bytes + optional `?name=` + optional
 * `X-Walgit-Attachment-Sha256` (computed here via `crypto.subtle.digest`
 * when available; OMITTED on non-secure origins where `crypto.subtle` is
 * undefined — the server verifies the header only when present, S4) →
 * `201 {name, size, sha256, content_type, url}`.
 */

/**
 * Attach the attachments surface onto a repo client instance.
 * @param {import("./repo.js").RepoClient} repo repo client to extend
 */
export function attachAttachments(repo) {
  const client = repo.client;
  const p = (suffix = "") => repo._path(suffix);

  repo.attachments = {
    /**
     * Upload one image: `POST …/attachments?name=` with raw bytes.
     * @param {Blob|Uint8Array|ArrayBuffer|string} data image bytes (File/Blob preferred — carries `.name`)
     * @param {{name?: string, sha256?: string, contentType?: string}} [opts]
     * @returns {Promise<{name: string, size: number, sha256: string, content_type: string, url: string}>}
     */
    upload: async (data, { name, sha256, contentType, ...opts } = {}) => {
      const fileName = name ?? data?.name ?? "image";
      const bytes = data instanceof Uint8Array ? data : new Uint8Array(await toArrayBuffer(data));
      // S4: the sha header is optional-when-present. Non-secure origins
      // (http:// LAN hosts) have no crypto.subtle — omit the header there
      // instead of failing; the server always hashes the spool itself.
      let digest = sha256 ?? null;
      if (!digest) {
        try {
          digest = await sha256Hex(bytes);
        } catch {
          digest = null; // no subtle crypto here — server-side hash covers integrity
        }
      }
      const headers = { "Content-Type": contentType ?? "application/octet-stream" };
      if (digest) headers["X-Walgit-Attachment-Sha256"] = digest;
      return client._call(p(`/attachments?name=${encodeURIComponent(fileName)}`), {
        method: "POST",
        headers,
        body: bytes,
        ...opts,
      });
    },
  };
}

/** Query-string helper: skips null/undefined/empty; encodes key and value. */
export function attachmentQs(params) {
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
  throw new Error("attachments.upload: data must be bytes, a string, or a Blob/File");
}

/** Lowercase hex sha256 of bytes (WebCrypto; null when subtle is unavailable). */
export async function sha256Hex(bytes) {
  const subtle = globalThis.crypto?.subtle;
  if (!subtle) throw new Error("no subtle crypto");
  const digest = await subtle.digest("SHA-256", bytes);
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}
