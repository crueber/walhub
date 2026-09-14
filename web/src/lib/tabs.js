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
  // WAL / settings. WAL has no tab of its own since issue #123 (it is a
  // settings sidebar section now), so the kept /wal route highlights
  // Settings; a highlight-nothing id would leave the tab bar blank.
  wal: "settings",
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

/**
 * tabBadge(summary, tabId) → number: the open-count badge numerator for a
 * repo tab (issue #319). The summary carries open_issues/open_pulls from
 * the shared P4 index; only the Issues and Pulls tabs badge. Returns 0
 * for every other tab, for missing/loading summaries, and for servers
 * that predate the fields — the tab render hides the badge at 0 (GitHub
 * semantics: no zero badges).
 */
export function tabBadge(summary, tabId) {
  if (!summary) return 0;
  if (tabId === "issues") return Math.max(0, summary.open_issues ?? 0);
  if (tabId === "pulls") return Math.max(0, summary.open_pulls ?? 0);
  return 0;
}

/**
 * showChecksTab(summary, opts) → boolean: the Checks-tab visibility rule
 * (issue #505, extended #513). The summary carries has_checks from the
 * CAS'd hot-window checks index (false = none — the #319 badge discipline).
 * Fail-open everywhere else: loading/deleted summaries (null/undefined)
 * and pre-#505 servers (no field) keep the tab — hiding on unknown
 * would flicker the strip on every load and strand old servers with no
 * way into the Checks page. The ONE exception is an explicit auth
 * failure (issue #513: `opts.denied` — the shell's summary fetch
 * answered 401, so "unknown" is the steady state, not a transient):
 * a gated viewer can reach no check data at all, so the tab hides.
 * Loading stays fail-open (flicker/old-server reasons above). This gates
 * the tab only, never navigation: the /:owner/:name/checks route still
 * renders its empty state (deep links stay sane) and activeTab keeps
 * mapping checks/check segments above.
 */
export function showChecksTab(summary, opts = {}) {
  if (opts.denied) return false;
  if (!summary) return true;
  return summary.has_checks !== false;
}
