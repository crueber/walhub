// web/test/unit/mentions-autolink.test.js — Forgejo #440: @user (and legacy
// @email principals) → /{principal} profile autolinks in rendered bodies.
//
// The pass lives in renderMarkdownHtml (marked → resolveMarkdownUrls →
// linkifyIssueRefs → linkifyMentions), so the whole match table runs headless
// under node --test. Table mirrors internal/identity/mentions_test.go (the
// server ParseMentions grammar); renderer divergences are pinned here too:
// @org/team never links, ALL grammar-valid tokens link with no existence
// probe (decision (a) — dead links acceptable, GitHub behavior), href is the
// lowercased principal while display keeps the author's case. The DOMPurify
// gate itself needs a DOM (real-Chromium pass); what node pins is the duty
// split: generated anchors carry a plain relative href — inside the existing
// allowlist, zero sanitizer changes. No server changes (notification fan-out
// already lands mentioned/team_mention).
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  renderMarkdownHtml,
  linkifyMentions,
  linkifyMentionText,
  PURIFY_CONFIG,
} from "../../src/lib/render-md.js";

const CTX = { owner: "o", repo: "r" };

// --- basic shapes (mirrors mentions_test.go "user") -------------------------------

test("@username links to /{principal}", () => {
  assert.ok(
    renderMarkdownHtml("ping @bob please", CTX).includes('<a href="/bob">bob</a>'),
  );
});

test("@email principal links to the lowercased /{principal}, display as written", () => {
  const html = renderMarkdownHtml("ping @Carol@Example.COM please", CTX);
  assert.ok(html.includes('<a href="/carol@example.com">Carol@Example.COM</a>'), html);
});

test("start and end of text bound a mention", () => {
  assert.ok(renderMarkdownHtml("@amy leads", CTX).includes('href="/amy"'), "start");
  assert.ok(renderMarkdownHtml("thanks @zed", CTX).includes('href="/zed"'), "end");
  assert.ok(
    renderMarkdownHtml("@amy leads, thanks @zed", CTX).includes('href="/amy"'),
    "both link",
  );
});

// --- team spellings never link -------------------------------------------------------

test("@org/team stays plain (teams live under settings/teams URLs, out of scope)", () => {
  for (const src of ["cc @Acme/Backend for review", "see @acme/backend.", "hi @UPPER/has here"]) {
    const html = renderMarkdownHtml(src, CTX);
    assert.ok(!html.includes("<a href"), `${src} stays plain: ${html}`);
    assert.ok(html.includes("@"), "token text intact");
  }
});

// --- trailing punctuation (mirrors "trailing-punct") ----------------------------------

test("trailing sentence punctuation stays outside the link", () => {
  const html = renderMarkdownHtml("hi @bob.", CTX);
  assert.ok(html.includes('@<a href="/bob">bob</a>.'), html);
  const comma = renderMarkdownHtml("(@bob), @bob,", CTX);
  assert.equal((comma.match(/<a href="\/bob">bob<\/a>/g) ?? []).length, 2, comma);
});

// --- exempt surfaces (mirrors "fence-skipped" / "inline-skipped") ----------------------

test("fenced code blocks pass through unlinked", () => {
  const html = renderMarkdownHtml("hi @amy\n```\n@zed\n```\nafter", CTX);
  assert.ok(html.includes('href="/amy"'), `outside links: ${html}`);
  assert.ok(!html.includes('href="/zed"'), `fence skipped: ${html}`);
});

test("inline code spans pass through unlinked", () => {
  const html = renderMarkdownHtml("use `@zed` not @amy", CTX);
  assert.ok(html.includes("<code>@zed</code>"), html);
  assert.ok(html.includes('href="/amy"'), "outside still links");
  assert.ok(!html.includes('href="/zed"'), "code skipped");
});

test("existing markdown links pass through unlinked", () => {
  const html = renderMarkdownHtml("[see @bob](x)", CTX);
  assert.ok(html.includes('<a href="x">see @bob</a>'), html);
  assert.ok(!html.includes('href="/bob"'), "no nested mention link");
});

test("marked-autolinked URLs are not double-processed", () => {
  const html = renderMarkdownHtml("go https://x.test/@bob now", CTX);
  assert.equal((html.match(/<a /g) ?? []).length, 1, `single anchor: ${html}`);
  assert.ok(html.includes('href="https://x.test/@bob"'), "URL href intact");
});

