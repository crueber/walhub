// web/test/unit/staged-inline-567.test.js — Forgejo #567: staged inline
// review comments render in-thread at their line until review submit.
//
// Before the fix, staging a keyed draft composer (Pull.jsx DiffFile,
// #560/#566) handed {anchor, body} to the page pending list and unmounted
// the composer — the row showed nothing. Only the FinishReview button
// counter + form pending list rendered staged drafts; inline rows rendered
// only open composers (draftAt) + posted threads (threadsAt → ThreadCard).
//
// Fix: the page-level pending list (the SAME signal feeding the
// finish-review modal list + button count — one source of truth, no forked
// state) locates per row into StagedCards below the same anchor line, in
// the existing inline-card slot (ml-14 mt-1 rounded border, shared with
// ThreadCard/composer). Edit re-opens the keyed composer in place
// pre-filled (CommentComposer initialValue); remove unstages through the
// shared mutation. ThreadIndex lists staged entries marked staged,
// jumping to the staged card via the staged-<i>/flashStaged idiom.
//
// Pinned here (per the inline-composer-560.test.js convention): stage→card
// placement below the line with no invisible moment, staged pill + author
// + plain body in both themes, keyed multi-instance (multiple staged per
// file/line), edit round-trip incl. stale-index resolution, unstage
// agreement across card + FinishReview list + button count, ThreadIndex
// listing + linking, the #502 anon gate, the unified-only conversation
// renderer (the Files-tab DiffBody posts threads directly — no staged
// state to mirror), 390px scroll-width neutrality, no new deps,
// Tailwind-only composition, and the law-12 doc amendment.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");

const PULL = () => read("../../src/pages/Pull.jsx");
const COMPOSER = () => read("../../src/components/CommentComposer.jsx");
const FILES = () => read("../../src/pages/PullFiles.jsx");
const CSS = () => read("../../src/ui.css");
const DOC = () => read("../../../docs/go/12_web_ui.md");

const diffFile = () => {
  const s = PULL();
  return s.slice(s.indexOf("function DiffFile(props)"), s.indexOf("function StagedCard(props)"));
};

// --- 1. stage→card renders below the anchor line, same slot ------------------

test("staged entries locate per row and render below the line", () => {
  const s = PULL();
  assert.match(s, /const stagedAt = \(hi, ri\) => stagedByKey\(\)\.byKey\.get\(`\$\{hi\}:\$\{ri\}`\)/, "keyed per-line lookup mirrors threadsAt");
  const forRows = s.indexOf("<For each={rows}>");
  const stagedFor = s.indexOf("stagedAt(hi(), ri())");
  assert.ok(forRows > 0 && stagedFor > forRows, "staged <For> lives inside the row <For>");
  const draftShow = s.indexOf("draftAt(hi(), ri())");
  const threadsFor = s.indexOf("threadsAt(hi(), ri())");
  assert.ok(stagedFor > draftShow, "staged cards sit below the composer slot");
  assert.ok(stagedFor < threadsFor, "staged cards sit above the posted thread cards");
});

test("staged card shares the inline-card slot (no wider scroll than the composer)", () => {
  const s = PULL();
  const card = s.slice(s.indexOf("function StagedCard(props)"), s.indexOf("function ThreadCard(props)"));
  assert.ok(
    card.includes("ml-14 mt-1 rounded border border-zinc-200 p-2 dark:border-zinc-700"),
    "StagedCard composes the exact ThreadCard/composer slot classes",
  );
  const overflow = s.indexOf("overflow-x-auto", s.indexOf("function DiffFile(props)"));
  assert.ok(overflow > 0 && overflow < s.indexOf("stagedAt(hi(), ri())"), "cards render inside the existing overflow-x-auto wrapper — no new scroll container");
});

test("staged placement matches posted-thread placement (same line rule)", () => {
  const d = diffFile();
  assert.ok(d.includes("stagedByKey"), "DiffFile derives staged placement fresh every render");
  assert.match(d, /\(row\.newNo === no && a\.side === "NEW"\) \|\| \(row\.oldNo === no && a\.side === "OLD"\)/, "staged entries match on the same start-line rule as threads");
  assert.ok(d.includes("(p.anchor?.path ?? \"\") !== props.file.path"), "only this file's staged entries render inline");
});

