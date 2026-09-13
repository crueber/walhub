// web/test/unit/milestone-icons-484.test.js — Forgejo #484: the
// milestone-open/milestone-done user-provided SVGs join the shared #465
// icons.jsx mechanism and apply state-mapped on four surfaces: the
// milestones-page open-card state chip, the closed rows, the issues-list
// milestone chip, and (Forgejo #495) the issues-toolbar Milestones link +
// the milestones page heading — the repo tab-strip option-(a) placement
// was wrong (it painted the icon on the Issues tab instead of the
// Milestones link) and is gone. No DOM: the icon map + JSX pinned as
// source text, mirroring icons-465.test.js / issue-comment-481.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const ICONS = srcOf("../../src/lib/icons.jsx");
const MILESTONES = srcOf("../../src/pages/Milestones.jsx");
const ISSUES = srcOf("../../src/pages/Issues.jsx");
const REPO = srcOf("../../src/pages/Repo.jsx");
const TABS = srcOf("../../src/lib/tabs.js");

// Verbatim transcriptions of the issue-provided SVG paths
// (/tmp/svg-icons/, embedded AS LABELED per the #465 files-are-truth
// precedent: open=16, done=24 — the issue body prose lists them swapped).
const OPEN_D =
  "M8.354 2.664a.5.5 0 0 0-.708 0L2.664 7.646a.5.5 0 0 0 0 .708l4.982 4.982a.5.5 0 0 0 .708 0l4.982-4.982a.5.5 0 0 0 0-.708zm-1.768-1.06a2 2 0 0 1 2.828 0l4.982 4.982a2 2 0 0 1 0 2.828l-4.982 4.982a2 2 0 0 1-2.828 0L1.604 9.414a2 2 0 0 1 0-2.828z";
const DONE_D =
  "m22.115 10.055l-8.17-8.17a2.76 2.76 0 0 0-3.89 0l-8.17 8.17a2.76 2.76 0 0 0 0 3.89l8.17 8.17c.535.535 1.24.805 1.945.805s1.41-.27 1.945-.805l8.17-8.17a2.76 2.76 0 0 0 0-3.89m-10.73 5.12a1.25 1.25 0 0 1-.885.365c-.32 0-.64-.12-.885-.365l-2.27-2.27l1.06-1.06L10.5 13.94l5.47-5.47l1.06 1.06z";

// Strip line comments so paint/literal scans only see code.
function codeOf(src) {
  return src.replaceAll(/\/\/[^\n]*/g, "");
}

function block(src, start, end) {
  const s = src.indexOf(start);
  assert.ok(s !== -1, `expected block start ${start}`);
  const e = src.indexOf(end, s);
  assert.ok(e !== -1, `expected block end ${end}`);
  return src.slice(s, e);
}

