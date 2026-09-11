// web/test/unit/refs-autolink.test.js — Forgejo #340: #N → issues/N and
// PRN (any case) → pull/N autolinks in comment/issue/PR bodies.
//
// The pass lives in renderMarkdownHtml (marked → resolveMarkdownUrls →
// linkifyIssueRefs), so the whole match table runs headless under
// node --test. The DOMPurify gate itself needs a DOM (real-Chromium pass);
// what node pins is the duty split: generated anchors carry a plain relative
// href — inside the existing allowlist, zero sanitizer changes.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import {
  renderMarkdownHtml,
  linkifyIssueRefs,
  linkifyRefText,
  PURIFY_CONFIG,
} from "../../src/lib/render-md.js";

const require = createRequire(import.meta.url);
const readSrc = (rel) =>
  require("node:fs").readFileSync(new URL(rel, import.meta.url), "utf8");

const CTX = { owner: "o", repo: "r" };

// --- basic shapes ---------------------------------------------------------------

test("#N links to /{o}/{r}/issues/N, single and multi-digit", () => {
  assert.ok(
    renderMarkdownHtml("fix #3", CTX).includes('<a href="/o/r/issues/3">#3</a>'),
  );
  assert.ok(
    renderMarkdownHtml("see #123", CTX).includes('<a href="/o/r/issues/123">#123</a>'),
  );
});

test("PRN in any case links to /{o}/{r}/pull/N, original text preserved", () => {
  for (const ref of ["PR3", "pr3", "Pr3", "pR3"]) {
    const html = renderMarkdownHtml(`see ${ref}`, CTX);
    assert.ok(html.includes(`<a href="/o/r/pull/3">${ref}</a>`), `${ref} links as written`);
  }
  assert.ok(
    renderMarkdownHtml("see PR123", CTX).includes('<a href="/o/r/pull/123">PR123</a>'),
  );
});

// --- boundaries that link ----------------------------------------------------------

test("start/end of text, whitespace, and punctuation bound a ref", () => {
  assert.ok(renderMarkdownHtml("#3", CTX).includes('href="/o/r/issues/3"'), "start+end");
  assert.ok(renderMarkdownHtml("a #3 b", CTX).includes('href="/o/r/issues/3"'), "spaces");
  assert.ok(renderMarkdownHtml("(#3)", CTX).includes('href="/o/r/issues/3"'), "parens");
  assert.ok(renderMarkdownHtml("#3, next", CTX).includes('href="/o/r/issues/3"'), "comma");
  assert.ok(renderMarkdownHtml("end #3.", CTX).includes('href="/o/r/issues/3"'), "sentence dot");
  assert.ok(renderMarkdownHtml("a\n#3", CTX).includes('href="/o/r/issues/3"'), "newline");
  assert.ok(renderMarkdownHtml("see PR3!", CTX).includes('href="/o/r/pull/3"'), "bang after PR");
});

// --- boundaries that do NOT link -----------------------------------------------------

test("in-word refs do not link", () => {
  for (const src of ["x#3", "#3x", "xPR3", "PR3x", "PR3abc", "#3abc", "aPR3b"]) {
    const html = renderMarkdownHtml(src, CTX);
    assert.ok(!html.includes("<a href"), `${src} stays plain: ${html}`);
  }
});

test("# must be followed by digits only", () => {
  for (const src of ["#x3", "#-3", "# 3"]) {
    const html = renderMarkdownHtml(src, CTX);
    assert.ok(!html.includes("/issues/"), `${src} stays plain: ${html}`);
  }
});

test("#3.2 does not link (version number, not a ref)", () => {
  const html = renderMarkdownHtml("v#3.2 out", CTX);
  assert.ok(!html.includes("/issues/"), `no link: ${html}`);
  assert.ok(html.includes("#3.2"), "text intact");
});

// --- headings --------------------------------------------------------------------------

test("heading markers never match (# needs a directly following digit)", () => {
  assert.ok(!renderMarkdownHtml("# 3 days", CTX).includes("/issues/"), "# + space + 3");
  assert.ok(!renderMarkdownHtml("### c", CTX).includes("/issues/"), "### marker");
});

test("##3 links the inner #3 (# is a punctuation boundary — pinned)", () => {
  const html = renderMarkdownHtml("##3", CTX);
  assert.ok(html.includes('#<a href="/o/r/issues/3">#3</a>'), html);
});

// --- exempt surfaces ---------------------------------------------------------------------

test("existing markdown links pass through unlinked", () => {
  const html = renderMarkdownHtml("[see #3](x)", CTX);
  assert.ok(html.includes('<a href="x">see #3</a>'), html);
  assert.ok(!html.includes("/issues/"), "no nested ref link");
  const pr = renderMarkdownHtml("[PR3](x)", CTX);
  assert.ok(pr.includes('<a href="x">PR3</a>'), pr);
  assert.ok(!pr.includes("/pull/"), "no nested PR link");
});

test("inline code spans pass through unlinked", () => {
  const html = renderMarkdownHtml("`#3` and `PR3`", CTX);
  assert.ok(html.includes("<code>#3</code>"), html);
  assert.ok(html.includes("<code>PR3</code>"), html);
  assert.ok(!html.includes("/issues/") && !html.includes("/pull/"), "no links in code");
});

