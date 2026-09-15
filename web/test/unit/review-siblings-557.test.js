// web/test/unit/review-siblings-557.test.js — Forgejo #557: the
// FinishReview form + ThreadIndex nav join the sibling card/padding idiom
// #554 gave ReviewsList. Styling/markup only — every gate, fetch, verdict
// value, anchor, staging flow, and navigation stays byte-identical.
//
// - FinishReview: the form composes the sibling card padding (card p-3,
//   the ReviewsList #554 / CommentComposer form.card idiom), so the staged
//   list, the #479 fields, and the button row sit inside the border
//   instead of touching its edges.
// - ThreadIndex: the nav composes the same padding (card mb-4 p-3 — the
//   mb-4 conversation spacing stays, padding joins it), so the pill
//   entries sit inside the border instead of touching its edges.
// - No new CSS, no new classes: composition of the canonical .card /
//   .card-header idioms only (guideline §2 by reference).
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
  return src.slice(start, end > 0 ? end : start + 8000);
}

test("FinishReview form composes the sibling card padding", () => {
  const form = blockOf(PULL, "FinishReview");
  assert.match(form, /<form class="card p-3" aria-label="Finish review"/, "card p-3 — the ReviewsList #554 padding idiom");
  assert.ok(form.includes('<h2 class="card-header">Finish review</h2>'), "card-header title stays (#521)");
  assert.ok(!form.includes('<form class="card" aria-label="Finish review"'), "bare unpadded form gone");
});

test("ThreadIndex nav composes the sibling card padding", () => {
  const index = blockOf(PULL, "ThreadIndex");
  assert.match(index, /<nav class="card mb-4 p-3" aria-label="Comments index">/, "card mb-4 p-3 — spacing kept, padding joined");
  assert.ok(index.includes('<h2 class="card-header">'), "card-header title stays (#521)");
  assert.ok(!index.includes('<nav class="card mb-4" aria-label="Comments index">'), "bare unpadded nav gone");
});

test("no new styling surface: composition only", () => {
  const form = blockOf(PULL, "FinishReview");
  const index = blockOf(PULL, "ThreadIndex");
  assert.ok(!form.includes("style="), "no inline styles on the FinishReview form");
  assert.ok(!index.includes("style="), "no inline styles on the ThreadIndex nav");
  assert.ok(!CSS.includes("FinishReview") && !CSS.includes("ThreadIndex") && !CSS.includes("finish-review"), "no form/nav-scoped CSS added");
});

test("no behavior change: staging, anchors, submit, jump intact", () => {
  const form = blockOf(PULL, "FinishReview");
  const index = blockOf(PULL, "ThreadIndex");
  assert.ok(form.includes("no staged line comments"), "empty copy byte-identical");
  assert.ok(form.includes("anchorLabel(p.anchor)"), "staged anchor labels kept");
  assert.ok(form.includes("props.onUnstage(i())"), "unstage flow kept");
  assert.ok(form.includes("finish-review-body"), "body field wiring kept");
  assert.ok(form.includes("finish-review-verdict"), "verdict field wiring kept");
  assert.ok(form.includes("props.client.pulls.reviews.submit(props.num,"), "submit flow intact");
  assert.ok(form.includes("onClick={props.onDone}"), "cancel intact");
  assert.ok(index.includes("sortThreadsForIndex(props.threads"), "index order kept");
  assert.ok(index.includes("freshnessOf(t.anchor,"), "freshness truth kept");
  assert.ok(index.includes("props.onJump(e.target)"), "jump affordance kept");
  assert.ok(index.includes("anchorLabel(e.t.anchor)"), "entry labels kept");
  assert.ok(PULL.includes('<div class="card p-3" aria-label="Reviews">'), "ReviewsList #554 padding untouched");
});
