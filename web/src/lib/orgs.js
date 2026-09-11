// web/src/lib/orgs.js — org helpers (Forgejo #348).
// Headless-testable: no Solid, no DOM — importable in Node.
//
// The server is authoritative for org writes (CheckOrgOwner gates PUT org,
// member add/remove, and team writes — 403 "org owner required") and for
// edit affordances (the owner-profile GET carries `can_edit`, true for org
// owners via the OwnerEditor seam); these helpers only shape client state:
// create-form validation mirroring identity.ValidOrg (`^[a-z0-9-]{1,39}$`),
// the create body, the owners/detailed is_org badge predicate, and the
// roster-role lookup behind the settings read-only gate.

/** Org slug rule, mirroring identity.ValidOrg (lowercase, 1–39 chars). */
export const ORG_NAME_RE = /^[a-z0-9-]{1,39}$/;

/**
 * validateOrgName(name) → {org} or {error}. Normalizes (trim + lowercase)
 * before checking, mirroring the server's create path (which lowercases
 * the org segment). Empty → required error; charset/length miss → the
 * rule text (never a bare "invalid").
 */
export function validateOrgName(name) {
  const org = String(name ?? "").trim().toLowerCase();
  if (!org) return { error: "organization name is required" };
  if (!ORG_NAME_RE.test(org)) {
    return {
      error:
        "organization names are lowercase letters, digits, and hyphens, 1–39 characters",
    };
  }
  return { org };
}

/**
 * orgCreateBody(form) → the SDK POST body: exactly {org, display_name,
 * description}, coerced to strings. display_name/description are free
 * text ("" = unset); org is normalized (trim + lowercase) so a retry
 * with different casing still addresses the same namespace.
 */
export function orgCreateBody(form) {
  const src = form && typeof form === "object" ? form : {};
  const str = (v) => (typeof v === "string" ? v : "");
  return {
    org: str(src.org).trim().toLowerCase(),
    display_name: str(src.display_name),
    description: str(src.description),
  };
}

/**
 * isOrgRow(row) → whether an owners/detailed row is an org namespace.
 * The server always serves is_org (never null/omitted — Forgejo #348);
 * anything else (legacy servers, null rows) reads as a user.
 */
export function isOrgRow(row) {
  return !!row && row.is_org === true;
}

/**
 * myOrgRole(roster, principal) → "owner" | "member" | null. Looks up the
 * caller's role in a members.json doc ({members: [{principal, role}]});
 * case-insensitive on the principal (the server normalizes the same
 * way). Unknown principals, missing rosters, and unknown role
 * spellings are null — the settings page gates to read-only, never to
 * a 403 form.
 */
export function myOrgRole(roster, principal) {
  const want = String(principal ?? "").trim().toLowerCase();
  const members =
    roster && Array.isArray(roster.members) ? roster.members : [];
  if (!want) return null;
  for (const m of members) {
    if (!m || typeof m !== "object") continue;
    if (String(m.principal ?? "").trim().toLowerCase() !== want) continue;
    if (m.role === "owner" || m.role === "member") return m.role;
    return null;
  }
  return null;
}
