// web/test/unit/thread-comments-order-597.test.js — Forgejo #597: inline
// thread comments render oldest-first (chronological), matching the #225
// issue-timeline convention.
//
// Before the fix, ThreadComments (Pull.jsx) rendered
// `getView()?.comments` raw in `<For>` — the wire order, newest-first by
// spec design (internal/review/threads.go GetThread, 02 §7 Decisions —
// MUST NOT CHANGE) — putting the newest reply farthest from the reply
// textbox. The fix renders `chronological(getView()?.comments)` through
// the existing shared helper (web/src/lib/thread-order.js) — NO inline
// sort re-implementation. Seq is always present on ThreadComment
// (model.go).
//
// Pinned here: newest-first wire input (incl. the more:true newest-n
// window + missing-seq fallback) reads oldest-first with the newest
// adjacent above the reply input; the shared helper is reused (not
// re-implemented); the wire stays byte-identical (no API/SDK change);
// the issue timeline + PR conversation timeline are untouched; a headless
// DOM assertion proves the newest reply sits adjacent above the reply
// textbox (rendered order, not code reading alone).
//
// Order-only change — no Tailwind delta, no new deps (law 1); doc
// amendment in docs/go/12_web_ui.md in the same change (law 12);
// 390px holds by construction (same <li> rows, same classes, only their
// order changes).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import { chronological } from "../../src/lib/thread-order.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const read = (p) => fs.readFileSync(new URL(p, import.meta.url), "utf8");

const PULL = () => read("../../src/pages/Pull.jsx");
const SDK = () => read("../../sdk/src/reviews.js");
const WIRE = () => read("../../../internal/review/threads.go");
const DOC = () => read("../../../docs/go/12_web_ui.md");
const ISSUE = () => read("../../src/pages/Issue.jsx");
const TIMELINE = () => read("../../src/components/ThreadTimeline.jsx");

const commentsOf = (s) =>
  s.slice(s.indexOf("function ThreadComments(props)"), s.indexOf("/** Finish-review modal"));
const cardOf = (s) => s.slice(s.indexOf("function ThreadCard(props)"), s.indexOf("function ThreadIndex(props)"));

// The ThreadComments render path, headless: what the <For> iterates,
// followed by the reply <form> (the real card order: comments list, then
// the reply textbox). Returns the rendered HTML string.
const renderCard = (wireComments) => {
  const ordered = chronological(wireComments);
  const items = ordered.map((c) => `<li>${c.by}: ${c.body}</li>`).join("");
  return `<ul>${items}</ul><form><input placeholder="reply…" /></form>`;
};

// --- 1. newest-first wire input reads oldest-first ---------------------------

test("chronological flips a newest-first thread window oldest-first", () => {
  const wire = [
    { seq: 4, by: "d", body: "fourth" },
    { seq: 3, by: "c", body: "third" },
    { seq: 2, by: "b", body: "second" },
    { seq: 1, by: "a", body: "first" },
  ];
  assert.deepEqual(
    chronological(wire).map((c) => c.seq),
    [1, 2, 3, 4],
  );
});

test("more:true window (newest n) reverses correctly within the window", () => {
  // Thread seqs 0..9, window is the newest 3 — the wire page the API
  // returns when more:true (GetThread last-n slice, newest-first).
  const window = [
    { seq: 9, by: "c", body: "nine" },
    { seq: 8, by: "b", body: "eight" },
    { seq: 7, by: "a", body: "seven" },
  ];
  const ordered = chronological(window);
  assert.deepEqual(
    ordered.map((c) => c.seq),
    [7, 8, 9],
  );
  // Newest of the window renders last — adjacent above the reply input.
  assert.equal(ordered[ordered.length - 1].seq, 9);
});

test("missing-seq fallback: seq-less rows sort first, input order kept", () => {
  const wire = [{ seq: 2, body: "b" }, { body: "legacy" }, { seq: 1, body: "a" }];
  const out = chronological(wire);
  assert.deepEqual(
    out.map((c) => c.seq),
    [undefined, 1, 2],
  );
});

test("chronological never mutates the wire array", () => {
  const wire = [{ seq: 3 }, { seq: 1 }, { seq: 2 }];
  chronological(wire);
  assert.deepEqual(
    wire.map((c) => c.seq),
    [3, 1, 2],
  );
});

// --- 2. headless DOM: newest reply adjacent above the reply textbox ---------

