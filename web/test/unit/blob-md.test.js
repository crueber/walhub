// web/test/unit/blob-md.test.js — issue #27 regression: the Blob MD preview
// path (renderBody from lib/render-md.js) must resolve without throwing.
// renderBody itself needs a DOM (covered by the real-Chromium pass), so node
// pins the marked layer plus the call-site wiring.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { renderMarkdownHtml, renderBody } from "../../src/lib/render-md.js";

const require = createRequire(import.meta.url);

test("MD preview pipeline renders HTML without throwing", () => {
  const src = "# hello\n\n[evil](javascript:alert(1)) [ok](/rel)\n\n```js\nconst a = 1;\n```\n";
  let html;
  assert.doesNotThrow(() => {
    html = renderMarkdownHtml(src);
  });
  assert.ok(html.includes("<h1>hello</h1>"), "heading renders");
  assert.ok(html.includes('<a href="/rel">ok</a>'), "safe link kept");
  assert.ok(html.includes("<pre><code"), "fenced code renders");
});

test("renderBody fails closed without a DOM (never returns raw HTML)", () => {
  assert.throws(() => renderBody("# hi"), /requires a DOM/);
});

test("Blob.jsx renders preview through renderBody", () => {
  const fs = require("node:fs");
  const path = new URL("../../src/pages/Blob.jsx", import.meta.url);
  const src = fs.readFileSync(path, "utf8");
  assert.match(src, /import\s*\{\s*renderBody\s*\}\s*from\s*"\.\.\/lib\/render-md\.js"/);
  assert.match(src, /renderBody\(/);
});

test("no src file references the retired markdown-lite modules", () => {
  const fs = require("node:fs");
  const path = require("node:path");
  const root = new URL("../../src", import.meta.url);
  const files = [];
  const walk = (dir) => {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, e.name);
      if (e.isDirectory()) walk(p);
      else if (/\.(jsx?)$/.test(e.name)) files.push(p);
    }
  };
  walk(new URL(root).pathname);
  for (const f of files) {
    const src = fs.readFileSync(f, "utf8");
    assert.ok(!src.includes("lib/markdown.js"), `${f} still references markdown.js`);
    assert.ok(!src.includes("lib/sanitize.js"), `${f} still references sanitize.js`);
  }
});
