// web/src/lib/writeGate.js — Forgejo #502: the one anonymous-write pattern.
//
// With OIDC anonymous-read mode (#345) a signed-out visitor can browse
// everything but execute ZERO writes. Every write affordance consults this
// module before rendering or firing: anonymous viewers get a link to the
// /login-required interstitial (action preserved in ?next=) instead of an
// SDK mutation; server 401s that slip through (stale identity) route the
// same way via write401Target. The SDK's 401→popup retry never fires for
// these flows — pages pre-flight (preferred) or pass { noPopupAuth: true }.
//
// Pure functions over the two payloads the shell already fetches (same
// shapes as lib/identity.js): me (GET /api/v1/me, null when signed out)
// and discovery (GET /api/v1). Headless-testable under node --test; pages
// read both through the shared "me"/"discovery" useData keys (zero new
// requests — law 6). No new deps.

/**
 * True when the viewer must not write: a usable auth mode where the
 * identity is absent (signed out) or explicitly anonymous. None mode and
 * unloaded discovery never gate (nothing to log in to / legacy nav).
 */
export function isAnonymousViewer(me, discovery) {
  const mode = discovery?.auth?.mode;
  if (mode !== "token" && mode !== "oidc") return false;
  if (me == null) return true;
  return me.anonymous !== false;
}

/**
 * Server-side sanitizeNext mirror (auth_oidc.go): the return target must
 * start with a single "/" (no "//host", no scheme). Anything else → "/".
 */
export function sanitizeNextClient(next) {
  if (typeof next !== "string" || !next.startsWith("/") || next.startsWith("//")) return "/";
  return next;
}

/**
 * anonWriteTarget({me, discovery}, next, action) → the interstitial href
 * for an anonymous write attempt, or null when the viewer may write.
 * next is the attempted ACTION url (preserved through login); action names
 * it in the interstitial copy ("Star this repository", …).
 */
export function anonWriteTarget({ me = null, discovery = null } = {}, next = "/", action = "") {
  if (!isAnonymousViewer(me, discovery)) return null;
  const safe = sanitizeNextClient(next);
  let href = `/login-required?next=${encodeURIComponent(safe)}`;
  if (action) href += `&action=${encodeURIComponent(action)}`;
  return href;
}

/**
 * write401Target(err, {me, discovery}, fallbackNext, action) → the
 * interstitial href when a mutation 401s for an anonymous viewer (stale
 * identity, direct API use), or null when the error is not an anonymous
 * write refusal (non-401, or signed-in callers keep the tray/popup paths).
 */
export function write401Target(err, { me = null, discovery = null } = {}, fallbackNext = "/", action = "") {
  if (err?.status !== 401 && err?.unauthorized !== true) return null;
  return anonWriteTarget({ me, discovery }, fallbackNext, action);
}

/**
 * interstitialLoginHref(discovery, next) → the OIDC entry the interstitial
 * Log-in button uses: the discovery-advertised login_url with the sanitized
 * return target ("" when login is unavailable — the page renders the
 * disabled explanation instead).
 */
export function interstitialLoginHref(discovery, next = "/") {
  const u = discovery?.auth?.login_url;
  if (typeof u !== "string" || u === "" || discovery?.auth?.browser_login !== true) return "";
  return `${u}?next=${encodeURIComponent(sanitizeNextClient(next))}`;
}
