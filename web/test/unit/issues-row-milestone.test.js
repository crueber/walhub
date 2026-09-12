// web/test/unit/issues-row-milestone.test.js — Forgejo #380: the issues
// list rows show the milestone on each row and the comment count carries
// a message-bubble indicator instead of the word "comments".
//
// Row meta order is comment count → milestone → updated time; the
// milestone segment renders only when the card has one
// (Card.milestone is nullable — no backend change, the id already rides
// the list projection). Titles resolve through milestoneDisplay over the
// page-owned `milestones:{full}` set: pending renders a placeholder
// (never a raw-hex flash), deleted/unknown ids fall back to the bare id
// (same self-heal as the thread sidebar). The chip reuses the
// label-chip row language with max-w truncation (#334 safety).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const rows = () => srcOf("../../src/pages/Issues.jsx");

test("row resolves milestone titles through the shared cached set (no backend change)", () => {
  const s = rows();
  assert.ok(
    s.includes('import { milestoneDisplay, milestoneFilterHref } from "../lib/milestones.js"'),
    "reuses the milestone helpers, no new resolution path",
  );
  assert.ok(
    s.includes("`milestones:${ctx.full}`") && s.includes("ctx.repoClient.milestones.list()") && s.includes("TTL.milestones"),
    "page owns the milestones:{full} cache entry (same key + TTL as the thread sidebar)",
  );
});

test("meta order is comment count → milestone → updated time", () => {
  const s = rows();
  const meta = s.slice(s.indexOf("ml-auto shrink-0"));
  let at = -1;
  for (const t of ["issue.comment_count", "issue.milestone", "issue.updated_at"]) {
    const i = meta.indexOf(t, at + 1);
    assert.ok(i > at, `${t} follows in separator order count · milestone · time`);
    at = i;
  }
});

test("milestone segment renders only when the card has one", () => {
  const s = rows();
  assert.ok(s.includes("<Show when={issue.milestone != null}>"), "no milestone element for the common unset case");
});

test("pending set never flashes a raw hex id; unknown ids self-heal to the bare id", () => {
  const s = rows();
  assert.ok(s.includes("milestoneDisplay(getMilestoneSet()?.milestones, issue.milestone)"), "title resolves via milestoneDisplay");
  assert.ok(s.includes('<Show when={!d().pending} fallback={<span class="muted">…</span>}>'), "cold load renders a placeholder, never the bare id");
  assert.ok(!s.includes("{issue.milestone} comments") && !s.includes(">{issue.milestone}<") , "the raw id never renders directly");
});

test("milestone links to the filtered list, matching the sidebar behavior", () => {
  const s = rows();
  assert.ok(s.includes("href={milestoneFilterHref(ctx.full, issue.milestone)}"), "click lands on /issues?milestone=<id>");
  assert.ok(s.includes("title={`issues on milestone ${d().text}`}"), "link carries the resolved title");
});

test("the visible word 'comments' is gone; a decorative bubble precedes the count", () => {
  const s = rows();
  assert.ok(!s.includes("{issue.comment_count} comments ·"), "old literal count text is gone from the row meta");
  assert.ok(!s.includes("comments · <DateTime"), "no literal 'comments' beside the timestamp");
  assert.ok(s.includes('<span aria-hidden="true">💬 </span>'), "bubble indicator is decorative (aria-hidden)");
  assert.ok(
    s.includes("aria-label={`${issue.comment_count} comments`}") && s.includes("title={`${issue.comment_count} comments`}"),
    "the count keeps an accessible label + hover title",
  );
});

test("long milestone titles truncate sanely within the flex row", () => {
  const s = rows();
  assert.ok(s.includes("chip max-w-40 truncate"), "milestone chip reuses the label-chip language with a truncation cap");
  assert.ok(s.includes("min-w-0"), "truncation context preserved on the row");
});
