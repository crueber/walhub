// web/test/unit/header-gap-463.test.js — Forgejo #463: the repository
// header action strip (Star/Watch/Fork/Clone) uses ONE spacing mechanism —
// the row's gap-2. TasksOverlay previously returned an unconditional wrapper
// <div class="tasks-indicator relative"> whose inner <Show> guarded only the
// pill, so the idle (common) case left an empty 0-width flex item in the row
// and flex gap applied on both sides of it: Star-to-Watch rendered 2× gap-2
// while the other gaps stayed 1×. The fix renders nothing when idle (the
// <Show> sits above the wrapper div); the relative wrapper exists only with
// the pill and still anchors the absolute tasks-drop popover when tasks run.
// This extends the #447 contract (pill shape/metrics) to spacing. Client
// presentation only: Clone details-metrics unchanged, no backend change, no
// new deps. No DOM: JSX pinned as source text, mirroring
// header-pills-447.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const REPO = srcOf("../../src/pages/Repo.jsx");

function block(src, start, end) {
  const s = src.indexOf(start);
  assert.ok(s !== -1, `expected block start ${start}`);
  const e = src.indexOf(end, s);
  assert.ok(e !== -1, `expected block end ${end}`);
  return src.slice(s, e);
}

const GUARD = "<Show when={getRunning().length || getDone().length}>";
const WRAPPER = '<div class="tasks-indicator relative"';

test("idle renders nothing: the Show guards above the wrapper div", () => {
  const overlay = block(REPO, "function TasksOverlay(props)", "// --- shell ---");
  assert.ok(overlay.includes(GUARD), "TasksOverlay keeps the running/done guard");
  assert.ok(overlay.includes(WRAPPER), "the relative popover anchor still exists for the active case");
  const ret = overlay.slice(overlay.indexOf("return ("));
  // The guard opens before any element: idle contributes zero flex items.
  assert.ok(ret.indexOf(GUARD) !== -1 && ret.indexOf(GUARD) < ret.indexOf(WRAPPER),
    "the <Show> sits above the wrapper div — idle renders nothing, not an empty wrapper");
  assert.ok(!ret.slice(0, ret.indexOf(GUARD)).includes("<div"),
    "no element precedes the guard in the return (no unconditional flex item)");
  // Renders nothing, not a hidden placeholder: no out-of-flow workaround.
  assert.ok(!overlay.includes("display: contents") && !overlay.includes('"hidden"') && !overlay.includes("style={{ display",
    "idle is absence (no element), not a hidden/contents placeholder"));
});

test("gap-2 owns all spacing: single mechanism, no child adds spacing", () => {
  const cluster = block(REPO, '<div class="ml-auto flex items-center gap-2">', "</div>");
  assert.ok(cluster.includes("<StarToggle"), "StarToggle sits in the actions cluster");
  assert.ok(cluster.includes("<WatchToggle"), "WatchToggle sits in the actions cluster");
  assert.ok(cluster.includes("<TasksOverlay"), "TasksOverlay sits in the actions cluster");
  // Forgejo #502: the Fork label href goes through forkHref() (composer for
  // writers, log-in interstitial for anonymous) — same shell, same cluster.
  assert.ok(cluster.includes("href={forkHref()}"), "Fork sits in the actions cluster");
  assert.ok(cluster.includes("<CloneMenu"), "CloneMenu sits in the actions cluster");
  // No per-pair spacing: no space-x, no margin utilities on any row child
  // (the row's own ml-auto alignment is the one deliberate exception).
  const inner = cluster.slice(cluster.indexOf(">") + 1);
  for (const cls of ["space-x", "ml-", "mr-", "mx-", "ms-", "me-"]) {
    assert.ok(!inner.includes(cls),
      `no ${cls} on any row child — gap-2 is the only spacing mechanism`);
  }
  // The tasks wrapper carries positioning only (relative anchor), never margin.
  const overlay = block(REPO, "function TasksOverlay(props)", "// --- shell ---");
  assert.ok(overlay.includes(WRAPPER + " ref={root}>"),
    "tasks wrapper is exactly `tasks-indicator relative` — no spacing utility");
});

