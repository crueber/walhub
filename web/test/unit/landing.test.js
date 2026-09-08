// web/test/unit/landing.test.js — landing page (/) + owners move to /explore
// (issue #187): source pins for CTA hrefs, concept alt texts, the served GIF
// prefix, and the zero-API-call rule. No DOM: JSX is pinned as source text,
// the pure helpers (LANDING_CTAS/CONCEPT_ALTS/conceptSrc) would need JSX
// transform, so everything asserts on file contents.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const LANDING = srcOf("../../src/pages/Landing.jsx");
const CONCEPT = srcOf("../../src/components/ConceptGif.jsx");
const INDEX = srcOf("../../src/index.jsx");
const APP = srcOf("../../src/App.jsx");
const OWNERS = srcOf("../../src/pages/Owners.jsx");

test("routes: / is Landing, /explore is Owners, * falls back to Landing", () => {
  assert.match(INDEX, /<Route path="\/" component=\{Landing\} \/>/);
  assert.match(INDEX, /<Route path="\/explore" component=\{Owners\} \/>/);
  assert.match(INDEX, /<Route path="\*" component=\{Landing\} \/>/);
  assert.ok(!INDEX.match(/<Route path="\/" component=\{Owners\}/), "/ must not render Owners");
});

test("nav points at /explore; brand stays /", () => {
  assert.ok(APP.includes('href="/explore"'), "nav must link /explore");
  assert.ok(!APP.includes(">owners<"), "owners nav label must be gone");
  assert.ok(!APP.includes("/how-it-works"), "how-it-works must not be in the header nav (issue #195)");
  assert.ok(APP.includes('href="/"'), "brand link stays /");
});

test("landing CTAs: /explore, #quickstart, /setup", () => {
  for (const href of ['href="/explore"', 'href="#quickstart"', 'href="/setup"']) {
    assert.ok(LANDING.includes(href), `Landing must link ${href}`);
  }
  assert.ok(LANDING.includes("Browse repositories"), "primary CTA label");
  assert.ok(LANDING.includes("Push in 30 seconds"), "secondary CTA label");
});

test("concept alt texts name all four scenes", () => {
  for (const name of ["push", "bucket", "fetch", "collab"]) {
    assert.ok(LANDING.includes(`name="${name}"`), `Landing must embed concept ${name}`);
  }
  assert.ok(LANDING.includes("bucket writes them, then acknowledges"), "push alt");
  assert.ok(LANDING.includes("disappears and a fresh instance connects"), "bucket alt");
  assert.ok(LANDING.includes("first receives ref names, then pack"), "fetch alt");
  assert.ok(
    LANDING.includes("write-ahead log stays git-only"),
    "collab alt",
  );
});

test("ConceptGif serves /_ui/concepts/ (B1), still-first with reduced-motion swap", () => {
  assert.ok(CONCEPT.includes("/_ui/concepts/"), "served prefix must be /_ui/concepts/");
  assert.ok(!CONCEPT.match(/src="\/concepts\//), "must never use bare /concepts/");
  assert.ok(CONCEPT.includes("-still.gif"), "still-first source");
  assert.ok(CONCEPT.includes("prefers-reduced-motion"), "reduced-motion swap");
  assert.ok(CONCEPT.includes("<noscript>"), "noscript still");
  assert.ok(CONCEPT.includes('loading="lazy"'), "lazy below the fold");
  assert.ok(CONCEPT.includes('decoding="async"'), "async decode (N6)");
  assert.ok(CONCEPT.includes('width="640"'), "explicit dimensions");
  assert.ok(CONCEPT.includes('height="360"'), "explicit dimensions");
  assert.ok(!CONCEPT.includes('role="img"'), "no figure role (N1)");
  assert.ok(!CONCEPT.includes("aria-label"), "exactly one accessible name: img alt (N1)");
  assert.ok(CONCEPT.includes("alt={props.alt}"), "alt from call site");
});

test("Landing makes zero API calls (static front door)", () => {
  for (const token of ["useData(", "repos.", "fetch(", "EventSource", "XMLHttpRequest"]) {
    assert.ok(!LANDING.includes(token), `Landing must not contain ${token}`);
  }
  assert.ok(!LANDING.includes("from \"../../sdk/"), "Landing must not import the SDK");
});

test("Owners page moved: route comment + slimmed intro linking /", () => {
  assert.ok(OWNERS.includes('route "/explore"'), "header comment names /explore");
  assert.ok(OWNERS.includes('href="/"'), "slimmed intro links back to /");
  assert.ok(OWNERS.includes("What is walhub?"), "slimmed intro one-liner");
});
