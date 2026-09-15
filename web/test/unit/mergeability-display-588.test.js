// web/test/unit/mergeability-display-588.test.js — Forgejo #588:
// The PR sidebar rendered raw wire words ("mergeable (behind)",
// "conflicts: …") with no color. Display-only fix: one shared
// mergeabilityDisplay(state, detail) in web/src/lib/pull-state.js maps
// every mergeState() return AND every mergeable.state wire value to a
// human phrase + tone (green = Able, soft red = blocked states, muted
// zinc = pending/terminal), applied at the sidebar mergeability value.
// Forgejo #592 removed the second call site (the MergeBox state line)
// so the sidebar is the ONE headline — the helper itself is unchanged.
// Wire and internal values stay byte-identical — mergeState returns,
// mergeable.state comparisons, merge-button enable logic untouched.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import {
  mergeabilityDisplay,
  MERGEABILITY_OK_CLS,
  MERGEABILITY_BAD_CLS,
  MERGEABILITY_MUTED_CLS,
  MERGEABILITY_BEHIND_SUB,
} from "../../src/lib/pull-state.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const PULL = srcOf("../../src/pages/Pull.jsx");
const MERGEBOX = srcOf("../../src/components/MergeBox.jsx");
const LIB = srcOf("../../src/lib/pull-state.js");
const UICSS = srcOf("../../src/ui.css");

const GREEN = "text-emerald-600 dark:text-emerald-400";
const RED = "text-red-600 dark:text-red-400";
const MUTED = "text-zinc-500 dark:text-zinc-400";

test("clean/behind read Able to be Merged in green; behind adds the update sub-line", () => {
  for (const s of ["clean", "mergeable"]) {
    const v = mergeabilityDisplay(s, {});
    assert.equal(v.text, "Able to be Merged");
    assert.equal(v.cls, GREEN);
    assert.equal(v.sub, null);
  }
  const wireBehind = mergeabilityDisplay("behind", {});
  assert.equal(wireBehind.text, "Able to be Merged");
  assert.equal(wireBehind.cls, GREEN);
  assert.equal(wireBehind.sub, MERGEABILITY_BEHIND_SUB);
  const machineBehind = mergeabilityDisplay("mergeable", { mergeable: { state: "behind" } });
  assert.equal(machineBehind.text, "Able to be Merged");
  assert.equal(machineBehind.cls, GREEN);
  assert.equal(machineBehind.sub, MERGEABILITY_BEHIND_SUB);
});

test("blocked headline follows the mergeState priority: checks, then reviews, then conflicts", () => {
  assert.deepEqual(mergeabilityDisplay("blocked", { checksBlockers: ["ci (missing)"] }), {
    text: "Checks failing",
    cls: RED,
    sub: null,
  });
  assert.deepEqual(
    mergeabilityDisplay("blocked", { reviewDecision: "CHANGES_REQUESTED" }),
    { text: "Changes requested", cls: RED, sub: null },
  );
  assert.deepEqual(mergeabilityDisplay("blocked", { mergeable: { state: "dirty" } }), {
    text: "Merge conflicts",
    cls: RED,
    sub: null,
  });
  assert.deepEqual(mergeabilityDisplay("dirty", {}), {
    text: "Merge conflicts",
    cls: RED,
    sub: null,
  });
  // Every cause present: checks win (the mergeState order).
  assert.equal(
    mergeabilityDisplay("blocked", {
      checksBlockers: ["ci (missing)"],
      reviewDecision: "CHANGES_REQUESTED",
      mergeable: { state: "dirty" },
    }).text,
    "Checks failing",
  );
  // Draft wins over every blocker (the mergeState order: draft first).
  assert.equal(
    mergeabilityDisplay("draft", {
      checksBlockers: ["ci (missing)"],
      reviewDecision: "CHANGES_REQUESTED",
      mergeable: { state: "dirty" },
    }).text,
    "Draft pull request",
  );
});

test("five blocked conditions render soft red; file/task detail stays out of the headline", () => {
  const blocked = [
    mergeabilityDisplay("draft", {}),
    mergeabilityDisplay("blocked", { checksBlockers: ["ci"] }),
    mergeabilityDisplay("blocked", { reviewDecision: "CHANGES_REQUESTED" }),
    mergeabilityDisplay("blocked", { mergeable: { state: "dirty", conflicts: ["a.js", "b.js"] } }),
    mergeabilityDisplay("failed", {}),
  ];
  const texts = blocked.map((v) => v.text).sort();
  assert.deepEqual(texts, [
    "Changes requested",
    "Checks failing",
    "Draft pull request",
    "Merge conflicts",
    "Merge failed",
  ].sort());
  for (const v of blocked) assert.equal(v.cls, RED);
  // The conflict file list stays in the amber reasons line, not the headline.
  assert.ok(!mergeabilityDisplay("blocked", { mergeable: { state: "dirty", conflicts: ["a.js"] } }).text.includes("a.js"));
  // The task error stays in the task line, not the headline.
  assert.ok(!mergeabilityDisplay("failed", { task: { error: "boom" } }).text.includes("boom"));
});

test("zero-checks decision: pending + zeroChecks reads No checks required (NEUTRAL, not red)", () => {
  // An empty required set blocks nothing (checks-empty.js:
  // requiredCheckBlockers([]) === []), so red would falsely signal
  // blocked — muted zinc, and only on the pending headline: a clean
  // wire still reads Able to be Merged.
  assert.deepEqual(mergeabilityDisplay("ready", { zeroChecks: true }), {
    text: "No checks required",
    cls: MUTED,
    sub: null,
  });
  assert.equal(mergeabilityDisplay("clean", { zeroChecks: true }).text, "Able to be Merged");
  assert.equal(mergeabilityDisplay("ready", {}).text, "Checking mergeability");
});

