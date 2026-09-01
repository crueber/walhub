/**
 * Popup auth flow (12_web_ui.md §1.2).
 *
 * Opens `<base>/api-browser/v1/authenticate` in a popup, waits for a
 * `postMessage` of `{type: "repos:authenticated"}` from OUR origin, resolves.
 * Single-flight is the caller's job (core.js holds one `popupAuth` promise per
 * client instance); a rejected promise clears that slot so a later call can
 * retry.
 *
 * Testability: the module reads its collaborators off a mutable `deps` object
 * (`open`, `setTimeout`, `clearTimeout`, event registry) so `node --test` can
 * inject fakes without a DOM.
 */
import { ReposError } from "./errors.js";

const AUTH_TIMEOUT_MS = 120_000;
const POPUP_FEATURES = "width=520,height=640";

/**
 * Collaborators, overridable for tests.
 * @type {{open: Function, addEventListener: Function, removeEventListener: Function, setTimeout: Function, clearTimeout: Function, origin: () => string}}
 */
export const deps = {
  open: (url, name, features) => globalThis.open?.(url, name, features),
  addEventListener: (t, fn) => globalThis.addEventListener?.(t, fn),
  removeEventListener: (t, fn) => globalThis.removeEventListener?.(t, fn),
  setTimeout: (fn, ms) => globalThis.setTimeout?.(fn, ms),
  clearTimeout: (id) => globalThis.clearTimeout?.(id),
  /** Page origin the message must come from; overridable for tests. */
  origin: () => globalThis.location?.origin ?? "",
};

/**
 * Open the authenticate popup and await the `repos:authenticated` message.
 * Rejects with `ReposError(504, "popup auth timed out")` after 120 s.
 * Cleanup runs exactly once: listener removed, timer cleared, popup closed.
 *
 * @param {string} base API base URL
 * @returns {Promise<void>}
 */
export function openAuthPopup(base) {
  return new Promise((resolve, reject) => {
    let settled = false;
    let timer;
    const win = deps.open(`${base}/api-browser/v1/authenticate`, "repos-auth", POPUP_FEATURES);
    const cleanup = () => {
      if (settled) return;
      settled = true;
      deps.removeEventListener("message", onMsg);
      if (timer !== undefined) deps.clearTimeout(timer);
      try {
        win?.close?.();
      } catch {
        // the popup may already be gone
      }
    };
    const onMsg = (ev) => {
      if (ev.origin !== deps.origin()) return; // our origin only
      if (ev.data?.type !== "repos:authenticated") return;
      cleanup();
      resolve();
    };
    timer = deps.setTimeout(() => {
      cleanup();
      reject(new ReposError(504, "popup auth timed out"));
    }, AUTH_TIMEOUT_MS);
    deps.addEventListener("message", onMsg);
  });
}

/**
 * Probe: can a popup auth run in this environment?
 * @returns {boolean}
 */
export function canAuthenticate() {
  return typeof deps.open === "function" && typeof deps.addEventListener === "function";
}

/**
 * The popup URL for a base (used by tests and diagnostics).
 * @param {string} base
 * @returns {string}
 */
export function authenticateUrl(base) {
  return `${base}/api-browser/v1/authenticate`;
}