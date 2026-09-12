/**
 * Visibility badge + option helpers (Forgejo #345, modes split #374): the
 * summary `visibility` (`"public"|"authenticated"|"private"`, unwired →
 * `""`) and the detailed-listing rows carry the spelling, so badges render
 * with no extra fetch — the same payload discipline as the mirror badge
 * (Forgejo #281).
 *
 * Unknown/empty visibility renders nothing (never assume private).
 */

/** Badge descriptor for a summary doc or listing row. */
export function visibilityBadge(doc) {
  const vis = String(doc?.visibility ?? "").toLowerCase();
  switch (vis) {
    case "public":
      return { show: true, label: "public", title: "public — anyone may read" };
    case "authenticated":
      return {
        show: true,
        label: "authenticated",
        title: "private — signed-in users may read",
      };
    case "private":
      return {
        show: true,
        label: "private",
        title: "private — owner or organization members only",
      };
    default:
      return { show: false, label: "", title: "" };
  }
}

/** The three spellings the access PUT accepts (server re-validates). */
export const VISIBILITY_OPTIONS = ["public", "authenticated", "private"];

/** True when the spelling is one the server accepts. */
export function isVisibility(v) {
  return v === "public" || v === "authenticated" || v === "private";
}

/**
 * visibilityOptions(isOrg) → the owner-appropriate select options
 * (Forgejo #374): public + logged-in-only + the owner-shaped private
 * mode. User-owned repos offer "private — owner only"; org-owned repos
 * offer "private — org members only" (the org-vs-user verdict rides the
 * #348 owner-kind marker — `orgs.get` resolving vs 404ing). The old
 * "members only" label is gone.
 */
export function visibilityOptions(isOrg) {
  return [
    { value: "public", label: "public — anyone may read" },
    { value: "authenticated", label: "private — logged in only" },
    isOrg
      ? { value: "private", label: "private — org members only" }
      : { value: "private", label: "private — owner only" },
  ];
}
