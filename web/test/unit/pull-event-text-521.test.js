// web/test/unit/pull-event-text-521.test.js — Forgejo #521: the PR
// conversation page matches the issue page's design language. Pure-logic
// cover (headless-testable, no Solid/DOM) plus source pins for the JSX/CSS
// idiom changes — renderBody itself needs a DOM (browser pass), so these
// tests pin the call sites, not the pixels.
//
// What #521 changes, and why each pin exists:
// - eventText moves from Pull.jsx into lib/pull-state.js as pullEventText
//   (headless-testable) with ONE behavior change: "opened" becomes a
//   one-line system row ("{actor} opened") because the description now
//   lives in a dedicated first-comment block rendering the LIVE pr.body
//   editable view (internal/pulls/model.go: the opened event carries the
//   ORIGINAL body; body edits append no event, so the timeline row would
//   go stale and duplicate the block). The row keeps its event-0 anchor.
// - Review bodies + thread comments route through renderBody with the same
//   repo mdCtx the timeline gets; the finish-review modal draft preview
//   stays plain text by design.
// - Page uses the issue-page grid idiom; ReviewSummaryBar composes into
//   the sidebar (Forgejo #531 reworks the sidebar into the Issue.jsx:549
//   one-container divide-y panel — see pr-structure-531.test.js — so the
//   sidebar pins live there, not here).
// - chip-merged already ships in ui.css (#517); the pulls-list wire
//   carries no merged signal (PROut has state open|closed only), so the
//   list path needs no change — no wire change, per the issue.
//   (Superseded by Forgejo #530, which adds PROut.merged and routes the
//   list chip through pullListChip — see the #530 pin at the bottom.)
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import { pullEventText } from "../../src/lib/pull-state.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const PULL = srcOf("../../src/pages/Pull.jsx");
const TIMELINE = srcOf("../../src/components/ThreadTimeline.jsx");
const CSS = srcOf("../../src/ui.css");
const MERGEBOX = srcOf("../../src/components/MergeBox.jsx");
const PULLS = srcOf("../../src/pages/Pulls.jsx");

function count(src, needle) {
  return src.split(needle).length - 1;
}

test("opened is a system row, commented stays a body", () => {
  assert.equal(pullEventText({ type: "opened" }), "opened");
  assert.equal(pullEventText({ type: "commented" }), null);
});

test("all other branches preserved verbatim from the Pull.jsx original", () => {
  assert.equal(pullEventText({ type: "title_changed", from: "a", to: "b" }), "retitled “a” → “b”");
  assert.equal(pullEventText({ type: "state_changed", to: "closed" }), "closed");
  assert.equal(pullEventText({ type: "state_changed", to: "open" }), "reopened");
  assert.equal(
    pullEventText({ type: "merged", merge_commit_sha: "abcdef1234567890", strategy: "squash" }),
    "merged as abcdef123456 (squash)"
  );
  assert.equal(pullEventText({ type: "merged" }), "merged as  (merge)");
  assert.equal(
    pullEventText({ type: "head_force_pushed", from: "1111111111111111", to: "2222222222222222" }),
    "head force-pushed 111111111111 → 222222222222"
  );
  assert.equal(pullEventText({ type: "referenced" }), "referenced");
});

test("Pull.jsx delegates textFor to the lib helper (no local fork)", () => {
  assert.match(PULL, /import \{[^}]*pullEventText[^}]*\} from "\.\.\/lib\/pull-state\.js"/);
  assert.match(PULL, /textFor=\{pullEventText\}/);
  assert.doesNotMatch(PULL, /function eventText\(ev\)/);
});

test("ThreadTimeline contract untouched (the ONE renderer, no fork)", () => {
  assert.match(TIMELINE, /innerHTML=\{renderBody\(ev\.body \?\? "", props\.mdCtx\)\}/);
  assert.doesNotMatch(TIMELINE, /pull-state|pullEventText|Pull\.jsx/);
});

test("review bodies + thread comments render through renderBody with mdCtx", () => {
  assert.match(PULL, /innerHTML=\{renderBody\(rv\.body \?\? "", props\.mdCtx\)\}/);
  assert.match(PULL, /innerHTML=\{renderBody\(c\.body \?\? "", props\.mdCtx\)\}/);
  assert.doesNotMatch(PULL, /whitespace-pre-wrap">\{rv\.body\}|whitespace-pre-wrap text-sm">\{c\.body\}/);
  // Plain-text sites by design (#567-scoped: two now) — the finish-review
  // modal staged-line preview and the in-thread StagedCard body both
  // render p.body plain (staged text is a draft, never markdown); review
  // bodies + thread comments stay renderBody.
  assert.equal(count(PULL, "whitespace-pre-wrap"), 2);
  assert.match(PULL, /<span class="flex-1 whitespace-pre-wrap">\{p\.body\}<\/span>/);
  assert.match(PULL, /<p class="whitespace-pre-wrap text-sm">\{props\.entry\?\.body\}<\/p>/);
  // One shared repo mdCtx (the #340 contract) feeds every call site.
  assert.match(PULL, /const mdCtx = \{ owner: ctx\.owner, repo: ctx\.name \};/);
  assert.ok(count(PULL, "mdCtx={mdCtx}") >= 4, "timeline + description + reviews + diff");
});

