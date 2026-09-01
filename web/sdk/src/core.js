/**
 * ReposClient core (12_web_ui.md §1.0–§1.4, §1.6).
 *
 * Base URL resolution (§1.3), lane selection (§1.2), the fetch wrapper, SSE
 * envelope handling (§1.4) and the 401→popup single-flight retry (§1.2).
 * Plain `fetch` everywhere — never `EventSource` (the server streams only when
 * Accept carries `text/event-stream`, and only fetch can set that header plus
 * auth).
 *
 * Every method derives its own AbortController from the caller's signal (§1.6:
 * the caller owns the signal, the SDK owns the reader and closes it).
 */
import { ReposError } from "./errors.js";
import { readSse } from "./sse.js";
import { RepoClient } from "./repo.js";
import { attachAdmin } from "./admin.js";
import { openAuthPopup, canAuthenticate } from "./auth.js";

/** Off-DOM default base, tests only (§1.3). */
export const DEFAULT_BASE = "http://127.0.0.1:8080";

/** Client-side convention code for an aborted call (never sent by the server). */
export const STATUS_ABORTED = 499;

/**
 * Lane selection (§1.2, order fixed):
 *  1. explicit `token` option → "bearer"      (Authorization: Bearer, credentials: omit)
 *  2. page origin == API base → "same-origin" (session cookie, credentials: same-origin)
 *  3. else                    → "browser"     (/{o}/{r}/api-browser/…, credentials: include,
 *                                               redirect: manual)
 *
 * @param {{token?: string}} opts
 * @param {string} base resolved API base
 * @returns {"bearer"|"same-origin"|"browser"}
 */
export function selectLane(opts, base) {
  if (opts?.token) return "bearer";
  let pageOrigin = "";
  try {
    pageOrigin = globalThis.location?.origin ?? "";
  } catch {
    pageOrigin = ""; // off-DOM
  }
  if (pageOrigin && pageOrigin === new URL(base).origin) return "same-origin";
  return "browser";
}

/**
 * Resolve the API base URL (§1.3, first wins):
 * `configure({base})` → `<script data-base>` → `import.meta.url` origin → page
 * origin → off-DOM default (tests only).
 *
 * @param {{base?: string}} [opts]
 * @returns {string} origin, no trailing slash
 */
export function resolveBase(opts = {}) {
  if (opts.base) return stripTrailingSlash(opts.base);
  const doc = globalThis.document;
  if (doc) {
    const el = doc.querySelector("script[data-base]");
    const fromAttr = el?.getAttribute?.("data-base");
    if (fromAttr) return stripTrailingSlash(fromAttr);
  }
  // The SDK is a module, so import.meta.url always works in the browser (§1.3);
  // file:/test: URLs (Node) are skipped so the off-DOM default survives tests.
  const metaOrigin = httpOrigin(new URL(".", import.meta.url).href);
  if (metaOrigin) return metaOrigin;
  const pageOrigin = httpOrigin(globalThis.location?.origin ?? "");
  if (pageOrigin) return pageOrigin;
  return DEFAULT_BASE;
}

/** Origin of an http(s) URL; "" for file:/test:/opaque or unparseable. */
function httpOrigin(url) {
  try {
    const u = new URL(url);
    return u.protocol === "http:" || u.protocol === "https:" ? u.origin : "";
  } catch {
    return "";
  }
}

function stripTrailingSlash(url) {
  return url.endsWith("/") ? url.slice(0, -1) : url;
}


/** Repo-scoped paths ride the browser lane rewrite; `/api/v1/…` does not. */
function isRepoScoped(path) {
  return !path.startsWith("/api/");
}

/**
 * Wire URL for a path under the resolved lane (§1.2): browser lane rewrites
 * `/{o}/{r}/api/…` → `/{o}/{r}/api-browser/…`.
 */
export function laneUrl(base, lane, path) {
  if (lane === "browser" && isRepoScoped(path)) {
    const m = path.match(/^(\/[^/]+\/[^/]+)\/api(\/.*)?$/);
    if (m) return `${base}${m[1]}/api-browser${m[2] ?? ""}`;
  }
  return `${base}${path}`;
}

/**
 * Request init for a lane (§1.2): credentials/redirect/Authorization rules.
 * Caller `headers` merge first; the Accept default (sse) merges after.
 */
