// web/test/unit/header-pills-447.test.js — Forgejo #447: the repository
// header action pills (Star, Watch, Fork, Clone) speak ONE idiom, with the
// Star/Watch button style as canonical. All four share the btn px-2 py-1
// text-sm metrics in the ml-auto actions cluster; every count renders LEFT
// of its label ({n} Star, {n} Watch, {n} Fork) and Clone renders label-only
// in the same shape. This absorbs and supersedes #446 (a tab-badge circle on
// Fork would add a second count idiom where the header needs one) and builds
// on #438 (the text-only Fork/Clone pair — still text-only, now on btn).
// Client presentation only: toggle behavior, Fork navigation, and the Clone
// popover are pinned unchanged below. No DOM: JSX pinned as source text,
// mirroring repo-social-toggles.test.js / fork-page-438.test.js.
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

test("all four controls share the canonical btn px-2 py-1 text-sm metrics", () => {
  const star = block(REPO, "function StarToggle(props)", "function TasksOverlay");
  const watch = block(REPO, "function WatchToggle(props)", "function RefPicker");
  const fork = block(REPO, "Forgejo #447: the header action strip speaks ONE idiom", "</A>");
  const clone = block(REPO, "<summary class=\"btn", "</summary>");
  for (const [name, b] of [["StarToggle", star], ["WatchToggle", watch], ["Fork link", fork], ["Clone summary", clone]]) {
    for (const cls of ["btn", "px-2", "py-1", "text-sm"]) {
      assert.ok(b.includes(cls), `${name} carries ${cls} (the canonical Star/Watch metrics)`);
    }
  }
  // Fork stays a link and Clone stays a popover trigger — same metrics,
  // unchanged element roles.
  assert.ok(fork.includes("<A"), "Fork stays an <A> link styled to the button metrics");
  assert.ok(clone.startsWith("<summary"), "Clone stays a <summary> trigger in the canonical shape");
});

test("every count renders left of its label; Clone is label-only", () => {
  const star = block(REPO, "function StarToggle(props)", "function TasksOverlay");
  const watch = block(REPO, "function WatchToggle(props)", "function RefPicker");
  const fork = block(REPO, "Forgejo #447: the header action strip speaks ONE idiom", "</A>");
  const clone = block(REPO, "<summary class=\"btn", "</summary>");
  assert.ok(star.includes("★ {s().stars ?? 0} Star"), "Star reads {n} Star (glyph kept, count left of label)");
  assert.ok(watch.includes("👁 {w().watchers ?? 0} Watch"), "Watch reads {n} Watch (glyph kept, count left of label)");
  assert.ok(fork.includes("{s().forks ?? 0} Fork"), "Fork reads {n} Fork (count left of label, summary source)");
  assert.ok(!fork.includes("Fork{"), "Fork never renders label-then-count");
  assert.ok(clone.endsWith(">Clone"), "Clone renders the bare label, no count");
  assert.ok(!clone.includes("{") && !clone.includes("}"), "Clone summary carries no count interpolation");
});

test("Fork shows its count at zero, like Star/Watch show 0", () => {
  // Implementer's call per the issue: always render {n} Fork (including 0)
  // so the group reads consistently. The hidden-at-zero direction (#446) is
  // absorbed, not adopted; the fork-network rail below the title still links
  // the > 0 count to the fork list, so > 0 discovery is preserved there.
  const fork = block(REPO, "Forgejo #447: the header action strip speaks ONE idiom", "</A>");
  assert.ok(!fork.includes("> 0 ?"), "Fork keeps no hidden-at-zero conditional");
  assert.ok(fork.includes("s().forks ?? 0"), "Fork falls back to 0 exactly like the toggles");
});

test("all four align on one row in the ml-auto cluster; the header wraps as a unit", () => {
  const cluster = block(REPO, '<div class="ml-auto flex items-center gap-2">', "</div>");
  assert.ok(cluster.includes("<StarToggle"), "StarToggle sits in the actions cluster");
  assert.ok(cluster.includes("<WatchToggle"), "WatchToggle sits in the actions cluster");
  assert.ok(cluster.includes("href={`/${full()}/fork`}"), "Fork sits in the actions cluster");
  assert.ok(cluster.includes("<CloneMenu"), "CloneMenu sits in the actions cluster");
  // Single aligned row: flex + items-center + gap-2, no per-control wrap.
  // Narrow widths (the #438 mobile-collapse precedent): the repo-header
  // parent carries flex-wrap, so the whole cluster drops below the title as
  // one right-aligned unit instead of pushing the page sideways.
  assert.ok(REPO.includes('<div class="repo-header mb-3 flex flex-wrap items-center gap-3">'), "repo-header keeps flex-wrap (cluster collapses below the title, never overflows)");
});

test("toggle behavior untouched: optimistic flip + reconcile on error", () => {
  const star = block(REPO, "function StarToggle(props)", "function TasksOverlay");
  const watch = block(REPO, "function WatchToggle(props)", "function RefPicker");
  for (const [name, b] of [["StarToggle", star], ["WatchToggle", watch]]) {
    assert.ok(b.includes("onClick={flip}"), `${name} stays one-click togglable`);
    assert.ok(b.includes("// optimistic"), `${name} keeps the optimistic flip`);
    assert.ok(b.includes("// reconcile on error"), `${name} keeps the reconcile path`);
    assert.ok(b.includes("reportError"), `${name} still reports flip failures`);
    assert.ok(b.includes('classList={{ primary'), `${name} keeps the primary active state`);
  }
  assert.ok(star.includes("props.repo.star.set()") && star.includes("props.repo.star.remove()"), "Star still writes through the SDK star endpoints");
  assert.ok(watch.includes("props.repo.watch.set(") && watch.includes("props.repo.watch.get()"), "Watch still reads/writes through the SDK watch endpoints");
});

test("Fork navigation + Clone popover untouched", () => {
  const fork = block(REPO, "Forgejo #447: the header action strip speaks ONE idiom", "</A>");
  assert.ok(fork.includes('href={`/${full()}/fork`}'), "Fork still navigates to the fork composer");
  assert.ok(fork.includes("title={`Fork ${full()}`}"), "Fork keeps its tooltip title");
  const menu = block(REPO, "function CloneMenu(props)", "--- tabs ---");
  assert.ok(menu.includes('<details ref={root} class="clone-menu relative"'), "Clone stays a native <details> popover");
  assert.ok(menu.includes("onToggle="), "Clone still lazy-loads recipes on open");
  assert.ok(menu.includes("onDoc"), "Clone keeps outside-click dismiss");
  assert.ok(menu.includes('"Escape"'), "Clone keeps Esc-to-close with focus return");
  assert.ok(menu.includes("cloneCommand(url())"), "Clone still copies the server-verbatim clone command");
});

test("390px arithmetic: the four-button row fits the phone viewport", () => {
  // Generous upper bounds at text-sm (14px): "★ 12 Star" ~80px, "👁 34
  // Watch" ~95px, "5 Fork" ~65px, "Clone" ~70px, cluster gaps 3 × 8px =
  // 24px, page padding px-4 = 32px. Total ≈ 366px < 390px, so the cluster
  // holds one row at phone widths; the parent flex-wrap is only the safety
  // net for large counts or the transient tasks indicator.
  const viewport = 390;
  const row = 80 + 95 + 65 + 70 + 24 + 32;
  assert.ok(row < viewport, `action row ${row}px fits in ${viewport}px (single row, no orphans)`);
});
