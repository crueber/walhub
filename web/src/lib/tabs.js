// web/src/lib/tabs.js — the repository tab matcher (§2.6).
//
// Pure module (no Solid, no DOM): derive the active repo tab from the FIRST
// path segment after /:owner/:name. An earlier regex-over-the-whole-pathname
// matcher mis-highlighted blob/tree paths whose filenames contain a tab word
// (blob/main/cmd/walhub/checks.go lit up Checks) — every tab had this bug,
// because the filename, not the section, matched (issue #25).

/** First-segment → tab id. Every value names a tab in Repo.jsx TABS; any
 *  section absent here falls back to "code", which always exists and
 *  highlights (a highlight-nothing id would leave the tab bar blank). */
const SECTION_TABS = {
  // Code (repo root + file browsing).
  tree: "code",
  blob: "code",
  // Commits.
  commits: "commits",
  commit: "commits",
  // Issues (+ issue sub-collections, all rendered under the Issues tab).
  issues: "issues",
  labels: "issues",
  milestones: "issues",
  // Pulls (incl. /pull/:num sub-pages, rendered under the Pulls tab).
  pulls: "pulls",
  pull: "pulls",
  // Checks / releases (own tabs AND own routes — but a *filename* deeper in
  // a blob/tree path must never map here; only the first segment decides).
  checks: "checks",
  check: "checks",
  releases: "releases",
  release: "releases",
  // WAL / settings.
  wal: "wal",
  settings: "settings",
};

/**
 * Map a repo pathname to its tab id.
 *
 * Only the first segment after /:owner/:name decides — everything deeper
 * (ref, rest path, filenames like checks.go or settings.json) is ignored.
 * Unknown or missing sections fall back to "code".
 */
export function activeTab(pathname) {
  const path = String(pathname ?? "").split(/[?#]/, 1)[0];
  const segs = path.split("/").filter(Boolean);
  if (segs.length < 3) return "code"; // /:owner, /:owner/:name, or /
  return SECTION_TABS[segs[2].toLowerCase()] ?? "code";
}
