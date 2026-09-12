import { test } from "node:test";
import assert from "node:assert/strict";

import {
  AUTOGROW_MAX_VH,
  autogrowMaxPx,
  clampAutogrowHeight,
  initAutogrow,
  growTextarea,
} from "../../src/lib/autogrow.js";

/** Profile bio auto-grow (Forgejo #419): pure height math + stub-element DOM behavior. */

/** Minimal textarea stub: style bag + measurable scrollHeight + caret. */
function stubEl({ scrollHeight = 0, value = "", selectionStart = null, min = 0 } = {}) {
  return {
    style: {},
    scrollHeight,
    scrollTop: 0,
    value,
    selectionStart,
    __autogrowMinPx: min,
  };
}

test("stated cap is 50vh", () => {
  assert.equal(AUTOGROW_MAX_VH, 50);
  assert.equal(autogrowMaxPx(800), 400);
  assert.equal(autogrowMaxPx(1000), 500);
});

test("autogrowMaxPx sanitizes bad viewports toward the safe floor", () => {
  assert.equal(autogrowMaxPx(NaN), 0);
  assert.equal(autogrowMaxPx(-100), 0);
  assert.equal(autogrowMaxPx(undefined), 0);
  assert.equal(autogrowMaxPx(800, 25), 200);
});

test("clamp fits content between floor and cap", () => {
  assert.deepEqual(clampAutogrowHeight(200, { minHeightPx: 100, maxHeightPx: 400 }), {
    heightPx: 200,
    overflowing: false,
  });
});

test("clamp never shrinks below the rows floor (min bound)", () => {
  assert.deepEqual(clampAutogrowHeight(20, { minHeightPx: 108, maxHeightPx: 400 }), {
    heightPx: 108,
    overflowing: false,
  });
  assert.deepEqual(clampAutogrowHeight(0, { minHeightPx: 108, maxHeightPx: 400 }), {
    heightPx: 108,
    overflowing: false,
  });
});

test("clamp caps long bios and reports overflow (max bound)", () => {
  const r = clampAutogrowHeight(1200, { minHeightPx: 108, maxHeightPx: 400 });
  assert.deepEqual(r, { heightPx: 400, overflowing: true });
});

test("clamp lets the floor win when min > max (short viewport)", () => {
  assert.deepEqual(clampAutogrowHeight(300, { minHeightPx: 500, maxHeightPx: 200 }), {
    heightPx: 500,
    overflowing: false,
  });
});

test("clamp sanitizes non-finite inputs", () => {
  assert.deepEqual(clampAutogrowHeight(NaN, { minHeightPx: 100, maxHeightPx: 400 }), {
    heightPx: 100,
    overflowing: false,
  });
  assert.deepEqual(clampAutogrowHeight(-50, { minHeightPx: 100, maxHeightPx: 400 }), {
    heightPx: 100,
    overflowing: false,
  });
});

test("initAutogrow records the mounted rows height as the floor", () => {
  const el = { offsetHeight: 108, clientHeight: 100 };
  assert.equal(initAutogrow(el), 108);
  assert.equal(el.__autogrowMinPx, 108);
});

test("initAutogrow falls back to clientHeight, then 0", () => {
  const el = { clientHeight: 90 };
  assert.equal(initAutogrow(el), 90);
  assert.equal(initAutogrow({}), 0);
  assert.equal(initAutogrow(null), 0);
});

test("growTextarea grows to fit and hides the inner scrollbar", () => {
  const el = stubEl({ scrollHeight: 200, min: 108 });
  const r = growTextarea(el, { maxHeightPx: 400 });
  assert.deepEqual(r, { heightPx: 200, overflowing: false });
  assert.equal(el.style.height, "200px");
  assert.equal(el.style.overflowY, "hidden");
  assert.equal(el.scrollTop, 0);
});

test("growTextarea shrinks back toward rows when lines are deleted", () => {
  const el = stubEl({ scrollHeight: 40, min: 108 });
  el.style.height = "400px"; // previously grown
  const r = growTextarea(el, { maxHeightPx: 400 });
  assert.deepEqual(r, { heightPx: 108, overflowing: false });
  assert.equal(el.style.height, "108px");
});

test("growTextarea caps a very long bio and scrolls internally", () => {
  const el = stubEl({ scrollHeight: 1200, min: 108 });
  const r = growTextarea(el, { maxHeightPx: 400 });
  assert.deepEqual(r, { heightPx: 400, overflowing: true });
  assert.equal(el.style.height, "400px");
  assert.equal(el.style.overflowY, "auto");
});

test("growTextarea scrolls a trailing caret into view past the cap (Enter-at-end)", () => {
  const value = "a\nb\nc\n";
  const el = stubEl({ scrollHeight: 1200, value, selectionStart: value.length, min: 108 });
  growTextarea(el, { maxHeightPx: 400 });
  assert.equal(el.scrollTop, 1200);
});

test("growTextarea leaves mid-text caret scroll alone past the cap", () => {
  const el = stubEl({ scrollHeight: 1200, value: "a\nb\nc\n", selectionStart: 1, min: 108 });
  growTextarea(el, { maxHeightPx: 400 });
  assert.equal(el.scrollTop, 0);
});

test("growTextarea honors an explicit minHeightPx over the recorded floor", () => {
  const el = stubEl({ scrollHeight: 40, min: 108 });
  const r = growTextarea(el, { minHeightPx: 90, maxHeightPx: 400 });
  assert.equal(r.heightPx, 90);
});

test("growTextarea defaults to a 50vh cap off window.innerHeight", () => {
  globalThis.window = { innerHeight: 800 };
  try {
    const el = stubEl({ scrollHeight: 1200, min: 108 });
    const r = growTextarea(el);
    assert.deepEqual(r, { heightPx: 400, overflowing: true });
  } finally {
    delete globalThis.window;
  }
});

test("growTextarea is unbounded headless without a viewport unless capped", () => {
  assert.ok(!("window" in globalThis));
  const el = stubEl({ scrollHeight: 1200, min: 108 });
  assert.deepEqual(growTextarea(el), { heightPx: 1200, overflowing: false });
});

test("growTextarea tolerates a null element", () => {
  assert.deepEqual(growTextarea(null), { heightPx: 0, overflowing: false });
});
