// web/test/unit/reviews-card-554.test.js — Forgejo #554: the Reviews
// section and its finish-review trigger join the sibling card/padding/
// button idioms. Styling/markup only — every gate, fetch, verdict value,
// dismiss flow, and navigation stays byte-identical.
//
// - ReviewsList: the ONE .card panel composes the sibling card padding
//   (card p-3, the CommentComposer form.card idiom), so review rows and
//   the "No reviews yet." empty state sit inside the border instead of
//   touching its edges. Flat divider list, card-header, card-meta rows,
//   stale marker, and dismiss affordance unchanged.
// - Finish-review trigger: proper "Finish review" casing on the page's
//   button idiom (btn px-3 py-1, the same sizing its cancel sibling in
//   the FinishReview modal wears) with the staged-count suffix kept.
// - No new CSS, no new classes: composition of the canonical .card /
//   .card-header / .btn idioms only (guideline §2 by reference).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const PULL = srcOf("../../src/pages/Pull.jsx");
const CSS = srcOf("../../src/ui.css");

function blockOf(src, fnName) {
  const start = src.indexOf(`function ${fnName}(`);
  assert.ok(start > 0, `${fnName} found`);
  const end = src.indexOf("/**", start + 1);
  return src.slice(start, end > 0 ? end : start + 6000);
}

test("ReviewsList panel composes the sibling card padding", () => {
  const list = blockOf(PULL, "ReviewsList");
  assert.match(list, /<div class="card p-3" aria-label="Reviews">/, "card p-3 — the CommentComposer padding idiom");
  assert.ok(list.includes('<h2 class="card-header">Reviews</h2>'), "card-header title stays (#521)");
  assert.match(list, /<ul class="divide-y divide-zinc-200 dark:divide-zinc-800">/, "flat divider list unchanged");
  assert.ok(!list.includes('<li class="card">'), "still no per-review boxes (no #545 regression)");
});

test("empty state reads inside the padded panel", () => {
  const list = blockOf(PULL, "ReviewsList");
  const panel = list.indexOf('<div class="card p-3" aria-label="Reviews">');
  const empty = list.indexOf("No reviews yet.");
  assert.ok(panel > 0 && empty > panel, "empty fallback renders inside the padded card");
  assert.ok(list.includes('<li class="text-sm text-zinc-500 dark:text-zinc-400">No reviews yet.</li>'), "empty copy byte-identical");
});

test("finish-review trigger wears the button idiom with proper casing", () => {
  assert.ok(
    PULL.includes('<button type="button" class="btn px-3 py-1" onClick={() => setFinishing(true)}>'),
    "trigger is the page button idiom (same sizing as the modal cancel sibling)"
  );
  assert.ok(PULL.includes("Finish review{(getPending().length"), "proper casing with the staged-count suffix kept");
  assert.ok(!PULL.includes("finish review{(getPending().length"), "bare lowercase trigger gone");
});

test("no new styling surface: composition only", () => {
  const list = blockOf(PULL, "ReviewsList");
  assert.ok(!list.includes("style="), "no inline styles on the Reviews panel");
  assert.ok(!CSS.includes("reviews-card") && !CSS.includes("ReviewsList"), "no Reviews-scoped CSS added");
});

test("no behavior change: rows, dismiss, staging, submit intact", () => {
  const list = blockOf(PULL, "ReviewsList");
  assert.ok(list.includes('<div class="card-meta">'), "card-meta rows kept");
  assert.ok(list.includes("reviewVerdictChip(rv.state ?? rv.kind)"), "verdict mapping kept");
  assert.ok(list.includes(">stale</span>"), "stale marker kept");
  assert.ok(list.includes("submitDismiss(rv.seq)"), "dismiss affordance keeps its flow");
  assert.ok(list.includes("props.client.pulls.reviews.dismiss(props.num, seq,"), "dismiss endpoint intact");
  assert.ok(list.includes("props.reload()"), "reload intact");
  assert.ok(list.includes("renderBody(rv.body"), "review markdown intact");
  assert.ok(PULL.includes("setFinishing(true)"), "trigger still opens the finish-review form");
  assert.ok(PULL.includes("onClick={props.onDone}"), "modal cancel intact");
  assert.ok(PULL.includes("props.client.pulls.reviews.submit(props.num,"), "submit flow intact");
});
