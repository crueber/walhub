// web/test/unit/composer-dismiss-566.test.js — Forgejo #566: inline
// unstaged diff comment composers are dismissable.
//
// Two surfaces: the PR conversation page DiffFile keyed drafts
// (web/src/pages/Pull.jsx — closeDraft(key)) and the Files tab
// staged-selection composer (web/src/pages/PullFiles.jsx —
// setStaged(null)). Before the fix, Cancel rendered as invisible text
// (the `.link` class has zero CSS rules in shipped web/src/ui.css —
// Tailwind v4 preflight resets the button to plain muted text) and
// Escape did nothing.
//
// Fix (option (b) from the issue — small secondary .btn treatment, the
// canonical button idiom per guideline §2 Controls, readable in both
// themes via the class itself; the systemic unstyled-.link sweep is a
// SEPARATE follow-up, deliberately not bundled here):
// Cancel wears `btn ml-2 px-2 py-0.5 text-xs` on both composers, and a
// panel-level onKeyDown dismisses on Escape and refocuses the trigger
// that staged the draft (the SplitCloseMenu convention in
// CommentComposer.jsx:38-43 — Escape closes, focus returns; panel-level
// handler, so no document listener and no onCleanup).
//
// Pinned here: visible Cancel treatment on both surfaces, Escape wiring
// on both surfaces, per-key dismissal isolation, no-onStage/no-POST on
// dismiss, fresh-empty re-stage, refocus-trigger behavior (focus, never
// click — no toggle-fight), getCreated untouched, the out-of-scope
// .link uses left alone, and the law-12 doc amendment.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");

const PULL = () => read("../../src/pages/Pull.jsx");
const FILES = () => read("../../src/pages/PullFiles.jsx");
const CSS = () => read("../../src/ui.css");
const DOC = () => read("../../../docs/go/12_web_ui.md");

// --- 1. visible Cancel affordance: .btn Cancel at the LEFT of the
// composer bottom row, both surfaces (#587-scoped update: #587 moved
// Cancel out of the header <p> into CommentComposer's onCancel bottom-row
// slot — the pins below now assert the handler wiring + button-free
// header instead of the old header-button markup) ---

test("conversation draft Cancel lives in the composer bottom row (not the header)", () => {
  const s = PULL();
  assert.ok(
    s.includes('onCancel={() => closeDraft(draftKey(hi(), ri()))}'),
    "draft composer takes onCancel closing only its own key",
  );
  const panel = s.slice(s.indexOf("Dismissable draft composer"), s.indexOf("CommentComposer", s.indexOf("Dismissable draft composer")));
  assert.ok(!panel.includes("<button"), "no <button> anywhere in the draft header block — the header <p> keeps only the anchor label");
  assert.ok(!panel.includes('class="link'), "no .link class anywhere in the draft composer block");
  const composer = read("../../src/components/CommentComposer.jsx");
  assert.ok(composer.includes('onClick={() => props.onCancel()}'), "the bottom-row Cancel invokes the caller's onCancel");
});

test("Files-tab staged Cancel lives in the composer bottom row (not the header)", () => {
  const s = FILES();
  assert.ok(
    s.includes('onCancel={() => dismissStaged(false)}'),
    "staged composer takes onCancel dismissing without refocus",
  );
  const panel = s.slice(s.indexOf("Dismissable staged composer"), s.indexOf("CommentComposer", s.indexOf("Dismissable staged composer")));
  assert.ok(!panel.includes("<button"), "no <button> anywhere in the staged header block — the header <p> keeps only the anchor label");
  assert.ok(!panel.includes('class="link'), "no .link class anywhere in the staged composer block");
});

test("the .btn idiom carries both themes itself (canonical class, guideline §2)", () => {
  const css = CSS();
  const rule = css.slice(css.indexOf(".btn {"), css.indexOf(".btn-active"));
  assert.ok(rule.includes("dark:"), ".btn ships dark: variants — Cancel reads in both themes with no per-call-site theme code");
});

// --- 2. Escape dismissal, both surfaces ---

