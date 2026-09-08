/**
 * Create-repo client group (Forgejo #210): the explicit
 * `POST /api/v1/repos` twin (+ `/api-browser/v1` lane twin) producing a
 * placeholder (non-real until the first push adopts it).
 *
 * Thin fetch wrapper with the SDK's lane/401 rules. Naming validation
 * mirrors the server's `git.ParseRepoId` (contract.go:32-47): two segments,
 * each [A-Za-z0-9._-]{1,100}, no leading dot, not `..`, optional `.git`
 * suffix stripped. The server re-validates everything; this only fails
 * fast with inline field errors (never a tray).
 */

/**
 * Validate an owner/name pair client-side (server re-validates).
 * @param {string} owner
 * @param {string} name
 * @returns {{owner: string, name: string, error?: string}}
 */
export function validateRepoName(owner, name) {
  const o = String(owner ?? "").trim();
  let n = String(name ?? "").trim().replace(/\.git$/, "");
  const part = (s) =>
    s.length >= 1 &&
    s.length <= 100 &&
    s !== ".." &&
    !s.startsWith(".") &&
    /^[A-Za-z0-9._-]+$/.test(s);
  if (!o || !n) return { owner: o, name: n, error: "owner and name are required" };
  if (!part(o)) return { owner: o, name: n, error: `invalid owner ${JSON.stringify(o)}` };
  if (!part(n)) return { owner: o, name: n, error: `invalid name ${JSON.stringify(n)}` };
  if (o.includes("/") || n.includes("/")) return { owner: o, name: n, error: "owner and name must be single segments" };
  return { owner: o, name: n };
}

/** Reserved single-segment UI route names (server 06 §3.3): creating under
 * one is allowed (git/API paths unaffected) but the /:owner UI page
 * misroutes — the server answers a non-blocking `warning` (surfaced here
 * as `isUiRouteCollision`, never an error). */
const UI_ROUTE_NAMES = new Set([
  "import",
  "api",
  "keys",
  "setup",
  "explore",
  "how-it-works",
  "new",
  "notifications",
]);

/**
 * Non-blocking UI-route-collision check for the create form.
 * @param {string} owner
 * @returns {boolean}
 */
export function isUiRouteCollision(owner) {
  return UI_ROUTE_NAMES.has(String(owner ?? "").trim().toLowerCase());
}

/**
 * Attach the create surface onto the client instance (top-level — the
 * target repo does not exist yet, so this is not repo-scoped; the
 * `client.imports` pattern in import.js §5).
 * @param {import("./core.js").ReposClient} client client to extend
 */
export function attachCreate(client) {
  const lanePath = (suffix = "") => {
    const lane = client.lane === "browser" ? "api-browser" : "api";
    return `/${lane}/v1/repos${suffix}`;
  };
  const json = (doc) => ({
    body: JSON.stringify(doc ?? {}),
    headers: { "Content-Type": "application/json" },
  });

  client.repos = {
    /**
     * Create (write): `POST …/repos` →
     * `201 {owner, name, full_name, placeholder, clone_url, html_url, warning?}`
     * (+ `Location: repo URL`), idempotent `200 {…, already:true}` for a
     * same-principal re-create, `409` plain text carrying the winner URL
     * for a conflicting create.
     * @param {{owner: string, name: string, object_format?: string, placeholder?: boolean, visibility?: string}} payload
     */
    create: (payload = {}, opts) =>
      client._call(lanePath(), { method: "POST", ...json(payload), ...opts }),
  };
}
