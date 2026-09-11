import { test } from "node:test";
import assert from "node:assert/strict";

import {
  ISSUE_STATE_BOTH,
  ISSUE_STATE_DEFAULT,
  resolveIssueState,
  issueListState,
} from "../../src/lib/issueState.js";

test("absent state param resolves to the open-only default", () => {
  assert.equal(resolveIssueState(undefined), "open");
  assert.equal(resolveIssueState(undefined), ISSUE_STATE_DEFAULT);
  assert.equal(resolveIssueState(null), "open");
});

test("explicit both-choice resolves to both, in either spelling", () => {
  assert.equal(resolveIssueState("all"), ISSUE_STATE_BOTH);
  // Legacy empty `?state=` links (pre-#323 both-choice) still read as both.
  assert.equal(resolveIssueState(""), ISSUE_STATE_BOTH);
});

test("explicit open/closed resolve exactly", () => {
  assert.equal(resolveIssueState("open"), "open");
  assert.equal(resolveIssueState("closed"), "closed");
});

test("unknown values pass through for the server to reject", () => {
  assert.equal(resolveIssueState("bogus"), "bogus");
});

test("wire mapping: both omits the param, open/closed ride through", () => {
  // The SDK qs() skips "" so the param is omitted → server returns both.
  assert.equal(issueListState("all"), "");
  assert.equal(issueListState("open"), "open");
  assert.equal(issueListState("closed"), "closed");
  assert.equal(issueListState(undefined), "open");
});

test("end to end: default query is open-only, both-choice is wire-omitted", () => {
  // Bare visit: no param → query sends state=open.
  assert.equal(issueListState(resolveIssueState(undefined)), "open");
  // Both-choice: ?state=all → query sends state="" (omitted on the wire).
  assert.equal(issueListState(resolveIssueState("all")), "");
  assert.equal(issueListState(resolveIssueState("")), "");
  // Deep links honored exactly.
  assert.equal(issueListState(resolveIssueState("closed")), "closed");
  assert.equal(issueListState(resolveIssueState("open")), "open");
});
