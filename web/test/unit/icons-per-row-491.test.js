// web/test/unit/icons-per-row-491.test.js — Forgejo #491: the #481
// shared-svg + imperative-body-insert shape mounted an EMPTY svg (no path
// child) on every issue-list row but the last. Root cause: the glyph JSX
// lived at module level (one shared DOM node), so rendering one Icon per
// <For> row moved the SAME node from row to row — only the last row kept it.
// The fix (lib/icons.jsx): each ICONS entry is a factory returning its OWN
// complete inline <svg>, and Icon invokes the factory on every render, so
// every row mints fresh nodes — no shared shape, no runtime composition.
//
// Cover is two halves, and the split is deliberate:
// (1) STRUCTURAL pins (source text): entries are per-render factories, no
// module-level shared element, Icon invokes the entry per render. THESE are
// the regression pin — they fail on the #481 shape.
// (2) SSR FUNCTIONAL pins: the real icons.jsx (compiled with the project's
// own babel-preset-solid, generate ssr) rendered through a per-row <For>
// harness via renderToString — every row's svg carries its path, all fifteen
// names render, unknown renders nothing. Honesty note: SSR stringifies, so
// it cannot exhibit the client move-not-clone hazard — it proves the fixed
// file renders end to end, not that sharing is gone. Rendered-browser
// verification stays explicitly open (shared-daemon loopback guard).
//
// Toolchain note (law 1: no new deps): @babel/core + babel-preset-solid are
// NOT new dependencies — they are the exact copies already installed inside
// the declared vite-plugin-solid devDependency closure (the same pair the
// vite build compiles this file with), resolved through that declared edge.
import { test, before } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
const path = require("node:path");
const { pathToFileURL, fileURLToPath } = require("node:url");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const ICONS = srcOf("../../src/lib/icons.jsx");

// Strip line comments so shape scans only see code.
function codeOf(src) {
  return src.replaceAll(/\/\/[^\n]*/g, "");
}

const NAMES = [
  "watch-on", "watch-off", "star-on", "star-off", "fork", "clone",
  "notify-on", "notify-off", "light-mode", "dark-mode", "plus",
  "issue-comment", "milestone-open", "milestone-done", "label",
];

