// web/test/unit/blob-lines.test.js — issue #243: blob line selection with
// #L links. Pins the headless-testable rule (web/src/lib/blob-lines.js:
// split + hash codec + range helpers) and the Blob.jsx wiring (per-line
// table, gutter anchors, drag/hash handlers), per the settingsNav.js
// precedent. Row highlight, drag, scroll, and both themes are covered by
// the real-Chromium pass, not node --test.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import {
  splitLines,
  rangeOf,
  dragRange,
  lineHash,
  parseLineHash,
  inSelection,
  sameSelection,
} from "../../src/lib/blob-lines.js";

const require = createRequire(import.meta.url);

test("splitLines pops only the single trailing empty from a final newline", () => {
  assert.deepEqual(splitLines("a\nb\n"), ["a", "b"]);
  assert.deepEqual(splitLines("a\nb"), ["a", "b"]);
  assert.deepEqual(splitLines(""), [""]);
  assert.deepEqual(splitLines("a\n\n"), ["a", ""]);
  assert.deepEqual(splitLines(null), [""]);
  assert.deepEqual(splitLines(undefined), [""]);
});

test("rangeOf normalizes drag direction to ascending, rejects non-lines", () => {
  assert.deepEqual(rangeOf(5, 9), { start: 5, end: 9 });
  assert.deepEqual(rangeOf(9, 5), { start: 5, end: 9 });
  assert.deepEqual(rangeOf(7, 7), { start: 7, end: 7 });
  assert.deepEqual(rangeOf("12", "20"), { start: 12, end: 20 });
  for (const bad of [[0, 5], [5, 0], [-3, 5], ["a", 5], [5, NaN], [1.5, 5], [null, 5], [5, undefined], [{}, 5]]) {
    assert.equal(rangeOf(bad[0], bad[1]), null, `rejected: ${JSON.stringify(bad)}`);
  }
});

test("dragRange falls back to the focus line when there is no anchor", () => {
  assert.deepEqual(dragRange(null, 4), { start: 4, end: 4 });
  assert.deepEqual(dragRange(undefined, 4), { start: 4, end: 4 });
  assert.deepEqual(dragRange(3, 9), { start: 3, end: 9 });
  assert.deepEqual(dragRange(9, 3), { start: 3, end: 9 });
  assert.equal(dragRange(3, null), null);
});

test("lineHash formats single lines and ranges, ascending", () => {
  assert.equal(lineHash(12, 12), "#L12");
  assert.equal(lineHash(12, 20), "#L12-L20");
  assert.equal(lineHash(20, 12), "#L12-L20");
  for (const bad of [[0, 0], [0, 5], ["x", 5], [5, NaN], [null, null]]) {
    assert.equal(lineHash(bad[0], bad[1]), "", `invalid: ${JSON.stringify(bad)}`);
  }
});

test("parseLineHash reads #L<n> and #L<n>-L<m>, ignores everything else", () => {
  assert.deepEqual(parseLineHash("#L12"), { start: 12, end: 12 });
  assert.deepEqual(parseLineHash("#L12-L20"), { start: 12, end: 20 });
  assert.deepEqual(parseLineHash("#L20-L12"), { start: 12, end: 20 });
  assert.deepEqual(parseLineHash("#L007"), { start: 7, end: 7 });
  for (const bad of ["", "#", "#L", "#l12", "#12", "#L0", "#L-3", "#L12-", "#L12-L", "#L12-20",
    "#L12-L20x", "#L1.5", "#wal", "#L12#L13", undefined, null, 0, {}, []]) {
    assert.equal(parseLineHash(bad), null, `ignored: ${String(bad)}`);
  }
});

test("hash round-trips: format(parse(x)) is stable", () => {
  for (const h of ["#L1", "#L12", "#L12-L20", "#L999-L1000"]) {
    const sel = parseLineHash(h);
    assert.equal(lineHash(sel.start, sel.end), h, h);
  }
});

test("inSelection is inclusive on both ends, null-safe", () => {
  const sel = { start: 12, end: 20 };
  assert.equal(inSelection(12, sel), true);
  assert.equal(inSelection(20, sel), true);
  assert.equal(inSelection(15, sel), true);
  assert.equal(inSelection(11, sel), false);
  assert.equal(inSelection(21, sel), false);
  assert.equal(inSelection(12, null), false);
  assert.equal(inSelection(0, sel), false);
});

test("sameSelection guards hash-echo loops", () => {
  assert.equal(sameSelection(null, null), true);
  assert.equal(sameSelection({ start: 1, end: 1 }, { start: 1, end: 1 }), true);
  assert.equal(sameSelection({ start: 1, end: 2 }, { start: 1, end: 2 }), true);
  assert.equal(sameSelection({ start: 1, end: 1 }, { start: 1, end: 2 }), false);
  assert.equal(sameSelection(null, { start: 1, end: 1 }), false);
  assert.equal(sameSelection({ start: 1, end: 1 }, null), false);
});

test("Blob.jsx renders the per-line selection table, not the two-<pre> layout", () => {
  const fs = require("node:fs");
  const src = fs.readFileSync(new URL("../../src/pages/Blob.jsx", import.meta.url), "utf8");
  assert.match(src, /from "\.\.\/lib\/blob-lines\.js"/, "imports the selection model");
  assert.match(src, /<table[^>]*class="blob-table"/, "single table pairs gutter + code");
  assert.match(src, /<tr[^>]*id=\{`L\$\{n\}`\}/, "rows carry L<n> ids");
  assert.match(src, /href=\{`#L\$\{n\}`\}/, "gutter anchors link #L<n>");
  assert.match(src, /aria-label=\{`Line \$\{n\}`\}/, "gutter anchors are labelled");
  assert.match(src, /onMouseDown.*onNumMouseDown/, "mousedown starts the drag anchor");
  assert.match(src, /onMouseOver.*onNumMouseOver/, "mouseover extends the drag");
  assert.match(src, /onClick.*onNumClick/, "click/keyboard pushes the hash");
  assert.match(src, /ev\.shiftKey \? anchor/, "keyboard: plain Enter jumps, Shift+Enter extends");
  assert.match(src, /hashchange/, "back/forward + pasted URLs re-highlight");
  assert.match(src, /scrollIntoView/, "shared URLs scroll to the target");
  assert.match(src, /replaceState/, "drag frames replace, not push");
  assert.match(src, /"line-hl"/, "selection highlight class");
  assert.ok(!src.includes("blob-gutter"), "old gutter <pre> is gone");
  assert.match(src, /highlight\(line, props\.lang\)/, "tokenizer runs per line");
});

test("ui.css carries the table + highlight rules for both themes", () => {
  const fs = require("node:fs");
  const css = fs.readFileSync(new URL("../../src/ui.css", import.meta.url), "utf8");
  assert.match(css, /\.blob-table/, "table structure");
  assert.match(css, /\.blob-num/, "gutter column");
  assert.match(css, /\.blob-code code \{\s*white-space: pre;/, "code cells preserve whitespace");
  assert.match(css, /\.blob-row\.line-hl > td/, "row highlight");
  assert.match(css, /\.dark \.blob-row\.line-hl > td/, "dark-theme highlight");
});