test("fenced code blocks pass through unlinked", () => {
  const html = renderMarkdownHtml("```\n#3 PR3\n```", CTX);
  assert.ok(html.includes("#3 PR3"), html);
  assert.ok(!html.includes("/issues/") && !html.includes("/pull/"), "no links in fence");
});

test("URLs with # fragments are not double-processed", () => {
  const html = renderMarkdownHtml("go https://x.test/a#3 now", CTX);
  assert.equal((html.match(/<a /g) ?? []).length, 1, `single anchor: ${html}`);
  assert.ok(html.includes('href="https://x.test/a#3"'), "fragment href intact");
});

test("marked-emitted entities are never corrupted", () => {
  const html = renderMarkdownHtml("it's #3", CTX);
  assert.ok(html.includes("it&#39;s"), `entity intact: ${html}`);
  assert.ok(html.includes('<a href="/o/r/issues/3">#3</a>'), "real ref still links");
});

// --- href semantics --------------------------------------------------------------------------

test("leading zeros link as written with a numeric href", () => {
  const hash = renderMarkdownHtml("#003", CTX);
  assert.ok(hash.includes('<a href="/o/r/issues/3">#003</a>'), hash);
  const pr = renderMarkdownHtml("pr007", CTX);
  assert.ok(pr.includes('<a href="/o/r/pull/7">pr007</a>'), pr);
});

test("dead refs still link (no existence check in the renderer)", () => {
  assert.ok(
    renderMarkdownHtml("#999", CTX).includes('<a href="/o/r/issues/999">#999</a>'),
  );
});

test("absurd digit runs never become exponential-notation hrefs", () => {
  const html = renderMarkdownHtml("#123456789012345678901234567890", CTX);
  assert.ok(!html.includes("e+"), `no exponent: ${html}`);
  assert.ok(html.includes("/o/r/issues/123456789012345678901234567890"), html);
});

// --- ctx gating -------------------------------------------------------------------------------

test("missing ctx (or ctx without owner/repo) leaves refs plain", () => {
  assert.ok(!renderMarkdownHtml("fix #3 PR3").includes("<a href"), "no ctx");
  assert.ok(!renderMarkdownHtml("fix #3 PR3", undefined).includes("<a href"), "undefined ctx");
  assert.ok(!renderMarkdownHtml("fix #3 PR3", { ref: "main" }).includes("<a href"), "ref-only ctx");
  assert.equal(linkifyIssueRefs(null, CTX), null, "null passes through");
});

test("thread-style ctx ({owner, repo}, no ref/dir) links refs; file ctx also resolves URLs", () => {
  const thread = renderMarkdownHtml("fix #3", { owner: "o", repo: "r" });
  assert.ok(thread.includes('<a href="/o/r/issues/3">#3</a>'), thread);
  const file = renderMarkdownHtml("[a](b.md) and #3", { owner: "o", repo: "r", ref: "main", dir: "" });
  assert.ok(file.includes('href="/o/r/blob/main/b.md"'), `relative link resolves: ${file}`);
  assert.ok(file.includes('<a href="/o/r/issues/3">#3</a>'), `ref links alongside: ${file}`);
  assert.ok(!file.includes("/o/r/blob/main/o/r/issues"), "ref href not mangled by the resolver");
});

// --- sanitizer duty split: zero allowlist changes -----------------------------------------------

test("generated anchors need nothing the allowlist lacks", () => {
  assert.ok(PURIFY_CONFIG.ALLOWED_TAGS.includes("a"), "a allowed");
  assert.ok(PURIFY_CONFIG.ALLOWED_ATTR.includes("href"), "href allowed");
  const html = renderMarkdownHtml("#3 PR3", CTX);
  for (const m of html.matchAll(/<a ([^>]*)>/g)) {
    assert.match(m[1], /^href="[^"]*"$/, `href-only anchor: ${m[0]}`);
  }
});

// --- linkifyRefText: the reusable plain-text core (diff.js follow-up reuses this) ---------------

test("linkifyRefText links plain-text refs with a caller base", () => {
  assert.equal(
    linkifyRefText("fix #3, see PR3", "/o/r"),
    'fix <a href="/o/r/issues/3">#3</a>, see <a href="/o/r/pull/3">PR3</a>',
  );
  assert.equal(linkifyRefText("x#3 #3x #3.2", "/o/r"), "x#3 #3x #3.2", "boundaries hold");
  assert.equal(linkifyRefText("", "/o/r"), "", "empty");
});

// --- call-site wiring: every thread surface carries owner/repo -------------------------------------

test("Issue/Pull pages pass owner/repo mdCtx to ThreadTimeline", () => {
  const issue = readSrc("../../src/pages/Issue.jsx");
  assert.match(issue, /mdCtx=\{\{ owner: ctx\.owner, repo: ctx\.name \}\}/);
  const pull = readSrc("../../src/pages/Pull.jsx");
  assert.match(pull, /mdCtx=\{\{ owner: ctx\.owner, repo: ctx\.name \}\}/);
  const thread = readSrc("../../src/components/ThreadTimeline.jsx");
  assert.match(thread, /renderBody\(ev\.body \?\? "", props\.mdCtx\)/);
  const issueNew = readSrc("../../src/pages/IssueNew.jsx");
  assert.match(issueNew, /renderBody\(getBody\(\), \{ owner: ctx\.owner, repo: ctx\.name \}\)/);
});
