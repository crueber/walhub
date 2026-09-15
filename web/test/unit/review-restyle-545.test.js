// web/test/unit/review-restyle-545.test.js — Forgejo #545: the PR review
// surface (Reviews section + Finish review form) joins the system styling
// idioms. Styling/markup only — every gate, fetch, verdict value, and
// navigation stays byte-identical.
//
// - ReviewsList: ONE .card panel with the card-header title (the #521/#531
//   conversation-column idiom) holding an unstyled flat list — no
//   card-list (zero CSS rules), no card-in-card (item .card chrome gone).
// - Verdicts: the ONE reviewVerdictChip mapping onto the .chip family
//   (APPROVED → chip-open, CHANGES_REQUESTED → chip-closed, COMMENTED →
//   chip-neutral, REVIEW_REQUIRED/unknown → chip-draft, dismissed →
//   chip-neutral) at every verdict call site; no per-callsite bg colors.
// - Finish review: the #479 canonical shape (label.grid.gap-1 +
//   text-sm font-medium span + .input control, help scoped via id +
//   aria-describedby), submit on the .btn.primary idiom, cancel plain.
// - Rendered-output pins (structure + control classes), not just source
//   reading — the undefined classes this issue removes were invisible in
//   source reading, which is exactly how they shipped.
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
const CHECKS = srcOf("../../src/pages/Checks.jsx");

function blockOf(src, fnName) {
  const start = src.indexOf(`function ${fnName}(`);
  assert.ok(start > 0, `${fnName} found`);
  const end = src.indexOf("/**", start + 1);
  return src.slice(start, end > 0 ? end : start + 4000);
}

test("no undefined component classes remain in the review surface", () => {
  assert.ok(!PULL.includes("card-list"), "card-list wrapper gone from Pull.jsx");
  assert.ok(!PULL.includes("btn-primary"), "btn-primary gone — the idiom is btn primary");
  assert.ok(!PULL.includes('class="field"'), "label.field gone — the idiom is label.grid.gap-1");
  assert.ok(!PULL.includes("decisionBadge"), "pill-based decisionBadge gone — reviewVerdictChip owns verdicts");
});

test("ReviewsList is one panel with a flat unstyled list, never card-in-card", () => {
  const list = blockOf(PULL, "ReviewsList");
  assert.match(list, /<div class="card p-3" aria-label="Reviews">/, "the ONE outer panel stays (padded, #554)");
  assert.ok(list.includes('<h2 class="card-header">Reviews</h2>'), "card-header title stays (#521)");
  assert.match(list, /<ul class="divide-y divide-zinc-200 dark:divide-zinc-800">/, "flat divider list, no wrapper class");
  assert.ok(!list.includes('<li class="card">'), "no per-review boxes — the double chrome is gone");
  assert.ok(list.includes("first:pt-0 last:pb-0"), "first row flush like the flat PR-list rows");
});

test("each review row reads author, chip verdict, timestamp in the card-meta language", () => {
  const list = blockOf(PULL, "ReviewsList");
  assert.ok(list.includes('<div class="card-meta">'), "rows speak card-meta");
  assert.ok(list.includes("{rv.by}"), "author kept");
  assert.ok(list.includes("<DateTime value={rv.at}"), "timestamp keeps the ONE DateTime");
  assert.ok(list.includes("dismissed #${rv.dismisses}"), "dismissed text kept");
  assert.ok(list.includes("(rv.commit_sha ?? \"\").slice(0, 12)"), "sha line kept");
  assert.ok(list.includes(">stale</span>"), "stale marker kept");
  assert.ok(list.includes("submitDismiss(rv.seq)"), "dismiss affordance keeps its flow");
  assert.ok(list.includes("No reviews yet."), "empty fallback kept");
});

