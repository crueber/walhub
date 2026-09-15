// web/src/lib/pull-state.js — Forgejo #517 pure helpers (dependency-free,
// headless-testable per 12 §5) plus the Forgejo #521 timeline text map: the
// PR header state badge, the Close/Reopen visibility rule, and the
// conversation textFor.
//
// The server contract (internal/pulls/service.go UpdatePR): state flips are
// `PUT …/pulls/{num}` `{state: "open"|"closed"}` (no reason — PR state is
// open|closed only), auth is **author or triage** (hierarchical P6 ladder,
// so write ⊇ triage), and closing OR reopening a merged PR is refused
// (409 — Forgejo #594 closed the reopen gap). Client
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
 * reviewVerdictLabel(state) → string: the ONE wire→display mapping for
 * review verdicts on the PR page (Forgejo #561). The API wire contract
 * (internal/review/model.go) is exact — APPROVED|CHANGES_REQUESTED|
 * COMMENTED states, REVIEW_REQUIRED decision, DISMISSED rollup-only — and
 * stays byte-identical on the wire; only rendered text is mapped here.
 * Known values map to human labels; unknown values pass through as-is
 * (debuggable, never blank); missing (null/undefined/"") reads Unknown.
 */
export function reviewVerdictLabel(state) {
  switch (state) {
    case "APPROVED":
      return "Approved";
    case "CHANGES_REQUESTED":
      return "Changes requested";
    case "COMMENTED":
      return "Commented";
    case "REVIEW_REQUIRED":
      return "Review required";
    case "DISMISSED":
      return "Dismissed";
    default:
      break;
  }
  if (state == null || state === "") return "Unknown";
  return String(state);
}

/**
 * Mergeability tone classes (Forgejo #588): Tailwind utilities only, no
 * ad-hoc CSS. Green is the emerald family, soft red is the muted red
 * already used for warnings, pending/terminal reads the muted zinc —
 * every class carries its dark: variant (guideline F2).
 */
export const MERGEABILITY_OK_CLS = "text-emerald-600 dark:text-emerald-400";
export const MERGEABILITY_BAD_CLS = "text-red-600 dark:text-red-400";
export const MERGEABILITY_MUTED_CLS = "text-zinc-500 dark:text-zinc-400";

/** Secondary detail line for a behind-but-mergeable branch (08 §2: behind
 *  stays mergeable; the update-branch button covers remediation). */
export const MERGEABILITY_BEHIND_SUB = "branch is behind — update";

/**
 * mergeabilityDisplay(state, detail) → {text, cls, sub}: the ONE
 * machine→display mapping for mergeability on the PR page (Forgejo #588,
 * the #561 pattern). `state` accepts EITHER a MergeBox machine value
 * (draft|blocked|mergeable|ready|merging|failed|merged — mergeState() in
 * MergeBox.jsx) OR a mergeable.state wire value
 * (clean|behind|dirty|up_to_date); `detail` disambiguates the blocked
 * headline and carries the behind wire object:
 * {mergeable, checksBlockers[], reviewDecision, draft, zeroChecks}.
 *
 * Priority mirrors mergeState() order (merged → failed → merging →
 * draft → checks → reviews → conflicts); the amber "blocking merge: …"
 * reasons line keeps the full detail, this headline stays one phrase.
 * Wire/internal values stay byte-identical — only rendered text/classes
 * are mapped here. Missing (null/undefined/"") reads Unknown; any other
 * unrecognized non-empty value passes through as-is (debuggable, never
 * blank — the #561 precedent).
 *
 * Decisions: zeroChecks rewrites ONLY the pending/ready headline ("No
 * checks required", NEUTRAL muted — an empty required set blocks nothing
 * per checks-empty.js, so red would falsely signal blocked; a
 * clean/behind wire still reads Able to be Merged). Behind stays
 * mergeable per 08 §2: green headline + the behind sub-line.
 */
export function mergeabilityDisplay(state, detail = {}) {
  const d = detail ?? {};
  const blockers = Array.isArray(d.checksBlockers) ? d.checksBlockers : [];
  const wire = d.mergeable?.state;
  const behind = state === "behind" || wire === "behind";
  if (state === "merged" || state === "up_to_date") {
    return { text: "Already merged", cls: MERGEABILITY_MUTED_CLS, sub: null };
  }
  if (state === "failed") {
    return { text: "Merge failed", cls: MERGEABILITY_BAD_CLS, sub: null };
  }
  if (state === "merging") {
    return { text: "Merging…", cls: MERGEABILITY_MUTED_CLS, sub: null };
  }
  if (state === "draft" || d.draft) {
    return { text: "Draft pull request", cls: MERGEABILITY_BAD_CLS, sub: null };
  }
  if (blockers.length > 0) {
    return { text: "Checks failing", cls: MERGEABILITY_BAD_CLS, sub: null };
  }
  if (d.reviewDecision === "CHANGES_REQUESTED") {
    return { text: "Changes requested", cls: MERGEABILITY_BAD_CLS, sub: null };
  }
  if (state === "dirty" || wire === "dirty") {
    return { text: "Merge conflicts", cls: MERGEABILITY_BAD_CLS, sub: null };
  }
  if (state === "clean" || state === "mergeable" || behind) {
    return {
      text: "Able to be Merged",
      cls: MERGEABILITY_OK_CLS,
      sub: behind ? MERGEABILITY_BEHIND_SUB : null,
    };
  }
  if (state === "ready" || state === "checking" || state === "unknown") {
    if (d.zeroChecks) {
      return { text: "No checks required", cls: MERGEABILITY_MUTED_CLS, sub: null };
    }
    return { text: "Checking mergeability", cls: MERGEABILITY_MUTED_CLS, sub: null };
  }
  if (state == null || state === "") return { text: "Unknown", cls: MERGEABILITY_MUTED_CLS, sub: null };
  return { text: String(state), cls: MERGEABILITY_MUTED_CLS, sub: null };
}

/**
 * pullCommentLock(thread, pr) → {locked, reason}: the Forgejo #594 client
 * mirror of the server threadLocked gate (internal/pulls + internal/review
 * service.go). Merged wins (merge stamps StateClosed too, so thread.state
 * alone cannot tell merged from plain-closed); the lock keys on CURRENT
 * state — a reopen flips a plain-closed PR back, so the composer reacts
 * to pull stream frames with no reload. Loading (no thread yet) reads
 * unlocked, like the badge default.
 */
export function pullCommentLock(thread, pr) {
  if (pr?.merged) return { locked: true, reason: "Merged — commenting is locked" };
  if ((thread?.state ?? "open") !== "open") return { locked: true, reason: "This conversation is closed" };
  return { locked: false, reason: null };
}

/**
 * reviewRequestsEditable(canEdit, thread, pr) → bool: the Forgejo #599
 * client mirror of the server review-request gate (internal/review
 * threads.go Add/RemoveRequests: 422 unless open + unmerged). Role first
 * (canEdit is the page's role-only canReview()), then the #594 live-state
 * lock — closed/merged hides the picker affordance while the
 * requested-reviewer chips stay visible read-only. Keys on the live
 * thread/pr fetch like pullCommentLock, so a reopen restores the picker
 * with no reload.
 */
export function reviewRequestsEditable(canEdit, thread, pr) {
  if (!canEdit) return false;
  return !pullCommentLock(thread, pr).locked;
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
