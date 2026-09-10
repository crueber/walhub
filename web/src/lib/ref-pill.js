// web/src/lib/ref-pill.js — issue #252: the header pill follows the viewed ref.
//
// The summary API is ref-blind by design (Head is the default-branch target
// only), so the pill is client composition: each ref-addressed tab (Tree,
// Blob, Commits, Commit, CheckDetail) publishes its resolved {name, sha} into
// the RepoCtx `viewed` signal, and the pill reads context-first with the
// summary head as fallback (non-ref tabs never publish, so Issues/Pulls/
// Settings keep the default-branch head — no #214 regression).
//
// Pure and dependency-free so `node --test` covers the derivation headlessly
// (no SolidJS install, same rule as lib/collab.js).

/** Strip the refs/heads|tags prefix for display (moved from Repo.jsx — the single definition). */
export function shortRef(name) {
  return String(name ?? "").replace(/^refs\/(?:heads|tags)\//, "");
}

/**
 * pillHead(viewed, summaryHead) → the head object the pill renders:
 * context-first, summary fallback. Either side may be null/undefined
 * (loading, empty repo) — the label then falls back to "refs".
 */
export function pillHead(viewed, summaryHead) {
  if (viewed && viewed.sha) return viewed;
  return summaryHead ?? null;
}

/**
 * pillLabel(head) → pill text:
 * - no head → "refs" (the pre-#214 placeholder contract);
 * - sha-addressed (empty name, or name === sha — commit detail, blob/tree at
 *   a raw sha, checks/:sha) → the short sha, honestly (no "sha @ sha");
 * - otherwise → `{branch} @ {shortsha}` (the #214 shape).
 */
export function pillLabel(head) {
  if (!head || !head.sha) return "refs";
  const sha = String(head.sha);
  const name = shortRef(head.name ?? "");
  if (!name || name === sha) return sha.slice(0, 10);
  return `${name} @ ${sha.slice(0, 10)}`;
}
