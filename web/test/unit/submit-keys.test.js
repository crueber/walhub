import { test } from "node:test";
import assert from "node:assert/strict";

import { onSubmitKeys } from "../../src/lib/submitKeys.js";

/** Cmd/Ctrl+Enter shared submit helper (Forgejo #450): combos, guards, swallow. */

/** Minimal keydown stub: records preventDefault/stopPropagation calls. */
function keyEvent(overrides = {}) {
  const e = {
    key: "Enter",
    metaKey: false,
    ctrlKey: false,
    shiftKey: false,
    defaultPrevented: false,
    propagationStopped: false,
    preventDefault() {
      e.defaultPrevented = true;
    },
    stopPropagation() {
      e.propagationStopped = true;
    },
    ...overrides,
  };
  return e;
}

test("returns a handler function", () => {
  assert.equal(typeof onSubmitKeys(() => {}), "function");
});

test("meta+Enter fires the submit with the event and swallows it", () => {
  let got = null;
  const e = keyEvent({ metaKey: true });
  onSubmitKeys((ev) => {
    got = ev;
  })(e);
  assert.equal(got, e);
  assert.equal(e.defaultPrevented, true);
  assert.equal(e.propagationStopped, true);
});

test("ctrl+Enter fires the submit (Windows/Linux)", () => {
  let calls = 0;
  const e = keyEvent({ ctrlKey: true });
  onSubmitKeys(() => {
    calls++;
  })(e);
  assert.equal(calls, 1);
  assert.equal(e.defaultPrevented, true);
});

test("meta+ctrl+Enter together also fires", () => {
  let calls = 0;
  onSubmitKeys(() => {
    calls++;
  })(keyEvent({ metaKey: true, ctrlKey: true }));
  assert.equal(calls, 1);
});

test("plain Enter (no modifier) is a no-op — newline inserts natively", () => {
  let calls = 0;
  const e = keyEvent();
  onSubmitKeys(() => {
    calls++;
  })(e);
  assert.equal(calls, 0);
  assert.equal(e.defaultPrevented, false);
  assert.equal(e.propagationStopped, false);
});

test("shift+meta+Enter does NOT submit (reserved for future soft-submit)", () => {
  let calls = 0;
  const e = keyEvent({ metaKey: true, shiftKey: true });
  onSubmitKeys(() => {
    calls++;
  })(e);
  assert.equal(calls, 0);
  assert.equal(e.defaultPrevented, false);
});

test("shift+ctrl+Enter does NOT submit either", () => {
  let calls = 0;
  const e = keyEvent({ ctrlKey: true, shiftKey: true });
  onSubmitKeys(() => {
    calls++;
  })(e);
  assert.equal(calls, 0);
  assert.equal(e.defaultPrevented, false);
});

test("no-op while busy — never double-submits against a disabled button", () => {
  let calls = 0;
  const e = keyEvent({ metaKey: true });
  onSubmitKeys(() => {
    calls++;
  }, { isBusy: () => true })(e);
  assert.equal(calls, 0);
  assert.equal(e.defaultPrevented, false);
  assert.equal(e.propagationStopped, false);
});

test("fires when the busy guard reports idle", () => {
  let calls = 0;
  onSubmitKeys(() => {
    calls++;
  }, { isBusy: () => false })(keyEvent({ ctrlKey: true }));
  assert.equal(calls, 1);
});

test("modifier + non-Enter key never fires", () => {
  for (const key of ["a", "s", "Escape", "Tab", "ArrowDown", "Enter "] ) {
    let calls = 0;
    const e = keyEvent({ metaKey: true, key });
    onSubmitKeys(() => {
      calls++;
    })(e);
    assert.equal(calls, 0);
    assert.equal(e.defaultPrevented, false);
  }
});

test("meta alone on a non-Enter key is a no-op (no keyup/other-key fire)", () => {
  let calls = 0;
  onSubmitKeys(() => {
    calls++;
  })(keyEvent({ metaKey: true, key: "s" }));
  assert.equal(calls, 0);
});

test("non-function submit is a no-op that swallows nothing", () => {
  const e = keyEvent({ metaKey: true });
  onSubmitKeys(null)(e);
  onSubmitKeys(undefined)(e);
  onSubmitKeys("save")(e);
  assert.equal(e.defaultPrevented, false);
});

test("null/undefined event is a no-op", () => {
  let calls = 0;
  const h = onSubmitKeys(() => {
    calls++;
  });
  h(null);
  h(undefined);
  assert.equal(calls, 0);
});

test("works without stopPropagation (bare stub events)", () => {
  let calls = 0;
  const e = { key: "Enter", metaKey: true, ctrlKey: false, shiftKey: false, preventDefault() {} };
  onSubmitKeys(() => {
    calls++;
  })(e);
  assert.equal(calls, 1);
});