test("conversation draft panel dismisses on Escape and refocuses the trigger", () => {
  const s = PULL();
  const panel = s.slice(s.indexOf("Dismissable draft composer"), s.indexOf("</div>", s.indexOf("CommentComposer", s.indexOf("Dismissable draft composer"))));
  assert.ok(panel.includes('onKeyDown={(e) => {'), "panel-level key handler (catches the bubbled textarea Escape)");
  assert.ok(panel.includes('if (e.key !== "Escape") return;'), "only Escape dismisses");
  assert.ok(panel.includes("e.preventDefault();"), "Escape swallowed (SplitCloseMenu convention)");
  assert.ok(panel.includes("e.stopPropagation();"), "Escape never reaches a staging handler (no toggle-fight)");
  assert.ok(panel.includes("closeDraft(key);"), "Escape drops the draft");
  assert.ok(panel.includes("refocusTrigger(key);"), "Escape refocuses the gutter trigger that staged it");
});

test("Files-tab staged panel dismisses on Escape with refocus", () => {
  const s = FILES();
  const panel = s.slice(s.indexOf("Dismissable staged composer"), s.indexOf("</div>", s.indexOf("CommentComposer", s.indexOf("Dismissable staged composer"))));
  assert.ok(panel.includes('onKeyDown={(e) => {'), "panel-level key handler");
  assert.ok(panel.includes('if (e.key !== "Escape") return;'), "only Escape dismisses");
  assert.ok(panel.includes("e.preventDefault();") && panel.includes("e.stopPropagation();"), "Escape swallowed, no re-stage");
  assert.ok(panel.includes("dismissStaged(true);"), "Escape dismisses with trigger refocus");
});

test("no document-level Escape listeners (nothing to leak — no onCleanup needed)", () => {
  assert.ok(!PULL().includes('addEventListener("keydown"'), "Pull.jsx adds no document keydown listener");
  assert.ok(!FILES().includes('addEventListener("keydown"'), "PullFiles.jsx adds no document keydown listener");
});

// --- 3. refocus-trigger behavior: focus, never click ---

test("conversation refocus targets the staging gutter button by draft key", () => {
  const s = PULL();
  assert.ok(s.includes("const triggerRefs = new Map();"), "per-key trigger registry");
  assert.ok(
    s.includes("ref={(el) => el && triggerRefs.set(draftKey(hi(), ri()), el)}"),
    "gutter + registers itself under its own draft key",
  );
  assert.ok(s.includes("const refocusTrigger = (key) => triggerRefs.get(key)?.focus?.();"), "refocus is focus() with guards");
  assert.ok(!s.includes("refocusTrigger = (key) => triggerRefs.get(key)?.click"), "refocus never clicks (focus cannot re-stage)");
});

test("Files-tab refocus targets the comment-on-selection trigger", () => {
  const s = FILES();
  assert.ok(
    s.includes('document.querySelector(\'[aria-label^="Comment on selected lines"]\')?.focus?.();'),
    "refocus finds the DiffBody selection trigger by its aria-label and focuses it",
  );
  assert.ok(!s.includes(".click()"), "no synthetic clicks anywhere on the dismiss path");
});

// --- 4. per-key dismissal isolation ---

test("conversation dismissal removes only that keyed draft", () => {
  const s = PULL();
  assert.match(s, /const closeDraft = \(key\) =>/, "per-key close helper keeps its shape");
  assert.ok(s.includes("next.delete(key);"), "Map delete of exactly the dismissed key");
  assert.ok(!s.includes("getDrafts().clear") && !s.includes("setDrafts(new Map())"), "no whole-collection wipe on dismiss");
});

test("Files-tab dismissal clears only the staged selection", () => {
  const s = FILES();
  const dismiss = s.slice(s.indexOf("const dismissStaged"), s.indexOf("};", s.indexOf("const dismissStaged")) + 2);
  assert.ok(dismiss.includes("setStaged(null);"), "staged selection cleared");
  assert.ok(!dismiss.includes("setCreated"), "getCreated untouched — a posted thread is history, not a draft");
});

// --- 5. dismissal calls nothing: no onStage, no POST ---

