// web/src/lib/orgs.js — org helpers (Forgejo #348).
// Headless-testable: no Solid, no DOM — importable in Node.
//
// The server is authoritative for org writes (CheckOrgOwner gates PUT org,
// member add/remove, and team writes — 403 "org owner required") and for
// edit affordances (the owner-profile GET carries `can_edit`, true for org
// owners via the OwnerEditor seam); these helpers only shape client state:
// create-form validation mirroring identity.ValidOrg (`^[a-z0-9-]{1,39}$`),
// the create body, and the owners/detailed is_org badge predicate. (The
// settings read-only gate keys on the org-slug owner profile's `can_edit`
// — server-authoritative via the OwnerEditor seam — never on a local
// roster lookup.)

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
 * isValidOwnerPart(name) → whether name is usable as a repo owner segment.
 * Mirrors git.validPart for the owner half of ParseRepoId (ASCII
 * [A-Za-z0-9._-], 1–100 chars, no leading dot, not ".."): Forgejo #370 —
 * raw emails (or any @-carrying principal) can never be an owner, so the
 * import/new-repo dropdowns filter them out (the server 400s bad targets
 * regardless — the dropdown is honesty, never the gate).
 */
export function isValidOwnerPart(name) {
  const s = String(name ?? "");
  if (s.length === 0 || s.length > 100 || s === ".." || s[0] === ".") return false;
  return /^[A-Za-z0-9._-]+$/.test(s);
}

/**
 * allowedOwners(client, principal) → Promise<string[]>.
 * The #346 owner-dropdown options: `[self, ...memberOrgs]` (sorted),
 * filtered to valid owner segments (#370 — a stale email principal
 * never reaches the options, so the bad-target import failure cannot
 * be selected into existence).
 * Resolves via `client.orgs.list()` + one `client.orgs.members.get(org,
 * principal)` probe per org (the SDK maps 404 → null = not a member).
 * Any failure degrades to `[self]` — the SERVER is authoritative (a
 * foreign owner gets a 403 naming the allowed owners); the dropdown is
 * honesty, never the gate. Host admins needing a foreign namespace use
 * the API directly (the UI cannot see the admin flag on `me`).
 */
export async function allowedOwners(client, principal) {
  const self = String(principal ?? "").trim();
  if (!self) return [];
  const keep = (list) => list.filter(isValidOwnerPart);
  let orgs;
  try {
    orgs = (await client.orgs.list()) ?? [];
  } catch {
    return keep([self]);
  }
  const mine = [];
  for (const entry of orgs) {
    const name = typeof entry === "string" ? entry : entry?.org;
    if (!name) continue;
    try {
      const row = await client.orgs.members.get(name, self);
      if (row) mine.push(name);
    } catch {
      // A failed probe is "not proven a member" — skip, keep the rest.
    }
  }
  return keep([self, ...mine.sort()]);
}
