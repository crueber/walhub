// web/src/lib/invites.js — invitation display rules (Forgejo #362).
//
// Pure module (no Solid, no DOM): the expiry predicate, kind/scope labels,
// and the API-accept-URL → inbox-deep-link parser shared by the org
// Invitations tab and the /invitations inbox page. Headless-testable under
// `node --test`; all DOM (DateTime, chips, buttons) lives at the call
// sites, the same split settingsNav.js uses.

/**
 * isInviteExpired(inv, now?) → true when expires_at parses and is at or
 * before now. Missing or unparseable expiry reads as NOT expired (fail
 * open on display — pre-#362 inbox rows omit the field; the server still
 * fails closed with 409 "invitation expired" on accept).
 */
export function isInviteExpired(inv, now = Date.now()) {
  const raw = inv?.expires_at;
  if (raw === undefined || raw === null || raw === "") return false;
  const t = new Date(raw).getTime();
  if (!Number.isFinite(t)) return false;
  return t <= now;
}

/**
 * inviteKind(inv) → "repo" | "org". The issuer list/preview shapes carry
 * kind; inbox rows (mine) predate it and derive from which scope is set.
 */
export function inviteKind(inv) {
  if (inv?.kind === "repo" || inv?.kind === "org") return inv.kind;
  return inv?.repo ? "repo" : "org";
}

/**
 * inviteScope(inv) → the display target: "acme/repo" for repo invites,
 * "acme" for org invites, "" when neither scope is set.
 */
export function inviteScope(inv) {
  if (inv?.repo) return String(inv.repo);
  if (inv?.org) return String(inv.org);
  return "";
}

/**
 * invitePageLink(acceptUrl) → "/invitations?id=&token=" deep link parsed
 * out of the API accept URL the create endpoints return
 * ("/api/v1/invitations/{id}?token={token}"). "" when the input does not
 * parse — call sites fall back to printing the API link verbatim.
 */
export function invitePageLink(acceptUrl) {
  if (typeof acceptUrl !== "string") return "";
  const m = acceptUrl.match(/\/api\/v1\/invitations\/([^/?#]+)\?token=([^&#]+)/);
  if (!m) return "";
  return `/invitations?id=${encodeURIComponent(m[1])}&token=${encodeURIComponent(m[2])}`;
}