// --- 2. card idiom: author + plain body + staged pill, both themes -----------

test("staged card shows author, plain body, and a staged pill", () => {
  const s = PULL();
  const card = s.slice(s.indexOf("function StagedCard(props)"), s.indexOf("function ThreadCard(props)"));
  assert.ok(card.includes("props.stagedBy"), "author rides the card (the stager)");
  assert.ok(card.includes("whitespace-pre-wrap"), "body keeps the FinishReview plain-text treatment");
  assert.ok(!card.includes("markdown-body") && !card.includes("renderBody"), "staged text is a draft — never markdown-rendered");
  assert.match(card, />\s*staged\s*</, "visible staged pill");
});

test("staged pill is distinct from resolved/outdated and reads in both themes", () => {
  const s = PULL();
  const card = s.slice(s.indexOf("function StagedCard(props)"), s.indexOf("function ThreadCard(props)"));
  assert.ok(
    card.includes('class="chip chip-draft"'),
    "canonical amber chip (ui.css both-themes tokens — no per-callsite bg override per the #545 F2 rule)",
  );
  assert.ok(!card.includes("bg-amber-100") && !card.includes("bg-zinc-200"), "no hardcoded pill bg — color lives in ui.css");
  assert.ok(!card.includes("<style"), "no inline <style> in the card");
});

test("staged card carries a stable jump id + aria label, flashing like a thread card", () => {
  const s = PULL();
  const card = s.slice(s.indexOf("function StagedCard(props)"), s.indexOf("function ThreadCard(props)"));
  assert.ok(card.includes("id={`staged-${props.index}`}"), "id staged-<pending-index> is the ThreadIndex jump target");
  assert.ok(card.includes("aria-label={`Staged comment on ${label()}`}"), "line-labelling aria-label");
  assert.ok(card.includes("outline outline-2 outline-emerald-500"), "flashed card draws the ThreadCard emerald outline");
});

test("staged card controls are visible buttons (no new invisible .link)", () => {
  const s = PULL();
  const card = s.slice(s.indexOf("function StagedCard(props)"), s.indexOf("function ThreadCard(props)"));
  assert.ok(!card.includes('class="link"'), "no .link in StagedCard — .link ships zero CSS rules (the #566 lesson)");
  assert.ok(card.includes('class="btn ml-2 px-2 py-0.5 text-xs"'), "edit/remove use the #566 canonical small-btn treatment");
});

// --- 3. keyed multi-instance: several staged per file/line --------------------

