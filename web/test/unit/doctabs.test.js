// web/test/unit/doctabs.test.js — issue #170: the Tree directory-docs tab set.
//
// orderDocFiles (README case-insensitive first, rest alphabetical),
// docCandidates (current-directory *.md/*.markdown blobs + non-md readme
// fallback), defaultDocFile, and the #anchor round-trip incl. special
// characters. Pure lib/doctabs.js — no Solid, no DOM.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  isMarkdownName,
  isReadmeName,
  orderDocFiles,
  docCandidates,
  defaultDocFile,
  docBlobPath,
  docFetchArgs,
  docSlug,
  docFromHash,
} from "../../src/lib/doctabs.js";
test("source set: .md/.markdown any case; other extensions excluded", () => {
  for (const n of ["README.md", "notes.MD", "guide.Markdown", "a.markdown", ".md"]) {
    assert.equal(isMarkdownName(n), true, n);
  }
  for (const n of ["main.go", "README", "README.txt", "notes.mdx", "md", "", null, undefined]) {
    assert.equal(isMarkdownName(n), false, String(n));
  }
});

test("README match is case-insensitive across both extensions", () => {
  for (const n of ["README.md", "readme.md", "Readme.MD", "README.MARKDOWN", "readme.markdown"]) {
    assert.equal(isReadmeName(n), true, n);
  }
  for (const n of ["readmes.md", "my-readme.md", "README.txt", "README", "notes.md"]) {
    assert.equal(isReadmeName(n), false, n);
  }
});

test("ordering: README first, rest alphabetical case-insensitive", () => {
  assert.deepEqual(
    orderDocFiles(["notes.md", "README.md", "a-Guide.markdown", "CHANGELOG.md"]),
    ["README.md", "a-Guide.markdown", "CHANGELOG.md", "notes.md"],
  );
});

test("ordering: lowercase readme.md still first; multiple readmes stable", () => {
  assert.deepEqual(
    orderDocFiles(["z.md", "readme.md", "README.MARKDOWN", "a.md"]),
    ["README.MARKDOWN", "readme.md", "a.md", "z.md"],
  );
});

test("ordering: no README → plain alphabetical; empty → empty", () => {
  assert.deepEqual(orderDocFiles(["b.md", "A.md", "c.markdown"]), ["A.md", "b.md", "c.markdown"]);
  assert.deepEqual(orderDocFiles([]), []);
  assert.deepEqual(orderDocFiles(null), []);
});

test("ordering does not mutate its input", () => {
  const input = ["z.md", "README.md", "a.md"];
  orderDocFiles(input);
  assert.deepEqual(input, ["z.md", "README.md", "a.md"]);
});

test("candidates: current-directory md blobs only (trees/submodules skipped)", () => {
  const entries = [
    { name: "README.md", type: "blob" },
    { name: "docs", type: "tree" },
    { name: "notes.markdown", type: "blob" },
    { name: "main.go", type: "blob" },
    { name: "vendor", type: "commit" },
    { name: "CHANGELOG.MD", type: "blob" },
  ];
  assert.deepEqual(docCandidates(entries, null), ["README.md", "CHANGELOG.MD", "notes.markdown"]);
});

test("candidates: empty or md-less listings → hidden section", () => {
  assert.deepEqual(docCandidates([], null), []);
  assert.deepEqual(docCandidates([{ name: "main.go", type: "blob" }], null), []);
  assert.deepEqual(docCandidates(null, null), []);
  assert.equal(defaultDocFile([]), null);
});

test("candidates: non-md server readme appended (old single-readme render kept)", () => {
  const entries = [{ name: "README", type: "blob" }];
  const out = docCandidates(entries, { name: "README", contents: "# hi" });
  assert.deepEqual(out, ["README"]);
});

test("candidates: md server readme needs no duplicate", () => {
  const entries = [{ name: "README.md", type: "blob" }];
  const out = docCandidates(entries, { name: "README.md", contents: "# hi" });
  assert.deepEqual(out, ["README.md"]);
});

test("default selection is the README-first head", () => {
  assert.equal(defaultDocFile(["README.md", "a.md"]), "README.md");
  assert.equal(defaultDocFile(["a.md"]), "a.md");
});

test("anchor round-trip survives spaces, quotes, unicode, and '#'", () => {
  const ordered = ["README.md", "my notes (v2).md", "Café \"quotes\" #.markdown"];
  for (const name of ordered) {
    const slug = docSlug(name);
    assert.ok(!slug.includes(" ") && !slug.includes("#"), `slug safe: ${slug}`);
    assert.equal(docFromHash(`#${slug}`, ordered), name);
  }
});

test("blob path encodes each segment, keeps slashes as separators", () => {
  assert.equal(docBlobPath("", "notes.md"), "notes.md");
  assert.equal(docBlobPath("docs", "a b.md"), "docs/a%20b.md");
  assert.equal(docBlobPath("", "my notes (v2)#1.md"), "my%20notes%20(v2)%231.md");
  assert.equal(docBlobPath("a/b", "100%.md"), "a/b/100%25.md");
  assert.equal(docBlobPath("Café", "x?y.md"), "Caf%C3%A9/x%3Fy.md");
});

test("anchor: missing/malformed/foreign hashes fall back to null", () => {
  const ordered = ["README.md", "a.md"];
  assert.equal(docFromHash("", ordered), null);
  assert.equal(docFromHash("#nope.md", ordered), null);
  assert.equal(docFromHash("#%E0%A4%A", ordered), null); // malformed % sequence
  assert.equal(docFromHash("#a.md", []), null);
});

test("fetch args (issue #172): resolved 40-hex sha + encoded path pass through", () => {
  const sha = "85ab5fbcc2bbcab038377fd84741687bb6a19c64";
  assert.deepEqual(docFetchArgs(sha, "", "README.md"), { rev: sha, path: "README.md" });
  assert.deepEqual(docFetchArgs(sha, "docs", "a b.md"), { rev: sha, path: "docs/a%20b.md" });
  assert.deepEqual(docFetchArgs(sha, "", "my notes (v2)#1.md"), {
    rev: sha,
    path: "my%20notes%20(v2)%231.md",
  });
});

test("fetch args (issue #172): UI display strings never become the revision", () => {
  const sha = "85ab5fbcc2bbcab038377fd84741687bb6a19c64";
  // Full ref names (header-pill text) would split the blob route's {rev}.
  for (const display of [
    "refs/heads/main",
    "refs/tags/v1.0",
    "main",
    "SHA:85AB5FBCC2BBCAB038377FD84741687BB6A19C64",
    sha.toUpperCase(), // backend shas are lowercase; never guess case
    sha.slice(0, 12), // short shas are display truncations, not revisions
    "",
    null,
    undefined,
  ]) {
    assert.equal(docFetchArgs(display, "", "README.md"), null, String(display));
  }
  // No tab file, no fetch — even with a valid sha.
  assert.equal(docFetchArgs(sha, "", ""), null);
  assert.equal(docFetchArgs(sha, "", null), null);
});