export function laneInit(lane, token, headers, sse) {
  /** @type {Record<string,string>} */
  const h = { ...headers };
  if (sse && !h["Accept"]) h["Accept"] = "application/json, text/event-stream";
  let credentials = "same-origin";
  let redirect;
  if (lane === "bearer") {
    h["Authorization"] = `Bearer ${token}`;
    credentials = "omit";
  } else if (lane === "browser") {
    credentials = "include";
    redirect = "manual";
  }
  const init = { credentials, headers: h };
  if (redirect) init.redirect = redirect;
  return init;
}

/**
 * One client instance: base URL + lane + fetch + single-flight popup auth.
 * Injectable `fetch` (constructor or configure) for tests (§5).
 */
export class ReposClient {
  /**
   * @param {{base?: string, token?: string, fetch?: typeof fetch,
   *          onProgress?: (p: Object) => void, authenticate?: () => Promise<void>} | string} [opts]
   *   string form = base URL shorthand.
   */
  constructor(opts = {}) {
    if (typeof opts === "string") opts = { base: opts };
    this._base = resolveBase(opts);
    this._token = opts.token ?? "";
    this._fetch = opts.fetch ?? globalThis.fetch.bind(globalThis);
    /** Client-global progress handler (configure-set), §1.4 step 3. */
    this.onProgress = opts.onProgress ?? null;
    /** Popup auth step; overridable for tests. @type {(() => Promise<void>) | null} */
    this.authenticate = opts.authenticate ?? (canAuthenticate() ? () => openAuthPopup(this._base) : null);
    /** Single-flight popup promise — at most one per client (§1.2). @type {Promise<void>|null} */
    this._popupAuth = null;
  }

  /**
   * Re-configure (§1.1): base URL and lane are re-evaluated on the next call.
   * @param {{base?: string, token?: string, onProgress?: Function, fetch?: typeof fetch}} opts
   * @returns {ReposClient} this, for chaining
   */
  configure(opts = {}) {
    if (opts.base !== undefined) this._base = stripTrailingSlash(opts.base);
    if (opts.token !== undefined) this._token = opts.token;
    if (opts.onProgress !== undefined) this.onProgress = opts.onProgress;
    if (opts.fetch !== undefined) this._fetch = opts.fetch;
    return this;
  }

  /** Resolved base (diagnostics/tests). */
  get base() {
    return this._base;
  }

  /** Current lane (diagnostics/tests). */
  get lane() {
    return selectLane({ token: this._token }, this._base);
  }

  /**
   * Repo-scoped client group (§1.1).
   * @param {string} fullName "owner/name" (trailing ".git" tolerated)
   */
  repo(fullName) {
    const r = new RepoClient(this, fullName);
    attachAdmin(r);
    return r;
  }

  // ── non-repo surface (§1.1: /api/v1/…) ──────────────────────────────────

  /** @returns {Promise<import("./types.js").Discovery>} */
  discovery() {
    return this._call("/api/v1", { method: "GET" });
  }

  /** @returns {Promise<import("./types.js").Me>} */
  me() {
    return this._call("/api/v1/me", { method: "GET" });
  }

  /**
   * Popup sign-in: opens `<base>/api-browser/v1/authenticate`, awaits the
   * authenticated postMessage (single-flight like any other 401 path).
   * @returns {Promise<void>}
   */
  signIn() {
    return this._authenticate();
  }

  /** @returns {Promise<string[]>} */
  ownersList() {
    return this._call("/api/v1/owners", { method: "GET" });
  }

  /** @param {string} owner @returns {Promise<string[]>} */
  ownerRepos(owner) {
    return this._call(`/api/v1/owners/${encodeURIComponent(owner)}/repos`, { method: "GET" });
  }

  /** `owners.list()` / `owners.repos(o)` per the §1.1 table naming. */
  get owners() {
    const self = this;
    return {
      list: () => self.ownersList(),
      repos: (owner) => self.ownerRepos(owner),
    };
  }

  // ── internals ───────────────────────────────────────────────────────────

  /**
   * Single-flight popup auth (§1.2): a second 401 while a popup is open MUST
   * reuse the in-flight promise, never open a second window. A rejected
   * promise clears the slot so a later call can retry.
   */
  _authenticate() {
    if (this._popupAuth) return this._popupAuth;
    if (!this.authenticate) {
      return Promise.reject(new ReposError(501, "popup auth unavailable in this environment"));
    }
    const p = Promise.resolve().then(() => this.authenticate());
    this._popupAuth = p;
    const clear = () => {
      if (this._popupAuth === p) this._popupAuth = null;
    };
    p.then(clear, clear);
    return p;
  }

