// web/src/lib/issue-events.js — honest single-line text for issue system events.
//
// `issueEventText(ev)` returns null for comment kinds (opened/commented —
// rendered as markdown bodies) and a plain sentence fragment for every
// other kind. The fragment never names an actor: ThreadTimeline renders
// system rows as "{actor} {fragment}", so the row reads e.g. "anon added
// the approved label". Fragments never assert what the event does not
// carry — in particular a close with no reason renders as plain "closed",
// never "closed as completed" (the API defaults an omitted reason to
// completed, so the UI always sends an explicit reason instead; see
// closePatch).
//
// `closePatch(reason)` builds the PATCH body for an explicit-reason close.
// The API contract (02 §7, internal/issues service.go) defaults an omitted
// state_reason to completed, so callers must pass "completed" or
// "not_planned" — anything else throws rather than silently claiming
// completion.

export const CLOSE_COMPLETED = "completed";
export const CLOSE_NOT_PLANNED = "not_planned";

function quoteList(names) {
  return names.map((n) => `“${n}”`).join(", ");
}

function labelFragment(ev) {
  const added = ev.added ?? [];
  const removed = ev.removed ?? [];
  const parts = [];
  if (added.length) parts.push(`added the ${quoteList(added)} label${added.length === 1 ? "" : "s"}`);
  if (removed.length) parts.push(`removed the ${quoteList(removed)} label${removed.length === 1 ? "" : "s"}`);
  return parts.length ? parts.join(" and ") : "changed the labels";
}

function assigneeFragment(ev) {
  const added = ev.added ?? [];
  const removed = ev.removed ?? [];
  const parts = [];
  if (added.length) parts.push(`assigned ${added.join(", ")}`);
  if (removed.length) parts.push(`unassigned ${removed.join(", ")}`);
  return parts.length ? parts.join(" and ") : "changed the assignees";
}

function milestoneFragment(ev) {
  if (ev.from == null && ev.to != null) return `added this to the “${ev.to}” milestone`;
  if (ev.to == null && ev.from != null) return `removed this from the “${ev.from}” milestone`;
  if (ev.from != null && ev.to != null) return `moved this from “${ev.from}” to “${ev.to}”`;
  return "changed the milestone";
}

export function issueEventText(ev) {
  switch (ev?.type) {
    case "opened":
    case "commented":
      return null; // rendered as markdown body
    case "title_changed":
      return `retitled “${ev.from}” → “${ev.to}”`;
    case "labels_changed":
      return labelFragment(ev);
    case "assignees_changed":
      return assigneeFragment(ev);
    case "state_changed":
      if (ev.to === "closed") {
        if (ev.reason === CLOSE_NOT_PLANNED) return "closed as not planned";
        if (ev.reason === CLOSE_COMPLETED) return "closed as completed";
        return "closed"; // no reason recorded — do not assert completion
      }
      return "reopened";
    case "milestone_changed":
      return milestoneFragment(ev);
    case "referenced":
      return `referenced this from #${ev.source?.num ?? "?"}`;
    case "cross_referenced":
      return `referenced this from ${ev.source?.repo ?? "?"}#${ev.source?.num ?? "?"}`;
    case "closed_by_pr":
      return `closed this via #${ev.pr_num} (${ev.keyword})`;
    default:
      return ev?.type ?? "event";
  }
}

/** Explicit-reason close body. Throws on anything but completed|not_planned
 *  so a missing/typo reason can never silently close "as completed". */
export function closePatch(reason) {
  if (reason !== CLOSE_COMPLETED && reason !== CLOSE_NOT_PLANNED) {
    throw new Error(`close reason must be "${CLOSE_COMPLETED}" or "${CLOSE_NOT_PLANNED}", got ${JSON.stringify(reason)}`);
  }
  return { state: "closed", state_reason: reason };
}

/** Header state line for a closed thread: honest about the recorded reason. */
export function closedStateLabel(stateReason) {
  if (stateReason === CLOSE_NOT_PLANNED) return "Closed as not planned";
  if (stateReason === CLOSE_COMPLETED) return "Closed as completed";
  return "Closed";
}
