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
 * Generic-URL mirror contract (Forgejo #401): the non-GitHub branch
 * mirrors the server's `canonicalGenericURL`
 * (`internal/repoimport/url.go:196-208`, header contract url.go:4-13) —
 * lowercase host, strip default port, trim trailing slashes, remove one
 * trailing `.git` — so the "canonical:" hint shows the same string the
 * server will gate and clone. GitHub behavior is unchanged (canonical
 * `.git` form). Anything the server would refuse (embedded credentials,
 * a non-default explicit port, an unsupported scheme) passes through
 * verbatim — the server 400s it, and the hint must not pretend
 * otherwise.
 *
 * @param {string} raw pasted URL or `owner/repo` shorthand
 * @returns {{url: string, kind: string, owner?: string, name?: string, error?: string}}
 */
// Default ports folded into the canonical form — mirrors `isDefaultPort`
// (`internal/repoimport/url.go`); a non-default explicit port is refused
// by the server, so the hint passes it through verbatim (see above).
const GENERIC_DEFAULT_PORTS = { https: "443", http: "80", ssh: "22", git: "9418" };

const ID_PART = /^[A-Za-z0-9._-]{1,100}$/;
const validIdPart = (s) => ID_PART.test(s) && !s.startsWith(".") && s !== "..";

/**
 * Mirror of the server's `canonicalGenericURL`
 * (`internal/repoimport/url.go:196-208`): rebuild `scheme://host/path`
 * with the host lowercased, any default port stripped, trailing slashes
 * trimmed, then one trailing `.git` removed. Query and fragment are
 * preserved verbatim (distinct strings, same host gate — never silently
 * dropped), exactly like the server. Returns `null` when the input is
 * not canonicalizable here (unparseable, unsupported scheme, embedded
 * credentials, or a non-default port the server would refuse) so the
 * caller falls back to the verbatim hint.
 *
 * @param {string} s trimmed source string
 * @returns {{url: string, owner?: string, name?: string} | null}
 */
function canonicalGenericHint(s) {
  let u;
  try {
    u = new URL(s);
  } catch {
    return null;
  }
  const scheme = u.protocol.replace(/:$/, "").toLowerCase();
  const defPort = GENERIC_DEFAULT_PORTS[scheme];
  if (defPort === undefined) return null;
  if (u.username || u.password) return null;
  const host = u.hostname.toLowerCase();
  if (!host) return null;
  if (u.port && u.port !== defPort) return null;
  let p = u.pathname.replace(/\/+$/, "");
  if (p.endsWith(".git")) p = p.slice(0, -".git".length);
  const hostPart = host.startsWith("[") || !host.includes(":") ? host : `[${host}]`;
  const url = `${scheme}://${hostPart}${p}${u.search}${u.hash}`;
  const segs = p.split("/").filter(Boolean);
  let owner;
  let name;
  if (segs.length >= 2) {
    const [o, n] = segs.slice(-2);
    if (validIdPart(o) && validIdPart(n)) {
      owner = o;
      name = n;
    }
  } else if (segs.length === 1 && validIdPart(segs[0])) {
    name = segs[0];
  }
  const out = { url };
  if (owner !== undefined) out.owner = owner;
  if (name !== undefined) out.name = name;
  return out;
}

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
  if (s.startsWith("file://")) return { url: s, kind: "file" };
  const generic = canonicalGenericHint(s);
  if (generic) {
    const out = { url: generic.url, kind: "generic" };
    if (generic.owner !== undefined) out.owner = generic.owner;
    if (generic.name !== undefined) out.name = generic.name;
    return out;
  }
  return { url: s, kind: "generic" };
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
