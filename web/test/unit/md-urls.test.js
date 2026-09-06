// web/test/unit/md-urls.test.js — issues #182/#185: relative-URL resolution
// at render time (web/src/lib/render-md.js) + the prose CSS pass (ui.css).
// #185 amends the #182 "others → raw" trade-off: images still go raw, but
// EVERY other relative link lands on an in-app view (blob; trailing-slash
// or repo-root → tree).
//
// The resolver is a pure string layer over marked's HTML, so the whole
// matrix runs headless under node --test. The DOMPurify gate itself needs a
// DOM (covered by the real-Chromium pass); what node pins is the duty split:
// dangerous schemes pass through the resolver byte-identical — never laundered
// into same-origin URLs that would bypass the sanitizer.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { renderMarkdownHtml, resolveMarkdownUrls } from "../../src/lib/render-md.js";

const require = createRequire(import.meta.url);
const readSrc = (rel) =>
  require("node:fs").readFileSync(new URL(rel, import.meta.url), "utf8");

const SUB = { owner: "o", repo: "r", ref: "main", dir: "docs" };
const ROOT = { owner: "o", repo: "r", ref: "main", dir: "" };

const hrefOf = (html) => html.match(/href="([^"]*)"/)?.[1];
const srcOf = (html) => html.match(/src="([^"]*)"/)?.[1];

// --- link matrix ---------------------------------------------------------------

test("relative .md links resolve to the in-app blob view at the same ref", () => {
  assert.equal(hrefOf(renderMarkdownHtml("[a](b.md)", SUB)), "/o/r/blob/main/docs/b.md");
  assert.equal(hrefOf(renderMarkdownHtml("[a](./b.md)", SUB)), "/o/r/blob/main/docs/b.md");
  assert.equal(hrefOf(renderMarkdownHtml("[a](sub/b.md)", SUB)), "/o/r/blob/main/docs/sub/b.md");
  assert.equal(hrefOf(renderMarkdownHtml("[a](../x.md)", SUB)), "/o/r/blob/main/x.md");
  assert.equal(hrefOf(renderMarkdownHtml("[a](b.md)", ROOT)), "/o/r/blob/main/b.md");
});

test("root-absolute links resolve against the repo root", () => {
  assert.equal(hrefOf(renderMarkdownHtml("[a](/x.md)", SUB)), "/o/r/blob/main/x.md");
});

test("dot segments normalize; .. past the root clamps (never escapes)", () => {
  assert.equal(hrefOf(renderMarkdownHtml("[a](a/./b.md)", SUB)), "/o/r/blob/main/docs/a/b.md");
  assert.equal(hrefOf(renderMarkdownHtml("[a](a//b.md)", SUB)), "/o/r/blob/main/docs/a/b.md");
  const up = hrefOf(renderMarkdownHtml("[a](../../../../etc/passwd)", SUB));
  assert.ok(!up.split("?")[0].split("#")[0].split("/").includes(".."), `no .. segment: ${up}`);
  assert.ok(up.startsWith("/o/r/blob/main/"), `clamped inside the repo: ${up}`);
});

test("non-markdown relative links resolve to the in-app blob view (issue #185)", () => {
  assert.equal(hrefOf(renderMarkdownHtml("[a](f.zip)", SUB)), "/o/r/blob/main/docs/f.zip");
  assert.equal(hrefOf(renderMarkdownHtml("[a](/abs/f.zip)", SUB)), "/o/r/blob/main/abs/f.zip");
  // .md as a directory name is not a markdown file — still the blob route
  assert.equal(hrefOf(renderMarkdownHtml("[a](x.md/y)", SUB)), "/o/r/blob/main/docs/x.md/y");
  // the issue's case: a LICENSE-style link lands on the blob view, not raw
  assert.equal(hrefOf(renderMarkdownHtml("[MIT @ Christopher Rueber](LICENSE)", ROOT)), "/o/r/blob/main/LICENSE");
});

