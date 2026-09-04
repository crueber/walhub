import { test } from "node:test";
import assert from "node:assert/strict";

import { sha256hex, anchorContext, anchorContextSha, parsePatchFiles } from "../../src/lib/diff.js";

test("sha256hex matches known vectors", () => {
  assert.equal(sha256hex(""), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  assert.equal(sha256hex("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  // Multi-block input (100 bytes exercises the padding path).
  assert.equal(sha256hex("a".repeat(100)), "2816597888e4a0d3a36b82b83316ab32680eb8f00f8cd3b904d681246d285a0e");
});

test("anchorContextSha pins the Go fixed vector", () => {
  // internal/review/model.go TestDriftHashVector: DriftHash("src/main.go",
  // ["a","b"], ["c"]) — the §8 dogfood rule, same bytes both sides.
  const hunk = {
    path: "src/main.go",
    lines: [
      { t: " ", text: "a" },
      { t: " ", text: "b" },
      { t: "+", text: "NEW" },
      { t: " ", text: "c" },
    ],
  };
  assert.equal(
    anchorContextSha(hunk, { start: 2, count: 1 }),
    "89e40705caa54ab6ad1deb39b0de14005dad1361f2289531ed05af1a065f7610"
  );
});

test("anchorContext collects contiguous context only", () => {
  const hunk = {
    path: "p",
    lines: [
      { t: " ", text: "far" },
      { t: "-", text: "old" },
      { t: " ", text: "n1" },
      { t: " ", text: "n2" },
      { t: "+", text: "new" },
      { t: " ", text: "a1" },
      { t: "-", text: "gone" },
      { t: " ", text: "too-far" },
    ],
  };
  const ctx = anchorContext(hunk, { start: 4, count: 1 });
  assert.deepEqual(ctx.before, ["n1", "n2"]);
  assert.deepEqual(ctx.after, ["a1"]);
  // The -/+ run ends each run: "far" and "too-far" are not neighbors.
  assert.notEqual(anchorContextSha(hunk, { start: 4, count: 1 }), anchorContextSha(hunk, { start: 0, count: 8 }));
});

test("anchorContextSha trims trailing whitespace, keeps leading", () => {
  const mk = (pad) => ({
    path: "p",
    lines: [{ t: " ", text: `x${pad}` }, { t: "+", text: "n" }, { t: " ", text: "y" }],
  });
  assert.equal(anchorContextSha(mk("  "), { start: 1, count: 1 }), anchorContextSha(mk(""), { start: 1, count: 1 }));
  assert.notEqual(anchorContextSha(mk(""), { start: 1, count: 1 }), anchorContextSha({ path: "p", lines: [{ t: " ", text: "  x" }, { t: "+", text: "n" }, { t: " ", text: "y" }] }, { start: 1, count: 1 }));
});

test("anchorContextSha bounds each side to 3 lines", () => {
  const lines = ["1", "2", "3", "4", "5"].map((text) => ({ t: " ", text }));
  lines.push({ t: "+", text: "n" });
  const hunk = { path: "p", lines };
  const ctx = anchorContext(hunk, { start: 5, count: 1 });
  assert.deepEqual(ctx.before, ["3", "4", "5"]);
});

test("anchorContextSha works off a real parsed hunk", () => {
  const patch = [
    "diff --git a/src/main.go b/src/main.go",
    "--- a/src/main.go",
    "+++ b/src/main.go",
    "@@ -118,7 +118,7 @@ func main() {",
    " ctx1",
    " ctx2",
    "-old",
    "+new",
    " ctx3",
    " ctx4",
    "",
  ].join("\n");
  const { files } = parsePatchFiles(patch);
  const file = files[0];
  const hunk = { path: file.path, lines: file.hunks[0].lines };
  const sha = anchorContextSha(hunk, { start: 3, count: 1 });
  assert.match(sha, /^[0-9a-f]{64}$/);
  // Same anchor re-derived from the same diff is stable (view-time drift
  // check compares against the stored context_sha, never relocates).
  assert.equal(sha, anchorContextSha(hunk, { start: 3, count: 1 }));
});
