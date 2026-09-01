// web/test/unit/diff.test.js — §2.8 hand-rolled unified-diff parser + helpers.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  parsePatchFiles, splitRows, linkifyBody, groupTrailers, trailerValue,
} from "../../src/lib/diff.js";

const MOD = `diff --git a/x.txt b/x.txt
index 1111111..2222222 100644
--- a/x.txt
+++ b/x.txt
@@ -1,3 +1,3 @@
 a
-b
+c
 d
`;

test("parse happy path: one modified file, per-file badges counted", () => {
  const { files } = parsePatchFiles(MOD, "abc");
  assert.equal(files.length, 1);
  const f = files[0];
  assert.equal(f.path, "x.txt");
  assert.equal(f.added, false);
  assert.equal(f.deleted, false);
  assert.equal(f.isBinary, false);
  assert.equal(f.hunks.length, 1);
  assert.equal(f.additions, 1);
  assert.equal(f.deletions, 1);
  assert.deepEqual(f.hunks[0].lines.map((l) => l.t), [" ", "-", "+", " "]);
  assert.deepEqual(f.hunks[0].lines.map((l) => l.text), ["a", "b", "c", "d"]);
  assert.equal(f.hunks[0].oldStart, 1);
  assert.equal(f.hunks[0].newStart, 1);
  assert.equal(f.hunks[0].oldLines, 3);
  assert.equal(f.hunks[0].newLines, 3);
});

test("empty patch → no files", () => {
  assert.deepEqual(parsePatchFiles("", "s").files, []);
  assert.deepEqual(parsePatchFiles(undefined, "s").files, []);
});

test("new file: --- /dev/null marks added", () => {
  const { files } = parsePatchFiles(`diff --git a/new.txt b/new.txt
new file mode 100644
index 0000000..789
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+hello
+world
`, "s");
  assert.equal(files[0].added, true);
  assert.equal(files[0].oldPath, null);
  assert.equal(files[0].additions, 2);
});

test("deleted file: +++ /dev/null marks deleted", () => {
  const { files } = parsePatchFiles(`diff --git a/gone.txt b/gone.txt
deleted file mode 100644
index 789..0000000
--- a/gone.txt
+++ /dev/null
@@ -1,1 +0,0 @@
-bye
`, "s");
  assert.equal(files[0].deleted, true);
  assert.equal(files[0].path, "gone.txt");
  assert.equal(files[0].deletions, 1);
});

test("rename: display key is the NEW path (server stats[] convention)", () => {
  const { files } = parsePatchFiles(`diff --git a/old.txt b/renamed.txt
similarity index 95%
rename from old.txt
rename to renamed.txt
--- a/old.txt
+++ b/renamed.txt
@@ -1 +1 @@
-same
+same
`, "s");
  const f = files[0];
  assert.equal(f.path, "renamed.txt");
  assert.equal(f.oldPath, "old.txt");
});

test("binary files: 'Binary files … differ' and 'GIT binary patch' mark isBinary", () => {
  const { files } = parsePatchFiles(`diff --git a/img.png b/img.png
index 11..22 100644
Binary files a/img.png and b/img.png differ
diff --git a/data.bin b/data.bin
index 33..44 100644
GIT binary patch
literal 10
`, "s");
  assert.equal(files.length, 2);
  assert.equal(files[0].isBinary, true);
  assert.equal(files[1].isBinary, true);
  assert.equal(files[0].hunks.length, 0);
});

test("multiple files are split on 'diff --git'", () => {
  const { files } = parsePatchFiles(`diff --git a/a.txt b/a.txt
--- a/a.txt
+++ b/a.txt
@@ -1 +1 @@
-1
+2
diff --git a/b.txt b/b.txt
--- a/b.txt
+++ b/b.txt
@@ -1 +1 @@
-3
+4
`, "s");
  assert.deepEqual(files.map((f) => f.path), ["a.txt", "b.txt"]);
  assert.deepEqual(files.map((f) => f.additions), [1, 1]);
});

test("hunk header without line counts (@@ -1 +1 @@)", () => {
  const { files } = parsePatchFiles(`diff --git a/a b/a
--- a/a
+++ b/a
@@ -1 +1 @@
-x
+y
`, "s");
  const h = files[0].hunks[0];
  assert.equal(h.oldLines, 1);
  assert.equal(h.newLines, 1);
});

test('"\\ No newline at end of file" is skipped', () => {
  const { files } = parsePatchFiles(`diff --git a/a b/a
--- a/a
+++ b/a
@@ -1 +1 @@
-x
\\ No newline at end of file
+y
+y2
\\ No newline at end of file
`, "s");
  assert.deepEqual(files[0].hunks[0].lines.map((l) => l.text), ["x", "y", "y2"]);
  assert.equal(files[0].additions, 2);
});

