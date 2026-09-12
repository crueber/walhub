// web/test/unit/visibility-reseed.test.js — Forgejo #394: the
// reseed-when-clean state machine (lib/visibilityReseed.js) behind the
// Settings visibility select and the Access tab. Pins: seed on first
// doc, reseed-when-clean (follows truth), no-clobber-when-dirty,
// rebase-on-save, and loading-vs-public (no-data-yet is null, never
// "public"; the public default applies only to a present doc missing
// the field).

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  sameValue,
  docVisibility,
  createTrack,
  isSeeded,
  isDirty,
  reseed,
  rebase,
} from "../../src/lib/visibilityReseed.js";

const rowsEqual = (a, b) => JSON.stringify(a ?? []) === JSON.stringify(b ?? []);

test("loading-vs-public: no doc yet is null, never public", () => {
  assert.equal(docVisibility(undefined), null);
  assert.equal(docVisibility(null), null);
  const t = createTrack();
  assert.equal(isSeeded(t), false);
  assert.equal(isDirty(t), false); // loading is not an edit
  // A no-data delivery changes nothing (stays unseeded, same reference).
  assert.equal(reseed(t, docVisibility(undefined)), t);
  assert.equal(reseed(t, docVisibility(null)), t);
});

test("genuinely-missing visibility on a present doc defaults to public", () => {
  assert.equal(docVisibility({ version: 1 }), "public");
  assert.equal(docVisibility({ version: 1, visibility: null }), "public");
  assert.equal(docVisibility({ version: 1, visibility: undefined }), "public");
  assert.equal(docVisibility({ version: 7, visibility: "private" }), "private");
});

test("first doc seeds both value and baseline", () => {
  let t = createTrack();
  t = reseed(t, docVisibility({ version: 7, visibility: "private" }));
  assert.deepEqual(t, { value: "private", base: "private" });
  assert.equal(isSeeded(t), true);
  assert.equal(isDirty(t), false);
});

test("reseed-when-clean: a newer doc moves a clean form to truth", () => {
  let t = reseed(createTrack(), docVisibility({ version: 6, visibility: "public" }));
  // The #394 screenshot shape: entry delivers version 7 / private while
  // the select still shows public — a clean form must follow.
  t = reseed(t, docVisibility({ version: 7, visibility: "private" }));
  assert.deepEqual(t, { value: "private", base: "private" });
  assert.equal(isDirty(t), false);
  // Same-truth redelivery is a no-op in content (fresh pair, same fields).
  const again = reseed(t, docVisibility({ version: 7, visibility: "private" }));
  assert.deepEqual(again, { value: "private", base: "private" });
});

test("no-clobber-when-dirty: user edits survive newer docs", () => {
  let t = reseed(createTrack(), docVisibility({ version: 6, visibility: "public" }));
  t = { ...t, value: "private" }; // user picks private, has not saved
  assert.equal(isDirty(t), true);
  // A newer doc (Access-tab save elsewhere, poll revalidation) must not
  // touch the pending edit — same reference back.
  const kept = reseed(t, docVisibility({ version: 7, visibility: "authenticated" }));
  assert.equal(kept, t);
  assert.deepEqual(kept, { value: "private", base: "public" });
  assert.equal(isDirty(kept), true);
});

test("rebase-on-save: the saved value becomes the new baseline", () => {
  let t = reseed(createTrack(), docVisibility({ version: 6, visibility: "public" }));
  t = { ...t, value: "private" };
  assert.equal(isDirty(t), true);
  // Success echoes the saved spelling — form settles clean at it.
  t = rebase(t, "private");
  assert.deepEqual(t, { value: "private", base: "private" });
  assert.equal(isDirty(t), false);
  // A matching later doc keeps it clean.
  t = reseed(t, docVisibility({ version: 7, visibility: "private" }));
  assert.equal(isDirty(t), false);
});

test("failure reseed settles clean at server truth via rebase", () => {
  let t = reseed(createTrack(), docVisibility({ version: 6, visibility: "public" }));
  t = { ...t, value: "private" };
  // Save failed (403/409): authoritative + loud means adopting truth.
  t = rebase(t, "authenticated");
  assert.deepEqual(t, { value: "authenticated", base: "authenticated" });
  assert.equal(isDirty(t), false);
});

test("custom equality covers sibling fields (Access role bindings)", () => {
  const rowsOf = (bindings) => (bindings ?? []).map((b) => ({ subject: b.subject, role: b.role }));
  let t = reseed(createTrack(), rowsOf([{ subject: "user:a@x", role: "admin" }]), rowsEqual);
  assert.equal(isDirty(t, rowsEqual), false);
  // Clean form follows a remotely-added binding.
  t = reseed(
    t,
    rowsOf([
      { subject: "user:a@x", role: "admin" },
      { subject: "user:b@x", role: "read" },
    ]),
    rowsEqual,
  );
  assert.equal(t.value.length, 2);
  assert.equal(isDirty(t, rowsEqual), false);
  // Dirty rows are never clobbered (same reference back).
  const edited = { ...t, value: [...t.value, { subject: "user:c@x", role: "read" }] };
  assert.equal(isDirty(edited, rowsEqual), true);
  assert.equal(reseed(edited, rowsOf([]), rowsEqual), edited);
});

test("sameValue is strict equality", () => {
  assert.equal(sameValue("public", "public"), true);
  assert.equal(sameValue("public", "private"), false);
  assert.equal(sameValue(null, undefined), false);
});
