// web/src/lib/profile.js — owner-profile form helpers (Forgejo #234).
// Headless-testable: no Solid, no DOM — importable in Node.
//
// The server is authoritative for edit rights (the profile GET carries
// `can_edit`, computed from the host-admin / name-match / org-owner rule);
// these helpers only shape client state: empty defaults for a new profile
// and normalization of a fetched doc into form fields.

/** Field keys the edit form (and the SDK PUT body) carries. */
export const PROFILE_FIELDS = ["display_name", "location", "timezone", "bio_markdown"];

/**
 * emptyProfile(owner) → form-state defaults for an unset profile.
 * Mirrors the server's empty-profile convention (`""` = unset).
 */
export function emptyProfile(owner) {
  return {
    owner: String(owner ?? ""),
    display_name: "",
    location: "",
    timezone: "",
    bio_markdown: "",
  };
}

/**
 * normalizeProfile(doc, owner) → form state. Coerces a fetched profile doc
 * (missing/null fields, unknown-owner empties) into the exact shape the
 * form edits; non-string values fall back to `""` so inputs stay
 * controlled. Extra keys (updated_at, can_edit) never leak into the PUT
 * body — the caller picks PROFILE_FIELDS when saving.
 */
export function normalizeProfile(doc, owner) {
  const src = doc && typeof doc === "object" ? doc : {};
  const str = (v) => (typeof v === "string" ? v : "");
  return {
    owner: str(src.owner) || String(owner ?? ""),
    display_name: str(src.display_name),
    location: str(src.location),
    timezone: str(src.timezone),
    bio_markdown: str(src.bio_markdown),
  };
}

/**
 * profileSaveBody(form) → the SDK PUT body: exactly the four editable
 * fields, in order. The server ignores anything else and stamps
 * updated_at itself.
 */
export function profileSaveBody(form) {
  const src = form && typeof form === "object" ? form : {};
  const str = (v) => (typeof v === "string" ? v : "");
  return {
    display_name: str(src.display_name),
    location: str(src.location),
    timezone: str(src.timezone),
    bio_markdown: str(src.bio_markdown),
  };
}
