// web/src/lib/pullState.js — Pulls list state-param default (issue #332):
// a bare visit (no ?state=) shows open pull requests only; an explicit
// both-choice is `?state=all` in the URL (shareable/refreshable) and is
// sent on the wire as an OMITTED state param — the pulls list endpoint
// accepts only open|closed|absent (absent = both), so `all` never reaches
// the server. A legacy empty `?state=` still reads as both. Unknown values
// pass through untouched (the server answers 400, as before).
// Mirrors web/src/lib/issueState.js (issue #323) for the pulls tab bar.

/** URL token for the explicit "open + closed" choice. */
export const PULL_STATE_BOTH = "all";

/** Default when the URL carries no state param. */
export const PULL_STATE_DEFAULT = "open";

/**
 * Resolve the raw `?state=` URL param to the effective tab value:
 * absent → "open"; "all"/"" → "all" (both); anything else passes through.
 */
export function resolvePullState(raw) {
  if (raw === undefined || raw === null) return PULL_STATE_DEFAULT;
  if (raw === "" || raw === PULL_STATE_BOTH) return PULL_STATE_BOTH;
  return raw;
}

/**
 * Map the resolved tab value to the wire query value: "all" → ""
 * (the SDK's qs() skips empty values, so the param is omitted and the
 * server returns both states); open/closed ride through verbatim.
 */
export function pullListState(resolved) {
  if (resolved === PULL_STATE_BOTH) return "";
  return resolved ?? PULL_STATE_DEFAULT;
}
