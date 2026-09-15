// web/test/unit/composer-cancel-row-587.test.js — Forgejo #587: move
// Cancel from the header row to the bottom action row (lower left,
// opposite submit) in the inline comment composers.
//
// Before: Pull.jsx (conversation diff-line draft) and PullFiles.jsx
// (Files-tab staged composer) each rendered a small secondary Cancel
// (.btn, the #566 idiom) in the header <p> next to "commenting on
// <anchor>", above a CommentComposer whose bottom row was
// `flex flex-wrap items-center justify-end gap-2` with the primary
// submit right-aligned.
//
// Fix (one mechanism, no per-page fork): CommentComposer takes an
// optional onCancel/cancelLabel prop pair rendering Cancel at the LEFT
// of the bottom row (left slot = Cancel, right slot = the existing
// close/submit cluster in its own inner flex, so the cluster never
// spreads — the row is justify-between only when onCancel is present,
// the pre-#587 flat right-aligned row otherwise). Both call sites drop
// their header-row button and pass the handler instead; the header <p>
// keeps only the anchor label. Behavior UNCHANGED: Cancel/Escape drops
// only the keyed draft, calls nothing, refocuses the trigger.
//
// Pinned here: bottom-row left-slot Cancel sharing the flex row with
// submit; button-free header <p> on both surfaces; both call-site
// wirings; no-onCancel consumers (issues page, PR main composer) render
// exactly as before; Escape/refocus/no-POST intact; Tailwind-only +
// .btn (no new CSS); no new deps; law-12 doc amendment; 390px reasoning.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");

const COMPOSER = () => read("../../src/components/CommentComposer.jsx");
const PULL = () => read("../../src/pages/Pull.jsx");
const FILES = () => read("../../src/pages/PullFiles.jsx");
const ISSUE = () => read("../../src/pages/Issue.jsx");
const CSS = () => read("../../src/ui.css");
const DOC = () => read("../../../docs/go/12_web_ui.md");

// --- 1. bottom row: Cancel-left / cluster-right, one mechanism ---

test("onCancel renders Cancel at the LEFT of the bottom row", () => {
  const s = COMPOSER();
  assert.ok(s.includes("props.onCancel"), "onCancel prop consumed");
  assert.ok(s.includes("{props.cancelLabel ?? \"Cancel\"}"), "cancelLabel with Cancel default");
  // The onCancel branch row (the literal row div — slicing from the
  // prose "justify-between" mention in the comment above would catch the
  // fallback's actions() first): Cancel button first, cluster second.
  const row = s.slice(s.indexOf('<div class="flex flex-wrap items-center justify-between gap-2">'));
  const cancelIdx = row.indexOf("props.onCancel()}");
  assert.ok(cancelIdx > 0, "Cancel button invokes the caller handler");
  const clusterIdx = row.indexOf("{actions()}");
  assert.ok(clusterIdx > cancelIdx, "Cancel precedes the submit cluster — left slot, opposite submit");
  assert.ok(row.slice(0, 80).includes("justify-between"), "onCancel row spreads Cancel-left / cluster-right");
});

test("Cancel shares the bottom flex row with the submit", () => {
  const s = COMPOSER();
  const branch = s.slice(s.indexOf("when={props.onCancel}"));
  // The onCancel branch is one outer flex row holding the Cancel button
  // AND the right-slot cluster (which holds the type=submit).
  assert.ok(branch.includes('type="button" class="btn"'), "Cancel wears the canonical .btn");
  // Forgejo #594 appended the canonical disabled treatment (the
  // milestone-picker idiom) to the submit class — the pin tracks the
  // full literal, primary idiom intact.
  assert.ok(s.includes('class="btn primary disabled:cursor-not-allowed disabled:opacity-50"'), "primary submit intact");
  assert.ok(
    branch.includes('<div class="flex flex-wrap items-center justify-end gap-2">{actions()}</div>'),
    "right slot is its own inner flex — the cluster never spreads across the row",
  );
});

test("the close/submit cluster is one shared definition (no per-shape fork)", () => {
  const s = COMPOSER();
  assert.ok(s.includes("const actions = () => ("), "single actions() cluster definition");
  assert.equal((s.match(/\{actions\(\)\}/g) ?? []).length, 2, "used exactly twice: fallback row + onCancel right slot");
  assert.equal((s.match(/type="submit"/g) ?? []).length, 1, "exactly one submit button definition — no fork");
});

// --- 2. without onCancel: layout unchanged ---

test("without onCancel the row renders exactly as before", () => {
  const s = COMPOSER();
  assert.ok(
    s.includes('fallback={<div class="flex flex-wrap items-center justify-end gap-2">{actions()}</div>}'),
    "fallback keeps the pre-#587 class string with the cluster as flat children (Show renders no wrapper — DOM identical)",
  );
  const fallback = s.slice(s.indexOf("fallback={<div"));
  assert.ok(!fallback.slice(0, 120).includes("justify-between"), "no justify-between without onCancel");
  const rendered = s.slice(s.indexOf('<form class="card mt-3'), s.indexOf("when={props.onCancel}"));
  assert.ok(!rendered.includes("<button"), "no button in the rendered form before the onCancel branch — Cancel lives only in the branch");
});