test("one shared verdict mapping serves every verdict call site", () => {
  const defs = PULL.match(/function reviewVerdictChip\(/g) ?? [];
  assert.equal(defs.length, 1, "exactly one mapping definition");
  const map = blockOf(PULL, "reviewVerdictChip");
  assert.ok(map.includes('case "APPROVED":') && map.includes('"chip chip-open"'), "APPROVED → emerald chip");
  assert.ok(map.includes('case "CHANGES_REQUESTED":') && map.includes('"chip chip-closed"'), "CHANGES_REQUESTED → red chip");
  assert.ok(map.includes('case "COMMENTED":') && map.includes('"chip chip-neutral"'), "COMMENTED → neutral chip");
  assert.ok(map.includes('"chip chip-draft"'), "REVIEW_REQUIRED/unknown → amber chip");
  assert.ok(PULL.includes('reviewVerdictChip(summary()?.decision ?? "REVIEW_REQUIRED")'), "summary-bar decision rides the mapping");
  assert.ok(PULL.includes("reviewVerdictChip(r.state)"), "reviewer chips ride the mapping");
  assert.ok(PULL.includes("reviewVerdictChip(rv.state ?? rv.kind)"), "review cards ride the mapping");
  assert.ok(PULL.includes('"chip chip-neutral"'), "dismissed/requested read neutral");
  assert.ok(!PULL.includes("bg-emerald-100"), "no hardcoded emerald verdict bg");
  assert.ok(!PULL.includes("bg-red-100"), "no hardcoded red verdict bg");
  assert.ok(!PULL.includes("bg-amber-100"), "no hardcoded amber verdict bg");
});

test("Finish review follows the #479 form structure", () => {
  const form = blockOf(PULL, "FinishReview");
  assert.equal((form.match(/<label class="grid gap-1[^"]*">/g) ?? []).length, 2, "both fields are label.grid.gap-1");
  assert.equal((form.match(/<span class="text-sm font-medium">/g) ?? []).length, 2, "both labels are font-medium spans");
  assert.ok(form.includes('<textarea id="finish-review-body" class="input w-full"'), "Body is .input w-full with an id");
  assert.ok(form.includes('<select id="finish-review-verdict" class="input w-full"'), "Verdict is .input w-full with an id");
  assert.ok(form.includes('id="finish-review-head-help"'), "reviewing-sha help carries an id");
  assert.equal((form.match(/aria-describedby="finish-review-head-help"/g) ?? []).length, 2, "both controls scope the help line");
  assert.ok(form.includes('class="muted mt-1 text-xs"'), "help line speaks the muted helper");
});

test("Finish review buttons: one primary idiom, plain cancel, busy swap kept", () => {
  const form = blockOf(PULL, "FinishReview");
  // Forgejo #594 appended the canonical disabled treatment (the
  // milestone-picker idiom) to the submit class — the pin tracks the
  // full literal, primary idiom intact.
  assert.ok(form.includes('class="btn primary px-3 py-1 disabled:cursor-not-allowed disabled:opacity-50"'), "submit is the defined btn primary idiom");
  assert.ok(form.includes("{getBusy() ? \"submitting…\" : \"submit review\"}"), "busy label swap kept");
  assert.ok(form.includes('<button type="button" class="btn px-3 py-1" onClick={props.onDone}>'), "cancel stays a plain btn");
});

test("chip-neutral joins the chip family in ui.css composition", () => {
  assert.match(CSS, /\.chip-neutral \{ @apply inline-flex rounded px-1\.5 py-px text-\[10px\] font-medium uppercase tracking-wide/);
  assert.ok(CSS.includes("bg-zinc-200 text-zinc-700"), "neutral reads zinc in light");
  assert.ok(CSS.includes("dark:bg-zinc-700/60 dark:text-zinc-300"), "neutral reads zinc in dark");
});

test("Checks.jsx card-list rides along (trivial: the class carried no rules)", () => {
  assert.ok(!CHECKS.includes("card-list"), "no card-list on the Checks page either");
  assert.ok(CHECKS.includes('<ul class="space-y-2">'), "the real layout utility stays");
});

test("no behavior change: flows, verdict values, staging, navigation intact", () => {
  const form = blockOf(PULL, "FinishReview");
  assert.ok(form.includes("props.client.pulls.reviews.submit(props.num,"), "submit flow intact");
  assert.ok(form.includes("state: getVerdict()"), "verdict value intact");
  assert.ok(form.includes("commit_sha: props.head"), "head sha intact");
  assert.ok(form.includes("threads: props.pending"), "staged threads intact");
  assert.ok(form.includes('value="COMMENTED"') && form.includes('value="APPROVED"') && form.includes('value="CHANGES_REQUESTED"'), "verdict values intact");
  assert.ok(form.includes("props.onUnstage(i())"), "unstage intact");
  const list = blockOf(PULL, "ReviewsList");
  assert.ok(list.includes("props.client.pulls.reviews.dismiss(props.num, seq,"), "dismiss flow intact");
  assert.ok(list.includes("props.reload()"), "reload intact");
  assert.ok(PULL.includes("renderBody(rv.body"), "review markdown intact");
});
