// web/test/unit/terminal-sidebar-602.test.js — Forgejo #602: terminal PRs
// (merged, or closed unmerged) swap the sidebar's Review summary slot for
// a Status section and hide the moot Mergeability section. Open PRs are
// unchanged; Reviewers / Checks keep rendering on terminal PRs.
// Pure UI branching in web/src/pages/Pull.jsx — no new wire fields, no API
// change (everything reads the page payload already in hand).
//
// - isTerminalPull(thread, pr): merged wins (merge stamps StateClosed too,
//   so state alone cannot tell merged from plain-closed), then closed via
//   the pr spelling with the thread-header fallback; loading reads open.
// - terminalMergeDetail(pr, events): pr sidecar fields win, the merged
//   timeline event covers a sidecar that has not caught up.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import { isTerminalPull, terminalMergeDetail } from "../../src/lib/pull-state.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const PULL = srcOf("../../src/pages/Pull.jsx");
const LIB = srcOf("../../src/lib/pull-state.js");
const UICSS = srcOf("../../src/ui.css");

function asideOf(src) {
  const start = src.indexOf('<aside aria-label="Details"');
  assert.ok(start > 0, "aside found");
  return src.slice(start, src.indexOf("</aside>", start));
}

const openThread = { state: "open" };
const closedThread = { state: "closed" };
const plainPr = { merged: false };
const mergedPr = { merged: true };

test("isTerminalPull: open reads non-terminal, loading reads open", () => {
  assert.equal(isTerminalPull(openThread, plainPr), false);
  assert.equal(isTerminalPull(undefined, undefined), false);
  assert.equal(isTerminalPull(openThread, undefined), false);
  assert.equal(isTerminalPull(undefined, plainPr), false);
});

test("isTerminalPull: merged wins over either state (stamp-in-flight included)", () => {
  assert.equal(isTerminalPull(openThread, mergedPr), true);
  assert.equal(isTerminalPull(closedThread, mergedPr), true);
  assert.equal(isTerminalPull({ state: "open" }, { merged: true }), true);
});

test("isTerminalPull: plain-closed is terminal via thread or pr spelling", () => {
  assert.equal(isTerminalPull(closedThread, plainPr), true);
  assert.equal(isTerminalPull(closedThread, undefined), true);
  assert.equal(isTerminalPull(undefined, { state: "closed" }), true);
  assert.equal(isTerminalPull(openThread, { state: "closed", merged: false }), true);
});

test("terminalMergeDetail: pr sidecar wins, merged event is the fallback", () => {
  const pr = { merge_commit_sha: "pr".padEnd(40, "a"), merge_strategy: "squash", merged_by: "alice" };
  const events = [{ type: "merged", merge_commit_sha: "ev".padEnd(40, "b"), strategy: "merge", actor: "bob" }];
  assert.deepEqual(terminalMergeDetail(pr, events), {
    sha: pr.merge_commit_sha,
    strategy: "squash",
    by: "alice",
  });
  // Sidecar not caught up yet: the merged event carries SHA + strategy.
  assert.deepEqual(
    terminalMergeDetail({ merged: true }, events),
    { sha: events[0].merge_commit_sha, strategy: "merge", by: "bob" }
  );
});

test("terminalMergeDetail: missing reads empty (chip alone suffices)", () => {
  assert.deepEqual(terminalMergeDetail(undefined, []), { sha: "", strategy: "", by: "" });
  assert.deepEqual(terminalMergeDetail({ merged: true }, [{ type: "opened" }]), {
    sha: "",
    strategy: "",
    by: "",
  });
  assert.deepEqual(terminalMergeDetail(undefined, undefined), { sha: "", strategy: "", by: "" });
});

test("helpers live beside pullBadgeView/pullCloseVisibility, no new deps", () => {
  assert.match(LIB, /export function isTerminalPull\(thread, pr\)/);
  assert.match(LIB, /export function terminalMergeDetail\(pr, events/);
  assert.match(
    PULL,
    /import \{[^}]*isTerminalPull[^}]*terminalMergeDetail[^}]*\} from "\.\.\/lib\/pull-state\.js"/
  );
});