test("tasks running/done still render the pill + overlay behavior intact", () => {
  const overlay = block(REPO, "function TasksOverlay(props)", "// --- shell ---");
  // Pill: shape, busy/failed borders, dot vs pulse, kind label, +n others, pct.
  assert.ok(overlay.includes('<button type="button" class="pill cursor-pointer"'),
    "the pill button keeps its shape");
  assert.ok(overlay.includes('"!border-amber-500"') && overlay.includes('"!border-red-500"'),
    "busy-amber / failed-red borders intact");
  assert.ok(overlay.includes("animate-pulse"), "running pulse dot intact");
  assert.ok(overlay.includes("<span>{kind(head.kind)}</span>"), "kind label intact");
  assert.ok(overlay.includes("+{others}"), "+n overflow count intact");
  assert.ok(overlay.includes("pct.toFixed(0)"), "progress percent intact");
  // Popover: right-anchored drop, running + done rows, outside-click close.
  assert.ok(overlay.includes("tasks-drop card absolute right-0 z-30 mt-2 w-96"),
    "the popover keeps its right-anchored w-96 drop (anchor = the guarded wrapper)");
  assert.ok(overlay.includes("<For each={getRunning()}>") && overlay.includes("<For each={getDone()}>"),
    "running + lingered-done rows intact");
  assert.ok(overlay.includes("onDoc"), "outside-click dismiss intact");
  assert.ok(overlay.includes("LINGER_MS"), "finished-task linger intact");
  assert.ok(overlay.includes("BUSY_MS") && overlay.includes("IDLE_MS"), "busy/idle poll cadence intact");
});

test("sibling audit: no other empty flex items in the row", () => {
  // Star/Watch return their <button> straight out of <Show> — no wrapper div
  // that could linger as an empty flex item before their fetches resolve.
  for (const [name, fn, end] of [["StarToggle", "function StarToggle(props)", "function TasksOverlay"], ["WatchToggle", "function WatchToggle(props)", "function RefPicker"]]) {
    const body = block(REPO, fn, end);
    const ret = body.slice(body.indexOf("return ("));
    assert.ok(!ret.includes("<div"), `${name} renders no wrapper div (button straight out of <Show>)`);
  }
  // Fork is a single <span> pill shell with two inner links (#464 split
  // navigation) — still one flex item, no wrapper div around it.
  const fork = block(REPO, "Forgejo #447: the header action strip speaks ONE idiom", "</span>");
  assert.ok(fork.includes('<span class="btn px-2 py-1 text-sm'), "Fork is one pill shell — the flex item is the pill itself");
  const menu = block(REPO, "function CloneMenu(props)", "--- tabs ---");
  const menuRet = menu.slice(menu.indexOf("return ("));
  assert.ok(menuRet.includes('<details ref={root} class="clone-menu relative"'),
    "Clone stays a native <details> popover");
  assert.ok(!menuRet.slice(0, menuRet.indexOf("<details")).includes("<div"),
    "nothing wraps the Clone <details> — the flex item is the pill itself");
  // Clone details-metrics unchanged by the #463 fix (the #465 icon leads the
  // label inside the same shape).
  assert.ok(menu.includes('<summary class="btn cursor-pointer px-2 py-1 text-sm select-none"><Icon name="clone" /> Clone</summary>'),
    "Clone details-metrics unchanged");
});

test("390px arithmetic: removing the idle item only shrinks the row", () => {
  // The fix strictly REMOVES a flex item when idle, so the loaded row is the
  // same four pills the #447 test fits in 390px (≈386px with the #465 1em
  // icons); the transient tasks pill is covered by the parent flex-wrap
  // safety net, and the w-96 tasks popover is capped by the #278 viewport
  // bound like every panel.
  const viewport = 390;
  const row = 80 + 95 + 80 + 75 + 24 + 32;
  assert.ok(row < viewport, `idle row ${row}px still fits in ${viewport}px`);
  assert.ok(REPO.includes('<div class="repo-header mb-3 flex flex-wrap items-center gap-3">'),
    "repo-header keeps flex-wrap (transient pill collapses, never overflows)");
});