test("PR description is a styled first-comment block on the live pr.body", () => {
  assert.match(PULL, /aria-label="Pull request description"/);
  assert.match(PULL, /innerHTML=\{renderBody\(props\.body \?\? "", props\.mdCtx\)\}/);
  // Hidden when the PR has no description (never an empty box).
  assert.match(PULL, /<Show when=\{String\(props\.body \?\? ""\)\.trim\(\)\}>/);
  // Byline attribution comes from the opened event's history record.
  assert.match(PULL, /actor=\{openedEvent\(\)\?\.actor \?\? thread\(\)\?\.author\}/);
  assert.match(PULL, /at=\{openedEvent\(\)\?\.at \?\? thread\(\)\?\.updated_at\}/);
  assert.match(PULL, /body=\{pr\(\)\?\.body\}/);
});

test("header keeps the single #517 badge (unified, never stacked) + muted number", () => {
  assert.equal(count(PULL, "badge().cls"), 1, "exactly one badge surface");
  assert.match(PULL, /<span class="text-zinc-500 dark:text-zinc-400">#\{num\(\)\}<\/span> \{thread\(\)\?\.title\}/);
  assert.match(PULL, /<header class="mb-4 border-b border-zinc-200 pb-3 dark:border-zinc-800">/);
});

test("page uses the issue-page grid idiom; summary bar composes into the sidebar", () => {
  assert.match(PULL, /<div class="issue-page grid gap-4 md:grid-cols-\[1fr_16rem\]">/);
  assert.match(PULL, /<aside aria-label="Details" class="grid content-start gap-3">/);
  const asideAt = PULL.indexOf('<aside aria-label="Details"');
  assert.ok(asideAt > 0);
  const summaryAt = PULL.indexOf("<ReviewSummaryBar", asideAt);
  const mergeabilityAt = PULL.indexOf("Mergeability", asideAt);
  assert.ok(summaryAt > 0 && summaryAt < mergeabilityAt, "summary bar first in the sidebar");
  assert.doesNotMatch(PULL, /<div class="mb-4">\s*<ReviewSummaryBar/);
});

test("conversation cards keep the one card-header treatment (sidebar moved on)", () => {
  // Forgejo #531 reworks the sidebar into the Issue.jsx:549 one-panel
  // idiom (micro-labels, no card-headers — pinned in
  // pr-structure-531.test.js). The conversation column keeps .card-header.
  assert.match(CSS, /\.card-header \{ @apply mb-2 text-sm font-semibold; \}/);
  for (const title of ["Reviews", "Finish review", "Files"]) {
    assert.ok(PULL.includes(`<h2 class="card-header`), `${title} card rides .card-header`);
  }
  for (const gone of ["Review summary", ">Reviewers<", "Mergeability", ">Checks<", "Merge ({"]) {
    assert.ok(!PULL.includes(`<h2 class="card-header">${gone}`), `sidebar ${gone} heading is gone`);
  }
  assert.doesNotMatch(PULL, /<h2 class="mb-2 text-sm font-semibold">/);
  assert.doesNotMatch(PULL, /<h2 class="mb-2 flex items-center gap-2 text-sm font-semibold">/);
});

test("chip-merged ships in ui.css (light + dark); conversation badge uses it", () => {
  assert.match(CSS, /\.chip-merged \{ @apply[^;]*bg-purple-100 text-purple-800 dark:bg-purple-900\/60 dark:text-purple-300/);
  assert.match(PULL, /\$\{badge\(\)\.cls\}/);
});

test("pulls list renders the merged chip from PROut.merged (Forgejo #530)", () => {
  // #521 pinned the no-signal list path; #530 adds the wire field, so the
  // pin moves with it: the list chip goes through the headless helper with
  // merged-wins semantics, never the raw `chip-${state}` path.
  assert.match(PULLS, /import \{[^}]*pullListChip[^}]*\} from "\.\.\/lib\/pull-state\.js"/);
  assert.match(PULLS, /<span class=\{pullListChip\(pr\)\.cls\}>/);
  assert.match(PULLS, /\{pullListChip\(pr\)\.text\}/);
  assert.doesNotMatch(PULLS, /chip chip-\$\{pr\.state\}/);
});
