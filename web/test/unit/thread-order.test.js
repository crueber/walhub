import { test } from "node:test";
import assert from "node:assert/strict";

import {
  chronological,
  appendOlderWindow,
  olderCursor,
  anchorScrollTop,
} from "../../src/lib/thread-order.js";

const seqs = (rows) => rows.map((e) => e.seq);

// Wire windows arrive newest-first (02 §7 Decisions); the thread reads
// oldest → newest (issue #225).

test("chronological sorts a newest-first wire page oldest-first", () => {
  const wire = [{ seq: 4 }, { seq: 3 }, { seq: 2 }, { seq: 1 }, { seq: 0 }];
  assert.deepEqual(seqs(chronological(wire)), [0, 1, 2, 3, 4]);
});

test("chronological sorts any input order (stale cache, refetch)", () => {
  const mixed = [{ seq: 2 }, { seq: 0 }, { seq: 4 }, { seq: 1 }];
  assert.deepEqual(seqs(chronological(mixed)), [0, 1, 2, 4]);
});

test("chronological is stable: equal seqs keep input order", () => {
  const rows = [
    { seq: 1, type: "commented" },
    { seq: 1, type: "labels_changed" },
  ];
  const out = chronological(rows);
  assert.equal(out[0].type, "commented");
  assert.equal(out[1].type, "labels_changed");
});

test("chronological never mutates its input", () => {
  const wire = [{ seq: 2 }, { seq: 0 }, { seq: 1 }];
  chronological(wire);
  assert.deepEqual(seqs(wire), [2, 0, 1]);
});

test("chronological tolerates empty / missing input and seq-less rows", () => {
  assert.deepEqual(chronological([]), []);
  assert.deepEqual(chronological(undefined), []);
  assert.deepEqual(chronological(null), []);
  // Seq-less rows predate the log: they sort first, in input order.
  const out = chronological([{ seq: 1 }, {}, { seq: 0 }]);
  assert.deepEqual(
    out.map((e) => e.seq),
    [undefined, 0, 1],
  );
});

test("older windows append in seq space and read chronological (no dup, no jumps)", () => {
  // Newest page (view) + two older windows, wire order throughout.
  const view = [{ seq: 9 }, { seq: 8 }, { seq: 7 }];
  const older1 = [{ seq: 6 }, { seq: 5 }, { seq: 4 }];
  const older2 = [{ seq: 3 }, { seq: 2 }, { seq: 1 }, { seq: 0 }];
  let assembly = appendOlderWindow(view, []);
  assert.equal(olderCursor(assembly), 7);
  assembly = appendOlderWindow(assembly, older1);
  assert.equal(olderCursor(assembly), 4);
  assembly = appendOlderWindow(assembly, older2);
  assert.equal(olderCursor(assembly), 0);
  // The rendered thread is gap-free, oldest first, newest last.
  assert.deepEqual(seqs(chronological(assembly)), [0, 1, 2, 3, 4, 5, 6, 7, 8, 9]);
  // No event appears twice: assembly length equals distinct seq count.
  assert.equal(assembly.length, new Set(seqs(assembly)).size);
});

test("olderCursor pages from the newest when empty (after_seq=0)", () => {
  assert.equal(olderCursor([]), 0);
  assert.equal(olderCursor(undefined), 0);
  assert.equal(olderCursor(null), 0);
});

test("anchorScrollTop pins the viewport across a prepend above", () => {
  // 300 px of older rows prepended: the offset grows by exactly that.
  assert.equal(anchorScrollTop(500, 2000, 2300), 800);
  // No height change (failed fetch, empty page): offset untouched.
  assert.equal(anchorScrollTop(500, 2000, 2000), 500);
  // At the very top: the reader stays at the new top edge.
  assert.equal(anchorScrollTop(0, 2000, 2300), 300);
});
