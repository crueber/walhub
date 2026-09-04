// web/test/unit/reactions.test.js — issue #31: emoji mapping, %06x seq keys,
// per-event summary entries, reaction_changed row text.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  REACTIONS,
  reactionEmoji,
  seqKey,
  summaryEntries,
  reactionChangedText,
  adjustSummary,
} from "../../src/lib/reactions.js";

test("all eight wire contents map to distinct emoji glyphs", () => {
  assert.deepEqual(REACTIONS, ["+1", "-1", "laugh", "hooray", "confused", "heart", "rocket", "eyes"]);
  const glyphs = REACTIONS.map(reactionEmoji);
  assert.equal(new Set(glyphs).size, REACTIONS.length);
  assert.deepEqual(glyphs, ["👍", "👎", "😄", "🎉", "😕", "❤️", "🚀", "👀"]);
});

test("unknown contents pass through unchanged", () => {
  assert.equal(reactionEmoji("partyparrot"), "partyparrot");
  assert.equal(reactionEmoji(""), "");
});

test("seqKey renders %06x (minimum width 6, growing naturally)", () => {
  assert.equal(seqKey(0), "000000");
  assert.equal(seqKey(3), "000003");
  assert.equal(seqKey(255), "0000ff");
  assert.equal(seqKey(0xffffff + 1), "1000000");
});

test("summaryEntries returns nonzero counts in REACTIONS order", () => {
  const summary = { "000003": { eyes: 1, "+1": 3, laugh: 0 } };
  assert.deepEqual(summaryEntries(summary, 3), [["+1", 3], ["eyes", 1]]);
});

test("summaryEntries misses nothing: unknown contents appended, missing seq empty", () => {
  assert.deepEqual(summaryEntries(undefined, 0), []);
  assert.deepEqual(summaryEntries({}, 9), []);
  const summary = { "000000": { partyparrot: 2, heart: 1 } };
  assert.deepEqual(summaryEntries(summary, 0), [["heart", 1], ["partyparrot", 2]]);
});

test("reactionChangedText keeps glyph and word form", () => {
  assert.equal(
    reactionChangedText({ op: "add", content: "eyes", target_event_seq: 0 }),
    "reacted 👀 eyes on #0"
  );
  assert.equal(
    reactionChangedText({ op: "remove", content: "+1", target_event_seq: 12 }),
    "unreacted 👍 +1 on #12"
  );
});

test("adjustSummary adds into an empty/missing summary under the %06x key", () => {
  assert.deepEqual(adjustSummary(undefined, 3, "+1", +1), { "000003": { "+1": 1 } });
  assert.deepEqual(adjustSummary({}, 0, "eyes", +1), { "000000": { eyes: 1 } });
});

test("adjustSummary increments while preserving sibling contents and seqs", () => {
  const before = { "000003": { "+1": 3, eyes: 1 }, "000007": { heart: 2 } };
  assert.deepEqual(adjustSummary(before, 3, "+1", +1), {
    "000003": { "+1": 4, eyes: 1 },
    "000007": { heart: 2 },
  });
});

test("adjustSummary decrements to zero by pruning content then seq keys", () => {
  const before = { "000003": { "+1": 1, eyes: 2 } };
  assert.deepEqual(adjustSummary(before, 3, "+1", -1), { "000003": { eyes: 2 } });
  assert.deepEqual(adjustSummary({ "000003": { "+1": 1 } }, 3, "+1", -1), {});
});

test("adjustSummary clamps at zero: removing what is not there is a no-op", () => {
  assert.deepEqual(adjustSummary({}, 3, "+1", -1), {});
  assert.deepEqual(adjustSummary({ "000003": { eyes: 1 } }, 3, "+1", -1), {
    "000003": { eyes: 1 },
  });
});

test("adjustSummary never mutates its argument (signal reference-equality)", () => {
  const before = Object.freeze({ "000003": Object.freeze({ "+1": 1 }) });
  const after = adjustSummary(before, 3, "+1", +1);
  assert.deepEqual(before, { "000003": { "+1": 1 } });
  assert.notEqual(after, before);
  assert.deepEqual(after, { "000003": { "+1": 2 } });
});