test("pending/terminal states read muted: ready, merging, already-merged", () => {
  assert.deepEqual(mergeabilityDisplay("ready", {}), {
    text: "Checking mergeability",
    cls: MUTED,
    sub: null,
  });
  assert.deepEqual(mergeabilityDisplay("merging", {}), {
    text: "Merging…",
    cls: MUTED,
    sub: null,
  });
  assert.deepEqual(mergeabilityDisplay("merged", {}), {
    text: "Already merged",
    cls: MUTED,
    sub: null,
  });
  assert.deepEqual(mergeabilityDisplay("up_to_date", {}), {
    text: "Already merged",
    cls: MUTED,
    sub: null,
  });
});

test("unknown/missing decision: missing reads Unknown, unrecognized passes through (#561 precedent)", () => {
  for (const s of [null, undefined, ""]) {
    assert.deepEqual(mergeabilityDisplay(s, {}), { text: "Unknown", cls: MUTED, sub: null });
  }
  assert.equal(mergeabilityDisplay("SOME_FUTURE_STATE", {}).text, "SOME_FUTURE_STATE");
});

test("tone constants are Tailwind utilities only, emerald/red families, both themes", () => {
  assert.equal(MERGEABILITY_OK_CLS, GREEN);
  assert.equal(MERGEABILITY_BAD_CLS, RED);
  assert.equal(MERGEABILITY_MUTED_CLS, MUTED);
  assert.ok(GREEN.includes("dark:"), "green carries its dark variant");
  assert.ok(RED.includes("dark:"), "soft red carries its dark variant");
  assert.ok(!LIB.includes("mergeabilityDisplay") || !UICSS.includes("mergeability"), "no ad-hoc CSS rule for the helper");
  assert.ok(!MERGEBOX.includes("style="), "MergeBox carries no inline style");
});

test("exactly one display mapping lives in pull-state.js", () => {
  const defs = LIB.match(/function mergeabilityDisplay\(/g) ?? [];
  assert.equal(defs.length, 1, "exactly one mapping definition");
});

test("MergeBox renders NO status headline (Forgejo #592 — the #588 state line is gone)", () => {
  // #592 deleted the disp() memo + its <p> headline: the phrase must not
  // render twice. The helper import goes with it (sidebar owns the one
  // call site); the amber reasons line, tooltip, mergeState, and the
  // enabled() gate stay (pinned in the wire-intact test below).
  assert.ok(!MERGEBOX.includes("from \"../lib/pull-state.js\""), "helper import gone with the memo");
  assert.ok(!MERGEBOX.includes("mergeabilityDisplay("), "MergeBox no longer calls the helper");
  assert.ok(!MERGEBOX.includes("disp()"), "the disp() memo is gone, not forked");
  assert.ok(!MERGEBOX.includes("{disp().text}"), "no headline phrase renders in the MergeBox");
  assert.ok(!MERGEBOX.includes("{disp().sub}"), "no behind sub-line renders in the MergeBox");
  assert.ok(!MERGEBOX.includes("zeroChecks"), "the zeroChecks prop is gone with the memo");
  assert.ok(!PULL.includes("zeroChecks={zeroChecks}"), "the call-site arg is gone with the prop");
  // Headless DOM assertion: the sidebar still maps every tone through
  // the shared helper (green renders emerald, blocked soft red).
  assert.equal(mergeabilityDisplay("mergeable", {}).cls, GREEN);
  assert.equal(mergeabilityDisplay("blocked", { checksBlockers: ["ci"] }).cls, RED);
});

test("sidebar mergeability value renders the helper (mergeableText replaced)", () => {
  assert.ok(!PULL.includes("mergeableText"), "mergeableText is gone, not forked");
  assert.ok(PULL.includes("mergeabilityDisplay"), "sidebar maps through the shared helper");
  assert.ok(PULL.includes("mergeabilityView(mergeable(),"), "sidebar passes wire + page context");
  assert.ok(!PULL.includes('"mergeable (behind)"') && !PULL.includes("conflicts: ${"), "raw sidebar vocabulary gone");
  assert.ok(!PULL.includes(">unknown</p>") && !PULL.includes('"checking…"'), "raw fallback vocabulary gone");
});

test("wire/internal values stay byte-identical (returns, comparisons, enable logic, tooltips, reasons)", () => {
  assert.ok(MERGEBOX.includes('if (props.mergeable?.state === "dirty") return "blocked"'), "mergeState dirty arm intact");
  assert.ok(
    MERGEBOX.includes('props.mergeable?.state === "clean" || props.mergeable?.state === "behind"'),
    "mergeState clean/behind arm intact",
  );
  assert.ok(MERGEBOX.includes('props.reviewDecision === "CHANGES_REQUESTED"'), "mergeState review arm intact");
  assert.ok(MERGEBOX.includes('state() === "mergeable" && canMerge()'), "merge-button enable logic untouched");
  assert.ok(MERGEBOX.includes("blocked: ${b.join"), "disabled tooltip keeps machine wording");
  assert.ok(MERGEBOX.includes("blocking merge: {blockers().join"), "amber reasons line kept");
  assert.ok(MERGEBOX.includes("update branch"), "update-branch remediation kept");
  assert.ok(PULL.includes('mergeable={mergeable()}'), "mergeable prop still rides the wire object");
});
