// web/test/unit/issue-comment-481.test.js — Forgejo #481: the leftover
// speech-bubble emoji on the issue-list comment-count indicator becomes the
// user-provided comment-bubble SVG through the shared #465 icons.jsx
// mechanism. No DOM: the icon map + JSX pinned as source text, mirroring
// icons-465.test.js / create-menu-466.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const path = require("node:path");
const { fileURLToPath } = require("node:url");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const ICONS = srcOf("../../src/lib/icons.jsx");
const ISSUES = srcOf("../../src/pages/Issues.jsx");
const ISSUE = srcOf("../../src/pages/Issue.jsx");

// The speech-bubble emoji the list row rendered before #481.
const SPEECH_BUBBLE = "💬";

// Verbatim transcription of the issue-provided SVG path
// (<svg viewBox="0 0 16 16"><path fill="currentColor" d="…" />).
const PROVIDED_D =
  "M3.5 2A2.5 2.5 0 0 0 1 4.5v5A2.5 2.5 0 0 0 3.5 12H4v1.942a.98.98 0 0 0 1.625.738L8.688 12H12.5A2.5 2.5 0 0 0 15 9.5v-5A2.5 2.5 0 0 0 12.5 2zM2 4.5A1.5 1.5 0 0 1 3.5 3h9A1.5 1.5 0 0 1 14 4.5v5a1.5 1.5 0 0 1-1.5 1.5H8.312L5 13.898V11H3.5A1.5 1.5 0 0 1 2 9.5zM7.5 8h5a.5.5 0 0 0 0-1h-5a.5.5 0 0 0 0 1m-2-1h-2a.5.5 0 0 0 0 1h2a.5.5 0 0 0 0-1m-2 2a.5.5 0 0 0 0 1h5a.5.5 0 0 0 0-1zm7 1a.5.5 0 0 1 0-1h2a.5.5 0 0 1 0 1z";

// Strip line comments so paint/literal scans only see code.
function codeOf(src) {
  return src.replaceAll(/\/\/[^\n]*/g, "");
}

function walk(dir, out = []) {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) walk(p, out);
    else out.push(p);
  }
  return out;
}

test("issue-comment exists in the ICONS map, path transcribed verbatim", () => {
  assert.ok(ICONS.includes('"issue-comment"'), "issue-comment is registered");
  assert.ok(ICONS.includes(`d="${PROVIDED_D}"`), "d path transcribed verbatim from the provided SVG");
  assert.ok(ICONS.includes('viewBox: "0 0 16 16"'), "shipped viewBox 0 0 16 16 preserved");
  const entry = ICONS.slice(ICONS.indexOf('"issue-comment"'));
  assert.ok(entry.includes('fill="currentColor"'), "paints via currentColor");
  const code = codeOf(entry);
  assert.ok(!code.match(/#[0-9a-fA-F]{3,8}\b/), "no hex color literal in the entry");
  assert.ok(!code.includes("rgb("), "no rgb() literal in the entry");
});

test("map grows to fourteen entries on the one shared svg, no fetch/innerHTML", () => {
  assert.equal((ICONS.match(/viewBox: "/g) ?? []).length, 14, "fourteen icon entries, no more");
  assert.equal((codeOf(ICONS).match(/<svg/g) ?? []).length, 1, "a single shared <svg> renders every icon");
  const code = codeOf(ICONS);
  assert.ok(!code.includes("fetch("), "no runtime fetches — icons ship inside the bundle");
  assert.ok(!code.includes("?raw"), "no vite raw imports — the body is inline JSX");
  assert.ok(!code.includes("innerHTML") && !code.includes("dangerouslySetInnerHTML"), "no HTML injection — JSX only");
});

test("icons.jsx header comment reflects the new entry and consumer", () => {
  assert.ok(ICONS.includes("all eleven surfaces"), "consumer count updated to eleven");
  assert.ok(ICONS.includes("pages/Issues.jsx"), "the issue-list consumer is named");
  assert.ok(ICONS.includes("The 14 icon bodies"), "body count updated to fourteen");
  assert.ok(ICONS.includes("issue-comment") && ICONS.includes("#481"), "the twelfth body is attributed to #481");
  assert.ok(ICONS.includes("fourteen icon names"), "ICON_NAMES comment updated to fourteen");
});

test("Issues.jsx renders the shared icon instead of the emoji, count after", () => {
  assert.ok(ISSUES.includes('import Icon from "../lib/icons.jsx"'), "imports the shared mechanism");
  assert.ok(ISSUES.includes('<Icon name="issue-comment"'), "comment count carries the shared icon");
  assert.ok(!ISSUES.includes(SPEECH_BUBBLE), "the emoji span is gone from the list page");
  assert.ok(!ISSUES.includes("<svg"), "no hand-rolled <svg> at the call site");
  const row = ISSUES.slice(ISSUES.indexOf('<Icon name="issue-comment"'));
  assert.ok(row.startsWith('<Icon name="issue-comment" />'), "name-only call — sizing comes from .icon + the row's own classes");
  assert.ok(row.includes("{issue.comment_count}"), "the count still renders after the icon");
  assert.ok(ISSUES.includes('aria-label={`${issue.comment_count} comments`'), "accessible count label unchanged");
  for (const m of ISSUES.matchAll(/<Icon[^>]*>/g)) {
    assert.ok(!m[0].includes("class="), `${m[0]} adds no per-icon class (size/spacing comes from .icon + the row)`);
  }
});

test("speech-bubble emoji grep-zero across web/src", () => {
  const root = fileURLToPath(new URL("../../src/", import.meta.url));
  const hits = walk(root).filter((p) => fs.readFileSync(p, "utf8").includes(SPEECH_BUBBLE));
  assert.deepEqual(hits, [], "no file under web/src still carries the emoji");
});

test("Issue.jsx thread byline reconciled: bare text kept, decision noted", () => {
  assert.ok(!ISSUE.includes(SPEECH_BUBBLE), "no emoji on the thread page either");
  assert.ok(ISSUE.includes("{t().comment_count} comments"), "byline keeps the bare mid-sentence count");
  assert.ok(ISSUE.includes("#481"), "the why-not is noted at the call site, so the two surfaces agree");
});

test("no new dependencies", () => {
  const pkg = JSON.parse(srcOf("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime dependencies unchanged",
  );
});
