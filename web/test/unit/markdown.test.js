// web/test/unit/markdown.test.js — markdown-lite (§2.2) + the allowlist sanitizer.
import { test } from "node:test";
import assert from "node:assert/strict";
import { renderMarkdown } from "../../src/lib/markdown.js";
import { sanitize } from "../../src/lib/sanitize.js";

test("headings h1–h6", () => {
  const html = renderMarkdown("# a\n## b\n### c\n#### d\n##### e\n###### f");
  for (let i = 1; i <= 6; i++) assert.ok(html.includes(`<h${i}>`), `h${i} missing`);
});

test("paragraphs merge lines with <br> and split on blanks", () => {
  const html = renderMarkdown("one\ntwo\n\nthree");
  assert.equal(html, "<p>one<br>two</p>\n<p>three</p>");
});

test("fenced code keeps exact text and escapes HTML", () => {
  const html = renderMarkdown("```js\nconst a = \"<script>\";\n```");
  assert.ok(html.includes("<pre><code data-lang=\"js\">"));
  assert.ok(html.includes("const a = &quot;&lt;script&gt;&quot;;"));
  assert.ok(!html.includes("<script>"));
});

test("fenced code without a language", () => {
  const html = renderMarkdown("```\nplain\n```");
  assert.ok(html.includes("<pre><code>plain"));
});

test("inline: code, bold, italic, links with titles", () => {
  const html = renderMarkdown('`c` **b** *i* [t](/x "the title")');
  assert.ok(html.includes("<code>c</code>"));
  assert.ok(html.includes("<strong>b</strong>"));
  assert.ok(html.includes("<em>i</em>"));
  assert.ok(html.includes(`<a href="/x" title="the title">t</a>`));
});

test("images with alt and title", () => {
  const html = renderMarkdown('![alt text](/img.png "pic")');
  assert.ok(html.includes(`<img src="/img.png" alt="alt text" title="pic">`));
});

test("autolinks http(s) URLs and emails", () => {
  const html = renderMarkdown("go https://x.test/a now\nme at a@x.test");
  assert.ok(html.includes('<a href="https://x.test/a">https://x.test/a</a>'));
  assert.ok(html.includes('<a href="mailto:a@x.test">a@x.test</a>'));
});

test("hr", () => {
  assert.ok(renderMarkdown("---").includes("<hr>"));
  assert.ok(renderMarkdown("***").includes("<hr>"));
});

test("blockquote renders nested markdown", () => {
  const html = renderMarkdown("> **quoted**");
  assert.ok(html.includes("<blockquote><p><strong>quoted</strong></p></blockquote>"));
});

test("unordered and ordered lists", () => {
  assert.ok(renderMarkdown("- a\n- b").includes("<ul><li>a</li><li>b</li></ul>"));
  assert.ok(renderMarkdown("1. a\n2. b").includes("<ol><li>a</li><li>b</li></ol>"));
});

test("lists nest one level", () => {
  const html = renderMarkdown("- a\n  - a1\n- b");
  assert.ok(html.includes("<li>a<ul><li>a1</li></ul></li>"));
});

test("GFM table with header and body rows", () => {
  const html = renderMarkdown("| a | b |\n|---|---|\n| 1 | 2 |\n| 3 |");
  assert.ok(html.includes("<thead><tr><th>a</th><th>b</th></tr></thead>"));
  assert.ok(html.includes("<td>1</td><td>2</td>"));
  assert.ok(html.includes("<td>3</td><td></td>")); // missing cell → empty
});

test("inline HTML in markdown is escaped by the emitter", () => {
  const html = renderMarkdown("hello <script>alert(1)</script>");
  assert.ok(!html.includes("<script>"));
  assert.ok(html.includes("&lt;script&gt;"));
});

// --- sanitizer (the innerHTML gate for the preview) ------------------------------

test("sanitize keeps allowed tags and strips attributes to the allowlist", () => {
  const out = sanitize('<p class="x" onclick="evil()">hi</p>');
  assert.equal(out, "<p>hi</p>");
});

test("sanitize allows sanctioned attributes only", () => {
  const out = sanitize('<a href="/x" title="t" target="_blank">l</a><img src="/i.png" alt="a" width="9">');
  assert.equal(out, '<a href="/x" title="t">l</a><img src="/i.png" alt="a">');
});

test("sanitize drops javascript:, data: hrefs but keeps http/https/mailto/relative/#", () => {
  assert.equal(sanitize('<a href="javascript:alert(1)">x</a>'), "<a>x</a>");
  assert.equal(sanitize('<a href="data:text/html,bad">x</a>'), "<a>x</a>");
  assert.equal(sanitize('<a href="https://ok.test/">x</a>'), '<a href="https://ok.test/">x</a>');
  assert.equal(sanitize('<a href="mailto:a@b.c">x</a>'), '<a href="mailto:a@b.c">x</a>');
  assert.equal(sanitize('<a href="/rel">x</a>'), '<a href="/rel">x</a>');
  assert.equal(sanitize('<a href="#frag">x</a>'), '<a href="#frag">x</a>');
  assert.equal(sanitize('<a href="doc.md">x</a>'), '<a href="doc.md">x</a>');
});

test("sanitize drops script/style with their content, keeps text of unknown tags", () => {
  assert.equal(sanitize("<p>a<script>evil()</script>b</p>"), "<p>ab</p>");
  assert.equal(sanitize("<p>a<style>.x{}</style>b</p>"), "<p>ab</p>");
  assert.equal(sanitize("<aside>keep me</aside>"), "keep me");
});

test("sanitize passes plain text through escaped", () => {
  assert.equal(sanitize("a < b && c"), "a &lt; b &amp;&amp; c");
});

test("sanitize handles br/hr voids and self-closing img", () => {
  assert.equal(sanitize("a<br>b<hr>c<img src=\"/x.png\"/>"), 'a<br>b<hr>c<img src="/x.png">');
});