test("trailing-slash links (and the repo root) resolve to the tree view", () => {
  assert.equal(hrefOf(renderMarkdownHtml("[a](sub/)", SUB)), "/o/r/tree/main/docs/sub");
  assert.equal(hrefOf(renderMarkdownHtml("[a](/docs/)", SUB)), "/o/r/tree/main/docs");
  assert.equal(hrefOf(renderMarkdownHtml("[a](./)", SUB)), "/o/r/tree/main/docs");
  assert.equal(hrefOf(renderMarkdownHtml("[a](/)", SUB)), "/o/r/tree/main");
  assert.equal(hrefOf(renderMarkdownHtml("[a](sub/?a=b#L10)", SUB)), "/o/r/tree/main/docs/sub?a=b#L10");
});

test("markdown extensions match case-insensitively, leaf only", () => {
  assert.equal(hrefOf(renderMarkdownHtml("[a](B.MD)", ROOT)), "/o/r/blob/main/B.MD");
  assert.equal(hrefOf(renderMarkdownHtml("[a](notes.Markdown)", ROOT)), "/o/r/blob/main/notes.Markdown");
});

test("query strings and fragments survive rewriting (verbatim on views, &raw on raw)", () => {
  assert.equal(hrefOf(renderMarkdownHtml("[a](b.md#L10)", SUB)), "/o/r/blob/main/docs/b.md#L10");
  assert.equal(hrefOf(renderMarkdownHtml("[a](f.zip?a=b#L10)", SUB)), "/o/r/blob/main/docs/f.zip?a=b#L10");
  assert.equal(hrefOf(renderMarkdownHtml("[a](f.zip#L10)", SUB)), "/o/r/blob/main/docs/f.zip#L10");
  assert.equal(srcOf(renderMarkdownHtml("![i](a.png?a=b#L10)", SUB)), "/o/r/api/blob/main/docs/a.png?a=b&raw#L10");
});

test("anchors, absolute, protocol-relative and mailto URLs are untouched", () => {
  assert.equal(hrefOf(renderMarkdownHtml("[a](#quick-start)", SUB)), "#quick-start");
  assert.equal(hrefOf(renderMarkdownHtml("[a](https://x.test/a)", SUB)), "https://x.test/a");
  assert.equal(hrefOf(renderMarkdownHtml("[a](http://x.test/a)", SUB)), "http://x.test/a");
  assert.equal(hrefOf(renderMarkdownHtml("[a](//other.test/x)", SUB)), "//other.test/x");
  assert.equal(hrefOf(renderMarkdownHtml("[a](mailto:a@x.test)", SUB)), "mailto:a@x.test");
  assert.equal(hrefOf(renderMarkdownHtml("[a](ftp://x.test/y)", SUB)), "ftp://x.test/y");
  assert.equal(hrefOf(renderMarkdownHtml("[a]()", SUB)), "");
});

// --- image matrix ----------------------------------------------------------------

test("relative images always resolve to the raw-bytes endpoint", () => {
  assert.equal(srcOf(renderMarkdownHtml("![i](shots/a.png)", ROOT)), "/o/r/api/blob/main/shots/a.png?raw");
  assert.equal(srcOf(renderMarkdownHtml("![i](../shots/a.png)", SUB)), "/o/r/api/blob/main/shots/a.png?raw");
  assert.equal(srcOf(renderMarkdownHtml("![i](/abs.png)", SUB)), "/o/r/api/blob/main/abs.png?raw");
  assert.equal(srcOf(renderMarkdownHtml("![i](a.md)", SUB)), "/o/r/api/blob/main/docs/a.md?raw");
});

test("absolute images are untouched", () => {
  assert.equal(srcOf(renderMarkdownHtml("![i](https://x.test/a.png)", SUB)), "https://x.test/a.png");
});

// --- encoding: never decode, never re-encode ---------------------------------------

test("percent-encoded chars survive verbatim (no decode → no traversal)", () => {
  assert.equal(hrefOf(renderMarkdownHtml("[x](a%20b.md)", ROOT)), "/o/r/blob/main/a%20b.md");
  const tricky = hrefOf(renderMarkdownHtml("[x](../%2e%2e/x.md)", ROOT));
  assert.equal(tricky, "/o/r/blob/main/%2e%2e/x.md");
  assert.ok(!tricky.split("/").includes(".."), "encoded dots never become .. segments");
});

