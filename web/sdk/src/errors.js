/**
 * ReposError — every SDK failure is one of these.
 *
 * Carries `{status, message, url}` with `notFound` / `unauthorized` getters
 * (12_web_ui.md §1.1). `status` is the HTTP status code, or a client-side
 * convention code (499 aborted, 502 stream ended without a result,
 * 504 popup auth timed out) that the server never sends on the wire.
 */
export class ReposError extends Error {
  /**
   * @param {number} status HTTP status (or client-side convention code)
   * @param {string} message server message text, shown verbatim in the UI
   * @param {string} [url] the request URL that produced the error
   */
  constructor(status, message, url) {
    super(message);
    this.name = "ReposError";
    /** @type {number} */
    this.status = status;
    /** @type {string} */
    this.url = url ?? "";
  }

  /** true when the server answered 404 (unknown owner/repo/ref/path/sha). */
  get notFound() {
    return this.status === 404;
  }

  /** true when the server answered 401 (unauthenticated for this endpoint). */
  get unauthorized() {
    return this.status === 401;
  }
}

/**
 * 304 sentinel — NOT an error. The SDK resolves (never throws) this when a
 * GET answers `304 Not Modified` (revalidation hit: the caller already holds
 * the current body). The data layer treats it as silent keep-current: value
 * untouched, no error-tray entry. `Symbol.for` keeps identity across bundle
 * copies (SPA bundle vs `repos.js`), so compare with `===`, never by shape.
 */
export const NOT_MODIFIED = Symbol.for("walhub/not-modified");
