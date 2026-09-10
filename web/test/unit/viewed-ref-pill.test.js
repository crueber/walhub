// web/test/unit/viewed-ref-pill.test.js — issue #252 regression: the header
// pill follows the viewed ref (branch/sha from the route), not always the
// default HEAD. The derivation (lib/ref-pill.js) is headless-tested with real
// assertions; the wiring (context publication per tab) is source-pinned.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import { shortRef, pillHead, pillLabel } from "../../src/lib/ref-pill.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const HEAD = { name: "refs/heads/main", sha: "17d8363cb5a1b2c3d4e5f60718293a4b5c6d7e8f" };

test("ref-in-context wins: branch name @ short sha", () => {
  const viewed = { name: "refs/heads/feat/issue-120", sha: "abc123def456789012345678901234567890abcd" };
  assert.equal(pillHead(viewed, HEAD), viewed, "context-first");
  assert.equal(pillLabel(pillHead(viewed, HEAD)), "feat/issue-120 @ abc123def4");
});

test("tag-in-context: short tag name @ peeled sha", () => {
  const viewed = { name: "refs/tags/v1.0", sha: "deadbeef00112233445566778899aabbccddeeff" };
  assert.equal(pillLabel(pillHead(viewed, HEAD)), "v1.0 @ deadbeef00");
});

test("no ref in view: default head (no #214 regression)", () => {
  assert.equal(pillHead(null, HEAD), HEAD, "null viewed falls back");
  assert.equal(pillHead(undefined, HEAD), HEAD, "undefined viewed falls back");
  assert.equal(pillHead({ name: "", sha: "" }, HEAD), HEAD, "empty viewed falls back");
  assert.equal(pillLabel(pillHead(null, HEAD)), "main @ 17d8363cb5");
});

test("sha-addressed views show the short sha honestly", () => {
  // Commit detail / checks/:sha publish {name: "", sha}; tree/blob at a raw
  // sha resolve with an empty ref. No "sha @ sha", no " @ sha".
  const sha = "abc123def456789012345678901234567890abcd";
  assert.equal(pillLabel(pillHead({ name: "", sha }, HEAD)), "abc123def4");
  assert.equal(pillLabel(pillHead({ name: sha, sha }, HEAD)), "abc123def4", "defensive: name === sha still short");
});

test("no head at all: refs placeholder", () => {
  assert.equal(pillLabel(pillHead(null, null)), "refs");
  assert.equal(pillLabel(null), "refs");
  assert.equal(pillLabel({ name: "refs/heads/main", sha: "" }), "refs");
});

test("Repo shell: viewed signal in context, pill reads it first", () => {
  const s = srcOf("../../src/pages/Repo.jsx");
  assert.match(s, /const \[getViewed, setViewed\] = createSignal\(null\)/, "viewed signal in the shell");
  assert.ok(s.includes("viewed: getViewed,"), "context exposes viewed");
  assert.ok(s.includes("setViewed,"), "context exposes setViewed");
  assert.ok(
    s.includes("head={() => pillHead(getViewed(), s().head)}"),
    "pill head is context-first with summary fallback",
  );
  assert.match(s, /const label = \(\) => pillLabel\(head\(\)\)/, "picker trigger renders the derived label");
  assert.ok(s.includes('navigate(`/${props.full}/tree/'), "RefPicker navigation unchanged");
});

for (const [file, stmt] of [
  ["../../src/pages/Tree.jsx", 'ctx.setViewed({ name: t.ref ?? "", sha: t.sha })'],
  ["../../src/pages/Blob.jsx", 'ctx.setViewed({ name: b.ref ?? "", sha: b.sha })'],
]) {
  test(`${file}: publishes the resolved ref, clears on unmount`, () => {
    const s = srcOf(file);
    assert.ok(s.includes(stmt), `${file} publishes {name, sha} from the resolved payload`);
    assert.ok(s.includes("onCleanup(() => ctx.setViewed(null))"), `${file} clears the viewed ref on unmount`);
  });
}

test("Commits publishes the resolved ref (plumbed through setViewed prop)", () => {
  const s = srcOf("../../src/pages/Commits.jsx");
  assert.ok(s.includes("props.setViewed({ name: h0.ref ?? \"\", sha: h0.sha })"), "CommitList publishes");
  assert.ok(s.includes("onCleanup(() => props.setViewed(null))"), "CommitList clears on unmount");
  assert.ok(s.includes("setViewed={ctx.setViewed}"), "Commits passes setViewed down");
});

test("Commit + CheckDetail publish the sha (empty name → short sha pill)", () => {
  const commit = srcOf("../../src/pages/Commit.jsx");
  assert.ok(commit.includes('props.setViewed({ name: "", sha: props.sha })'), "CommitDetail publishes sha");
  assert.ok(commit.includes("setViewed={ctx.setViewed}"), "Commit passes setViewed down");
  const checks = srcOf("../../src/pages/CheckDetail.jsx");
  assert.ok(checks.includes('ctx.setViewed({ name: "", sha: sha() })'), "CheckDetail publishes sha");
  assert.ok(checks.includes("onCleanup(() => ctx.setViewed(null))"), "CheckDetail clears on unmount");
});

test("non-ref tabs never publish (default-head fallback preserved)", () => {
  for (const file of [
    "../../src/pages/Issues.jsx",
    "../../src/pages/Pulls.jsx",
    "../../src/pages/Settings.jsx",
    "../../src/pages/Checks.jsx",
    "../../src/pages/Releases.jsx",
  ]) {
    assert.ok(!srcOf(file).includes("setViewed"), `${file} leaves the viewed ref alone`);
  }
});
