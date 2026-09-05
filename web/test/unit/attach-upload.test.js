import { test } from "node:test";
import assert from "node:assert/strict";

import {
  MAX_BODY_BYTES,
  escapeAlt,
  insertAtCursor,
  replacePlaceholder,
  placeholderFor,
  markdownFor,
  uploadFailedFor,
  projectedLengthOk,
  isImageFile,
  filesFromPasteEvent,
  filesFromDropEvent,
  uploadFilesSequential,
} from "../../src/lib/attachUpload.js";

/** Paste/drop upload helpers (docs/features/02 §12): cursor splice, escaping, events. */

test("escapeAlt neutralizes markdown closers (S1)", () => {
  assert.equal(escapeAlt("shot.png"), "shot.png");
  assert.equal(escapeAlt("a](b).png"), "a\\]\\(b\\).png");
  assert.equal(escapeAlt("a\\b"), "a\\\\b");
  assert.equal(escapeAlt("x[y]"), "x\\[y\\]");
});

test("markdownFor builds an image link with escaped alt", () => {
  assert.equal(markdownFor("a](b).png", "/o/r/attachments/s/a%5D%28b%29.png"),
    "![a\\]\\(b\\).png](/o/r/attachments/s/a%5D%28b%29.png)");
});

test("placeholderFor / uploadFailedFor shapes", () => {
  assert.equal(placeholderFor("x.png"), "![uploading x.png…]()");
  assert.equal(uploadFailedFor("x.png", null), "![upload of x.png failed]()");
  assert.equal(uploadFailedFor("x.png", "415 blah\nsecond"), "![upload of x.png failed: 415 blah]()");
});

test("insertAtCursor splices and reports the cursor", () => {
  assert.deepEqual(insertAtCursor("hello world", 5, 5, "X"), { text: "helloX world", cursor: 6 });
  assert.deepEqual(insertAtCursor("hello world", 0, 5, "bye"), { text: "bye world", cursor: 3 });
  assert.deepEqual(insertAtCursor("abc", 99, 99, "Z"), { text: "abcZ", cursor: 4 });
  assert.deepEqual(insertAtCursor("abc", 2, 0, "Z"), { text: "Zc", cursor: 1 }); // swapped clamp
});

test("replacePlaceholder swaps first occurrence, appends when edited away", () => {
  assert.equal(replacePlaceholder("a PH b PH c", "PH", "R"), "a R b PH c");
  assert.equal(replacePlaceholder("no marker", "PH", "R"), "no marker\nR");
  assert.equal(replacePlaceholder("", "PH", "R"), "R");
});

test("projectedLengthOk enforces the 64 KiB body cap", () => {
  assert.equal(MAX_BODY_BYTES, 65536);
  assert.ok(projectedLengthOk("a".repeat(65500), "x".repeat(36)));
  assert.ok(!projectedLengthOk("a".repeat(65500), "x".repeat(37)));
});

test("isImageFile accepts images, rejects SVG and non-images", () => {
  assert.ok(isImageFile({ type: "image/png", name: "a.png" }));
  assert.ok(isImageFile({ type: "image/jpeg", name: "a.jpg" }));
  assert.ok(isImageFile({ type: "", name: "shot.WEBP" })); // typeless fallback by extension
  assert.ok(!isImageFile({ type: "image/svg+xml", name: "x.svg" }));
  assert.ok(!isImageFile({ type: "text/plain", name: "x.txt" }));
  assert.ok(!isImageFile({ type: "", name: "x.svg" }));
  assert.ok(!isImageFile(null));
});

test("filesFromPasteEvent reads files then items, [] for text", () => {
  const png = { type: "image/png", name: "a.png" };
  assert.deepEqual(filesFromPasteEvent({ clipboardData: { files: [png, { type: "text/plain" }] } }), [png]);
  const viaItems = filesFromPasteEvent({
    clipboardData: { files: [], items: [{ kind: "file", getAsFile: () => png }, { kind: "string" }] },
  });
  assert.deepEqual(viaItems, [png]);
  assert.deepEqual(filesFromPasteEvent({ clipboardData: { files: [], items: [] } }), []);
  assert.deepEqual(filesFromPasteEvent({}), []);
});

test("filesFromDropEvent keeps image-kind only", () => {
  const png = { type: "image/png", name: "a.png" };
  assert.deepEqual(
    filesFromDropEvent({ dataTransfer: { files: [png, { type: "video/mp4", name: "v.mp4" }] } }),
    [png]
  );
  assert.deepEqual(filesFromDropEvent({}), []);
});

test("uploadFilesSequential: placeholder → markdown at cursor", async () => {
  let text = "before|after";
  const area = { selectionStart: 6, selectionEnd: 7, setSelectionRange() {}, focus() {} };
  const seen = [];
  await uploadFilesSequential({
    files: [{ type: "image/png", name: "s.png" }],
    textarea: area,
    getText: () => text,
    setText: (t) => { seen.push(t); text = t; },
    upload: async (f) => ({ name: f.name, url: "/o/r/attachments/sha/s.png" }),
    onError: (e) => { throw e; },
  });
  assert.equal(seen[0], "before![uploading s.png…]()after");
  assert.equal(text, "before![s.png](/o/r/attachments/sha/s.png)after");
});

test("uploadFilesSequential: failure swaps a failure line and reports", async () => {
  let text = "body";
  const errors = [];
  await uploadFilesSequential({
    files: [{ type: "image/png", name: "s.png" }],
    textarea: null,
    getText: () => text,
    setText: (t) => { text = t; },
    upload: async () => { throw new Error("415 only PNG"); },
    onError: (e) => errors.push(e),
  });
  assert.equal(text, "body![upload of s.png failed: 415 only PNG]()");
  assert.equal(errors.length, 1);
});

test("uploadFilesSequential: skips files that would overflow 64 KiB", async () => {
  let text = "a".repeat(65536);
  const errors = [];
  let uploaded = 0;
  await uploadFilesSequential({
    files: [{ type: "image/png", name: "big.png" }],
    textarea: null,
    getText: () => text,
    setText: (t) => { text = t; },
    upload: async () => { uploaded++; return {}; },
    onError: (e) => errors.push(e),
  });
  assert.equal(uploaded, 0);
  assert.equal(errors.length, 1);
  assert.equal(text.length, 65536);
});

test("uploadFilesSequential: skips non-images, uploads sequentially", async () => {
  let text = "";
  const order = [];
  await uploadFilesSequential({
    files: [
      { type: "text/plain", name: "t.txt" },
      { type: "image/png", name: "1.png" },
      { type: "image/gif", name: "2.gif" },
    ],
    textarea: null,
    getText: () => text,
    setText: (t) => { text = t; },
    upload: async (f) => { order.push(f.name); return { name: f.name, url: `/u/${f.name}` }; },
    onError: () => {},
  });
  assert.deepEqual(order, ["1.png", "2.gif"]);
  assert.ok(text.includes("![1.png](/u/1.png)"));
  assert.ok(text.includes("![2.gif](/u/2.gif)"));
});
