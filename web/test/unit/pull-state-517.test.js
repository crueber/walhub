import { test } from "node:test";
import assert from "node:assert/strict";

import {
  normPrincipal,
  isPullAuthor,
  roleAtLeastTriage,
  canModifyPullState,
  pullBadgeView,
  pullCloseVisibility,
} from "../../src/lib/pull-state.js";

test("normPrincipal mirrors the backend (lowercase + trim)", () => {
  assert.equal(normPrincipal("  Alice@Example.com "), "alice@example.com");
  assert.equal(normPrincipal(null), "");
  assert.equal(normPrincipal(undefined), "");
});

test("author match is case-insensitive, empty never matches", () => {
  assert.equal(isPullAuthor("alice@x", "Alice@X"), true);
  assert.equal(isPullAuthor("alice@x", "bob@x"), false);
  assert.equal(isPullAuthor("", "alice@x"), false);
  assert.equal(isPullAuthor("alice@x", null), false);
  assert.equal(isPullAuthor(null, null), false);
});

test("triage floor is hierarchical (write/maintain/admin pass, read/null fail)", () => {
  for (const role of ["triage", "write", "maintain", "admin"]) {
    assert.equal(roleAtLeastTriage(role), true, role);
  }
  assert.equal(roleAtLeastTriage("read"), false);
  assert.equal(roleAtLeastTriage(null), false);
  assert.equal(roleAtLeastTriage(undefined), false);
  assert.equal(roleAtLeastTriage("bogus"), false);
});

test("canModifyPullState: author (any role) or triage+ (non-author)", () => {
  // Author with only read still modifies (backend: author OR triage).
  assert.equal(canModifyPullState({ author: "a@x", mePrincipal: "a@x", role: "read" }), true);
  // Triage closes others' PRs.
  assert.equal(canModifyPullState({ author: "a@x", mePrincipal: "t@x", role: "triage" }), true);
  // Write implies triage (hierarchical ladder).
  assert.equal(canModifyPullState({ author: "a@x", mePrincipal: "w@x", role: "write" }), true);
  // Below-triage non-author cannot.
  assert.equal(canModifyPullState({ author: "a@x", mePrincipal: "r@x", role: "read" }), false);
  // Anonymous cannot (null principal + null role).
  assert.equal(canModifyPullState({ author: "a@x", mePrincipal: null, role: null }), false);
});

test("badge: open/closed mirror the issue classes, merged wins", () => {
  assert.deepEqual(pullBadgeView({ state: "open" }, {}), { text: "Open", cls: "chip chip-open" });
  assert.deepEqual(pullBadgeView({ state: "closed" }, {}), { text: "Closed", cls: "chip chip-closed" });
  // Merge stamps StateClosed too — pr.merged is what tells them apart.
  assert.deepEqual(pullBadgeView({ state: "closed" }, { merged: true }), { text: "Merged", cls: "chip chip-merged" });
  assert.deepEqual(pullBadgeView({ state: "open" }, { merged: true }), { text: "Merged", cls: "chip chip-merged" });
  // Loading (no thread yet): badge reads open, like the thread default.
  assert.deepEqual(pullBadgeView(undefined, {}), { text: "Open", cls: "chip chip-open" });
});

test("visibility: open shows Close for author/triage, hidden below triage", () => {
  const open = { state: "open", author: "a@x" };
  assert.deepEqual(
    pullCloseVisibility({ thread: open, pr: {}, mePrincipal: "a@x", role: "read" }),
    { showClose: true, showReopen: false },
  );
  assert.deepEqual(
    pullCloseVisibility({ thread: open, pr: {}, mePrincipal: "t@x", role: "triage" }),
    { showClose: true, showReopen: false },
  );
  assert.deepEqual(
    pullCloseVisibility({ thread: open, pr: {}, mePrincipal: "r@x", role: "read" }),
    { showClose: false, showReopen: false },
  );
  assert.deepEqual(
    pullCloseVisibility({ thread: open, pr: {}, mePrincipal: null, role: null }),
    { showClose: false, showReopen: false },
  );
});

test("visibility: closed-unmerged shows Reopen under the same rule", () => {
  const closed = { state: "closed", author: "a@x" };
  assert.deepEqual(
    pullCloseVisibility({ thread: closed, pr: {}, mePrincipal: "a@x", role: "read" }),
    { showClose: false, showReopen: true },
  );
  assert.deepEqual(
    pullCloseVisibility({ thread: closed, pr: {}, mePrincipal: "t@x", role: "triage" }),
    { showClose: false, showReopen: true },
  );
  assert.deepEqual(
    pullCloseVisibility({ thread: closed, pr: {}, mePrincipal: "r@x", role: "read" }),
    { showClose: false, showReopen: false },
  );
});

test("visibility: merged PRs expose no control to anyone", () => {
  for (const role of ["read", "triage", "write", "maintain", "admin"]) {
    assert.deepEqual(
      pullCloseVisibility({ thread: { state: "closed", author: "a@x" }, pr: { merged: true }, mePrincipal: "a@x", role }),
      { showClose: false, showReopen: false },
      role,
    );
  }
  // Even a merged PR whose thread still reads open (merge stamp pending).
  assert.deepEqual(
    pullCloseVisibility({ thread: { state: "open", author: "a@x" }, pr: { merged: true }, mePrincipal: "a@x", role: "admin" }),
    { showClose: false, showReopen: false },
  );
});
