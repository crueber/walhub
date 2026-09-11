/**
 * Visibility badge helper (Forgejo #345): the summary `visibility`
 * (`"public"|"private"`, unwired → `""`) and the detailed-listing rows
 * carry the spelling, so badges render with no extra fetch — the same
 * payload discipline as the mirror badge (Forgejo #281).
 *
 * Unknown/empty visibility renders nothing (never assume private).
 */

/** Badge descriptor for a summary doc or listing row. */
export function visibilityBadge(doc) {
  const vis = String(doc?.visibility ?? "").toLowerCase();
  if (vis !== "public" && vis !== "private") return { show: false, label: "", title: "" };
  return {
    show: true,
    label: vis,
    title: vis === "public" ? "public — anyone may read" : "private — members only",
  };
}

/** The two spellings the access PUT accepts (server re-validates). */
export const VISIBILITY_OPTIONS = ["public", "private"];

/** True when the spelling is one the server accepts. */
export function isVisibility(v) {
  return v === "public" || v === "private";
}