// --- raw-HTML blocks get the same treatment ------------------------------------------

test("author raw-HTML img/a (double- and single-quoted) resolve too", () => {
  const dbl = resolveMarkdownUrls('<p><img src="i.png" alt="x"><a href="d.md">t</a></p>', ROOT);
  assert.ok(dbl.includes('src="/o/r/api/blob/main/i.png?raw"'), dbl);
  assert.ok(dbl.includes('href="/o/r/blob/main/d.md"'), dbl);
  const sng = resolveMarkdownUrls("<p><img src='i.png'><a href='d.md'>t</a></p>", ROOT);
  assert.ok(sng.includes("src='/o/r/api/blob/main/i.png?raw'"), sng);
  assert.ok(sng.includes("href='/o/r/blob/main/d.md'"), sng);
});

// --- no coordinates → no rewriting ----------------------------------------------------

test("missing ctx (or a ctx without ref) returns the HTML unchanged", () => {
  const src = "![i](a.png) [a](b.md)";
  const plain = renderMarkdownHtml(src);
  assert.equal(renderMarkdownHtml(src), plain);
  assert.equal(renderMarkdownHtml(src, undefined), plain);
  assert.equal(renderMarkdownHtml(src, { owner: "o", repo: "r", dir: "" }), plain);
  assert.equal(resolveMarkdownUrls(null, SUB), null);
});

// --- sanitizer duty split: the resolver must not launder dangerous schemes --------------

test("javascript:/data:/vbscript: pass through byte-identical for the gate to drop", () => {
  for (const evil of ["javascript:alert(1)", "JaVaScRiPt:alert(1)", "data:text/html,hi", "vbscript:msgbox"]) {
    const html = renderMarkdownHtml(`[evil](${evil})`, SUB);
    assert.ok(html.includes(`href="${evil}"`), `${evil} still visible to the sanitizer`);
    assert.ok(!html.includes("/o/r/"), `${evil} not laundered into a same-origin URL`);
  }
  const img = renderMarkdownHtml("![evil](javascript:alert(1))", SUB);
  assert.ok(!img.includes("/o/r/"), "evil image src not laundered");
});

// --- call-site wiring (source pins, same style as blob-md.test.js) -------------------------

test("Tree/Blob/Release pass file coordinates; threads forward an optional ctx", () => {
  const tree = readSrc("../../src/pages/Tree.jsx");
  assert.match(tree, /renderBody\(getDoc\(\)\?\.contents \?\? "", mdCtx\(\)\)/);
  assert.match(tree, /docRef=\{shortRef\(t\(\)\.ref\) \|\| t\(\)\.sha\}/);
  const blob = readSrc("../../src/pages/Blob.jsx");
  assert.match(blob, /ref: shortRef\(b\(\)\.ref\)/);
  assert.match(blob, /\.split\("\/"\)\.slice\(0, -1\)\.join\("\/"\)/);
  const rel = readSrc("../../src/pages/Release.jsx");
  assert.match(rel, /ref: rel\(\)\.tag, dir: ""/);
  const thread = readSrc("../../src/components/ThreadTimeline.jsx");
  assert.match(thread, /renderBody\(ev\.body \?\? "", props\.mdCtx\)/);
});

// --- prose CSS pass (source pins: every issue bullet has a rule, both themes) ------------------

test("ui.css prose rules cover the issue's element list in both themes", () => {
  const css = readSrc("../../src/ui.css");
  for (const sel of [
    ".markdown-body h1",
    ".markdown-body h4",
    ".markdown-body pre",
    ".markdown-body pre code",
    ".markdown-body th",
    ".markdown-body td",
    ".markdown-body blockquote",
    ".markdown-body hr",
    ".markdown-body img",
    ".markdown-body ul",
    ".markdown-body ol",
    'input[type="checkbox"]',
    ":has(",
    "nth-child(2n)",
  ]) {
    assert.ok(css.includes(sel), `${sel} styled`);
  }
  assert.ok(css.includes("dark:"), "dark-theme variants present");
});