test("terminal Status REPLACES the Review summary slot (one slot, never both)", () => {
  const aside = asideOf(PULL);
  // The first slot is a terminal Show whose fallback IS the old summary block.
  assert.match(aside, /<Show when=\{isTerminal\(\)\} fallback=\{/);
  const afterShow = aside.slice(aside.indexOf("<Show when={isTerminal()} fallback={"));
  const statusAt = afterShow.indexOf(">Status</span>");
  assert.ok(statusAt > 0, "Status section inside the terminal branch");
  // The fallback attribute textually precedes the children: the open-PR
  // summary lives between the Show open and the Status child.
  const head = afterShow.slice(0, statusAt);
  assert.ok(head.includes(">Review summary</span>"), "Review summary kept as the open-PR fallback");
  assert.ok(head.includes("<ReviewSummaryBar summary={summary()} head={head()} />"), "open-PR summary body intact");
  assert.match(aside, /<span class="mb-1 block text-xs font-medium uppercase text-zinc-500 dark:text-zinc-400">Status<\/span>/);
  assert.ok(afterShow.slice(statusAt).includes("</Show>"), "Status branch closes its Show");
});

test("Status value: merged chip + SHA/strategy detail, closed chip", () => {
  const aside = asideOf(PULL);
  assert.ok(aside.includes('<span class="chip chip-merged">Merged</span>'), "merged reuses chip-merged");
  assert.ok(aside.includes('<span class="chip chip-closed">Closed</span>'), "plain-closed reuses chip-closed");
  assert.match(aside, /<Show when=\{pr\(\)\?\.merged\} fallback=\{<span class="chip chip-closed">Closed<\/span>\}>/);
  // #595 idiom reused: full SHA in href, 12-char text, commit page.
  assert.match(aside, /<A class="link font-mono" href=\{\`\/\$\{ctx\.full\}\/commit\/\$\{mergedDetail\(\)\.sha\}\`\}>\s*\{mergedDetail\(\)\.sha\.slice\(0, 12\)\}/);
  assert.ok(aside.includes("mergedDetail().strategy"), "strategy rides the detail line");
  assert.ok(aside.includes("mergedDetail().by"), "merger credit rides the detail line");
  assert.match(aside, /<Show when=\{pr\(\)\?\.merged && mergedDetail\(\)\.sha\}>/);
});

test("Mergeability block is gated on open (hidden on terminal PRs)", () => {
  const aside = asideOf(PULL);
  const gateAt = aside.indexOf("<Show when={!isTerminal()}>");
  assert.ok(gateAt > 0, "open-only gate present");
  const mergeAt = aside.indexOf(">Mergeability</span>", gateAt);
  assert.ok(mergeAt > gateAt, "Mergeability section sits inside the open-only gate");
  const gateClose = aside.indexOf("</Show>", mergeAt);
  const reviewersAt = aside.indexOf(">Reviewers</span>");
  assert.ok(gateClose > mergeAt && gateClose < reviewersAt, "gate closes before Reviewers");
  assert.ok(aside.includes("mergeabilityView(mergeable(),"), "value line untouched inside the gate");
});

test("Reviewers + Checks stay outside the gate (render on terminal PRs)", () => {
  const aside = asideOf(PULL);
  const gateClose = aside.indexOf("</Show>", aside.indexOf(">Mergeability</span>"));
  for (const t of [">Reviewers</span>", ">Checks</span>", "<ReviewersPanel", "<CheckPill"]) {
    assert.ok(aside.indexOf(t, gateClose) > gateClose, `${t} renders after (outside) the terminal gate`);
  }
});

test("open-PR sections byte-identical: summary, mergeability value, reviewers, checks, merge", () => {
  const aside = asideOf(PULL);
  assert.ok(aside.includes("mergeabilityView(mergeable(),"), "mergeability value line kept");
  assert.ok(aside.includes("pr()?.base?.ref") && aside.includes("pr()?.head?.ref"), "base/head refs kept");
  assert.ok(aside.includes("/commits") && aside.includes("/files"), "commits/files links kept");
  assert.ok(aside.includes("<ReviewersPanel"), "reviewers panel kept");
  assert.ok(aside.includes("<CheckPill"), "checks pill kept");
  assert.ok(aside.includes("<MergeBox"), "merge section kept");
  // No wire/API change: the sidebar adds no fetch — helpers read the page payload.
  assert.ok(!aside.includes("repoClient.pulls.get"), "no new PR fetch in the sidebar");
  assert.ok(!aside.includes("repoClient.pulls.diff"), "no new diff fetch in the sidebar");
});

test("Tailwind-only: existing chips + shared link, no new card chrome, no new CSS", () => {
  const aside = asideOf(PULL);
  assert.ok(!aside.includes("card-header"), "no card-header headings (the #531 idiom holds)");
  assert.ok(aside.includes('class="link font-mono"'), "SHA link composes the shared .link rule (#568)");
  // No new ui.css selector for the Status section (the #568 comment census
  // grows 25 → 26 in prose only — the shared rule covers the new site).
  assert.ok(!/\.terminal|\.pull-status|\.pr-status/.test(UICSS), "no new ui.css selector");
  assert.ok(UICSS.includes("#602 terminal-Status"), "the shared-rule comment census covers the new site");
  const pkg = JSON.parse(srcOf("../../package.json"));
  assert.deepEqual(
    Object.keys(pkg.dependencies ?? {}).sort(),
    ["@solidjs/router", "dompurify", "marked", "solid-js"],
    "no new runtime deps (law 1)"
  );
});

test("law-12: the web-UI decision lands in the same change", () => {
  assert.match(srcOf("../../../docs/go/12_web_ui.md"), /#602/, "12_web_ui.md carries the #602 decision");
});
