// web/test/unit/menu-focus-477.test.js — Forgejo #477: the focus-first-item
// branch in both navbar menus was dead. `toggle` called `setOpen((o) => !o)`
// and then read `if (!getOpen())` — but Solid signals update synchronously,
// so after opening getOpen() is already true and the branch never fired on
// open (it fired only on close, into an unmounted panel).
//
// (1) Source pins: both CreateMenu.jsx and IdentityMenu.jsx capture the
// pre-toggle value (`const opening = !getOpen()`) and guard the
// queueMicrotask first-item focus on `if (opening)`.
// (2) Behavioral simulation of the fixed toggle pattern (closure signal +
// queued microtask): focus is scheduled on open, never on close.
// (3) Popover contract untouched in both files: Esc + focus return,
// outside-click dismiss, arrow-key walk, Tab-out, menu roles.
// No DOM: JSX pinned as source text, mirroring create-menu-466.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const CREATE = srcOf("../../src/components/CreateMenu.jsx");
const IDENTITY = srcOf("../../src/components/IdentityMenu.jsx");

for (const [name, src] of [
  ["CreateMenu", CREATE],
  ["IdentityMenu", IDENTITY],
]) {
  test(`${name}: toggle captures the pre-toggle value (#477)`, () => {
    assert.ok(src.includes("const opening = !getOpen()"), "reads the value before setOpen");
    assert.ok(src.includes("setOpen(opening)"), "sets the captured value");
    assert.ok(src.includes("if (opening)"), "focus guarded on the pre-toggle read");
    assert.ok(
      src.includes('queueMicrotask(() => menuRef?.querySelector("[role=menuitem]")?.focus())'),
      "focuses the first menuitem once the menu renders",
    );
  });

  test(`${name}: dead post-setOpen read is gone (#477)`, () => {
    assert.ok(!src.includes("setOpen((o) => !o)"), "updater-form setOpen followed by a read is gone");
  });

  test(`${name}: popover contract untouched (Esc/outside-click/arrows/Tab)`, () => {
    assert.ok(src.includes('document.addEventListener("click", onDocClick)'), "outside-click listener kept");
    assert.ok(src.includes("!root.contains(e.target)"), "inside-root clicks ignored");
    assert.ok(src.includes('e.key === "Escape"'), "Escape handler kept");
    assert.ok(src.includes("close(true)"), "Escape refocuses the trigger");
    assert.ok(src.includes("ArrowDown") && src.includes("ArrowUp"), "arrow-key walk kept");
    assert.ok(src.includes('e.key === "Tab"'), "Tab-out dismiss kept");
    assert.ok(src.includes('role="menu"') && src.includes('role="menuitem"'), "menu roles kept");
  });
}

// The fixed toggle pattern, simulated headlessly: a synchronous closure
// signal (Solid createSignal semantics) plus the queued focus callback.
function makeToggle(scheduleFocus) {
  let open = false;
  const getOpen = () => open;
  const setOpen = (v) => {
    open = v;
  };
  const toggle = (e) => {
    e.preventDefault();
    const opening = !getOpen();
    setOpen(opening);
    if (opening) scheduleFocus();
  };
  return { toggle, getOpen };
}

test("fixed toggle: focus scheduled on open, never on close", () => {
  let scheduled = 0;
  const { toggle, getOpen } = makeToggle(() => scheduled++);
  toggle({ preventDefault() {} });
  assert.equal(getOpen(), true, "first click opens");
  assert.equal(scheduled, 1, "focus scheduled on open");
  toggle({ preventDefault() {} });
  assert.equal(getOpen(), false, "second click closes");
  assert.equal(scheduled, 1, "no focus scheduled on close");
});

test("old pattern is dead: post-setOpen read never fires on open", () => {
  // Documents the #477 bug shape so it cannot regress unnoticed.
  let open = false;
  let scheduled = 0;
  const toggle = (e) => {
    e.preventDefault();
    open = !open; // setOpen((o) => !o), synchronous
    if (!open) scheduled++; // if (!getOpen()) — the dead branch
  };
  toggle({ preventDefault() {} });
  assert.equal(open, true);
  assert.equal(scheduled, 0, "old branch misses the open transition");
});
