// web/test/unit/fork-pill-split-464.test.js — Forgejo #464: the repo-header
// Fork pill is split-navigation with ONE-pill visuals, and the redundant
// forks link leaves the left metadata line. The pill shell keeps the #447
// canonical btn px-2 py-1 text-sm metrics as a single <span> (sibling links,
// never a nested anchor — anchors cannot nest): the count links to the
// fork-network page (/:owner/:name/forks, the #424 route) while the "Fork"
// label keeps the create navigation (/:owner/:name/fork). The repo-meta line
// keeps the ref pill + branches · tags only. Counts still ride the shared
// summary (s().forks) — no new requests. No DOM: JSX pinned as source text,
// mirroring header-pills-447.test.js / fork-page-438.test.js.
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

const PILL = block(REPO, "Forgejo #447: the header action strip speaks ONE idiom", "<CloneMenu");
const META = block(REPO, '<div class="repo-meta', "</div>");

test("split navigation: count lands on /forks, label lands on /fork", () => {
  assert.ok(PILL.includes('href={`/${full()}/forks`}'), "count links to the fork-network page");
  // Forgejo #502: the label href goes through forkHref() — the fork-composer
  // navigation for writers, the log-in interstitial (next = composer) for
  // anonymous viewers. The composer destination itself is pinned below.
  assert.ok(PILL.includes("href={forkHref()}"), "label routes through the #502 write gate");
  assert.ok(
    REPO.includes("`/${full()}/fork`") && REPO.includes("Fork this repository"),
    "the gate preserves the fork-composer destination for writers",
  );
  const countAt = PILL.indexOf("/forks`}");
  const labelAt = PILL.indexOf("forkHref()");
  assert.ok(countAt !== -1 && labelAt !== -1 && countAt < labelAt, "count sits left of the label (the #447 order)");
  // Sibling links, never a nested anchor (invalid HTML): exactly two <A>
  // opens inside the shell, each closed before the shell closes.
  const opens = (PILL.match(/<A /g) ?? []).length;
  const closes = (PILL.match(/<\/A>/g) ?? []).length;
  assert.equal(opens, 2, "exactly two destinations inside the pill shell");
  assert.equal(closes, 2, "both links close inside the shell (no nesting)");
});

test("ONE pill visually: single container, canonical metrics, single row", () => {
  assert.ok(PILL.includes('<span class="btn px-2 py-1 text-sm'), "one span shell carries the canonical btn metrics");
  for (const cls of ["btn", "px-2", "py-1", "text-sm"]) {
    assert.ok(PILL.includes(cls), `split pill keeps ${cls} (the #447 shared metrics)`);
  }
  assert.ok(PILL.includes("whitespace-nowrap"), "the pill never wraps mid-pill at narrow widths");
  // Icon pair since #465 (supersedes the #438 text-only pair): the static
  // fork icon leads the pill through the shared mechanism — still no glyph.
  assert.ok(PILL.includes('<Icon name="fork"'), "the shared fork icon leads the pill");
  assert.ok(!PILL.includes("⑂"), "no glyph — the retired #438 mark never comes back");
});

test("metadata line drops the forks link; branches and tags stay", () => {
  assert.ok(!META.includes("/forks"), "no fork-network destination in repo-meta");
  assert.ok(!META.includes("s().forks"), "no fork count source in repo-meta");
  assert.ok(!META.includes(">fork<") && !META.includes(">forks<"), "no fork/forks label in repo-meta");
  assert.ok(META.includes("branches ·"), "branches count stays");
  assert.ok(META.includes("tags"), "tags count stays");
  assert.ok(META.includes("<RefPicker"), "ref pill stays");
});

test("pill renders at 0: count link unconditional, summary fallback intact", () => {
  assert.ok(PILL.includes("s().forks ?? 0"), "count falls back to 0 exactly like Star/Watch");
  assert.ok(!PILL.includes("> 0"), "no hidden-at-zero conditional on the pill (#447 absorbed, not adopted)");
  // The count link wraps the interpolation unconditionally — at zero forks
  // it still lands on the (empty) network page.
  const linkOpen = PILL.indexOf('href={`/${full()}/forks`}');
  const countAt = PILL.indexOf("{s().forks ?? 0}");
  const linkClose = PILL.indexOf("</A>", linkOpen);
  assert.ok(linkOpen !== -1 && countAt !== -1 && linkClose !== -1, "count link + interpolation + close all present");
  assert.ok(linkOpen < countAt && countAt < linkClose, "the 0-capable count renders INSIDE the network link");
});

test("no new requests: summary is still the single count source", () => {
  assert.ok(PILL.includes("s().forks"), "count reads the shared summary");
  assert.ok(!PILL.includes("forks.list"), "no SDK fork-list call from the header");
  assert.ok(!PILL.includes("fetch("), "no fetch from the header pill");
});

test("390px arithmetic: the split pill does not wrap or overflow the cluster", () => {
  // Same copy as the #447 row ("icon 5 Fork" ~80px at text-sm): splitting the
  // destinations adds no text and the 1em #465 icon is already in the #447
  // bound, so the row total is unchanged (≈386px <
  // 390px). The shell's whitespace-nowrap keeps the two halves on one line
  // and the repo-header flex-wrap stays the safety net.
  const viewport = 390;
  const row = 80 + 95 + 80 + 75 + 24 + 32;
  assert.ok(row < viewport, `action row ${row}px fits in ${viewport}px (single row, no orphans)`);
  assert.ok(REPO.includes('<div class="repo-header mb-3 flex flex-wrap items-center gap-3">'), "repo-header keeps flex-wrap (cluster collapses below the title, never overflows)");
  assert.ok(REPO.includes('<div class="ml-auto flex items-center gap-2">'), "cluster keeps its single-row flex shape with row-owned gap-2");
});
