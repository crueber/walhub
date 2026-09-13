// web/test/unit/label-icon-485.test.js — Forgejo #485: the user-provided
// label/tag SVG joins the shared #465 icons.jsx mechanism and renders on
// three label surfaces: the issues-list labels filter trigger, the labels
// page heading, and (Forgejo #495) the issues-toolbar Labels link — the
// repo tab-strip option-(a) placement was wrong (it painted the icon on
// the Issues tab instead of the Labels link) and is gone; TABS, tabs.js,
// the #319 badge, and the #274 scroll stay byte-identical. No DOM: the
// icon map + JSX pinned as source text, mirroring icons-465.test.js /
// milestone-icons-484.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const ICONS = srcOf("../../src/lib/icons.jsx");
const ISSUES = srcOf("../../src/pages/Issues.jsx");
const LABELS = srcOf("../../src/pages/Labels.jsx");
const REPO = srcOf("../../src/pages/Repo.jsx");
const TABS = srcOf("../../src/lib/tabs.js");

// Verbatim transcription of the issue-provided SVG path
// (/tmp/svg-icons/label.svg: 1em, currentColor, viewBox 0 0 24 24).
const LABEL_D =
  "m19.293 9.951l-2.333-2.8c-.353-.423-.53-.635-.746-.787a2 2 0 0 0-.632-.295C15.327 6 15.052 6 14.502 6H7.2c-1.12 0-1.68 0-2.108.218a2 2 0 0 0-.874.874C4 7.52 4 8.08 4 9.2v5.6c0 1.12 0 1.68.218 2.108a2 2 0 0 0 .874.874c.427.218.987.218 2.105.218H14.5c.551 0 .826 0 1.081-.069c.226-.06.44-.16.632-.296c.216-.152.393-.363.746-.786l2.333-2.8c.608-.729.91-1.093 1.027-1.5c.102-.359.102-.74 0-1.098c-.116-.407-.42-.77-1.027-1.5";

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