test("consumers WITHOUT onCancel (issues page, PR main composer) pass no handler", () => {
  assert.ok(!ISSUE().includes("onCancel"), "Issue.jsx composer takes no onCancel — renders exactly as before");
  const pull = PULL();
  assert.equal((pull.match(/onCancel=\{/g) ?? []).length, 1, "Pull.jsx passes onCancel exactly once (the inline draft composer, not the main PR composer)");
  assert.ok(!FILES().includes("onCancel") || (FILES().match(/onCancel=\{/g) ?? []).length === 1, "PullFiles.jsx passes onCancel exactly once (the staged composer)");
});

// --- 3. call sites: handler passed, header button dropped ---

test("conversation draft passes onCancel and keeps a button-free header", () => {
  const s = PULL();
  assert.ok(s.includes("onCancel={() => closeDraft(draftKey(hi(), ri()))}"), "draft onCancel drops only its own key");
  // "<CommentComposer" (bracketed) skips the prose comment mentioning the
  // component by name.
  const panel = s.slice(s.indexOf("Dismissable draft composer"), s.indexOf("<CommentComposer", s.indexOf("Dismissable draft composer")) + 200);
  const pOpen = panel.indexOf('<p class="mb-1 text-xs text-zinc-500 dark:text-zinc-400">');
  const pClose = panel.indexOf("</p>", pOpen);
  const header = panel.slice(pOpen, pClose);
  assert.ok(header.includes("commenting on"), "header keeps the anchor label");
  assert.ok(!header.includes("<button"), "header <p> has no <button>");
});

test("Files-tab staged composer passes onCancel and keeps a button-free header", () => {
  const s = FILES();
  assert.ok(s.includes("onCancel={() => dismissStaged(false)}"), "staged onCancel dismisses without refocus (same as the old header button)");
  const panel = s.slice(s.indexOf("Dismissable staged composer"), s.indexOf("<CommentComposer", s.indexOf("Dismissable staged composer")) + 200);
  const pOpen = panel.indexOf('<p class="text-xs text-zinc-500 dark:text-zinc-400">');
  const pClose = panel.indexOf("</p>", pOpen);
  const header = panel.slice(pOpen, pClose);
  assert.ok(header.includes("commenting on"), "header keeps the anchor label");
  assert.ok(!header.includes("<button"), "header <p> has no <button>");
});

test("no header-row Cancel button survives on either surface", () => {
  // The old exact header-button markups (class + dismiss onClick together)
  // are gone; the class string alone may legitimately recur elsewhere.
  assert.ok(!PULL().includes('onClick={() => closeDraft(draftKey(hi(), ri()))}>'), "old draft header-button markup gone from Pull.jsx");
  assert.ok(!FILES().includes('onClick={() => dismissStaged(false)}>'), "old staged header-button markup gone from PullFiles.jsx");
});

// --- 4. behavior identical: Escape / refocus / no-POST ---

test("Escape + refocus paths are untouched (Cancel adds no new dismissal semantics)", () => {
  const pull = PULL();
  assert.ok(pull.includes("closeDraft(key);") && pull.includes("refocusTrigger(key);"), "Escape still drops the keyed draft + refocuses");
  const files = FILES();
  assert.ok(files.includes("dismissStaged(true);"), "Escape still dismisses with refocus");
  // The onCancel handlers ARE the old header-button calls verbatim:
  // closeDraft(key) without refocus (Escape refocuses), dismissStaged(false).
  assert.ok(pull.includes("onCancel={() => closeDraft(draftKey(hi(), ri()))}"), "Cancel path = old header Cancel call, no refocus added");
  assert.ok(files.includes("onCancel={() => dismissStaged(false)}"), "Cancel path = old header Cancel call, no refocus added");
});

test("Cancel handler calls nothing: no onStage, no POST", () => {
  const composer = COMPOSER();
  const cancelBtn = composer.slice(composer.indexOf('onClick={() => props.onCancel()}') - 200, composer.indexOf('onClick={() => props.onCancel()}') + 60);
  assert.ok(!cancelBtn.includes("onStage") && !cancelBtn.includes("threads.create") && !cancelBtn.includes("fetch("), "Cancel invokes only the caller handler");
  assert.ok(!composer.includes("props.onCancel()") || composer.includes("onClick={() => props.onCancel()}"), "single synchronous call — no busy guard, no async post");
});

// --- 5. lawfulness: Tailwind-only, no new CSS, no new deps, 390px ---

test("Cancel composes existing classes only — no new CSS", () => {
  const composer = COMPOSER();
  assert.ok(composer.includes('class="btn"'), "Cancel is the canonical .btn (both themes via the class itself)");
  assert.ok(composer.includes("flex flex-wrap items-center justify-between gap-2"), "row is Tailwind composition only");
  assert.ok(!CSS().includes("cancel"), "no cancel-specific rule added to ui.css");
});

test("no new runtime deps", () => {
  const pkg = JSON.parse(read("../../package.json"));
  for (const dep of ["solid-js", "@solidjs/router", "marked", "dompurify"]) {
    assert.ok(pkg.dependencies?.[dep], `still depends on ${dep}`);
  }
  assert.equal(Object.keys(pkg.dependencies ?? {}).length, 4, "exactly the four allowed runtime deps");
});

test("both bottom-row shapes wrap at narrow widths (390px reasoning)", () => {
  const s = COMPOSER();
  assert.ok(s.includes("flex flex-wrap items-center justify-between gap-2"), "onCancel row wraps");
  assert.ok(s.includes('<div class="flex flex-wrap items-center justify-end gap-2">{actions()}</div>'), "right-slot cluster wraps independently — Cancel + submit stack instead of overflowing");
});

test("docs/go/12_web_ui.md carries the FIXED (Forgejo #587) amendment", () => {
  assert.ok(DOC().includes("FIXED (Forgejo #587)"), "law-12 amendment present");
});
