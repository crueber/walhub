// web/test/unit/split-close.test.js — issue #311: the issue close
// affordances are SPLIT buttons (primary one-click close-as-completed +
// ▾ dropdown segment with the not-planned alternate), and an open menu
// dismisses on any outside click. No DOM: JSX is pinned as source text,
// mirroring clone-outside-close.test.js / nav-api-right.test.js.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const composer = () => srcOf("../../src/components/CommentComposer.jsx");

function block(src, startMarker, endMarker) {
  const s = src.indexOf(startMarker);
  assert.ok(s !== -1, `expected block start ${startMarker}`);
  const e = src.indexOf(endMarker, s);
  assert.ok(e !== -1, `expected block end ${endMarker}`);
  return src.slice(s, e);
}

test("split button: primary segment closes as completed in one click", () => {
  const menu = block(composer(), "function SplitCloseMenu(props)", "export default function CommentComposer");
  // The primary segment calls onPrimary without opening any menu.
  assert.ok(menu.includes("props.onPrimary()"), "primary segment must invoke the default action directly");
  const rest = composer();
  assert.ok(
    rest.includes("onPrimary={() => runClose(CLOSE_COMPLETED)}"),
    "Close primary must runClose(CLOSE_COMPLETED) — one click, completed",
  );
  assert.ok(
    rest.includes("onPrimary={() => commentAndClose(getBody(), CLOSE_COMPLETED)}"),
    "Comment-and-Close primary must post the body and close as completed",
  );
});

test("split button: dropdown offers only the not-planned alternate", () => {
  const s = composer();
  assert.ok(s.includes('alternateVerb="Close as not planned"'), "Close menu offers not-planned");
  assert.ok(
    s.includes('alternateVerb="Comment and close as not planned"'),
    "Comment-and-Close menu offers not-planned",
  );
  assert.ok(s.includes("alternateReason={CLOSE_NOT_PLANNED}"), "alternate carries CLOSE_NOT_PLANNED");
  // The completed path IS the primary button — the menu must not
  // duplicate it as a "completed" item.
  assert.ok(!s.includes("Close as completed</"), "menu must not contain a completed item");
});

test("split button: both segments share one root and disable together", () => {
  const menu = block(composer(), "function SplitCloseMenu(props)", "export default function CommentComposer");
  assert.ok(menu.includes("ref={root}"), "root wraps both segments + menu (one outside-click boundary)");
  assert.equal(
    (menu.match(/disabled=\{props\.disabled\}/g) ?? []).length,
    3,
    "primary, toggle, and menu item all disable from the same busy flag",
  );
});

test("outside click dismisses without fighting the toggle", () => {
  const menu = block(composer(), "function SplitCloseMenu(props)", "export default function CommentComposer");
  assert.ok(
    menu.includes('document.addEventListener("click", onDocClick)'),
    "must register a document click listener",
  );
  assert.ok(
    menu.includes("!root.contains(e.target)"),
    "handler must ignore clicks inside the split root (both segments + menu)",
  );
  assert.ok(menu.includes("close(false)"), "outside click closes WITHOUT stealing focus");
  assert.ok(
    menu.includes('document.removeEventListener("click", onDocClick)'),
    "must remove the listener in onCleanup",
  );
  assert.ok(menu.includes("onCleanup"), "cleanup wired via Solid onCleanup");
});

test("keyboard support intact: menu role, arrow nav, Escape refocuses toggle", () => {
  const menu = block(composer(), "function SplitCloseMenu(props)", "export default function CommentComposer");
  assert.ok(menu.includes('aria-haspopup="menu"'), "toggle advertises the menu");
  assert.ok(menu.includes("aria-expanded"), "toggle exposes expanded state");
  assert.ok(menu.includes('role="menu"') && menu.includes('role="menuitem"'), "menu roles kept");
  assert.ok(menu.includes('e.key === "Escape"'), "Escape handler kept");
  assert.ok(menu.includes("close(true)"), "Escape refocuses the toggle");
  assert.ok(menu.includes("ArrowDown") && menu.includes("ArrowUp"), "arrow-key navigation kept");
  assert.ok(menu.includes('type="button"'), "segments are native buttons (Enter/Space free)");
});

test("plain-button fallback kept for callers without reasons", () => {
  const s = composer();
  // closeChooser=false (PR close has no reasons) still renders plain buttons.
  assert.ok(s.includes("when={props.closeChooser}"), "chooser path stays opt-in");
  assert.ok(s.includes("fallback={"), "plain-button fallback kept");
});

test("event text reads sensibly for all four close paths", async () => {
  const { issueEventText } = await import("../../src/lib/issue-events.js");
  // One-click primary paths (completed) …
  assert.equal(
    issueEventText({ type: "state_changed", to: "closed", reason: "completed" }),
    "closed as completed",
  );
  // … and dropdown alternates (not_planned), for both Close and
  // Comment-and-Close (both land as state_changed with a reason).
  assert.equal(
    issueEventText({ type: "state_changed", to: "closed", reason: "not_planned" }),
    "closed as not planned",
  );
});
