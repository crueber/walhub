// web/src/lib/compare.js — headless branch-comparison helpers for the
// open-PR page (issue #34): split a head history against a base history,
// tip subjects, bounded-count formatting. Pure functions over the
// `commits()` page shape ({commits: [{sha, subject, …}], sha, more}) —
// DOM-free so `node --test` covers them; PullNew.jsx keeps only fetch+render.
//
// The trick: with no server compare endpoint, both histories (newest-first,
// n = PREVIEW_WINDOW) are fetched in parallel and intersected client-side.
// The first head commit also present in the base history approximates the
// merge-base (exact whenever both windows cover it); the first base commit
// present in the head history does the same from the other side. A walk
// that leaves its window without meeting reports its count as a lower
// bound ("42+") — but ONLY when that side's page says `more`; an
// exhausted window with no meeting point is exact (e.g. unrelated roots).

/** Preview window: both histories are fetched with n = this. */
export const PREVIEW_WINDOW = 100;

function shaList(commits) {
  return (Array.isArray(commits) ? commits : []).filter((c) => c?.sha);
}

/**
 * compareHistories(headPage, basePage) → { unique, ahead, behind,
 * truncatedHead, truncatedBase }: unique are the head-only commits that
 * would land on the PR (head walk up to the merge-base approximation);
 * behind counts base-only commits (base walk up to the same point from
 * the other side). Truncation flags are set only when the meeting point
 * fell outside a window that has more pages.
 */
export function compareHistories(headPage, basePage) {
  const head = shaList(headPage?.commits);
  const base = shaList(basePage?.commits);
  const baseSet = new Set(base.map((c) => c.sha));
  const headSet = new Set(head.map((c) => c.sha));

  const unique = [];
  let metBase = false;
  for (const c of head) {
    if (baseSet.has(c.sha)) {
      metBase = true;
      break;
    }
    unique.push(c);
  }
  let behind = 0;
  let metHead = false;
  for (const c of base) {
    if (headSet.has(c.sha)) {
      metHead = true;
      break;
    }
    behind += 1;
  }
  return {
    unique,
    ahead: unique.length,
    behind,
    truncatedHead: !metBase && headPage?.more === true,
    truncatedBase: !metHead && basePage?.more === true,
  };
}

/** First line of a commit's subject/message; "" when missing. */
export function tipSubject(commit) {
  const text = commit?.subject ?? commit?.message ?? "";
  return String(text).split("\n", 1)[0].trim();
}

/** "42", or "42+" when the walk left a window that has more pages. */
export function fmtBounded(n, truncated) {
  return truncated ? `${n}+` : `${n}`;
}

/**
 * Shorten a branch ref for ref-taking read APIs: the `commits?ref=`
 * resolver addresses branches by short name (`main`), while pickers and
 * the PR open call use full names (`refs/heads/main`). Anything else
 * (tags, SHAs) passes through untouched.
 */
export function toShortRef(ref) {
  return String(ref ?? "").replace(/^refs\/heads\//, "");
}