  /** Derive an AbortController from the caller's signal (§1.6). */
  _controller(signal) {
    const c = new AbortController();
    if (signal) {
      if (signal.aborted) c.abort();
      else signal.addEventListener("abort", () => c.abort(), { once: true });
    }
    return c;
  }

  /** Build {url, init} for one call under the current lane. */
  _request(path, { method = "GET", headers, body, sse = true } = {}) {
    const lane = selectLane({ token: this._token }, this._base);
    return {
      url: laneUrl(this._base, lane, path),
      init: { ...laneInit(lane, this._token, headers ?? {}, sse), method },
      body,
    };
  }

  /** Opaque redirect: browser lane `redirect: "manual"` → type "opaqueredirect"/status 0 (§1.2). */
  _isOpaqueRedirect(res) {
    return res.status === 0 && res.type === "opaqueredirect";
  }

  /** Fetch + error normalization (abort → ReposError(499)). */
  async _send(req, controller) {
    try {
      const init = { ...req.init, signal: controller.signal };
      if (req.body !== undefined) init.body = req.body;
      return await this._fetch(req.url, init);
    } catch (err) {
      if (err?.name === "AbortError" || controller.signal.aborted) {
        throw new ReposError(STATUS_ABORTED, "aborted", req.url);
      }
      throw err instanceof ReposError ? err : new ReposError(0, String(err?.message ?? err), req.url);
    }
  }

  /**
   * The fetch wrapper proper (§1.2): send → on 401 or opaque redirect,
   * single-flight popup auth → retry exactly once → dispatch by content type.
   */
  async _call(path, { method = "GET", headers, body, signal, onProgress, sse = true } = {}) {
    const req = this._request(path, { method, headers, body, sse });
    const controller = this._controller(signal);
    let res = await this._send(req, controller);
    if (res.status === 401 || this._isOpaqueRedirect(res)) {
      try {
        await this._authenticate();
      } catch (authErr) {
        if (authErr instanceof ReposError) throw authErr;
        throw new ReposError(res.status, "authentication failed", req.url);
      }
      controller.abort();
      const retryController = this._controller(signal);
      res = await this._send(req, retryController);
      return this._dispatch(res, { url: req.url, onProgress, signal: retryController.signal });
    }
    return this._dispatch(res, { url: req.url, onProgress, signal: controller.signal });
  }

  /** Route a response by content type (§1.4); non-2xx → ReposError. */
  async _dispatch(res, { url, onProgress, signal }) {
    if (!res.ok && res.status !== 0) {
      let text = "";
      try {
        text = await res.text();
      } catch {
        /* body unreadable; fall back to the status line */
      }
      throw new ReposError(res.status, text.trim() || `HTTP ${res.status}`, url);
    }
    const ct = (res.headers.get("content-type") ?? "").toLowerCase();
    if (ct.includes("text/event-stream")) {
      return this._envelope(res, { url, onProgress, signal });
    }
    return this._jsonResponse(res, url);
  }

  /** Plain-JSON path: parse and resolve; empty body → null. */
  async _jsonResponse(res, url) {
    const text = await res.text();
    if (text === "") return null;
    try {
      return JSON.parse(text);
    } catch {
      throw new ReposError(res.status, `invalid JSON from ${url}: ${text.slice(0, 200)}`, url);
    }
  }

  /**
   * SSE envelope handling (§1.4). notice/progress/task surface through the
   * per-call onProgress AND the client-global handler; `result` resolves;
   * `error` throws ReposError(status, message); end-of-stream without result
   * → ReposError(502, "stream ended without a result").
   */
  async _envelope(res, { url, onProgress, signal }) {
    const notify = (p) => {
      if (onProgress) onProgress(p);
      if (this.onProgress) this.onProgress(p);
    };
    let outcome = null;
    await readSse(res, (frame) => {
      switch (frame.event) {
        case "notice":
          notify({ event: "notice", text: frame.data?.text ?? "" });
          break;
        case "progress":
          notify({ event: "progress", ...frame.data });
          break;
        case "task":
          notify({ event: "task", ...frame.data });
          break;
        case "result":
          outcome = { value: frame.data };
          break;
        case "error":
          outcome = {
            error: new ReposError(frame.data?.status ?? 502, frame.data?.message ?? "stream error", url),
          };
          break;
        default:
          break; // unknown event: ignore (forward compatible)
      }
    }, { signal });
    if (outcome?.error) throw outcome.error;
    if (outcome) return outcome.value;
    throw new ReposError(502, "stream ended without a result", url);
  }
}

