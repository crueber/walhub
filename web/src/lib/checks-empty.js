// web/src/lib/checks-empty.js — the zero-contexts empty-state rule (Forgejo #518).
//
// Pure module (no Solid, no DOM): the wire contract maps zero reported
// contexts to a combined state of pending (the merge gate's load-bearing
// reading of "not started"), so the UI cannot use the combined state to
// tell "no CI has reported" from "CI is in flight". These helpers read the
// shape the client already fetches — the combined view's `statuses` array
// (GET …/checks/{sha} → {sha, state, total_counts, statuses}) — and derive
// the display-only empty state from it. The wire behavior stays untouched.

/** True when the view carries zero reported contexts. Null/undefined (still
 *  loading) and views without a statuses array (unknown shape) are NOT
 *  empty — the caller must keep showing the loading/real pill rather than
 *  claim nothing reported. A single pending context is NOT empty either:
 *  that is CI genuinely in flight and keeps the amber pill. */
export function isZeroChecks(view) {
  if (view == null) return false;
  if (!Array.isArray(view.statuses)) return false;
  return view.statuses.length === 0;
}

/** Empty-state title. Required checks configured but unreported is still
 *  "reported" wording (CI is expected — the blockers line names the
 *  missing contexts); only the no-require_checks case claims "configured",
 *  and an unknown required set (CheckDetail page — no policy fetch there)
 *  stays with the neutral "reported" wording rather than assert a config. */
export function zeroChecksTitle(required) {
  if (Array.isArray(required) && required.length === 0) return "No checks configured";
  return "No checks reported yet";
}

/** Merge-block advisory for required checks (Pull.jsx resolution moved
 *  here so the zero-contexts case is headless-pinned): every required
 *  context whose latest state is not success blocks, reported as
 *  "<context> (<state>)" or "<context> (missing)" when nothing reported
 *  it yet. Empty required set blocks nothing. */
export function requiredCheckBlockers(required, statuses) {
  if (!Array.isArray(required) || required.length === 0) return [];
  const byCtx = new Map((statuses ?? []).map((s) => [s.context, s.state]));
  return required
    .filter((c) => byCtx.get(c) !== "success")
    .map((c) => (byCtx.has(c) ? `${c} (${byCtx.get(c)})` : `${c} (missing)`));
}