test("label exists in the ICONS map, path transcribed verbatim", () => {
  assert.ok(ICONS.includes("label:"), "label is registered");
  assert.ok(ICONS.includes(`d="${LABEL_D}"`), "d path transcribed verbatim from label.svg");
  const entry = ICONS.slice(ICONS.indexOf("label:"));
  assert.ok(entry.includes('viewBox="0 0 24 24"'), "label keeps its shipped 24-unit box");
  assert.ok(entry.includes('fill="none"'), "label keeps its unfilled body");
  assert.ok(entry.includes('stroke="currentColor"'), "label paints via currentColor stroke");
  assert.ok(entry.includes('stroke-linecap="round"') && entry.includes('stroke-linejoin="round"'), "label keeps its round caps/joins");
  assert.ok(entry.includes('stroke-width="2"'), "label keeps its 2-unit stroke");
  const code = codeOf(entry);
  assert.ok(!code.match(/#[0-9a-fA-F]{3,8}\b/), "no hex color literal in the entry");
  assert.ok(!code.includes("rgb("), "no rgb() literal in the entry");
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

test("icons.jsx header comment reflects the new entry and consumers", () => {
  assert.ok(ICONS.includes("all twelve surfaces"), "consumer count updated to twelve");
  assert.ok(ICONS.includes("pages/Labels.jsx"), "the labels-page consumer is named");
  assert.ok(ICONS.includes("The 15 icons"), "entry count updated to fifteen");
  assert.ok(ICONS.includes("label") && ICONS.includes("#485"), "the new entry is attributed to #485");
  assert.ok(ICONS.includes("fifteen icon names"), "ICON_NAMES comment updated to fifteen");
});

test("Issues.jsx: labels filter trigger shows the icon left of the summary, a11y + truncation intact", () => {
  assert.ok(!ISSUES.includes("<svg"), "no hand-rolled <svg> at the call site");
  const trigger = block(ISSUES, "aria-haspopup", "label-drop");
  assert.ok(trigger.includes('<Icon name="label"'), "the trigger carries the label icon through the shared mechanism");
  assert.ok(trigger.indexOf('<Icon name="label"') < trigger.indexOf("{summary()}</span>"), "the icon sits left of the summary text");
  assert.ok(trigger.includes('<span class="min-w-0 flex-1 truncate">{summary()}</span>'), "the summary span stays the truncating element");
  assert.ok(trigger.includes("▾"), "the caret stays");
  assert.ok(trigger.includes("aria-label={`Filter by labels"), "the button aria-label stays the accessible signal");
  assert.ok(trigger.includes("title={summary()}"), "the button title stays");
  // The dropdown rows are untouched: color dot + check per label, clear row.
  assert.ok(ISSUES.includes('role="menuitemcheckbox"'), "the dropdown rows keep their menuitemcheckbox roles");
  // The icon is on the closed trigger only, not the dropdown rows (and the
  // #495 toolbar link below is out of scope here — pinned by its own test).
  const menuToToolbar = ISSUES.slice(ISSUES.indexOf('role="menu"'), ISSUES.indexOf("ml-auto flex gap-2"));
  assert.ok(!menuToToolbar.includes('<Icon name="label"'), "no label icon in the dropdown rows");
});

test("Labels.jsx: page heading carries the icon composed into the heading flow", () => {
  assert.ok(LABELS.includes('import Icon from "../lib/icons.jsx"'), "imports the shared mechanism");
  assert.ok(!LABELS.includes("<svg"), "no hand-rolled <svg> at the call site");
  const heading = block(LABELS, "<h2", "</h2>");
  assert.ok(heading.includes('<Icon name="label"'), "the heading carries the label icon");
  assert.ok(heading.includes("Labels"), "the heading text is unchanged");
  assert.ok(heading.indexOf("<Icon") < heading.indexOf("Labels"), "the icon sits beside the heading text");
  assert.ok(heading.includes("flex items-center gap-2"), "the icon composes into the heading flow (inline row with gap, not its own row)");
});

test("Repo.jsx: no strip icon (Forgejo #495) — TABS untouched, helpers gone", () => {
  const tabs = block(REPO, "const TABS = [", "];");
  assert.ok(!tabs.includes("labels"), "no Labels tab entry");
  assert.ok(!tabs.includes("milestones"), "no Milestones tab entry");
  assert.ok(!tabs.includes("<Icon"), "TABS stays data (no JSX in the array)");
  assert.equal((tabs.match(/\{ id: "/g) ?? []).length, 7, "still seven tab entries — no tab added");
  assert.ok(!REPO.includes("isLabelsPath"), "the labels-path helper is gone");
  assert.ok(!REPO.includes("isMilestonesPath"), "the milestones-path helper is gone");
  assert.ok(!REPO.includes('<Icon name="label"'), "no label icon anywhere on the tab strip");
  assert.ok(!REPO.includes('<Icon name="milestone'), "no milestone icon anywhere on the tab strip");
  assert.ok(TABS.includes('labels: "issues"'), "labels route still highlights the Issues tab");
  assert.ok(TABS.includes('milestones: "issues"'), "milestones route still highlights the Issues tab");
});

test("Issues.jsx toolbar: Labels link leads with the label icon, Milestones link with milestone-open", () => {
  assert.ok(ISSUES.includes('<Icon name="label" /> Labels'), "the Labels button leads with the label icon");
  assert.ok(ISSUES.includes('<Icon name="milestone-open" /> Milestones'), "the Milestones button leads with the open icon");
  const toolbar = block(ISSUES, '<div class="ml-auto flex gap-2">', "</div>");
  assert.ok(toolbar.includes("/labels"), "Labels link href intact");
  assert.ok(toolbar.includes("/milestones"), "Milestones link href intact");
  assert.ok(toolbar.indexOf('<Icon name="label"') < toolbar.indexOf("/> Labels"), "label icon sits before the Labels text");
  assert.ok(toolbar.indexOf('<Icon name="milestone-open"') < toolbar.indexOf("/> Milestones"), "milestone icon sits before the Milestones text");
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
  for (const [file, src] of [["Issues.jsx", ISSUES], ["Labels.jsx", LABELS], ["Repo.jsx", REPO]]) {
    assert.ok(!src.includes("<svg"), `${file} hand-rolls no <svg>`);
    for (const m of src.matchAll(/<Icon[^>]*>/g)) {
      if (m[0].includes("label")) {
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
