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
  assert.match(
    s,
    /navigate\(`\/\$\{props\.full\}\/tree\/\$\{kind === "tag" \? r\.name : shortRef\(r\.name\)\}`\)/,
    "pick navigates to /{full}/tree/{ref} (full tag name, short branch) unchanged",
  );
  assert.ok(s.includes('placeholder="filter refs…"'), "filter input kept");
});

test("Forgejo #482: the type <select> is gone; two toggle pills sit above the filter input", () => {
  const s = src();
  // Scope to the RefPicker body (other page controls keep their selects).
  const picker = s.slice(s.indexOf("function RefPicker"), s.indexOf("// --- star toggle"));
  assert.ok(!picker.includes("<select"), "no <select> remains in the picker");
  assert.ok(!picker.includes("<option"), "no <option> remains in the picker");
  assert.ok(s.includes('role="group"'), "pills form a group");
  assert.ok(s.includes('aria-label="Ref type"'), "group is labelled Ref type");
  assert.match(picker, />\s*Branches\s*</, "Branches pill kept");
  assert.match(picker, />\s*Tags\s*</, "Tags pill kept");
  // The btn pill idiom (#447/#465): btn metrics with the primary
  // active-state treatment, real buttons with aria-pressed.
  assert.ok(s.includes('class="btn px-2 py-1 text-sm"'), "pills use the btn pill metrics");
  assert.ok(s.includes('classList={{ primary: getKind() === "branches" }}'), "branches pill actives via primary");
  assert.ok(s.includes('classList={{ primary: getKind() === "tags" }}'), "tags pill actives via primary");
  assert.ok(s.includes('aria-pressed={getKind() === "branches"}'), "branches pill exposes pressed state");
  assert.ok(s.includes('aria-pressed={getKind() === "tags"}'), "tags pill exposes pressed state");
  assert.ok(s.includes('type="button"'), "pills are real buttons");
  // The switch contract is the old onChange behavior: setKind + clear +
  // re-stream, query untouched (persists across the switch).
  const m = s.match(/const switchKind = \(kind\) => \{[\s\S]*?\n  \};/);
  assert.ok(m, "kind switch helper kept");
  assert.ok(m[0].includes("setKind(kind);"), "switch sets the kind");
  assert.ok(m[0].includes("setRefs([]);"), "switch clears the list");
  assert.ok(m[0].includes("stream.run();"), "switch re-streams");
  assert.ok(!m[0].includes("setQuery"), "switch leaves the query untouched");
  // Layout: pills row on top, input full width below.
  assert.ok(s.includes("ref-controls mb-2 flex flex-col gap-2"), ".ref-controls is a column context");
  assert.ok(s.includes('class="input w-full"'), "filter input goes full width below the pills");
});

test("Forgejo #482: pinned default row renders first from the summary head via pick()", () => {
  const s = src();
  // The pin reads the SUMMARY head prop (the default-branch target), never
  // the context-first pill head: deriving from head() would badge whatever
  // branch is on screen (e.g. fix/…) as "default" while the real default
  // stays past the 50-ref page window.
  assert.ok(s.includes("pinnedDefault(summaryHead(), getKind())"), "pin derives from the summary head, branches only");
  assert.ok(!s.includes("pinnedDefault(head(), getKind())"), "pin never derives from the viewed-ref pill head");
  assert.ok(s.includes("summaryHead={() => s().head}"), "call site passes the raw summary head (not pillHead) for the pin");
  assert.ok(s.includes("head={() => pillHead(getViewed(), s().head)}"), "pill label keeps the context-first head (#252 untouched)");
  assert.ok(s.includes("dedupeRefs(pinned(), getRefs())"), "streamed list dedupes against the pin");
  const list = s.indexOf("ref-list");
  const pin = s.indexOf("ref-pinned");
  const streamed = s.indexOf("<For each={visibleRefs()}");
  assert.ok(list !== -1 && pin > list && streamed > pin, "pinned row sits first in .ref-list, above the streamed rows");
  assert.ok(s.includes(">default<"), "pinned row carries the default group label/badge");
  assert.ok(s.includes('<span class="pill ml-1">default</span>'), "default badge uses the .pill chip idiom");
  assert.ok(s.includes("onClick={() => pick(p())}"), "pinned row clicks through the pick() tree path");
  assert.ok(s.includes("pinnedDefault, dedupeRefs"), "pin derivation is shared from lib/ref-pill.js (headless-tested)");
});

test("Forgejo #482: popover stays opaque with the viewport bound", () => {
  const s = src();
  assert.ok(s.includes("ref-drop card absolute left-0"), "dropdown keeps its opaque card popover classes");
  assert.ok(css().includes("background: var(--panel)"), "repo.css .ref-drop keeps the var(--panel) background");
  assert.ok(css().includes("max-height: 420px"), "repo.css .ref-drop keeps its sizing");
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
