import { test } from "node:test";
import assert from "node:assert/strict";

import {
  issueEventText,
  closePatch,
  closedStateLabel,
  CLOSE_COMPLETED,
  CLOSE_NOT_PLANNED,
} from "../../src/lib/issue-events.js";

test("comment kinds return null (markdown bodies, not system rows)", () => {
  assert.equal(issueEventText({ type: "opened", body: "hi" }), null);
  assert.equal(issueEventText({ type: "commented", body: "hi" }), null);
});

test("labels_changed reads as a sentence, never +/- sigils", () => {
  assert.equal(
    issueEventText({ type: "labels_changed", added: ["approved"], removed: [] }),
    "added the “approved” label",
  );
  assert.equal(
    issueEventText({ type: "labels_changed", added: [], removed: ["approved"] }),
    "removed the “approved” label",
  );
  assert.equal(
    issueEventText({ type: "labels_changed", added: ["a", "b"], removed: [] }),
    "added the “a”, “b” labels",
  );
  assert.equal(
    issueEventText({ type: "labels_changed", added: ["a"], removed: ["b"] }),
    "added the “a” label and removed the “b” label",
  );
  assert.equal(issueEventText({ type: "labels_changed" }), "changed the labels");
});

test("assignees_changed reads as assigned/unassigned", () => {
  assert.equal(
    issueEventText({ type: "assignees_changed", added: ["alice"], removed: [] }),
    "assigned alice",
  );
  assert.equal(
    issueEventText({ type: "assignees_changed", added: [], removed: ["bob"] }),
    "unassigned bob",
  );
});

test("state_changed is honest about the reason", () => {
  assert.equal(
    issueEventText({ type: "state_changed", to: "closed", reason: "completed" }),
    "closed as completed",
  );
  assert.equal(
    issueEventText({ type: "state_changed", to: "closed", reason: "not_planned" }),
    "closed as not planned",
  );
  // No reason recorded: plain "closed", never an asserted completion.
  assert.equal(issueEventText({ type: "state_changed", to: "closed", reason: null }), "closed");
  assert.equal(issueEventText({ type: "state_changed", to: "closed" }), "closed");
  assert.equal(issueEventText({ type: "state_changed", to: "open" }), "reopened");
});

test("milestone fragments resolve ids to titles via the milestones list", () => {
  const ms = [
    { id: "000001", title: "v1.1" },
    { id: "000002", title: "v2.0" },
  ];
  assert.equal(
    issueEventText({ type: "milestone_changed", from: null, to: "000001" }, ms),
    "added this to the “v1.1” milestone",
  );
  assert.equal(
    issueEventText({ type: "milestone_changed", from: "000001", to: null }, ms),
    "removed this from the “v1.1” milestone",
  );
  assert.equal(
    issueEventText({ type: "milestone_changed", from: "000001", to: "000002" }, ms),
    "moved this from “v1.1” to “v2.0”",
  );
});

test("milestone fragments fall back to the bare id (deleted milestone self-heal)", () => {
  const ms = [{ id: "000001", title: "v1.1" }];
  // Unknown id renders bare — same stance as the sidebar (milestoneTitle).
  assert.equal(
    issueEventText({ type: "milestone_changed", from: null, to: "0000ff" }, ms),
    "added this to the “0000ff” milestone",
  );
  // No list at all (e.g. cache not yet loaded) — bare id, never blank.
  assert.equal(
    issueEventText({ type: "milestone_changed", from: null, to: "000001" }),
    "added this to the “000001” milestone",
  );
  assert.equal(
    issueEventText({ type: "milestone_changed", from: "000001", to: null }, []),
    "removed this from the “000001” milestone",
  );
});
test("reference rows are single-line fragments without actors", () => {
  assert.equal(
    issueEventText({ type: "referenced", source: { num: 3 } }),
    "referenced this from #3",
  );
  assert.equal(
    issueEventText({ type: "cross_referenced", source: { repo: "o/r", num: 4 } }),
    "referenced this from o/r#4",
  );
  assert.equal(
    issueEventText({ type: "closed_by_pr", pr_num: 7, keyword: "fixes" }),
    "closed this via #7 (fixes)",
  );
  assert.equal(issueEventText({ type: "title_changed", from: "a", to: "b" }), "retitled “a” → “b”");
});

test("closePatch sends an explicit reason; anything else throws", () => {
  assert.deepEqual(closePatch(CLOSE_COMPLETED), { state: "closed", state_reason: "completed" });
  assert.deepEqual(closePatch(CLOSE_NOT_PLANNED), { state: "closed", state_reason: "not_planned" });
  assert.throws(() => closePatch(undefined), /close reason must be/);
  assert.throws(() => closePatch(null), /close reason must be/);
  assert.throws(() => closePatch(""), /close reason must be/);
  assert.throws(() => closePatch("completed "), /close reason must be/);
});

test("closedStateLabel reflects the recorded reason honestly", () => {
  assert.equal(closedStateLabel("completed"), "Closed as completed");
  assert.equal(closedStateLabel("not_planned"), "Closed as not planned");
  assert.equal(closedStateLabel(null), "Closed");
  assert.equal(closedStateLabel(undefined), "Closed");
});
