// web/test/unit/review-requests-599.test.js — Forgejo #599: the
// review-request picker gate. reviewRequestsEditable(canEdit, thread, pr)
// is role-first, then the #594 live-state lock: closed/merged hides the
// picker affordance (and the chip ×) while the requested-reviewer chips
// stay visible read-only; a reopen restores the picker with no reload
// (same live thread/pr fetch as pullCommentLock). The chips-visibility
// half is structural in Pull.jsx ReviewersPanel (the chips <ul> sits
// outside the picker's <Show>); this suite pins the helper contract that
// <Show> reads.

import { test } from "node:test";
import assert from "node:assert/strict";

import { reviewRequestsEditable } from "../../src/lib/pull-state.js";

const open = { state: "open" };
const closed = { state: "closed" };
const plain = { merged: false };
const merged = { merged: true };

test("open + role → editable (the only true cell)", () => {
  assert.equal(reviewRequestsEditable(true, open, plain), true);
  // Loading (no thread/pr yet) reads open, like the badge default.
  assert.equal(reviewRequestsEditable(true, undefined, undefined), true);
});

test("no role → never editable, whatever the state", () => {
  for (const [thread, pr] of [[open, plain], [closed, plain], [open, merged], [closed, merged]]) {
    assert.equal(reviewRequestsEditable(false, thread, pr), false);
  }
  assert.equal(reviewRequestsEditable(null, open, plain), false);
});

test("terminal PR → picker hidden even with the role (chips stay read-only)", () => {
  assert.equal(reviewRequestsEditable(true, closed, plain), false);
  // Merged wins over either thread state, with the merged lock.
  assert.equal(reviewRequestsEditable(true, open, merged), false);
  assert.equal(reviewRequestsEditable(true, closed, merged), false);
  // Stamp-in-flight (merged true, header still open) stays hidden.
  assert.equal(reviewRequestsEditable(true, { state: "open" }, { merged: true }), false);
});

test("reopen restores (state open → editable again, no reload)", () => {
  // The no-reload unlock: ReviewersPanel re-reads this helper off the
  // stream-refreshed thread/pr props, so a reopened PR computes editable.
  assert.equal(reviewRequestsEditable(true, closed, plain), false);
  assert.equal(reviewRequestsEditable(true, open, plain), true);
});
