// web/src/lib/issueState.js — Issues list state-param default (issue #323):
// a bare visit (no ?state=) shows open issues only; an explicit both-choice
// is `?state=all` in the URL (shareable/refreshable) and is sent on the wire
// as an OMITTED state param — the issues list endpoint accepts only
// open|closed|absent (absent = both), so `all` never reaches the server.
// A legacy empty `?state=` still reads as both. Unknown values pass through
// untouched (the server answers 400, as before).

/** URL token for the explicit "open + closed" choice. */
export const ISSUE_STATE_BOTH = "all";

/** Default when the URL carries no state param. */
export const ISSUE_STATE_DEFAULT = "open";

/**
 * Resolve the raw `?state=` URL param to the effective select value:
 * absent → "open"; "all"/"" → "all" (both); anything else passes through.
 */
export function resolveIssueState(raw) {
  if (raw === undefined || raw === null) return ISSUE_STATE_DEFAULT;
  if (raw === "" || raw === ISSUE_STATE_BOTH) return ISSUE_STATE_BOTH;
  return raw;
}

/**
 * Map the resolved select value to the wire query value: "all" → ""
 * (the SDK's qs() skips empty values, so the param is omitted and the
 * server returns both states); open/closed ride through verbatim.
 */
export function issueListState(resolved) {
  if (resolved === ISSUE_STATE_BOTH) return "";
  return resolved ?? ISSUE_STATE_DEFAULT;
}