test("milestone-open/milestone-done exist in the ICONS map, paths transcribed verbatim as labeled", () => {
  assert.ok(ICONS.includes('"milestone-open"'), "milestone-open is registered");
  assert.ok(ICONS.includes('"milestone-done"'), "milestone-done is registered");
  assert.ok(ICONS.includes(`d="${OPEN_D}"`), "open d path transcribed verbatim from milestone-open.svg");
  assert.ok(ICONS.includes(`d="${DONE_D}"`), "done d path transcribed verbatim from milestone-done.svg");
  // As labeled (open=16, done=24) — NOT swapped to match the issue prose.
  const openEntry = ICONS.slice(ICONS.indexOf('"milestone-open"'), ICONS.indexOf('"milestone-done"'));
  const doneEntry = ICONS.slice(ICONS.indexOf('"milestone-done"'));
  assert.ok(openEntry.includes('viewBox="0 0 16 16"'), "open keeps its shipped 16-unit box");
  assert.ok(doneEntry.includes('viewBox="0 0 24 24"'), "done keeps its shipped 24-unit box");
  assert.ok(openEntry.includes('fill-rule="evenodd"') && openEntry.includes('clip-rule="evenodd"'), "open keeps its evenodd rules");
  for (const [name, entry] of [["milestone-open", openEntry], ["milestone-done", doneEntry]]) {
    assert.ok(entry.includes('fill="currentColor"'), `${name} paints via currentColor`);
    const code = codeOf(entry);
    assert.ok(!code.match(/#[0-9a-fA-F]{3,8}\b/), `${name}: no hex color literal in the entry`);
    assert.ok(!code.includes("rgb("), `${name}: no rgb() literal in the entry`);
  }
});

test("map holds fifteen complete per-entry svgs (#491), no fetch/innerHTML", () => {
  assert.equal((ICONS.match(/viewBox="0 0 /g) ?? []).length, 15, "fifteen icon entries, no more");
  assert.equal((codeOf(ICONS).match(/<svg/g) ?? []).length, 15, "one complete <svg> per entry — no shared shape for rows to fight over (#491)");
  const code = codeOf(ICONS);
  assert.ok(code.includes("currentColor"), "icons inherit text color via currentColor");
  assert.ok(!code.match(/#[0-9a-fA-F]{3,8}\b/), "no hex color literal in the icon layer");
  assert.ok(!code.includes("fetch("), "no runtime fetches — icons ship inside the bundle");
  assert.ok(!code.includes("?raw"), "no vite raw imports — the bodies are inline JSX");
  assert.ok(!code.includes("innerHTML") && !code.includes("dangerouslySetInnerHTML"), "no HTML injection — JSX only");
});

test("icons.jsx header comment reflects the new entries and consumers", () => {
  assert.ok(ICONS.includes("all twelve surfaces"), "consumer count updated to twelve");
  assert.ok(ICONS.includes("pages/Milestones.jsx"), "the milestones-page consumer is named");
  assert.ok(ICONS.includes("The 15 icons"), "entry count updated to fifteen");
  assert.ok(ICONS.includes("milestone") && ICONS.includes("#484"), "the new bodies are attributed to #484");
  assert.ok(ICONS.includes("AS LABELED"), "the prose/files viewBox discrepancy decision is noted");
  assert.ok(ICONS.includes("fifteen icon names"), "ICON_NAMES comment updated to fifteen");
});

test("Milestones.jsx: open chip carries the state-mapped icon, closed rows the done icon", () => {
  assert.ok(MILESTONES.includes('import Icon from "../lib/icons.jsx"'), "imports the shared mechanism");
  assert.ok(!MILESTONES.includes("<svg"), "no hand-rolled <svg> at the call sites");
  const chip = block(MILESTONES, '<span class="chip', "</span>");
  assert.ok(chip.includes('<Icon name={m.state === "closed" ? "milestone-done" : "milestone-open"}'), "open chip maps the icon off the payload state");
  assert.ok(chip.indexOf("<Icon") < chip.indexOf("{m.state}"), "the icon sits left of the state word");
  assert.ok(chip.includes("{m.state}"), "the state word stays (icon is decorative, not a replacement)");
  const closed = block(MILESTONES, "Closed</h3>", "</ul>");
  assert.ok(closed.includes('<Icon name="milestone-done"'), "each closed row carries the done icon");
  assert.ok(closed.indexOf('<Icon name="milestone-done"') < closed.indexOf("milestoneFilterHref"), "the icon sits left of the title link");
});

test("Issues.jsx: milestone chip icon is state-aware off the cached set, no new requests", () => {
  assert.ok(!ISSUES.includes("<svg"), "no hand-rolled <svg> at the call site");
  assert.ok(ISSUES.includes('<Icon name={icon()}'), "chip renders the derived icon through the shared mechanism");
  assert.ok(ISSUES.includes('? "milestone-done"') && ISSUES.includes(': "milestone-open"'), "closed milestones map to done, everything else to open");
  assert.ok(ISSUES.includes("getMilestoneSet()?.milestones"), "state resolves off the page-owned cached set");
  // No new requests: the page still owns exactly one milestones list window.
  assert.equal((ISSUES.match(/ctx\.repoClient\.milestones\.list\(\)/g) ?? []).length, 1, "still one milestones fetch — the icon rides the existing payload");
  assert.ok(!ISSUES.includes("milestones.get(") && !ISSUES.includes("milestones.read("), "no per-row milestone fetch added");
  // Truncation safety (#334): the max-w cap stays on the chip, the title
  // span is the truncating element, the shrink-0 icon never clips.
  assert.ok(ISSUES.includes("chip max-w-40 truncate"), "the chip keeps its truncation cap");
  assert.ok(ISSUES.includes('<span class="truncate">{d().text}</span>'), "the title text stays the truncating element");
  const chip = block(ISSUES, '<Icon name={icon()}', "{d().text}</span>");
  assert.ok(chip.includes("<span"), "icon precedes the title text inside the chip");
});

test("Repo.jsx: no strip icon (Forgejo #495) — TABS untouched, helpers gone", () => {
  const tabs = block(REPO, "const TABS = [", "];");
  assert.ok(!tabs.includes("milestones"), "no Milestones tab entry");
  assert.ok(!tabs.includes("<Icon"), "TABS stays data (no JSX in the array)");
  assert.equal((tabs.match(/\{ id: "/g) ?? []).length, 7, "still seven tab entries — no tab added");
  assert.ok(!REPO.includes("isMilestonesPath"), "the milestones-path helper is gone");
  assert.ok(!REPO.includes("isLabelsPath"), "the labels-path helper is gone");
  assert.ok(!REPO.includes('<Icon name="milestone'), "no milestone icon anywhere on the tab strip");
  assert.ok(!REPO.includes('<Icon name="label"'), "no label icon anywhere on the tab strip");
  assert.ok(TABS.includes('milestones: "issues"'), "milestones route still highlights the Issues tab");
  assert.ok(TABS.includes('labels: "issues"'), "labels route still highlights the Issues tab");
});

test("Issues.jsx toolbar: Milestones link leads with the milestone-open icon; Labels link with the label icon", () => {
  assert.ok(ISSUES.includes('<Icon name="milestone-open" /> Milestones'), "the Milestones button leads with the open icon");
  assert.ok(ISSUES.includes('<Icon name="label" /> Labels'), "the Labels button leads with the label icon");
  const toolbar = block(ISSUES, '<div class="ml-auto flex gap-2">', "</div>");
  assert.ok(toolbar.includes("/labels"), "Labels link href intact");
  assert.ok(toolbar.includes("/milestones"), "Milestones link href intact");
  assert.ok(toolbar.indexOf('<Icon name="label"') < toolbar.indexOf("/> Labels"), "label icon sits before the Labels text");
  assert.ok(toolbar.indexOf('<Icon name="milestone-open"') < toolbar.indexOf("/> Milestones"), "milestone icon sits before the Milestones text");
});

test("Milestones.jsx heading carries the milestone-open icon (Forgejo #495, mirroring Labels.jsx:83)", () => {
  const heading = block(MILESTONES, "<h2", "</h2>");
  assert.ok(heading.includes('<Icon name="milestone-open"'), "the heading carries the open icon");
  assert.ok(heading.includes("Milestones"), "the heading text is unchanged");
  assert.ok(heading.indexOf("<Icon") < heading.indexOf("Milestones"), "the icon sits beside the heading text");
  assert.ok(heading.includes("flex items-center gap-2"), "the icon composes into the heading flow (inline row with gap, not its own row)");
});

test("#319 badge + #274 scroll intact, no strip icon", () => {
  assert.ok(REPO.includes("const n = () => tabBadge(getSummary(), t.id)"), "badge numerator still derives per tab from the shared summary");
  assert.ok(REPO.includes('<span class="tab-badge"'), "the badge element is untouched");
  assert.ok(REPO.includes("tabsNav?.querySelector?.('[aria-current=\"page\"]')"), "scroll still targets the active tab");
  assert.ok(REPO.includes("el.scrollIntoView({"), "the scroll-into-view call is untouched");
  assert.ok(REPO.includes("{t.label}"), "the tab label text still renders (accessible name unchanged)");
});

test("decorative contract: icons add no accessible names, add no per-icon classes", () => {
  assert.ok(ICONS.includes('aria-hidden="true"'), "every svg carries aria-hidden");
  for (const [file, src] of [["Milestones.jsx", MILESTONES], ["Issues.jsx", ISSUES], ["Repo.jsx", REPO]]) {
    assert.ok(!src.includes("<svg"), `${file} hand-rolls no <svg>`);
    for (const m of src.matchAll(/<Icon[^>]*>/g)) {
      if (m[0].includes("milestone")) {
        assert.ok(!m[0].includes("class="), `${file}: ${m[0]} adds no per-icon class (size/spacing comes from .icon + the caller's classes)`);
      }
    }
  }
});

test("no new dependencies", () => {
  const pkg = JSON.parse(srcOf("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime dependencies unchanged",
  );
});
