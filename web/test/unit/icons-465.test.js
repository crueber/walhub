// web/test/unit/icons-465.test.js — Forgejo #465: the shared embedded SVG
// icon mechanism + the 10 provided icons across the six controls (Watch/Star
// toggles, Fork pill, Clone trigger, notification bell, theme toggle).
// Forgejo #466 adds the eleventh icon — the navbar create-button plus, a
// minimal inline stroke (no plus glyph shipped with #465) drawn through the
// same Icon shape — consumed by components/CreateMenu.jsx. Forgejo #481 adds
// the twelfth — the issue-list comment bubble, transcribed verbatim from the
// issue-provided file — consumed by pages/Issues.jsx. Every control
// renders a real icon through the ONE lib/icons.jsx component with the
// issue's state mapping; all emoji/unicode glyphs are gone; the #447 metrics
// and the #463 spacing contract are untouched. No DOM: JSX and
// the icon map are pinned as source text, mirroring
// header-pills-447.test.js / repo-social-toggles.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const ICONS = srcOf("../../src/lib/icons.jsx");
const REPO = srcOf("../../src/pages/Repo.jsx");
const TRAY = srcOf("../../src/components/NotificationTray.jsx");
const APP = srcOf("../../src/App.jsx");
const CSS = srcOf("../../src/ui.css");

function block(src, start, end) {
  const s = src.indexOf(start);
  assert.ok(s !== -1, `expected block start ${start}`);
  const e = src.indexOf(end, s);
  assert.ok(e !== -1, `expected block end ${end}`);
  return src.slice(s, e);
}

// Strip line comments so paint/literal scans only see code.
function codeOf(src) {
  return src.replaceAll(/\/\/[^\n]*/g, "");
}

// The five glyphs the six controls rendered before #465 (plus the retired
// #438 fork glyph, which must not come back on the pill either).
const OLD_GLYPHS = ["👁", "★", "🔔", "☀", "☾", "⑂"];

