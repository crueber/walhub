// web/test/unit/ref-picker-pill.test.js — issue #214 regression: the refs
// picker lives UNDER the head pill on the left (`{branch} @ {sha} ▾` opens
// the branch/tag picker); no standalone `refs` dropdown remains on the
// right (Clone stays). A move, not a rewrite — picker behavior
// (streaming, filtering, switching) is pinned unchanged.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const src = () => srcOf("../../src/pages/Repo.jsx");
const css = () => srcOf("../../css/repo.css");

test("RefPicker trigger is the head pill, not a standalone refs button", () => {
  const s = src();
  assert.ok(!s.includes("refs ▾"), "no standalone `refs ▾` trigger text remains");
  assert.match(s, /\{label\(\)\} ▾/, "trigger renders the pill label");
  // Issue #252: the label delegates to lib/ref-pill.js pillLabel (branch @
  // sha, short-sha for sha-addressed views, refs fallback without head).
  assert.match(s, /const label = \(\) => pillLabel\(head\(\)\)/, "pill label derives via pillLabel");
});

test("pillLabel contract lives in lib/ref-pill.js (headless-tested)", () => {
  const lib = srcOf("../../src/lib/ref-pill.js");
  assert.match(lib, /export function pillLabel\(head\)/, "pillLabel exported from the headless lib");
  assert.ok(lib.includes("refs"), "refs fallback kept");
});

test("single RefPicker call site: inside repo-meta, fed by the summary head", () => {
  const s = src();
  const uses = s.match(/<RefPicker /g) ?? [];
  assert.equal(uses.length, 1, "exactly one RefPicker usage (the pill)");
  const meta = s.indexOf("repo-meta");
  const picker = s.indexOf("<RefPicker ");
  const counts = s.indexOf("branches ·");
  assert.ok(meta !== -1 && picker > meta && counts > picker, "picker sits in repo-meta next to the branch/tag counts");
  assert.ok(s.includes("head={() => pillHead(getViewed(), s().head)}"), "pill head is context-first (viewed ref) with the summary head as fallback (issue #252)");
  assert.ok(s.includes('fallback={<span class="pill">empty</span>}'), "empty repos keep the static empty pill");
});

test("right-side controls keep Clone, lose the picker", () => {
  const s = src();
  const right = s.slice(s.indexOf("ml-auto"));
  assert.ok(!right.includes("<RefPicker"), "no picker on the right side");
  assert.ok(right.includes("<CloneMenu"), "Clone menu stays on the right");
});

test("picker behavior unchanged: stream, debounce, filter, switch", () => {
  const s = src();
  assert.ok(s.includes("repo.refStream(getKind(), { q: getQuery(), n: 50 }"), "SSE ref stream (50/page, query) unchanged");
  assert.ok(s.includes("setTimeout(() => { setRefs([]); stream.run(); }, 150)"), "150 ms filter debounce unchanged");
  assert.ok(s.includes('<option value="branches">branches</option>'), "branches option kept");
  assert.ok(s.includes('<option value="tags">tags</option>'), "tags option kept");
  assert.match(
    s,
    /navigate\(`\/\$\{props\.full\}\/tree\/\$\{kind === "tag" \? r\.name : shortRef\(r\.name\)\}`\)/,
    "pick navigates to /{full}/tree/{ref} (full tag name, short branch) unchanged",
  );
  assert.ok(s.includes('placeholder="filter refs…"'), "filter input kept");
});

test("picker keyboard contract: native button + Esc + autofocus + roles", () => {
  const s = src();
  assert.ok(s.includes('aria-haspopup="listbox"'), "trigger announces the popup");
  assert.ok(s.includes("aria-expanded={getOpen()}"), "trigger exposes open state");
  assert.match(s, /e\.key === "Escape" && getOpen\(\)/, "Esc dismisses the open picker");
  assert.ok(s.includes("trigger?.focus()"), "Esc returns focus to the pill trigger");
  assert.ok(s.includes("ref={(el) => el?.focus()}"), "filter input takes focus on open");
  assert.ok(s.includes('role="listbox"'), "dropdown carries the listbox role");
  assert.ok(s.includes('aria-label="Filter branches and tags"'), "filter input is labelled");
});

test("picker dropdown anchors left under the pill", () => {
  const s = src();
  assert.ok(s.includes("ref-drop card absolute left-0"), "dropdown opens left-aligned under the pill");
  assert.ok(!s.includes("ref-drop card absolute right-0"), "no right-anchored dropdown remains");
  assert.match(css(), /\.ref-drop \{\s*\n?\s*position: absolute; left: 0;/, "repo.css .ref-drop anchors left");
});
