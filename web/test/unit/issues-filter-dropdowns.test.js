// web/test/unit/issues-filter-dropdowns.test.js — Forgejo #416: the
// issues-list filter bar's Milestone and Labels fields are dropdowns fed
// from the repo caches, not free-text inputs.
//
// Milestone is a native single-select (consistency with the State field):
// options from the cached `milestones:{o}/{r}` set plus "All milestones"
// (clear) and "No milestone" (→ none). Labels is a multi-select popover in
// the LabelPicker idiom (08 §2, #107): outside-click/Esc close with focus
// restore, w-80 grid rows, selection mapping to the same comma-separated
// `labels` param. No backend change — same params, same endpoint.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const page = () => srcOf("../../src/pages/Issues.jsx");

const { parseLabelsParam, serializeLabelsParam, resolveMilestoneFilter } = await import(
  "../../src/lib/issueFilters.js"
);

// --- lib: labels param mapping -------------------------------------------

test("parseLabelsParam splits, trims, drops empties", () => {
  assert.deepEqual(parseLabelsParam("bug, ui"), ["bug", "ui"]);
  assert.deepEqual(parseLabelsParam(" bug ,,ui "), ["bug", "ui"]);
  assert.deepEqual(parseLabelsParam(""), []);
  assert.deepEqual(parseLabelsParam(undefined), []);
});

test("parseLabelsParam keeps unknown names (deep-link self-heal, never dropped)", () => {
  assert.deepEqual(parseLabelsParam("bug,deleted-label"), ["bug", "deleted-label"]);
});

test("parseLabelsParam drops exact duplicates, first spelling wins", () => {
  assert.deepEqual(parseLabelsParam("bug,bug,ui"), ["bug", "ui"]);
});

test("serializeLabelsParam joins; empty selection clears to ''", async () => {
  assert.equal(serializeLabelsParam(["bug", "ui"]), "bug,ui");
  assert.equal(serializeLabelsParam([]), "");
  assert.equal(serializeLabelsParam(undefined), "");
});

test("selection round-trips through the endpoint's comma-separated shape", async () => {
  const { toggleLabel } = await import("../../src/lib/labels.js");
  const afterAdd = toggleLabel(parseLabelsParam("bug"), "UI");
  assert.equal(serializeLabelsParam(afterAdd), "UI,bug");
  const afterRemove = toggleLabel(parseLabelsParam(serializeLabelsParam(afterAdd)), "bug");
  assert.equal(serializeLabelsParam(afterRemove), "UI");
});

// --- lib: milestone hydration --------------------------------------------

const SET = [{ id: "000001", title: "v1.1" }, { id: "000002", title: "v1.2" }];

test("resolveMilestoneFilter: unfiltered and none bind directly", () => {
  assert.deepEqual(resolveMilestoneFilter("", SET), { value: "", unknown: false, pending: false });
  assert.deepEqual(resolveMilestoneFilter(undefined, SET), { value: "", unknown: false, pending: false });
  assert.deepEqual(resolveMilestoneFilter("none", SET), { value: "none", unknown: false, pending: false });
});

test("resolveMilestoneFilter: known id binds verbatim", () => {
  assert.deepEqual(resolveMilestoneFilter("000001", SET), { value: "000001", unknown: false, pending: false });
});

test("resolveMilestoneFilter: unknown id falls back to the raw value, not a dropped filter", () => {
  assert.deepEqual(resolveMilestoneFilter("000009", SET), { value: "000009", unknown: true, pending: false });
});

test("resolveMilestoneFilter: pending while the set loads (select disables, never flashes bare ids)", () => {
  assert.deepEqual(resolveMilestoneFilter("000001", undefined), { value: "000001", unknown: false, pending: true });
  assert.deepEqual(resolveMilestoneFilter("", undefined), { value: "", unknown: false, pending: false });
  assert.deepEqual(resolveMilestoneFilter("none", undefined), { value: "none", unknown: false, pending: false });
});

// --- page: milestone single-select ----------------------------------------

test("milestone filter is a native select fed from the milestones cache", () => {
  const s = page();
  assert.ok(s.includes("<select"), "native select (State-field consistency)");
  assert.ok(s.includes("getMilestoneSet()?.milestones ?? []"), "options come from milestones:{o}/{r}");
  assert.ok(s.includes('<option value="">All milestones</option>'), "clear/unfiltered option");
  assert.ok(s.includes('<option value="none">No milestone</option>'), '"No milestone" maps to none');
  assert.ok(s.includes("{(m) => <option value={m.id}>{m.title}</option>}"), "one option per milestone");
});

test("milestone select disables while pending, hydrates unknown ids as raw values", () => {
  const s = page();
  assert.ok(s.includes("disabled={msFilter().pending}"), "disabled while the set settles — never a bare-id flash");
  assert.ok(s.includes("resolveMilestoneFilter(search.milestone,"), "deep-link hydration (?milestone=<id>/none)");
  assert.ok(s.includes("msFilter().unknown"), "unknown/deleted ids render");
  assert.ok(s.includes("<option value={msFilter().value}>{msFilter().value}</option>"), "raw-value fallback option");
  assert.ok(s.includes('setFilter("milestone", e.target.value)'), "same milestone param, same handler shape");
});