test("one shared mechanism: a single component exporting the fourteen named icons", () => {
  assert.ok(ICONS.includes("export default function Icon(props)"), "one default Icon component");
  assert.ok(ICONS.includes("export const ICON_NAMES"), "the name list is exported for consumers");
  for (const name of ["watch-on", "watch-off", "star-on", "star-off", "fork", "clone", "notify-on", "notify-off", "light-mode", "dark-mode", "plus", "issue-comment", "milestone-open", "milestone-done"]) {
    // Hyphenated names are quoted keys, single-word names are bare keys.
    assert.ok(ICONS.includes(`"${name}"`) || ICONS.includes(`\n  ${name}:`), `icon ${name} is registered`);
  }
  // Exactly fourteen entries: one viewBox per icon, one shared outer <svg>.
  assert.equal((ICONS.match(/viewBox: "/g) ?? []).length, 14, "fourteen icon entries, no more");
  assert.equal((codeOf(ICONS).match(/<svg/g) ?? []).length, 1, "a single shared <svg> renders every icon");
});

test("verbatim embedding: each icon keeps its shipped viewBox and 1em currentColor paint", () => {
  const boxes = {
    "watch-on": "0 0 16 16",
    "watch-off": "0 0 16 16",
    "star-on": "0 0 24 24",
    "star-off": "0 0 24 24",
    fork: "0 0 1200 1200",
    clone: "0 0 1024 1024",
    "notify-on": "0 0 1024 1024",
    "notify-off": "0 0 1024 1024",
    "light-mode": "0 0 1024 1024",
    "dark-mode": "0 0 24 24",
    // Forgejo #466: the create-button plus is drawn inline (no shipped
    // file), so its box is the conventional 16-unit grid.
    plus: "0 0 16 16",
    // Forgejo #481: the issue-list comment bubble keeps its shipped 16-unit box.
    "issue-comment": "0 0 16 16",
    // Forgejo #484: the milestone pair keeps its shipped boxes as labeled
    // (open 16, done 24 — the issue prose lists them swapped; files win).
    "milestone-open": "0 0 16 16",
    "milestone-done": "0 0 24 24",
  };
  for (const [name, box] of Object.entries(boxes)) {
    assert.ok(ICONS.includes(`viewBox: "${box}"`), `${name} keeps its shipped viewBox ${box} (mixed units scale through 1em)`);
  }
  assert.ok(ICONS.includes('width="1em"') && ICONS.includes('height="1em"'), "the shared svg stays 1em so every viewBox renders at the caller's font size");
  const code = codeOf(ICONS);
  assert.ok(code.includes("currentColor"), "icons inherit text color via currentColor");
  assert.ok(!code.match(/#[0-9a-fA-F]{3,8}\b/), "no hex color literal in the icon layer");
  assert.ok(!code.includes("rgb("), "no rgb() literal in the icon layer");
  assert.ok(!code.includes("?raw"), "no vite raw imports — the bodies are inline JSX");
  assert.ok(!code.includes("innerHTML") && !code.includes("dangerouslySetInnerHTML"), "no HTML injection — JSX only");
  assert.ok(!code.includes("fetch("), "no runtime fetches — icons ship inside the bundle");
});

test("decorative + sized by contract: aria-hidden on the svg, one .icon utility, no per-icon styling", () => {
  assert.ok(ICONS.includes('aria-hidden="true"'), "the shared svg carries aria-hidden (controls keep their own accessible names)");
  assert.ok(ICONS.includes("`icon ${props.class}`"), "caller classes compose onto the shared utility, never replace it");
  assert.ok(CSS.includes(".icon {"), "the one shared utility exists in ui.css");
  const rule = block(CSS, ".icon {", "}");
  assert.ok(rule.includes("shrink-0"), "icons never squash inside flex parents");
  assert.ok(rule.includes("vertical-align"), "baseline alignment pinned for non-flex contexts");
  assert.ok(!rule.includes("margin") && !rule.includes("mr-") && !rule.includes("ml-"), "the utility carries no spacing — row gaps stay with the caller (#463)");
  // Callers pass no sizing/spacing of their own: every <Icon> is name-only.
  for (const [file, src] of [["Repo.jsx", REPO], ["NotificationTray.jsx", TRAY], ["App.jsx", APP]]) {
    for (const m of src.matchAll(/<Icon[^>]*>/g)) {
      assert.ok(!m[0].includes("class="), `${file}: ${m[0]} adds no per-icon class (size/spacing comes from .icon + the control's own classes)`);
    }
  }
});

test("state mappings: toggles swap on/off, bell follows unread, theme keeps its Show, fork/clone static", () => {
  const watch = block(REPO, "function WatchToggle(props)", "function RefPicker");
  assert.ok(watch.includes('<Icon name={w().watching ? "watch-on" : "watch-off"}'), "watching swaps the eye, not watching the off eye");
  const star = block(REPO, "function StarToggle(props)", "function TasksOverlay");
  assert.ok(star.includes('<Icon name={s().viewer?.starred ? "star-on" : "star-off"}'), "starred swaps the filled star, unstarred the outline");
  const fork = block(REPO, "Forgejo #447: the header action strip speaks ONE idiom", "</span>");
  assert.ok(fork.includes('<Icon name="fork"'), "Fork pill carries the static fork icon");
  assert.ok(fork.indexOf('<Icon name="fork"') < fork.indexOf("{s().forks ?? 0}"), "the fork icon sits left of the count (icon, count, label)");
  const clone = block(REPO, "<summary class=\"btn", "</summary>");
  assert.ok(clone.includes('<Icon name="clone"'), "Clone trigger carries the static clone icon");
  assert.ok(clone.indexOf('<Icon name="clone"') < clone.indexOf("Clone</summary>") || clone.endsWith("Clone"), "the clone icon sits left of the Clone label");
  assert.ok(TRAY.includes('<Icon name={(unreadCount() ?? 0) > 0 ? "notify-on" : "notify-off"}'), "unread > 0 swaps the bell on, zero the bell off");
  assert.ok(APP.includes('<Show when={theme() === "dark"}'), "theme keeps its existing Show structure");
  assert.ok(APP.includes('<Icon name="light-mode"') && APP.includes('<Icon name="dark-mode"'), "theme swaps the sun/moon pair by name");
  assert.ok(APP.indexOf('<Icon name="dark-mode"') < APP.indexOf('<Icon name="light-mode"') || APP.includes('fallback={<Icon name="light-mode"'), "dark renders the dark-mode icon, light the light-mode icon (existing semantics preserved)");
});

test("no emoji/unicode glyph left in any of the six controls", () => {
  for (const [file, src] of [["Repo.jsx", REPO], ["NotificationTray.jsx", TRAY], ["App.jsx", APP]]) {
    for (const g of OLD_GLYPHS) {
      assert.ok(!src.includes(g), `${file} carries no ${g} (replaced by the shared Icon)`);
    }
  }
});

test("all six controls consume the one mechanism — no inline svg per page", () => {
  for (const [file, src] of [["Repo.jsx", REPO], ["NotificationTray.jsx", TRAY], ["App.jsx", APP]]) {
    assert.ok(src.includes("lib/icons.jsx"), `${file} imports the shared mechanism`);
    assert.ok(!src.includes("<svg"), `${file} hand-rolls no <svg> (icons.jsx owns the only one)`);
  }
});

test("doubled toggle state intact: icon swap pairs with primary + aria-pressed, texts unchanged", () => {
  const watch = block(REPO, "function WatchToggle(props)", "function RefPicker");
  const star = block(REPO, "function StarToggle(props)", "function TasksOverlay");
  for (const [name, b] of [["WatchToggle", watch], ["StarToggle", star]]) {
    assert.ok(b.includes("<Icon name="), `${name} swaps its icon with state`);
    assert.ok(b.includes("classList={{ primary"), `${name} keeps primary as the other half of the state signal (not color-only)`);
    assert.ok(b.includes("aria-pressed"), `${name} keeps aria-pressed`);
  }
  assert.ok(star.includes('"Unstar this repo"') && star.includes('"Star this repo"'), "star aria texts unchanged");
  assert.ok(watch.includes('"Unwatch this repo"') && watch.includes('"Watch this repo"'), "watch aria texts unchanged");
});

test("bell badge still overlays the icon; tray behavior unchanged", () => {
  assert.ok(TRAY.includes('class="absolute -right-1 -top-1 min-w-5 rounded-full bg-emerald-500'), "the unread badge keeps its absolute overlay position over the icon");
  assert.ok(TRAY.includes('aria-live="polite"'), "the badge stays the aria-live region");
  assert.ok(TRAY.includes('aria-label={unreadCount() ? `${unreadCount()} unread notifications` : "Notifications"}'), "the tray aria-label behavior is unchanged");
  assert.ok(TRAY.includes("onDoc") && TRAY.includes('"Escape"'), "outside-click + Esc behavior unchanged");
});

test("#447 metrics intact: same pills, counts still left of labels, icon first", () => {
  const star = block(REPO, "function StarToggle(props)", "function TasksOverlay");
  const watch = block(REPO, "function WatchToggle(props)", "function RefPicker");
  const fork = block(REPO, "Forgejo #447: the header action strip speaks ONE idiom", "</span>");
  const clone = block(REPO, "<summary class=\"btn", "</summary>");
  for (const [name, b] of [["StarToggle", star], ["WatchToggle", watch], ["Fork pill", fork], ["Clone summary", clone]]) {
    for (const cls of ["btn", "px-2", "py-1", "text-sm"]) {
      assert.ok(b.includes(cls), `${name} keeps ${cls} (the canonical Star/Watch metrics)`);
    }
  }
  const starIcon = star.indexOf("<Icon");
  const starCount = star.indexOf("{s().stars ?? 0}");
  assert.ok(starIcon !== -1 && starCount !== -1 && starIcon < starCount, "Star reads icon, count, label");
  const watchIcon = watch.indexOf("<Icon");
  const watchCount = watch.indexOf("{w().watchers ?? 0}");
  assert.ok(watchIcon !== -1 && watchCount !== -1 && watchIcon < watchCount, "Watch reads icon, count, label");
  assert.ok(fork.includes('<span class="btn px-2 py-1 text-sm'), "Fork stays one pill shell on the canonical metrics");
  assert.ok(fork.includes("whitespace-nowrap"), "the fork pill still never wraps mid-pill");
  assert.ok(clone.includes("px-2 py-1 text-sm"), "Clone keeps the canonical metrics");
});

test("no-overflow structure: icons are font-relative, wrap guards unchanged", () => {
  // Every icon renders a 1em box at the caller's font size — icons scale with
  // text and cannot force overflow by themselves at text-sm or default sizes.
  assert.ok(ICONS.includes('width="1em"') && ICONS.includes('height="1em"'), "all fourteen icons share the font-relative 1em box");
  assert.ok(REPO.includes('<div class="repo-header mb-3 flex flex-wrap items-center gap-3">'), "repo-header keeps flex-wrap (the cluster drops as one unit, never overflows)");
  assert.ok(REPO.includes('<div class="ml-auto flex items-center gap-2">'), "the cluster keeps row-owned gap-2 — Icon adds no spacing of its own (#463)");
});
