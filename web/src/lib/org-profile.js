// web/src/lib/org-profile.js — org-profile form helpers (Forgejo #359).
// Headless-testable: no Solid, no DOM — importable in Node.
//
// The org PUT is a full-document replace (same as the owner profile):
// the form seeds every field from the fetched org doc and saves all of
// them. Field spelling matches the owner profile (display_name, location,
// timezone, bio_markdown) plus the org tagline (description); the server
// stamps updated_at itself and owns the avatar pointer (avatar uploads
// go through orgs.avatar, never this body).

/** Field keys the edit form (and the SDK PUT body) carries. */
export const ORG_PROFILE_FIELDS = ["display_name", "description", "location", "timezone", "bio_markdown"];

/**
 * emptyOrgProfile(org) → form-state defaults for an unset profile.
 * Mirrors the server convention (`""` = unset).
 */
export function emptyOrgProfile(org) {
  return {
    org: String(org ?? ""),
    display_name: "",
    description: "",
    location: "",
    timezone: "",
    bio_markdown: "",
  };
}

/**
 * normalizeOrgProfile(doc, org) → form state. Coerces a fetched org doc
 * (missing/null fields) into the exact shape the form edits; non-string
 * values fall back to `""` so inputs stay controlled. Extra keys
 * (version, timestamps, avatar_*, can_edit) never leak into the PUT
 * body — the caller picks ORG_PROFILE_FIELDS when saving.
 */
export function normalizeOrgProfile(doc, org) {
  const src = doc && typeof doc === "object" ? doc : {};
  const str = (v) => (typeof v === "string" ? v : "");
  return {
    org: str(src.org) || String(org ?? ""),
    display_name: str(src.display_name),
    description: str(src.description),
    location: str(src.location),
    timezone: str(src.timezone),
    bio_markdown: str(src.bio_markdown),
  };
}

/**
 * orgSaveBody(form) → the SDK PUT body: exactly the five editable
 * fields, in order. The server ignores anything else and stamps
 * updated_at itself.
 */
export function orgSaveBody(form) {
  const src = form && typeof form === "object" ? form : {};
  const str = (v) => (typeof v === "string" ? v : "");
  return {
    display_name: str(src.display_name),
    description: str(src.description),
    location: str(src.location),
    timezone: str(src.timezone),
    bio_markdown: str(src.bio_markdown),
  };
}
