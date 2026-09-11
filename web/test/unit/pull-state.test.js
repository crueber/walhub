import { test } from "node:test";
import assert from "node:assert/strict";

import {
  PULL_STATE_BOTH,
  PULL_STATE_DEFAULT,
  resolvePullState,
  pullListState,
} from "../../src/lib/pullState.js";

test("absent state param resolves to the open-only default", () => {
  assert.equal(resolvePullState(undefined), "open");
  assert.equal(resolvePullState(undefined), PULL_STATE_DEFAULT);
  assert.equal(resolvePullState(null), "open");
});

test("explicit both-choice resolves to both, in either spelling", () => {
  assert.equal(resolvePullState("all"), PULL_STATE_BOTH);
  // Legacy empty `?state=` links (pre-#332 both-choice) still read as both.
  assert.equal(resolvePullState(""), PULL_STATE_BOTH);
});

test("explicit open/closed resolve exactly", () => {
  assert.equal(resolvePullState("open"), "open");
  assert.equal(resolvePullState("closed"), "closed");
});

test("unknown values pass through for the server to reject", () => {
  assert.equal(resolvePullState("bogus"), "bogus");
});

test("wire mapping: both omits the param, open/closed ride through", () => {
  // The SDK qs() skips "" so the param is omitted → server returns both.
  assert.equal(pullListState("all"), "");
  assert.equal(pullListState("open"), "open");
  assert.equal(pullListState("closed"), "closed");
  assert.equal(pullListState(undefined), "open");
});

test("end to end: default query is open-only, both-choice is wire-omitted", () => {
  // Bare visit: no param → query sends state=open.
  assert.equal(pullListState(resolvePullState(undefined)), "open");
  // Both-choice: ?state=all → query sends state="" (omitted on the wire).
  assert.equal(pullListState(resolvePullState("all")), "");
  assert.equal(pullListState(resolvePullState("")), "");
  // Deep links honored exactly.
  assert.equal(pullListState(resolvePullState("closed")), "closed");
  assert.equal(pullListState(resolvePullState("open")), "open");
});
