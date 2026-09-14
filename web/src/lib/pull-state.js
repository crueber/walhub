// web/src/lib/pull-state.js — Forgejo #517 pure helpers (dependency-free,
// headless-testable per 12 §5) plus the Forgejo #521 timeline text map: the
// PR header state badge, the Close/Reopen visibility rule, and the
// conversation textFor.
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
 * pullEventText(ev) → string|null: the PR conversation textFor for the ONE
 * ThreadTimeline renderer (Forgejo #521 — moved verbatim out of Pull.jsx so
 * the map is headless-testable; the timeline contract is untouched).
 * The `commented` kind renders as a body (returns null); every other kind
 * renders as a single-line muted system message ("{actor} {text}").
 *
 * The ONE #521 behavior change: "opened" is a system row ("{actor} opened"),
 * NOT a body. The description now lives in the dedicated first-comment
 * block rendering the live pr.body editable view (internal/pulls/model.go:
 * the opened event carries the ORIGINAL body; Body is the editable view —
 * body edits append no event, so the timeline row would go stale and, worse,
 * duplicate the block). The row keeps its event-0 anchor (deep links hold).
 */
export function pullEventText(ev) {
  switch (ev.type) {
    case "opened":
      return "opened";
    case "commented":
      return null; // rendered as body
    case "title_changed":
      return `retitled “${ev.from}” → “${ev.to}”`;
    case "state_changed":
      return ev.to === "closed" ? "closed" : "reopened";
    case "merged":
      return `merged as ${(ev.merge_commit_sha ?? "").slice(0, 12)} (${ev.strategy ?? "merge"})`;
    case "head_force_pushed":
      return `head force-pushed ${(ev.from ?? "").slice(0, 12)} → ${(ev.to ?? "").slice(0, 12)}`;
    default:
      return ev.type;
  }
}

/**
 * pullListChip(row) → {text, cls}: the Pulls.jsx list-row state chip.
 * Merged wins (merge stamps StateClosed too, so row.state alone cannot
 * tell merged from plain-closed — the row needs the PROut.merged flag,
 * Forgejo #530). Text follows the list lowercase convention ("merged"
 * alongside "open"/"closed"); the class reuses the #517 `chip-merged`.
 */
export function pullListChip(row) {
  if (row?.merged) return { text: "merged", cls: "chip chip-merged" };
  return { text: row?.state ?? "open", cls: `chip chip-${row?.state ?? "open"}` };
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