test("no shared shape: fifteen complete per-entry svgs, no body map", () => {
  const code = codeOf(ICONS);
  assert.equal((code.match(/<svg/g) ?? []).length, 15, "one complete <svg> per entry, no shared outer svg");
  assert.equal((code.match(/viewBox="0 0 /g) ?? []).length, 15, "each entry carries its own shipped viewBox");
  assert.ok(!code.includes("viewBox: "), "no viewBox map entries — the box lives on the svg itself");
  assert.ok(!code.includes(".body"), "no body composition — the glyph is inline in its own svg");
  assert.ok(!code.includes("viewBox={"), "no dynamic viewBox — nothing is composed at runtime");
  // Per-entry completeness, scanned on the RAW source: codeOf strips // line
  // comments, which would also eat the http:// inside the xmlns string.
  assert.equal((ICONS.match(/xmlns="http:\/\/www\.w3\.org\/2000\/svg"/g) ?? []).length, 15, "each svg is self-contained (own xmlns)");
  const headers = [...ICONS.matchAll(/^  ("[^"]+"|[a-z-]+): \(cls\) => \($/gm)];
  assert.equal(headers.length, 15, "all fifteen entries are (cls) factories");
  for (let i = 0; i < headers.length; i++) {
    const start = headers[i].index;
    const end = i + 1 < headers.length ? headers[i + 1].index : ICONS.indexOf("};", start);
    const chunk = ICONS.slice(start, end);
    const name = headers[i][1];
    assert.ok(chunk.includes("<svg"), `${name}: owns its svg element`);
    assert.ok(chunk.includes('xmlns="http://www.w3.org/2000/svg"'), `${name}: own xmlns`);
    assert.ok(chunk.includes('width="1em"') && chunk.includes('height="1em"'), `${name}: font-relative 1em box`);
    assert.ok(chunk.includes('viewBox="0 0 '), `${name}: own shipped viewBox`);
    assert.ok(chunk.includes('aria-hidden="true"'), `${name}: stays decorative`);
    assert.ok(chunk.includes("class={cls}"), `${name}: caller class composes onto .icon`);
    assert.ok(chunk.includes("<path") || chunk.includes("<g") || chunk.includes("<circle"), `${name}: glyph child inline (never an empty svg)`);
  }
});

test("regression pin: entries are per-render factories, never shared nodes", () => {
  const code = codeOf(ICONS);
  // Each entry is an arrow-function factory taking the caller class.
  const factories = code.match(/^\s{2}("[^"]+"|[a-z-]+): \(cls\) => \($/gm) ?? [];
  assert.equal(factories.length, 15, "all fifteen entries are (cls) factories — a factory body re-executes per render, minting fresh nodes");
  assert.ok(!code.includes("body: ("), "no module-level glyph values — module-level JSX evaluates ONCE to a single shared node");
  // Icon invokes the factory on every render through the entry record.
  // (Non-keyed <Show> hands children an accessor, so the code unwraps with
  // entry() first — pin the full call shape, not just the lookup.)
  assert.ok(code.includes("ICONS[props.name]"), "Icon looks the entry up per render");
  assert.ok(code.includes("entry()(cls())"), "Icon unwraps the entry and invokes the factory per render (fresh svg per row)");
  assert.ok(code.includes("<Show when={make()}"), "unknown names stay falsy — render nothing, never a broken glyph");
  assert.ok(code.includes("`icon ${props.class}`"), "caller classes still compose onto the shared .icon utility");
});

test("paint/bundle contract unchanged: currentColor-only, no fetch/innerHTML", () => {
  const code = codeOf(ICONS);
  assert.ok(code.includes("currentColor"), "icons inherit text color via currentColor");
  assert.ok(!code.match(/#[0-9a-fA-F]{3,8}\b/), "no hex color literal in the icon layer");
  assert.ok(!code.includes("rgb("), "no rgb() literal in the icon layer");
  assert.ok(!code.includes("?raw"), "no vite raw imports — the svgs are inline JSX");
  assert.ok(!code.includes("innerHTML") && !code.includes("dangerouslySetInnerHTML"), "no HTML injection — JSX only");
  assert.ok(!code.includes("fetch("), "no runtime fetches — icons ship inside the bundle");
});

test("header keeps the #465/#466/#481 attribution and states the #491 rule", () => {
  assert.ok(ICONS.includes("all twelve surfaces"), "consumer count stays twelve");
  assert.ok(ICONS.includes("pages/Issues.jsx"), "the issue-list consumer is named");
  assert.ok(ICONS.includes("The 15 icons"), "entry count stays fifteen");
  assert.ok(ICONS.includes("#466") && ICONS.includes("#481"), "the plus/comment-bubble attributions survive");
  assert.ok(ICONS.includes("fifteen icon names"), "ICON_NAMES comment stays fifteen");
  assert.ok(ICONS.includes("Forgejo #491"), "the no-sharing rule is attributed to #491");
  assert.ok(ICONS.includes("fresh nodes"), "the why (fresh nodes per render) is stated, not just the what");
});

// ---- SSR functional half: the REAL file, compiled with the project's own
// solid preset, rendered through a per-row <For> harness. ----

let ssr = null;

before(async () => {
  // Resolved through the declared vite-plugin-solid devDependency edge —
  // the same compiler pair the vite build uses (see toolchain note above).
  const plugRequire = createRequire(require.resolve("vite-plugin-solid"));
  const babel = plugRequire("@babel/core");
  const presetSolid = plugRequire.resolve("babel-preset-solid");
  const opts = { presets: [[presetSolid, { generate: "ssr" }]], babelrc: false, configFile: false };

  const unitDir = fileURLToPath(new URL("./", import.meta.url));
  const tmp = fs.mkdtempSync(path.join(unitDir, ".tmp-491-"));
  try {
    const iconsSrc = fileURLToPath(new URL("../../src/lib/icons.jsx", import.meta.url));
    const iconsOut = babel.transformFileSync(iconsSrc, opts);
    fs.writeFileSync(path.join(tmp, "icons.ssr.mjs"), iconsOut.code);
    // The per-row scenario from the issue: one Icon per <For> row, plus an
    // all-names sweep. Compiled with the same pass as the file under test.
    const harnessSrc = [
      `import Icon, { ICON_NAMES } from "./icons.ssr.mjs";`,
      `import { For } from "solid-js";`,
      `export function CommentRows(props) {`,
      `  return <For each={props.rows}>{(n) => <span class="row"><Icon name="issue-comment" /> {n}</span>}</For>;`,
      `}`,
      `export function AllIcons() {`,
      `  return <For each={ICON_NAMES}>{(n) => <Icon name={n} />}</For>;`,
      `}`,
      ``,
    ].join("\n");
    const harnessOut = babel.transformSync(harnessSrc, { ...opts, filename: "harness.jsx" });
    fs.writeFileSync(path.join(tmp, "harness.ssr.mjs"), harnessOut.code);

    const icons = await import(pathToFileURL(path.join(tmp, "icons.ssr.mjs")).href);
    const harness = await import(pathToFileURL(path.join(tmp, "harness.ssr.mjs")).href);
    const { renderToString } = await import("solid-js/web");
    const { createComponent } = await import("solid-js");
    ssr = { icons, harness, renderToString, createComponent };
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});

function svgsOf(html) {
  return [...html.matchAll(/<svg[^>]*>(.*?)<\/svg>/gs)];
}

test("SSR per-row scenario: eight issue-comment rows, every svg carries its path", () => {
  const { harness, renderToString, createComponent } = ssr;
  const html = renderToString(() => createComponent(harness.CommentRows, { rows: [0, 1, 2, 3, 4, 5, 6, 7] }));
  const svgs = svgsOf(html);
  assert.equal(svgs.length, 8, "eight rows render eight svgs (any list length/order — FIRST and middle rows included, not just the last)");
  for (const [i, m] of svgs.entries()) {
    assert.ok(m[1].includes("<path"), `row ${i}: svg mounts its path child (childElementCount >= 1)`);
    assert.ok(m[0].includes('viewBox="0 0 16 16"'), `row ${i}: the 16-unit comment-bubble box`);
    assert.ok(m[0].includes('class="icon"'), `row ${i}: the shared .icon utility`);
  }
  assert.equal((html.match(/<path/g) ?? []).length, 8, "eight paths total — none shared away, none dropped");
});

test("SSR sweep: all fifteen names render a non-empty svg with the shipped viewBox", () => {
  const { icons, renderToString, createComponent } = ssr;
  assert.deepEqual(icons.ICON_NAMES, NAMES, "ICON_NAMES keeps all fifteen names in asset order");
  const boxes = {
    "watch-on": "0 0 16 16", "watch-off": "0 0 16 16", "star-on": "0 0 24 24",
    "star-off": "0 0 24 24", fork: "0 0 1200 1200", clone: "0 0 1024 1024",
    "notify-on": "0 0 1024 1024", "notify-off": "0 0 1024 1024",
    "light-mode": "0 0 1024 1024", "dark-mode": "0 0 24 24", plus: "0 0 16 16",
    "issue-comment": "0 0 16 16", "milestone-open": "0 0 16 16",
    "milestone-done": "0 0 24 24", label: "0 0 24 24",
  };
  for (const name of NAMES) {
    const html = renderToString(() => createComponent(icons.default, { name }));
    const svgs = svgsOf(html);
    assert.equal(svgs.length, 1, `${name}: renders exactly one svg`);
    assert.ok(svgs[0][1].includes("<"), `${name}: svg is non-empty (glyph child present)`);
    assert.ok(svgs[0][0].includes(`viewBox="${boxes[name]}"`), `${name}: keeps its shipped viewBox ${boxes[name]}`);
    assert.ok(svgs[0][0].includes('aria-hidden="true"'), `${name}: stays decorative`);
  }
});

test("SSR unknown name renders nothing", () => {
  const { icons, renderToString, createComponent } = ssr;
  const html = renderToString(() => createComponent(icons.default, { name: "no-such-icon" }));
  assert.ok(!html.includes("<svg"), "unknown names render nothing (never a broken glyph)");
});

test("no new dependencies", () => {
  const pkg = JSON.parse(srcOf("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "runtime dependencies unchanged",
  );
});
