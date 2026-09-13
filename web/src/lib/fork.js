/**
 * Fork-form helpers (issue #424): the fork target defaults and shapes
 * shared by the Fork page. Pure logic — covered by node --test.
 */

/** Default fork name for a repo (`<repo>-fork`, the server default). */
export function forkDefaultName(repoName) {
  return `${String(repoName ?? "").trim()}-fork`;
}

/** Short branch name for a full ref (`refs/heads/x` → `x`, else verbatim). */
export function forkBranchShort(ref) {
  const s = String(ref ?? "");
  return s.startsWith("refs/heads/") ? s.slice("refs/heads/".length) : s;
}
