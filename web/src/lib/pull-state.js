// web/src/lib/pull-state.js — Forgejo #517 pure helpers (dependency-free,
// headless-testable per 12 §5): the PR header state badge + the Close/Reopen
// visibility rule.
//
// The server contract (internal/pulls/service.go UpdatePR): state flips are
// `PUT …/pulls/{num}` `{state: "open"|"closed"}` (no reason — PR state is
// open|closed only), auth is **author or triage** (hierarchical P6 ladder,
// so write ⊇ triage), and closing a merged PR is refused (409). Client
// gating is cosmetic per perms.jsx — the server is authoritative; 403/409
// surface in the error tray via the composer's reportError path.
//
// The badge mirrors the issue page convention (Issue.jsx header: title left,
// state badge right, `chip chip-open` / `chip chip-closed`): open and
// plain-closed reuse those exact classes. Merged wins over closed (merge
// closes the thread too — merge.go stamps StateClosed alongside Merged),
// rendered as "Merged" on the new `chip-merged` class (ui.css).

/** P6 ladder mirror (perms.jsx owns the canonical copy; order frozen by 08 §5). */
const LADDER = ["read", "triage", "write", "maintain", "admin"];

/** Backend normPrincipal mirror (pulls.go): lowercase + trim. */
export function normPrincipal(p) {
  return String(p ?? "").toLowerCase().trim();
}

/** Author match, backend-comparison semantics (empty author never matches). */
export function isPullAuthor(threadAuthor, mePrincipal) {
  const a = normPrincipal(threadAuthor);
  return a !== "" && a === normPrincipal(mePrincipal);
}

/** Hierarchical triage floor: triage, write, maintain, admin all pass. */
export function roleAtLeastTriage(role) {
  return LADDER.indexOf(role ?? "") >= LADDER.indexOf("triage");
}

/**
 * canModifyPullState({author, mePrincipal, role}) — the client mirror of
 * UpdatePR state auth (author or triage). Anonymous (null principal/role)
 * fails both arms naturally.
 */
export function canModifyPullState({ author, mePrincipal, role } = {}) {
  return isPullAuthor(author, mePrincipal) || roleAtLeastTriage(role);
}

/**
 * pullBadgeView(thread, pr) → {text, cls}: the header state badge.
 * Merged wins (merge stamps StateClosed too, so thread.state alone cannot
 * tell merged from plain-closed); loading (no thread yet) reads open.
 */
export function pullBadgeView(thread, pr) {
  if (pr?.merged) return { text: "Merged", cls: "chip chip-merged" };
  if ((thread?.state ?? "open") === "open") return { text: "Open", cls: "chip chip-open" };
  return { text: "Closed", cls: "chip chip-closed" };
}

/**
 * pullCloseVisibility({thread, pr, mePrincipal, role}) → {showClose, showReopen}:
 * unmerged only (merged PRs expose no lifecycle control — a merged close is
 * a server 409), same author-or-triage rule both ways, driven by the live
 * thread state (the page's state source of truth, like the issue page).
 */
export function pullCloseVisibility({ thread, pr, mePrincipal, role } = {}) {
  if (pr?.merged) return { showClose: false, showReopen: false };
  if (!canModifyPullState({ author: thread?.author, mePrincipal, role })) {
    return { showClose: false, showReopen: false };
  }
  if ((thread?.state ?? "open") === "open") return { showClose: true, showReopen: false };
  return { showClose: false, showReopen: true };
}
