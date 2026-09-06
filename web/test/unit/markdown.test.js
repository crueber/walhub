// web/test/unit/markdown.test.js — render-md.js (D-WEB-7: marked GFM +
// DOMPurify). The marked layer (renderMarkdownHtml) is Node-importable and
// asserted here; the DOMPurify layer (renderBody) needs a DOM and is covered
// by the real-Chromium pass (task-list/table comment, dark + light, zero
// console errors) — see the issue. Config-shape tests below pin the
// allowlist so a drift fails in node even though enforcement runs in browser.
import { test } from "node:test";
import assert from "node:assert/strict";
import { renderMarkdownHtml, renderBody, PURIFY_CONFIG } from "../../src/lib/render-md.js";

test("headings h1–h6, no id attributes", () => {
  const html = renderMarkdownHtml("# a\n## b\n### c\n#### d\n##### e\n###### f");
  for (let i = 1; i <= 6; i++) assert.ok(html.includes(`<h${i}>`), `h${i} missing`);
  assert.ok(!html.includes(" id="), "no heading ids (matches CSS)");
});

test("paragraphs merge lines with <br> and split on blanks", () => {
  const html = renderMarkdownHtml("one\ntwo\n\nthree");
  assert.ok(html.includes("<p>one<br>two</p>"), "continuation <br> preserved");
  assert.ok(html.includes("<p>three</p>"), "blank-line split preserved");
});

test("fenced code keeps exact text and escapes HTML, language-* class", () => {
  const html = renderMarkdownHtml("```js\nconst a = \"<script>\";\n```");
  // Conscious mapping (D-WEB-7): marked's standard class="language-js"
  // replaces the old data-lang attribute — nothing consumed data-lang
  // (no CSS/JS selector referenced it).
  assert.ok(html.includes('<pre><code class="language-js">'), "language class");
  assert.ok(html.includes("const a = &quot;&lt;script&gt;&quot;;"), "code HTML-escaped");
  assert.ok(!html.includes("<script>"), "no raw script tag in code");
});

test("fenced code without a language", () => {
  const html = renderMarkdownHtml("```\nplain\n```");
  assert.ok(html.includes("<pre><code>plain"));
});

test("inline: code, bold, italic, links with titles", () => {
  const html = renderMarkdownHtml('`c` **b** *i* [t](/x "the title")');
  assert.ok(html.includes("<code>c</code>"));
  assert.ok(html.includes("<strong>b</strong>"));
  assert.ok(html.includes("<em>i</em>"));
  assert.ok(html.includes(`<a href="/x" title="the title">t</a>`));
});

test("images with alt and title", () => {
  const html = renderMarkdownHtml('![alt text](/img.png "pic")');
  assert.ok(html.includes(`<img src="/img.png" alt="alt text" title="pic">`));
});

test("autolinks http(s) URLs and emails", () => {
  const html = renderMarkdownHtml("go https://x.test/a now\nme at a@x.test");
  assert.ok(html.includes('<a href="https://x.test/a">https://x.test/a</a>'), "bare URL autolinked");
  assert.ok(html.includes('<a href="mailto:a@x.test">a@x.test</a>'), "email autolinked");
});

test("hr", () => {
  assert.ok(renderMarkdownHtml("---").includes("<hr>"));
  assert.ok(renderMarkdownHtml("***").includes("<hr>"));
});

test("blockquote renders nested markdown", () => {
  const html = renderMarkdownHtml("> **quoted**");
  assert.ok(html.includes("<blockquote>"), "blockquote opens");
  assert.ok(html.includes("<strong>quoted</strong>"), "nested strong renders");
});

test("unordered and ordered lists", () => {
  assert.ok(renderMarkdownHtml("- a\n- b").includes("<ul>"), "ul opens");
  assert.ok(renderMarkdownHtml("- a\n- b").includes("<li>a</li>"), "ul items");
  assert.ok(renderMarkdownHtml("1. a\n2. b").includes("<ol>"), "ol opens");
  assert.ok(renderMarkdownHtml("1. a\n2. b").includes("<li>a</li>"), "ol items");
});

test("lists nest one level", () => {
  const html = renderMarkdownHtml("- a\n  - a1\n- b");
  assert.ok(html.includes("<ul>"), "nested ul present");
  assert.ok(html.includes("<li>a1</li>"), "nested item renders");
});

test("GFM table with header and body rows", () => {
  const html = renderMarkdownHtml("| a | b |\n|---|---|\n| 1 | 2 |\n| 3 |");
  assert.ok(html.includes("<th>a</th>") && html.includes("<th>b</th>"), "header row");
  assert.ok(html.includes("<td>1</td>"), "body cell");
  assert.ok(html.includes("<td>3</td>"), "ragged row kept");
  assert.ok(html.includes("<td></td>"), "missing cell → empty");
});

test("GFM strikethrough", () => {
  assert.ok(renderMarkdownHtml("~~gone~~").includes("<del>gone</del>"));
});

test("GFM task-list checkboxes are disabled", () => {
  const html = renderMarkdownHtml("- [x] done\n- [ ] todo");
  assert.ok(html.includes('type="checkbox"'), "checkbox inputs");
  assert.ok(html.includes("disabled"), "non-interactive (disabled)");
  assert.ok(html.includes("checked"), "checked item marked");
});

// --- sanitizer duty split ----------------------------------------------------
// marked passes raw HTML and dangerous URIs straight through at this layer;
// DOMPurify (renderBody, browser-only) carries 100% of the XSS duty.

test("marked layer passes raw HTML through (sanitizer duty, browser-covered)", () => {
  const html = renderMarkdownHtml("hello <script>alert(1)</script>");
  assert.ok(html.includes("<script>"), "passthrough visible at this layer");
  assert.ok(renderMarkdownHtml("[evil](javascript:alert(1))").includes("javascript:"), "uri passthrough visible");
});

test("renderBody refuses to run without a DOM (fail closed, never raw)", () => {
  assert.throws(() => renderBody("# hi"), /requires a DOM/);
});

test("PURIFY_CONFIG pins the GFM allowlist additions", () => {
  for (const t of ["del", "s", "input"]) assert.ok(PURIFY_CONFIG.ALLOWED_TAGS.includes(t), `${t} allowed`);
  for (const a of ["checked", "disabled", "type", "class"]) assert.ok(PURIFY_CONFIG.ALLOWED_ATTR.includes(a), `${a} allowed`);
});

test("PURIFY_CONFIG keeps the old gate: no script/style/iframe, no event attrs", () => {
  for (const t of ["script", "style", "iframe", "object", "embed", "noscript"]) {
    assert.ok(!PURIFY_CONFIG.ALLOWED_TAGS.includes(t), `${t} not allowed`);
    assert.ok(PURIFY_CONFIG.FORBID_CONTENTS.includes(t), `${t} content dropped`);
  }
  for (const a of ["onclick", "onerror", "target", "style"]) {
    assert.ok(!PURIFY_CONFIG.ALLOWED_ATTR.includes(a), `${a} not allowed`);
  }
});

test("PURIFY_CONFIG preserves the old tag/attribute core", () => {
  for (const t of ["p", "h1", "ul", "ol", "li", "a", "code", "pre", "em", "strong",
    "blockquote", "table", "thead", "tbody", "tr", "th", "td", "hr", "br", "img", "span"]) {
    assert.ok(PURIFY_CONFIG.ALLOWED_TAGS.includes(t), `${t} kept`);
  }
  for (const a of ["href", "src", "alt", "title"]) assert.ok(PURIFY_CONFIG.ALLOWED_ATTR.includes(a), `${a} kept`);
});