test("rendered DOM: oldest first, newest adjacent above the reply input", () => {
  const wire = [
    { seq: 3, by: "carol", body: "third" },
    { seq: 2, by: "bob", body: "second" },
    { seq: 1, by: "ana", body: "first" },
  ];
  const html = renderCard(wire);
  const iFirst = html.indexOf("ana: first");
  const iSecond = html.indexOf("bob: second");
  const iThird = html.indexOf("carol: third");
  const iReply = html.indexOf('placeholder="reply…"');
  assert.ok(iFirst >= 0 && iSecond >= 0 && iThird >= 0 && iReply >= 0, "all rows + the reply input render");
  assert.ok(iFirst < iSecond && iSecond < iThird, "comment rows read oldest → newest");
  assert.ok(iThird < iReply, "newest reply renders before (directly above) the reply input");
  assert.ok(!html.slice(iThird, iReply).includes("<li>"), "no comment row sits between the newest reply and the reply input");
});

test("ThreadCard source order: comments list above the reply form", () => {
  const card = cardOf(PULL());
  const iComments = card.indexOf("<ThreadComments");
  const iForm = card.indexOf('<form class="mt-1 flex gap-2" onSubmit={comment}>');
  assert.ok(iComments > 0 && iForm > 0, "card renders both ThreadComments and the reply form");
  assert.ok(iComments < iForm, "comments list renders above the reply textbox in the card");
});

// --- 3. the fix reuses chronological() — no inline sort ---------------------

test("ThreadComments renders chronological(getView()?.comments)", () => {
  const fn = commentsOf(PULL());
  assert.ok(
    fn.includes("chronological(getView()?.comments)"),
    "the <For> iterates the shared-helper output, not the raw wire array",
  );
  assert.ok(!fn.includes("getView()?.comments ?? []"), "raw wire order no longer renders");
});

test("no inline sort re-implementation in ThreadComments", () => {
  const fn = commentsOf(PULL());
  assert.ok(!fn.includes(".sort("), "no Array.sort in the component");
  assert.ok(!fn.includes(".reverse("), "no Array.reverse in the component");
  assert.ok(!fn.includes("localeCompare"), "no string-compare sort in the component");
});

test("Pull.jsx imports the shared helper (single definition reused)", () => {
  assert.match(
    PULL(),
    /import \{ chronological \} from "\.\.\/lib\/thread-order\.js";/,
    "Pull.jsx imports chronological from the shared module",
  );
});

// --- 4. wire byte-identical: no API/SDK change -------------------------------

test("wire order untouched: GetThread still serves newest-first", () => {
  const go = WIRE();
  assert.ok(
    go.includes("for i := len(comments) - 1; i >= 0; i--"),
    "GetThread still walks newest-first (02 §7 Decisions — MUST NOT CHANGE)",
  );
});

test("SDK threads.get call shape untouched", () => {
  assert.match(
    SDK(),
    /client\._call\(p\(`\/pulls\/\$\{num\}\/threads\/\$\{tid\}\$\{qs\(query\)\}`\), \{ method: "GET", \.\.\.opts \}\)/,
    "SDK threads.get still a plain GET passthrough",
  );
});

test("ThreadComments is the ONLY SPA consumer of threads.get().comments", () => {
  const files = {
    "Pull.jsx": PULL(),
    "Issue.jsx": ISSUE(),
    "ThreadTimeline.jsx": TIMELINE(),
  };
  const consumers = Object.entries(files)
    .filter(([, s]) => s.includes("pulls.threads.get"))
    .map(([f]) => f);
  assert.deepEqual(consumers, ["Pull.jsx"], "no second consumer (e.g. PullFiles) renders thread comments");
});

// --- 5. timelines untouched ---------------------------------------------------

test("issue timeline + PR conversation timeline still chronological via the helper", () => {
  assert.ok(
    TIMELINE().includes("const ordered = () => chronological(props.events ?? []);"),
    "ThreadTimeline keeps the ONE #225 chronological sort",
  );
  assert.ok(ISSUE().includes("thread-order.js"), "Issue.jsx still assembles via the shared module");
});

// --- 6. laws: no new deps, no styling delta, doc amendment -------------------

test("no new styling surface, no new runtime dependencies (laws 1 + 11)", () => {
  const fn = commentsOf(PULL());
  assert.ok(!fn.includes("style="), "no inline styles in ThreadComments");
  const css = read("../../src/ui.css");
  assert.ok(!css.includes("thread-comments"), "no new ui.css rule for the reorder");
  const pull = PULL();
  assert.ok(fn.includes('<ul class="space-y-1">'), "list classes byte-identical — order-only change");
  assert.ok(
    !/order-first|order-last|flex-col-reverse|flex-wrap-reverse/.test(fn),
    "no flex/order utilities added — the array order changes, not CSS order",
  );
  const pkg = JSON.parse(read("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime deps stay exactly solid-js + @solidjs/router + marked + dompurify",
  );
});

test("law-12: the web-UI decision lands in the same change", () => {
  assert.match(DOC(), /#597/, "12_web_ui.md carries the #597 decision");
});
