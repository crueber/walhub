// web/src/lib/access.js — access-subject helpers (Forgejo #361).
// Headless-testable: no Solid, no DOM — importable in Node.
//
// Mirrors identity.validSubject (internal/identity/access.go:41-57):
// a binding subject is `user:<email>` or `team:<org>/<slug>`, where the
// org matches identity.ValidOrg (`^[a-z0-9-]{1,39}$`), the slug matches
// identity.ValidSlug (`^[a-z0-9-]{1,64}$`), and the email matches
// identity.ValidPrincipal (parseable address, ≤ 254 chars, no `/`).
// The client checks are typo-catching only — the server revalidates the
// full document on PUT, so these helpers stay lenient where the server
// is lenient (email shape) and strict where it is strict (charsets).
// Like validateOrgName (lib/orgs.js), inputs normalize (trim + lowercase)
// before checking, so `Team:ACME/Frontend` composes to `team:acme/frontend`.

/** Org rule, mirroring identity.ValidOrg (lowercase, 1–39 chars). */
export const ACCESS_ORG_RE = /^[a-z0-9-]{1,39}$/;

/** Team-slug rule, mirroring identity.ValidSlug (lowercase, 1–64 chars). */
export const ACCESS_SLUG_RE = /^[a-z0-9-]{1,64}$/;

/**
 * looksLikeEmail(s) → whether s plausibly passes identity.ValidPrincipal:
 * non-empty, ≤ 254 chars, no whitespace or `/`, and an `@` with text on
 * both sides. Deliberately looser than mail.ParseAddress (quoted local
 * parts, display names) — the server is authoritative; this only catches
 * typos before the add.
 */
export function looksLikeEmail(s) {
  const v = String(s ?? "").trim();
  if (!v || v.length > 254 || /[\s/]/.test(v)) return false;
  const at = v.indexOf("@");
  return at > 0 && at < v.length - 1;
}

/**
 * validateAccessSubject(raw) → {subject} normalized, or {error} with a
 * friendly note for the Access-tab note line (never a bare "invalid").
 * Normalizes exactly like the server's user-subject path (trim +
 * lowercase) and, for team subjects, like validateOrgName (lowercase
 * before checking — the server only accepts lowercase org/slug, so
 * `Team:ACME/X` composes to the spelling the server accepts).
 */
export function validateAccessSubject(raw) {
  const text = String(raw ?? "").trim();
  if (!text) {
    return { error: "subject is required — e.g. user:jane@example.com or team:acme/frontend" };
  }
  const lower = text.toLowerCase();
  if (lower.startsWith("user:")) {
    const email = text.slice("user:".length).trim().toLowerCase();
    if (!looksLikeEmail(email)) {
      return { error: "user subjects look like user:jane@example.com — check the email spelling" };
    }
    return { subject: `user:${email}` };
  }
  if (lower.startsWith("team:")) {
    const rest = text.slice("team:".length).trim().toLowerCase();
    const slash = rest.indexOf("/");
    const org = slash < 0 ? "" : rest.slice(0, slash);
    const slug = slash < 0 ? "" : rest.slice(slash + 1);
    if (!slash || !ACCESS_ORG_RE.test(org) || !ACCESS_SLUG_RE.test(slug)) {
      return { error: "team subjects look like team:acme/frontend — lowercase letters, digits, and hyphens" };
    }
    return { subject: `team:${org}/${slug}` };
  }
  return { error: 'subject must start with "user:" or "team:" — e.g. user:jane@example.com or team:acme/frontend' };
}

/**
 * composeTeamSubject(org, slug) → the normalized `team:<org>/<slug>`
 * spelling the team dropdown writes into the subject field. Normalizes
 * (trim + lowercase) so a mixed-case roster row still composes the
 * server-accepted spelling; legality checking stays in
 * validateAccessSubject.
 */
export function composeTeamSubject(org, slug) {
  return `team:${String(org ?? "").trim().toLowerCase()}/${String(slug ?? "").trim().toLowerCase()}`;
}

/**
 * asTeamList(payload) → the team rows for the picker. The server returns
 * the teams list as a bare array (`[{slug, name?, members?}]`, see
 * Org.jsx TeamsTab); anything else (null, 404-mapped, envelope drift)
 * reads as no teams — the picker hides and free text stays the fallback.
 */
export function asTeamList(payload) {
  const rows = Array.isArray(payload) ? payload : payload?.teams;
  if (!Array.isArray(rows)) return [];
  return rows.filter((t) => t && typeof t.slug === "string" && t.slug);
}

/**
 * teamOptionLabel(team) → the dropdown row text: `slug — name`, or just
 * `slug` when the name is unset or repeats the slug.
 */
export function teamOptionLabel(team) {
  const slug = String(team?.slug ?? "");
  const name = String(team?.name ?? "").trim();
  if (!name || name.toLowerCase() === slug.toLowerCase()) return slug;
  return `${slug} — ${name}`;
}
