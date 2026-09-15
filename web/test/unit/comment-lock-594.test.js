// web/test/unit/comment-lock-594.test.js — Forgejo #594: the comment lock
// client contract. The lock helpers key on CURRENT thread state (reopen
// restores: state open → unlocked), merged wins with its own reason, and a
// server 409 lands in the error tray verbatim (toast, never a raw
// TypeError). The stream-reopen unlock is structural: the pages key
// `disabled` off the live thread/pr fetch, which the collab stream
// invalidates — so these helpers returning unlocked for state open IS the
// no-reload unlock.

import { test } from "node:test";
import assert from "node:assert/strict";

import { issueCommentLock } from "../../src/lib/issue-events.js";
import { pullCommentLock, pullCloseVisibility } from "../../src/lib/pull-state.js";
import { reportError, trayErrors } from "../../src/lib/data.js";
import { ReposError } from "../../sdk/src/errors.js";

test("issueCommentLock: open/unloading unlocked, closed locked with reason", () => {
  assert.deepEqual(issueCommentLock({ state: "open" }), { locked: false, reason: null });
  assert.deepEqual(issueCommentLock(undefined), { locked: false, reason: null });
  const closed = issueCommentLock({ state: "closed" });
  assert.equal(closed.locked, true);
  assert.match(closed.reason, /closed/i);
});

test("pullCommentLock: open unlocked, plain-closed locked, merged wins", () => {
  assert.deepEqual(pullCommentLock({ state: "open" }, {}), { locked: false, reason: null });
  assert.deepEqual(pullCommentLock(undefined, undefined), { locked: false, reason: null });
  const closed = pullCommentLock({ state: "closed" }, { merged: false });
  assert.equal(closed.locked, true);
  assert.match(closed.reason, /closed/i);
  // Merged wins over state either way, with the merged reason.
  for (const thread of [{ state: "closed" }, { state: "open" }]) {
    const m = pullCommentLock(thread, { merged: true });
    assert.equal(m.locked, true);
    assert.match(m.reason, /[Mm]erg/);
  }
});

test("pullCommentLock: reopen restores (state open → unlocked)", () => {
  // The no-reload unlock: the pages re-read this helper off the
  // stream-refreshed fetch, so a reopened thread computes unlocked.
  assert.deepEqual(pullCommentLock({ state: "open" }, { merged: false }), { locked: false, reason: null });
  assert.deepEqual(issueCommentLock({ state: "open" }), { locked: false, reason: null });
});

test("no reopen affordance for merged PRs (client already correct — pinned)", () => {
  // Binds the #594 merged-terminal contract client-side: even the author
  // or an admin sees no lifecycle control on a merged PR (the server 409s
  // both close and reopen there).
  for (const role of ["read", "triage", "write", "maintain", "admin"]) {
    assert.deepEqual(
      pullCloseVisibility({ thread: { state: "closed", author: "a@x" }, pr: { merged: true }, mePrincipal: "a@x", role }),
      { showClose: false, showReopen: false },
      role,
    );
  }
  // Closed-but-unmerged keeps Reopen for author/triage (reopen restores).
  assert.deepEqual(
    pullCloseVisibility({ thread: { state: "closed", author: "a@x" }, pr: { merged: false }, mePrincipal: "a@x", role: "read" }),
    { showClose: false, showReopen: true },
  );
});

test("a server 409 toasts verbatim (no raw TypeError)", () => {
  // The composer submit paths catch into reportError (tray toast); the
  // tray renders err.message verbatim, so the server's human-readable
  // lock reason is what the user sees. The 10 s fade timer is stubbed —
  // data-guard avoids arming it; this test needs the push itself.
  const realTimeout = globalThis.setTimeout;
  globalThis.setTimeout = () => 0;
  try {
    const serverText = "issue is closed: commenting is locked";
    reportError(new ReposError(409, serverText), "issue-comment-594");
    const entries = trayErrors();
    const hit = entries.filter((e) => e.key === "issue-comment-594");
    assert.equal(hit.length, 1);
    assert.equal(hit[0].message, serverText);
  } finally {
    globalThis.setTimeout = realTimeout;
  }
});
