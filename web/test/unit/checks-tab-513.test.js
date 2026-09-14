// web/test/unit/checks-tab-513.test.js — issue #513: the Checks tab hid
// on check-less repos for readers (#505) but stayed visible forever for
// viewers the server refuses a summary to (anonymous GET …/api → 401 on a
// gated repo): getSummary() never settles, so the #505 fail-open was the
// steady state, not a transient. The deployed #505 SPA was verified live
// (bundle carries the has_checks gate + reporting-API pointer — deploy
// lag ruled out), and the 401 is correct server behavior, so the fix is
// the decided policy: the shell distinguishes "summary loading/absent"
// from "summary explicitly auth-denied" and hides Checks on denial,
// keeping loading fail-open (flicker/old-server reasons). The rule stays
// the pure showChecksTab helper (settingsNav convention — no Solid, no
// DOM); the denial flag lives in the shell (Repo.jsx summaryDenied).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import { showChecksTab } from "../../src/lib/tabs.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");

function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}

const REPO = srcOf("../../src/pages/Repo.jsx");

test("showChecksTab: denial hides, loading still fails open", () => {
  // The #513 matrix: every summary state × denial flag. Denied always
  // hides (the viewer reaches no check data); non-denied keeps the
  // exact #505 rule (false hides, everything else shows).
  const cases = [
    // [summary, denied, expected]
    [undefined, false, true], // loading, first paint — no flicker
    [undefined, true, false], // 401 steady state — the live #513 case
    [null, false, true], // deleted — the #200 "not found" shell
    [null, true, false], // denied wins over absent too
    [{ has_checks: true }, false, true],
    [{ has_checks: true }, true, false],
    [{ has_checks: false }, false, false], // #505 zero-check hide
    [{ has_checks: false }, true, false],
    [{}, false, true], // pre-#505 server — no strand
    [{}, true, false],
    [{ open_issues: 3 }, false, true],
    [{ open_issues: 3 }, true, false],
  ];
  for (const [summary, denied, expected] of cases) {
    assert.equal(
      showChecksTab(summary, { denied }),
      expected,
      `summary=${JSON.stringify(summary)} denied=${denied}`,
    );
  }
});

test("showChecksTab: single-arg calls keep the #505 contract", () => {
  // Backward compatible — every existing caller shape (and the #505
  // test's bare calls) still behaves exactly as before.
  assert.equal(showChecksTab({ has_checks: true }), true);
  assert.equal(showChecksTab({ has_checks: false }), false);
  for (const s of [undefined, null, {}, { open_issues: 0 }]) {
    assert.equal(showChecksTab(s), true, String(s));
  }
  assert.equal(showChecksTab(undefined, {}), true);
  assert.equal(showChecksTab(undefined, { denied: false }), true);
});

test("Repo.jsx: the shell records an explicit summary denial", () => {
  // The fetcher catches only the SDK 401 (err.unauthorized — 404 still
  // maps to null via tolerateMissing, every other error still throws
  // into the tray path), resolves undefined (still "loading", never
  // "not found" — 401 is not deletion), and the flag resets at each
  // fetch start so repo switches and sign-ins never inherit it.
  assert.ok(REPO.includes("summaryDenied"), "the shell owns a denial signal");
  assert.ok(REPO.includes("tolerateMissing(repoClient.get(), null)"), "the #200 404→null mapping is untouched");
  assert.ok(REPO.includes("err?.unauthorized"), "only the explicit 401 records a denial");
  assert.ok(REPO.includes("setSummaryDenied(true)"), "denial sets the flag");
  assert.ok(REPO.includes("setSummaryDenied(false)"), "fetch start clears a stale denial");
  assert.ok(
    REPO.includes("showChecksTab(getSummary(), { denied: summaryDenied() })"),
    "the Checks gate reads the flag; loading (flag clear) still fails open",
  );
});

test("Repo.jsx: #513 changes nothing else about the tab contract", () => {
  // /api#checks-ci pointer (Repo.jsx meta line), deep-link empty state
  // (route + activeTab segments), #319 badge, TABS model, and the
  // hidden-state check stream all keep their #505 shapes.
  assert.ok(REPO.includes('href="/api#checks-ci"'), "reporting-API pointer kept");
  assert.ok(REPO.includes("checksHidden"), "hidden-state stream memo kept");
  assert.ok(REPO.includes("tabBadge(getSummary(), t.id)"), "#319 badge wiring kept");
  assert.ok(REPO.includes('t.id !== "checks" ||'), "only the Checks entry gates");
  assert.ok(REPO.includes('<For each={TABS}>'), "the full TABS render kept (the #274 pin)");
});
