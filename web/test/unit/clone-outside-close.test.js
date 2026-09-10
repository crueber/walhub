// web/test/unit/clone-outside-close.test.js — issue #255 regression: the
// CloneMenu popover (<details>) had an Escape handler but no document-level
// outside-click handler, so it stayed open until Esc or re-toggle. The fix
// mirrors the RefPicker/TasksOverlay pattern (document click listener that
// closes when the target is outside root, removed in onCleanup). The same
// gap existed on NotificationTray (Esc-only) and is fixed in the same
// change. No DOM: JSX is pinned as source text, mirroring
// nav-api-right.test.js / pull-dropdown-opaque.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const repo = () => srcOf("../../src/pages/Repo.jsx");
const tray = () => srcOf("../../src/components/NotificationTray.jsx");

function block(src, startMarker, endMarker) {
  const s = src.indexOf(startMarker);
  assert.ok(s !== -1, `expected block start ${startMarker}`);
  const e = src.indexOf(endMarker, s);
  assert.ok(e !== -1, `expected block end ${endMarker}`);
  return src.slice(s, e);
}

test("CloneMenu registers an outside-click close on document", () => {
  const menu = block(repo(), "function CloneMenu(props)", "// --- tabs ---");
  assert.ok(
    menu.includes('document.addEventListener("click", onDoc)'),
    "CloneMenu must register a document click listener",
  );
  assert.ok(
    menu.includes("!root.contains(e.target)"),
    "handler must ignore clicks inside the popover",
  );
  assert.ok(
    menu.includes("root.open = false"),
    "outside click must close the <details> popover",
  );
});

test("CloneMenu removes the listener on cleanup (no leak)", () => {
  const menu = block(repo(), "function CloneMenu(props)", "// --- tabs ---");
  assert.ok(
    menu.includes('document.removeEventListener("click", onDoc)'),
    "CloneMenu must remove the document click listener in onCleanup",
  );
});

test("CloneMenu keeps Esc + focus-return and the in-root trigger", () => {
  const menu = block(repo(), "function CloneMenu(props)", "// --- tabs ---");
  assert.ok(menu.includes('e.key === "Escape"'), "Escape handler kept");
  assert.ok(
    menu.includes('root.querySelector("summary")?.focus()'),
    "Escape still returns focus to the trigger",
  );
  const details = block(menu, "<details", "</details>");
  assert.ok(details.includes("<summary"), "trigger <summary> lives inside <details> so the native toggle never fights the outside handler");
});

test("NotificationTray shared the gap and gets the same close", () => {
  const s = tray();
  assert.ok(
    s.includes('document.addEventListener("click", onDoc)'),
    "tray must register a document click listener",
  );
  assert.ok(
    s.includes("!root.contains(e.target)") && s.includes("setOpen(false)"),
    "outside click must close the tray dropdown",
  );
  assert.ok(
    s.includes('document.removeEventListener("click", onDoc)'),
    "tray must remove the listener in onCleanup",
  );
  assert.ok(s.includes("ref={root}"), "tray root wraps bell + dropdown");
  assert.ok(s.includes('e.key === "Escape"'), "tray Escape handler kept");
});