test("bare LF inside a hunk is an empty context line", () => {
  const { files } = parsePatchFiles(`diff --git a/a b/a
--- a/a
+++ b/a
@@ -1,2 +1,2 @@
 a

 b
`, "s");
  assert.deepEqual(files[0].hunks[0].lines.map((l) => l.t), [" ", " ", " "]);
});

test("unknown junk lines between files do not crash the parser", () => {
  const { files } = parsePatchFiles(`some leading junk
diff --git a/a b/a
--- a/a
+++ b/a
@@ -1 +1 @@
-x
+y
trailing junk
`, "s");
  assert.equal(files.length, 1);
  assert.equal(files[0].additions, 1);
});

// --- split view (LCS window pairing) ------------------------------------------

test("splitRows: context passes through to both columns", () => {
  const rows = splitRows([{ t: " ", text: "ctx" }]);
  assert.deepEqual(rows, [{ left: { t: " ", text: "ctx" }, right: { t: " ", text: "ctx" } }]);
});

test("splitRows: pure add run → right-only rows", () => {
  const rows = splitRows([{ t: "+", text: "a" }, { t: "+", text: "b" }]);
  assert.equal(rows.length, 2);
  assert.deepEqual(rows.map((r) => r.right?.text), ["a", "b"]);
  assert.ok(rows.every((r) => r.left === null));
});

test("splitRows: pure del run → left-only rows", () => {
  const rows = splitRows([{ t: "-", text: "a" }]);
  assert.deepEqual(rows.map((r) => r.left?.text), ["a"]);
  assert.ok(rows.every((r) => r.right === null));
});

test("splitRows: LCS pairs equal -/+ lines in order", () => {
  const rows = splitRows([
    { t: "-", text: "keep1" }, { t: "-", text: "drop" }, { t: "-", text: "keep2" },
    { t: "+", text: "keep1" }, { t: "+", text: "new" }, { t: "+", text: "keep2" },
  ]);
  const pairs = rows.filter((r) => r.left && r.right);
  assert.deepEqual(pairs.map((r) => r.left.text), ["keep1", "keep2"]);
  assert.deepEqual(pairs.map((r) => r.right.text), ["keep1", "keep2"]);
  const leftOnly = rows.filter((r) => r.left && !r.right).map((r) => r.left.text);
  const rightOnly = rows.filter((r) => !r.left && r.right).map((r) => r.right.text);
  assert.deepEqual(leftOnly, ["drop"]);
  assert.deepEqual(rightOnly, ["new"]);
});

test("splitRows: runs longer than the 20-line window are chunked", () => {
  const lines = [];
  for (let i = 0; i < 45; i++) lines.push({ t: "-", text: `d${i}` });
  for (let i = 0; i < 45; i++) lines.push({ t: "+", text: `d${i}` });
  const rows = splitRows(lines);
  assert.equal(rows.filter((r) => r.left && r.right).length, 45);
  assert.equal(rows.length, 45);
});

// --- body linkification + trailers ---------------------------------------------

test("linkifyBody: shas become commit links, URLs become anchors, HTML escaped", () => {
  const html = linkifyBody("see <b>https://x.test/a</b> and abcdef1234567890abcdef1234567890abcdef12 done.", "/acme/repo");
  assert.ok(!html.includes("<b>")); // escaped, never raw HTML
  assert.ok(html.includes(`href="/acme/repo/commit/abcdef1234567890abcdef1234567890abcdef12"`));
  assert.ok(html.includes(`href="https://x.test/a"`));
  assert.ok(html.includes("&lt;b&gt;"));
});

test("linkifyBody: trailing punctuation stays outside the anchor", () => {
  const html = linkifyBody("visit https://x.test/page.", "");
  assert.ok(html.endsWith(`https://x.test/page</a>.`));
});

test("groupTrailers: People / merge-queue keys / Other", () => {
  const groups = groupTrailers([
    { key: "Signed-off-by", value: "A <a@x.test>" },
    { key: "merge-queue-at", value: "2026-01-01" },
    { key: "ci-sha", value: "abc" },
    { key: "Co-authored-by", value: "B <b@x.test>" },
    { key: "Weird-Thing", value: "x" },
  ]);
  const byName = Object.fromEntries(groups.map((g) => [g.group, g.trailers.map((t) => t.key)]));
  assert.deepEqual(byName["People"], ["Signed-off-by", "Co-authored-by"]);
  assert.deepEqual(byName["Merge queue"], ["merge-queue-at", "ci-sha"]);
  assert.deepEqual(byName["Other"], ["Weird-Thing"]);
});

test("trailerValue: sha, mail, and plain text", () => {
  assert.deepEqual(trailerValue("abcdef1234567890abcdef1234567890abcdef12"), { sha: "abcdef1234567890abcdef1234567890abcdef12" });
  assert.deepEqual(trailerValue("Ada <ada@x.test>"), { name: "Ada", email: "ada@x.test" });
  assert.deepEqual(trailerValue("<ada@x.test>"), { name: "", email: "ada@x.test" });
  assert.deepEqual(trailerValue("just text"), { text: "just text" });
});