test("no free-text milestone input remains", () => {
  assert.ok(!page().includes('placeholder="milestone or none"'), "bare input replaced by the dropdown");
});

// --- page: labels multi-select --------------------------------------------

test("labels filter is a LabelPicker-idiom popover fed from the labels cache", () => {
  const s = page();
  assert.ok(s.includes("all={getLabelSet()?.labels}"), "options come from labels:{o}/{r}");
  assert.ok(s.includes("role=\"menuitemcheckbox\""), "multi-select checkbox rows");
  assert.ok(s.includes("aria-checked={on()"), "aria-checked kept");
  assert.ok(s.includes("grid w-full grid-cols-[auto_minmax(0,1fr)_minmax(0,1fr)] items-center gap-2"), "grid rows (#334)");
  assert.ok(s.includes("label-drop scroll-slim card absolute left-0 z-30 mt-1 max-h-72 w-80"), "#278-bound w-80 panel");
  assert.ok(s.includes('placeholder="labels (a,b)"') === false, "bare input replaced by the dropdown");
});

test("labels trigger summarizes the selection; pending disables", () => {
  const s = page();
  assert.ok(s.includes('aria-haspopup="menu"'), "trigger announces the menu");
  assert.ok(s.includes("aria-expanded={getOpen()"), "expanded state exposed");
  assert.ok(s.includes("All labels"), "unfiltered summary");
  assert.ok(s.includes("disabled={props.pending}"), "trigger disables while the set settles");
  assert.ok(s.includes("pending={getLabelSet() === undefined}"), "pending derives from the unsettled cache");
});

test("labels selection maps to the same comma-separated param", () => {
  const s = page();
  assert.ok(s.includes("toggleLabel(parseLabelsParam(search.labels), name)"), "toggle is case-insensitive (02 §3.1)");
  assert.ok(s.includes("serializeLabelsParam("), "selection serializes to the endpoint's comma list");
  assert.ok(s.includes('setFilter("labels",'), "same labels param rides the request");
  assert.ok(s.includes("onClear={() => setFilter(\"labels\", \"\")}"), "Clear empties the param");
});

test("labels dropdown keeps unknown selected names visible and removable", () => {
  const s = page();
  assert.ok(s.includes("unknownSelected()"), "selected-but-unknown names render as bare rows");
  assert.ok(s.includes("(no longer in this repo)"), "unknown rows say why they are bare");
});

test("labels dropdown close behavior: outside-click + Esc with focus restore, toggle never closes", () => {
  const s = page();
  assert.ok(s.includes("!root.contains(e.target)"), "outside-click close (LabelPicker pattern)");
  assert.ok(s.includes('e.key === "Escape"'), "Esc close");
  assert.ok(s.includes("trigger?.focus()"), "focus restored to the trigger");
  assert.ok(s.includes("document.removeEventListener(\"click\", onDocClick)"), "outside handler removed in onCleanup");
  assert.ok(s.includes("document.removeEventListener(\"keydown\", onDocKey)"), "key handler removed in onCleanup");
  const closes = s.match(/setOpen\(false\)/g) ?? [];
  assert.equal(closes.length, 2, "only the outside-click and Esc handlers close — row toggles stay open for multi-add");
});

// --- invariants ------------------------------------------------------------

test("filter bar order and grid untouched (#415 stays green in spirit)", () => {
  const s = page();
  const form = s.slice(s.indexOf('aria-label="issue filters"'));
  let at = -1;
  for (const t of ["\n          State", "\n          Assignee", "\n          Labels", "\n          Milestone", "Refresh"]) {
    const i = form.indexOf(t, at + 1);
    assert.ok(i > at, `${JSON.stringify(t.trim())} follows in filter-bar order`);
    at = i;
  }
  assert.ok(
    s.includes("grid grid-cols-2 gap-x-3 gap-y-2 p-3 sm:grid-cols-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_minmax(0,1fr)_minmax(0,1fr)_auto]"),
    "column template unchanged",
  );
});

test("no backend change: same list endpoint, same query params", () => {
  const s = page();
  assert.ok(s.includes("ctx.repoClient.issues.list(query())"), "same list endpoint");
  assert.ok(
    s.includes("labels: search.labels") && s.includes("assignee: search.assignee") && s.includes("milestone: search.milestone"),
    "same query params ride the request",
  );
});

test("no new dependencies: only Solid, router, and relative lib imports", () => {
  const imports = [...page().matchAll(/^import .* from "([^"]+)";/gm)].map((m) => m[1]);
  for (const src of imports) {
    assert.ok(
      src === "solid-js" || src === "@solidjs/router" || src.startsWith("../") || src.startsWith("./"),
      `unexpected import source: ${src}`,
    );
  }
});

test("phone widths: the left-anchored w-80 panel fits a 390px viewport", () => {
  // w-80 = 320px; the #278 bound caps panels at 390 - 16 = 374px. The
  // Labels cell is the left column of its grid row, so a left-anchored
  // panel spans inward (cell-left .. +320) — unlike right-0, which would
  // hang past the viewport's left edge from a mid-grid cell.
  const viewport = 390;
  const cap = viewport - 16;
  assert.ok(320 <= cap, "the bound does not bite at w-80 — no shrink needed");
  assert.ok(page().includes("absolute left-0"), "panel anchors left, into the viewport");
});