test("multiple staged comments per file render simultaneously", () => {
  const s = PULL();
  assert.match(s, /<For each=\{stagedAt\(hi\(\), ri\(\)\)\}>\s*\{\(s\) => \(/, "a <For> over the per-line staged list — never a single <Show>");
  assert.match(s, /byKey\.get\(found\)\.push\(\{ p, i \}\)/, "placement collects every entry per line key");
  assert.ok(!s.includes("getStaged()") && !s.includes("stagedEntry)"), "no single-staged signal remains");
});

test("staged entries that no longer locate render once at the file end", () => {
  const s = PULL();
  assert.ok(s.includes("stagedByKey().unplaced"), "drifted staged anchors fall to the file end like posted threads");
  const unplaced = s.slice(s.indexOf("stagedByKey().unplaced"), s.indexOf("placement().unplaced"));
  assert.ok(unplaced.includes("<StagedCard"), "file-end fallback renders staged cards, never relocated lines");
  assert.ok(unplaced.includes("onEdit={null}"), "file-end cards carry no edit control — there is no row to reopen under");
});

// --- 4. edit round-trip: pre-filled composer, pending update ------------------

test("edit re-opens the keyed composer in place pre-filled with the staged body", () => {
  const s = PULL();
  assert.match(s, /const editStaged = \(hunkIdx, rowIdx, s\) =>/, "per-line edit helper");
  assert.ok(s.includes("editIndex: s.i"), "draft records the pending position");
  assert.ok(s.includes("initialBody: s.p.body"), "draft records the staged body");
  assert.ok(s.includes("initialValue={d().initialBody}"), "composer mounts pre-filled");
  assert.ok(COMPOSER().includes("const v = props.initialValue;"), "composer tracks initialValue reactively");
  assert.ok(COMPOSER().includes("if (v) setBody(v);"), "effect prefills on a real initialValue — re-prefills on edit-target switches, never clobbers typing");
  assert.ok(COMPOSER().includes('const [getBody, setBody] = createSignal("");'), "fresh mounts stay empty (the #566 pin holds)");
});

test("composer submit updates the pending entry on edit, stages on fresh", () => {
  const s = PULL();
  assert.match(s, /const idx = resolveStagedEdit\(d\(\)\);/, "submit resolves the edit target fresh");
  assert.match(s, /if \(idx >= 0\) props\.onUpdate\(idx, body\);/, "edit path updates the pending entry");
  assert.match(s, /props\.onStage\(\{ anchor: d\(\)\.anchor, body \}\)/, "fresh path still stages {anchor, body} (the #560 pin holds)");
  assert.match(s, /submitLabel=\{d\(\)\.editIndex != null \? "Save" : "Stage comment"\}/, "edit composer reads Save, fresh reads Stage comment");
});

test("stale pending indices resolve by anchor instead of clobbering", () => {
  const s = PULL();
  const fn = s.slice(s.indexOf("const resolveStagedEdit"), s.indexOf("};", s.indexOf("const resolveStagedEdit")) + 2);
  assert.ok(fn.includes("if (dd?.editIndex == null) return -1;"), "fresh drafts never take the update path");
  assert.ok(fn.includes("anchorLabel(cur.anchor) === anchorLabel(dd.anchor)"), "fast path verifies the anchor still sits at the recorded index");
  assert.ok(fn.includes("findIndex((p) => anchorLabel(p.anchor)"), "slow path re-resolves by anchor (+ original body) after mid-edit removals");
});

test("cancelling an edit drops only the draft — the staged entry stays", () => {
  const s = PULL();
  const panel = s.slice(s.indexOf("onClick={() => closeDraft(draftKey(hi(), ri()))}>"), s.indexOf("CommentComposer", s.indexOf("onClick={() => closeDraft(draftKey(hi(), ri()))}>")));
  assert.ok(!panel.includes("onUnstage") && !panel.includes("onUpdate"), "Cancel calls nothing but closeDraft");
});

// --- 5. unstage: one mutation feeding card + form list + button count ---------

test("card remove unstages through the shared pending-list mutation", () => {
  const s = PULL();
  assert.ok(s.includes("onUnstage={() => props.onUnstage(s.i)}"), "card remove passes the entry's live pending index");
  assert.ok(s.includes("props.onUnstage(i())"), "FinishReview per-item remove is untouched (the #566 pin holds)");
  assert.match(s, /const unstage = \(i\) => setPending\(\(list\) => list\.filter\(\(_, j\) => j !== i\)\)/, "single index-based removal over the one pending signal");
  assert.match(s, /const updatePending = \(i, body\) => setPending\(\(list\) => list\.map\(\(p, j\) => \(j === i \? \{ \.\.\.p, body \} : p\)\)\)/, "edits map over the same signal — no forked state");
});

test("FinishReview list + button count agree with the inline cards", () => {
  const s = PULL();
  assert.ok(s.includes("pending={getPending()}"), "form list + DiffFile cards read the same signal");
  assert.ok(s.includes("Finish review{(getPending().length"), "button count reads the same signal");
  const diffCall = s.slice(s.indexOf("<DiffFile"), s.indexOf("/>", s.indexOf("<DiffFile")));
  for (const prop of ["pending={getPending()}", "onUnstage={unstage}", "onUpdate={updatePending}", "stagedBy={getMe()?.principal}", "flashStaged={getFlashStaged}"]) {
    assert.ok(diffCall.includes(prop), `DiffFile receives ${prop}`);
  }
});

// --- 6. ThreadIndex lists + links staged --------------------------------------

test("ThreadIndex lists staged entries marked staged, linked to the inline card", () => {
  const s = PULL();
  const index = s.slice(s.indexOf("function ThreadIndex(props)"), s.indexOf("function ThreadComments(props)"));
  assert.ok(index.includes("props.pending ?? []"), "index reads the pending list");
  assert.ok(index.includes("<span> · staged</span>"), "staged entries are marked staged");
  assert.ok(index.includes("onClick={() => props.onJumpStaged(i())}"), "staged pills jump by pending index");
  assert.ok(index.includes("anchorLabel(p.anchor)"), "staged pills label by anchor like posted entries");
  assert.match(s, /const jumpToStaged = \(i\) => \{\s*setFlashStaged\(i\);\s*document\.getElementById\(`staged-\$\{i\}`\)\?\.scrollIntoView\(\{ block: "center" \}\);/, "jump scrolls to staged-<i> and flashes it (the jumpToThread idiom)");
});

test("index panel opens for staged-only reviews and counts them", () => {
  const s = PULL();
  const index = s.slice(s.indexOf("function ThreadIndex(props)"), s.indexOf("function ThreadComments(props)"));
  assert.ok(index.includes("<Show when={(props.threads ?? []).length > 0 || (props.pending ?? []).length > 0}>"), "staged-only reviews still get the panel");
  assert.ok(index.includes("+ ${(props.pending ?? []).length} staged"), "header counts staged alongside posted");
  assert.ok(s.includes("pending={getPending()} onJumpStaged={jumpToStaged}"), "page wires pending + jump into the index");
});

// --- 7. #502: anonymous viewers see nothing new -------------------------------

test("#502 gate: staged cards render only for commenters; anon pending stays empty", () => {
  const d = diffFile();
  const gates = (d.match(/props\.canComment (?:!== false|=== false)/g) ?? []).length;
  assert.ok(gates >= 4, `staged slots join the gated entry points (found ${gates})`);
  assert.ok(d.includes("<Show when={props.canComment !== false}>"), "staged render sits under the canComment gate");
});

// --- 8. unified + split: the conversation renderer is the one staged surface --

test("conversation DiffFile is the single unified staged surface; Files tab posts direct", () => {
  const s = PULL();
  const d = diffFile();
  assert.ok(!d.includes("getMode") && !d.includes('"split"'), "conversation DiffFile has no split mode — one unified renderer to fix");
  assert.ok(!FILES().includes("StagedCard") && !FILES().includes("props.pending"), "Files-tab DiffBody carries no staged state");
  assert.ok(FILES().includes("pulls.threads.create"), "Files-tab selection posts a thread directly — nothing to mirror");
});

// --- 9. themes + 390px: composition only, inside the existing scroll ---------

test("390px: staged cards add no width beyond the composer slot", () => {
  const s = PULL();
  const card = s.slice(s.indexOf("function StagedCard(props)"), s.indexOf("function ThreadCard(props)"));
  assert.ok(!/w-\d/.test(card) && !card.includes("min-w-") && !card.includes("max-w-"), "no fixed/min/max widths on the card");
  assert.ok(!card.includes("overflow"), "no new scroll container — the row's overflow-x-auto wrapper holds");
});

test("no new CSS patterns, no new runtime dependencies (laws 1 + 11)", () => {
  const css = CSS();
  assert.ok(!css.includes("staged-567") && !css.includes("staged-card") && !css.includes("StagedCard"), "no new ui.css rules for #567");
  assert.ok(!PULL().includes("<style"), "no inline <style> in Pull.jsx");
  const pkg = JSON.parse(read("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime deps stay exactly solid-js + @solidjs/router + marked + dompurify",
  );
});

test("law-12: the web-UI decision lands in the same change", () => {
  assert.match(DOC(), /#567/, "12_web_ui.md carries the #567 decision");
});
