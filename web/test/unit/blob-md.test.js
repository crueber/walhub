// web/test/unit/blob-md.test.js — issue #27 regression: the Blob MD preview path
// sanitize(renderMarkdown(x)) must resolve and return sanitized HTML without
// throwing (Blob.jsx once used sanitize without importing it).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { renderMarkdown } from "../../src/lib/markdown.js";
import { sanitize } from "../../src/lib/sanitize.js";

const require = createRequire(import.meta.url);

test("MD preview pipeline renders sanitized HTML without throwing", () => {
  const src = "# hello\n\n[evil](javascript:alert(1)) [ok](/rel)\n\n```js\nconst a = 1;\n```\n";
  let html;
  assert.doesNotThrow(() => {
    html = sanitize(renderMarkdown(src));
  });
  assert.ok(html.includes("<h1>hello</h1>"), "heading renders");
  assert.ok(!html.includes("javascript:"), "unsafe href stripped");
  assert.ok(html.includes('<a href="/rel">ok</a>'), "safe link kept");
  assert.ok(html.includes("<pre><code"), "fenced code renders");
});

test("Blob.jsx imports sanitize alongside renderMarkdown", () => {
  const fs = require("node:fs");
  const path = new URL("../../src/pages/Blob.jsx", import.meta.url);
  const src = fs.readFileSync(path, "utf8");
  assert.match(src, /import\s*\{\s*sanitize\s*\}\s*from\s*"\.\.\/lib\/sanitize\.js"/);
  assert.match(src, /sanitize\(renderMarkdown\(/);
});