test("conversation dismiss paths stage nothing and post nothing", () => {
  const s = PULL();
  // #587-scoped update: the Cancel call moved from a header onClick into
  // the composer's onCancel prop — adjacent to the onSubmit prop that
  // legitimately mentions onStage, so the old character-window check
  // would false-positive. Pin the handler expression itself instead:
  // it must be exactly the keyed closeDraft call.
  assert.ok(s.includes("onCancel={() => closeDraft(draftKey(hi(), ri()))}"), "Cancel handler is exactly the keyed drop — no onStage, no POST in the expression");
  const needle = "closeDraft(key);";
  const i = s.indexOf(needle);
  assert.ok(i > 0, "Escape dismiss call site present");
  const ctx = s.slice(Math.max(0, i - 400), i + 120);
  assert.ok(!ctx.includes("onStage"), "no onStage near the Escape dismiss call");
  assert.ok(!ctx.includes("threads.create") && !ctx.includes("fetch("), "no POST near the Escape dismiss call");
});

test("Files-tab dismiss helper stages nothing and posts nothing", () => {
  const s = FILES();
  const dismiss = s.slice(s.indexOf("const dismissStaged"), s.indexOf("};", s.indexOf("const dismissStaged")) + 2);
  assert.ok(!dismiss.includes("onCommentSelect"), "no selection replay");
  assert.ok(!dismiss.includes("submitThread") && !dismiss.includes("threads.create"), "no thread POST");
  assert.ok(!dismiss.includes("invalidate("), "no cache invalidation on dismiss");
});

// --- 6. re-stage opens a fresh empty composer ---

test("re-stage mounts a fresh empty CommentComposer on both surfaces", () => {
  const pull = PULL();
  assert.ok(
    pull.includes("when={props.canComment !== false && draftAt(hi(), ri())}"),
    "conversation composer lives under the per-key Show gate — dismiss unmounts it with its text",
  );
  const files = FILES();
  assert.ok(
    files.includes("<Show when={getStaged() && stagedAnchor()}>"),
    "staged composer lives under the staged gate — dismiss unmounts it with its text",
  );
  const composer = read("../../src/components/CommentComposer.jsx");
  assert.ok(composer.includes('const [getBody, setBody] = createSignal("");'), "composer body starts empty per mount");
  assert.ok(!pull.includes("draftText") && !files.includes("draftText"), "no draft-text stash survives dismissal");
});

// --- 7. out-of-scope .link uses deliberately untouched (separate follow-up) ---

test("the other .link uses are NOT swept (out of scope for #566)", () => {
  const pullLinks = (PULL().match(/class="link/g) ?? []).length;
  assert.ok(pullLinks >= 5, `Pull.jsx keeps its other .link uses (found ${pullLinks})`);
  for (const needle of ["submitDismiss(rv.seq)", "remove(who)", "onClick={toggle}", "props.onUnstage(i())"]) {
    assert.ok(PULL().includes(needle), `untouched .link-adjacent behavior intact: ${needle}`);
  }
  const fileLinks = (FILES().match(/class="link/g) ?? []).length;
  assert.ok(fileLinks >= 2, `PullFiles.jsx keeps its conversation/back .link uses (found ${fileLinks})`);
});

test("ui.css now carries the #568 shared .link rule — Cancel readability still comes from .btn, not it", () => {
  // #568-scoped update (was: "gains no .link rule here"): the systemic
  // follow-up landed option (a) — one shared .link rule for the 25
  // text-link sites. The composer Cancels deliberately do NOT use it:
  // dismiss actions are button-shaped (.btn), text links are .link.
  assert.ok(/\.link(?![\w-])/.test(CSS()), ".link now has its shared rule (Forgejo #568)");
  const rule = CSS().slice(CSS().indexOf(".link {"), CSS().indexOf("}", CSS().indexOf(".link {")) + 1);
  assert.ok(rule.includes("dark:"), "the shared rule reads in both themes");
});

// --- 8. lawfulness: no new deps, docs amended ---

test("no new runtime deps", () => {
  const pkg = JSON.parse(read("../../package.json"));
  for (const dep of ["solid-js", "@solidjs/router", "marked", "dompurify"]) {
    assert.ok(pkg.dependencies?.[dep], `still depends on ${dep}`);
  }
  assert.equal(Object.keys(pkg.dependencies ?? {}).length, 4, "exactly the four allowed runtime deps");
});

test("docs/go/12_web_ui.md carries the FIXED (Forgejo #566) amendment", () => {
  assert.ok(DOC().includes("FIXED (Forgejo #566)"), "law-12 amendment present");
});
