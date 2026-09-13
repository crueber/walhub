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
//
// Forgejo #482: the pinned default-branch row derives here too — the picker
// already holds the summary head (the default-branch target), so the pin is
// client composition with no wire change: pinnedDefault returns the {name,
// sha} to hoist when kind is branches and the head names a refs/heads/ ref,
// and dedupeRefs drops the streamed duplicate (small repos) so the pin never
// renders twice.

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

/**
 * pinnedDefault(head, kind) → the ref object to pin as the first row of the
 * branch list, or null. Pins only when kind === "branches" and head carries
 * a refs/heads/ name with a sha (the summary head is the default-branch
 * target — HeadTarget — so no wire change is needed to reach a default that
 * sorts past the 50-ref page window). Tags never pin (a heads/ name is not
 * a tag); empty/sha-addressed heads pin nothing.
 */
export function pinnedDefault(head, kind) {
  if (kind !== "branches") return null;
  if (!head || !head.sha) return null;
  const name = String(head.name ?? "");
  if (!name.startsWith("refs/heads/")) return null;
  return { name, sha: head.sha };
}

/**
 * dedupeRefs(pinned, refs) → refs minus the pinned row (matched by full
 * ref name), so small repos whose streamed page already contains the default
 * branch never render it twice. Null pinned returns the list untouched.
 */
export function dedupeRefs(pinned, refs) {
  const list = refs ?? [];
  if (!pinned) return list;
  return list.filter((r) => r && r.name !== pinned.name);
}