// --- email vs username (mirrors "email-addr-not-mention") -------------------------------

test("a bare address is never a mention (keeps its mailto link)", () => {
  const html = renderMarkdownHtml("mail me at jane@example.com", CTX);
  assert.ok(!html.includes('href="/jane'), `no profile link: ${html}`);
  assert.ok(html.includes('href="mailto:jane@example.com"'), "mailto kept");
});

test("@@ and a@b.com shapes never link", () => {
  for (const src of ["hi @@x", "a@b.com", "@bob@x stays", "x@bob"]) {
    const html = renderMarkdownHtml(src, CTX);
    assert.ok(!html.includes('href="/'), `${src} stays plain: ${html}`);
  }
});

test("invalid username shapes never link", () => {
  for (const src of ["@.bob", "@.."]) {
    const html = renderMarkdownHtml(src, CTX);
    assert.ok(!html.includes("<a href"), `${src} stays plain: ${html}`);
  }
});

// --- dead links still link (decision (a)) -------------------------------------------------

test("unresolvable handles still link (no existence probe in the renderer)", () => {
  assert.ok(
    renderMarkdownHtml("ping @nosuchuser123", CTX).includes('<a href="/nosuchuser123">nosuchuser123</a>'),
  );
});

// --- interplay with the ref pass (byte-identical refs) --------------------------------------

test("refs and mentions link side by side, neither mangled", () => {
  const html = renderMarkdownHtml("fix #3 by @bob, see PR4", CTX);
  assert.ok(html.includes('<a href="/o/r/issues/3">#3</a>'), html);
  assert.ok(html.includes('@<a href="/bob">bob</a>'), html);
  assert.ok(html.includes('<a href="/o/r/pull/4">PR4</a>'), html);
});

test("mention hrefs survive file ctx (resolver runs first, never sees them)", () => {
  const html = renderMarkdownHtml("[a](b.md) by @bob", { owner: "o", repo: "r", ref: "main", dir: "" });
  assert.ok(html.includes('href="/o/r/blob/main/b.md"'), `relative link resolves: ${html}`);
  assert.ok(html.includes('@<a href="/bob">bob</a>'), `mention intact: ${html}`);
  assert.ok(!html.includes("/o/r/blob/main/bob"), "mention href not mangled by the resolver");
});

test("mentions link with no ctx (profile hrefs need no owner/repo)", () => {
  assert.ok(renderMarkdownHtml("ping @bob").includes('@<a href="/bob">bob</a>'), "no ctx");
  assert.ok(renderMarkdownHtml("ping @bob", undefined).includes('href="/bob"'), "undefined ctx");
  assert.equal(linkifyMentions(null), null, "null passes through");
});

// --- sanitizer duty split: zero allowlist changes -----------------------------------------------

test("generated anchors need nothing the allowlist lacks", () => {
  assert.ok(PURIFY_CONFIG.ALLOWED_TAGS.includes("a"), "a allowed");
  assert.ok(PURIFY_CONFIG.ALLOWED_ATTR.includes("href"), "href allowed");
  const html = renderMarkdownHtml("ping @bob and @amy@example.com", CTX);
  for (const m of html.matchAll(/<a ([^>]*)>/g)) {
    if (m[1].startsWith("href=\"mailto:")) continue;
    assert.match(m[1], /^href="[^"]*"$/, `href-only anchor: ${m[0]}`);
  }
});

// --- linkifyMentionText: the pure plain-text core -----------------------------------------------

test("linkifyMentionText links tokens with conservative boundaries", () => {
  assert.equal(
    linkifyMentionText("ping @bob, cc @Acme/Backend."),
    'ping @<a href="/bob">bob</a>, cc @Acme/Backend.',
  );
  assert.equal(linkifyMentionText("x@bob @@bob @bob@x"), "x@bob @@bob @bob@x", "boundaries hold");
  assert.equal(linkifyMentionText(""), "", "empty");
  assert.equal(
    linkifyMentionText("@Carol@Example.COM"),
    '@<a href="/carol@example.com">Carol@Example.COM</a>',
    "email whole, href lowercase",
  );
});
