// web/test/unit/how-it-works.test.js — developer deep-dive page
// (/how-it-works, issue #191, R1): source pins for the route, nav entry,
// section anchors, verbatim GIF alt reuse, the zero-API-call rule, and the
// cross-links. No DOM: JSX is pinned as source text, mirroring landing.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const HOW = srcOf("../../src/pages/HowItWorks.jsx");
const DIAGRAMS = srcOf("../../src/components/HowDiagrams.jsx");
const LANDING = srcOf("../../src/pages/Landing.jsx");
const CONCEPT_ALTS = srcOf("../../src/pages/Landing.jsx");
const INDEX = srcOf("../../src/index.jsx");
const APP = srcOf("../../src/App.jsx");
const WAL = srcOf("../../src/pages/Wal.jsx");

test("route: /how-it-works renders HowItWorks next to the other static routes", () => {
  assert.ok(INDEX.includes('import HowItWorks from "./pages/HowItWorks.jsx"'), "index must import HowItWorks");
  assert.match(INDEX, /<Route path="\/how-it-works" component=\{HowItWorks\} \/>/);
  assert.match(INDEX, /<Route path="\*" component=\{Landing\} \/>/, "* fallback stays Landing");
});

test("nav carries how it works after explore; brand stays /", () => {
  assert.ok(APP.includes('href="/how-it-works"'), "nav must link /how-it-works");
  assert.ok(APP.includes(">how it works<"), "nav label is lowercase like the rest");
  assert.ok(
    APP.indexOf('href="/explore"') < APP.indexOf('href="/how-it-works"'),
    "how-it-works sorts after explore",
  );
});

test("sections: hero + TOC + idea/wal/push/read/checkpoints/boundaries anchors", () => {
  assert.ok(HOW.includes("How walhub works"), "hero H1");
  for (const id of ["idea", "wal", "push", "read", "checkpoints", "boundaries"]) {
    assert.ok(HOW.includes(`id="${id}"`), `section #${id} must exist`);
    assert.ok(HOW.includes(`href="#${id}"`), `TOC must link #${id}`);
  }
});

test("all four GIFs reused with verbatim alt text (single source of truth)", () => {
  // Alt strings live in Landing's CONCEPT_ALTS; the deep-dive imports them —
  // verbatim by construction, pinned here against drift on both sides.
  assert.ok(
    HOW.includes('import { CONCEPT_ALTS } from "./Landing.jsx"'),
    "alts must be imported from Landing, not copied",
  );
  for (const name of ["push", "bucket", "fetch", "collab"]) {
    assert.ok(HOW.includes(`name="${name}"`), `deep-dive must embed concept ${name}`);
  }
  assert.ok(CONCEPT_ALTS.includes("bucket writes them, then acknowledges"), "push alt");
  assert.ok(CONCEPT_ALTS.includes("disappears and a fresh instance connects"), "bucket alt");
  assert.ok(CONCEPT_ALTS.includes("first receives ref names, then pack"), "fetch alt");
  assert.ok(CONCEPT_ALTS.includes("write-ahead log stays git-only"), "collab alt");
  assert.ok(HOW.includes("<ConceptGif"), "reuse goes through ConceptGif (still-first)");
});

test("two inline static SVGs, no new animated assets, no new deps", () => {
  assert.ok(DIAGRAMS.includes("<svg"), "diagrams are inline SVG");
  assert.ok(DIAGRAMS.includes("CasDiagram"), "CAS commit-point diagram");
  assert.ok(DIAGRAMS.includes("CheckpointDiagram"), "checkpoint fold diagram");
  assert.ok(DIAGRAMS.includes("<details"), "each diagram carries a text fallback");
  assert.ok(!DIAGRAMS.includes(".gif"), "no new GIF assets");
  assert.ok(DIAGRAMS.includes("currentColor"), "theme-safe strokes");
  for (const token of ["from \"solid-js\"", "fetch(", "useData("]) {
    assert.ok(!DIAGRAMS.includes(token), `diagrams must not contain ${token}`);
  }
});

test("trace comments pin config keys (S3 drift pins)", () => {
  for (const key of [
    "wal.snapshot_every_entries",
    "wal.checkpoint_tail_bytes",
    "wal.checkpoint_interval",
    "wal.batch_window",
    "wal.max_batch",
    "wal.cas_max_retries",
  ]) {
    assert.ok(HOW.includes(key) || DIAGRAMS.includes(key), `trace must pin ${key}`);
  }
});

test("budgets are depths, not latency: cold refs = depth 2 (S2), push 4-if-synced (N2)", () => {
  assert.ok(HOW.includes("depth 2"), "cold refs wording is depth-2");
  assert.ok(HOW.includes("4 if already synced"), "push budget notes the synced case");
  assert.ok(HOW.includes("Budgets, not benchmarks"), "budgets framed as budgets");
  assert.ok(!HOW.toLowerCase().includes("latency"), "no latency promises");
});

test("creationToken wording: slot epoch seconds (N1)", () => {
  assert.ok(HOW.includes("slot epoch seconds"), "creationToken = slot epoch seconds");
});

test("HowItWorks makes zero API calls (static page like /)", () => {
  for (const token of ["useData(", "repos.", "fetch(", "EventSource", "XMLHttpRequest"]) {
    assert.ok(!HOW.includes(token), `HowItWorks must not contain ${token}`);
  }
  assert.ok(!HOW.includes("from \"../../sdk/"), "HowItWorks must not import the SDK");
  assert.ok(!HOW.includes("from \"../sdk/"), "HowItWorks must not import the SDK");
});

test("cross-links: Landing → deep-dive, deep-dive → / + /setup, Wal tab → #wal", () => {
  assert.ok(LANDING.includes('href="/how-it-works"'), "Landing links to the deep-dive");
  assert.ok(LANDING.includes("How it works →"), "Landing cross-link label");
  assert.ok(HOW.includes('href="/"'), "deep-dive links back to /");
  assert.ok(HOW.includes('href="/setup"'), "deep-dive links to /setup");
  assert.ok(HOW.includes('href="/explore"'), "deep-dive links to /explore");
  assert.ok(WAL.includes("/how-it-works#wal"), "Wal tab links to the deep-dive");
});
