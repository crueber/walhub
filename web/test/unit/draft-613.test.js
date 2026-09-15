// web/test/unit/draft-613.test.js — Forgejo #613 (option a): draft PRs.
//
// Backend contract (internal/pulls/service.go): `POST …/pulls` accepts
// `draft` (omitted = ready); `PUT …/pulls/{num}` `{draft: bool}` flips
// ready↔draft for author-or-triage (409 on merged); flips append
// `draft_changed` thread events (from/to "draft"/"ready") with
// `ready_for_review` / `converted_to_draft` stream + notify fan-out; the
// merge task refuses drafts with a narrated 409.
// UI contract: PullNew opens as draft via checkbox (buildOpenCall carries
// `draft` only when true, so plain opens stay byte-identical); the Pull
// header badge reads Draft on open drafts; the mark-ready / convert-to-draft
// toggle gates like Close/Reopen (pullDraftVisibility); the MergeBox draft
// arm (mergeState → "draft", disabled button + tooltip) is live now that
// pr.draft can actually be true.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

import {
  pullBadgeView,
  pullListChip,
  pullDraftVisibility,
  pullEventText,
  mergeabilityDisplay,
  MERGEABILITY_BAD_CLS,
} from "../../src/lib/pull-state.js";
import { buildOpenCall } from "../../src/lib/pr-composer.js";
import { ReposClient } from "../../sdk/src/index.js";
import { fakeFetch, jsonResponse } from "../helpers/fetch.js";

const require = createRequire(import.meta.url);
const fs = require("node:fs");
function srcOf(rel) {
  return fs.readFileSync(new URL(rel, import.meta.url), "utf8");
}
const MERGEBOX = srcOf("../../src/components/MergeBox.jsx");
const PULL = srcOf("../../src/pages/Pull.jsx");
const PULLNEW = srcOf("../../src/pages/PullNew.jsx");

// --- Half 1: header badge + list chip read the draft flag ---

test("badge: open draft reads Draft, closed draft reads Closed, merged wins", () => {
  assert.deepEqual(pullBadgeView({ state: "open" }, { draft: true }), { text: "Draft", cls: "chip chip-draft" });
  assert.deepEqual(pullBadgeView({ state: "closed" }, { draft: true }), { text: "Closed", cls: "chip chip-closed" });
  assert.deepEqual(pullBadgeView({ state: "open" }, { draft: true, merged: true }), { text: "Merged", cls: "chip chip-merged" });
  // Non-draft shapes are untouched (the #517 pin).
  assert.deepEqual(pullBadgeView({ state: "open" }, {}), { text: "Open", cls: "chip chip-open" });
  assert.deepEqual(pullBadgeView({ state: "open" }, { draft: false }), { text: "Open", cls: "chip chip-open" });
});

test("list chip: open draft reads draft, terminal states win", () => {
  assert.deepEqual(pullListChip({ state: "open", draft: true }), { text: "draft", cls: "chip chip-draft" });
  assert.deepEqual(pullListChip({ state: "closed", draft: true }), { text: "closed", cls: "chip chip-closed" });
  assert.deepEqual(pullListChip({ state: "open", draft: true, merged: true }), { text: "merged", cls: "chip chip-merged" });
  assert.deepEqual(pullListChip({ state: "open" }), { text: "open", cls: "chip chip-open" });
});

// --- Half 2: the toggle gates like Close/Reopen ---

test("draft visibility: draft shows Mark-ready, ready shows Convert, same author-or-triage rule", () => {
  const draft = { state: "open", author: "a@x" };
  const ready = { state: "open", author: "a@x" };
  // Author (any role) flips both ways.
  assert.deepEqual(
    pullDraftVisibility({ thread: draft, pr: { draft: true }, mePrincipal: "a@x", role: "read" }),
    { showReady: true, showDraft: false },
  );
  assert.deepEqual(
    pullDraftVisibility({ thread: ready, pr: {}, mePrincipal: "a@x", role: "read" }),
    { showReady: false, showDraft: true },
  );
  // Triage flips others' PRs.
  assert.deepEqual(
    pullDraftVisibility({ thread: draft, pr: { draft: true }, mePrincipal: "t@x", role: "triage" }),
    { showReady: true, showDraft: false },
  );
  // Below-triage non-author sees nothing.
  assert.deepEqual(
    pullDraftVisibility({ thread: draft, pr: { draft: true }, mePrincipal: "r@x", role: "read" }),
    { showReady: false, showDraft: false },
  );
  assert.deepEqual(
    pullDraftVisibility({ thread: draft, pr: { draft: true }, mePrincipal: null, role: null }),
    { showReady: false, showDraft: false },
  );
});

test("draft visibility: merged hides both; closed-but-unmerged still flips (orthogonal)", () => {
  const closed = { state: "closed", author: "a@x" };
  assert.deepEqual(
    pullDraftVisibility({ thread: { state: "open", author: "a@x" }, pr: { draft: true, merged: true }, mePrincipal: "a@x", role: "admin" }),
    { showReady: false, showDraft: false },
  );
  assert.deepEqual(
    pullDraftVisibility({ thread: closed, pr: { draft: true }, mePrincipal: "a@x", role: "read" }),
    { showReady: true, showDraft: false },
  );
  assert.deepEqual(
    pullDraftVisibility({ thread: closed, pr: {}, mePrincipal: "a@x", role: "read" }),
    { showReady: false, showDraft: true },
  );
});

// --- Half 3: the draft_changed timeline text ---

test("event text: draft_changed reads mark-ready / convert-to-draft", () => {
  assert.equal(pullEventText({ type: "draft_changed", from: "draft", to: "ready" }), "marked as ready for review");
  assert.equal(pullEventText({ type: "draft_changed", from: "ready", to: "draft" }), "converted to draft");
});

// --- Half 4: the composer carries draft only when true ---

test("buildOpenCall: draft rides only when true (wire compat)", () => {
  const base = { fromRepo: "o/r", fromRef: "refs/heads/feat", toRepo: "o/r", toRef: "refs/heads/main", title: "t" };
  assert.equal(buildOpenCall(base).payload.draft, undefined);
  assert.equal("draft" in buildOpenCall(base).payload, false);
  assert.equal(buildOpenCall({ ...base, draft: false }).payload.draft, undefined);
  assert.equal(buildOpenCall({ ...base, draft: true }).payload.draft, true);
});

// --- Half 5: the MergeBox draft branch is live ---

test("mergeabilityDisplay: draft machine state and draft PR shape read Draft pull request", () => {
  assert.deepEqual(mergeabilityDisplay("draft", {}).text, "Draft pull request");
  assert.equal(mergeabilityDisplay("draft", {}).cls, MERGEABILITY_BAD_CLS);
  // The exact shape Pull.jsx feeds the sidebar (draft: pr()?.draft).
  assert.deepEqual(mergeabilityDisplay({ state: "clean" }, { draft: true }).text, "Draft pull request");
});

test("MergeBox draft arm is wired live (source pins, the #592 pattern)", () => {
  // The machine parks drafts before every other non-terminal gate…
  assert.ok(MERGEBOX.includes('if (props.pr?.draft) return "draft"'), "mergeState draft arm intact");
  // …the button disables off mergeable (draft ≠ mergeable ⇒ disabled)…
  assert.ok(MERGEBOX.includes('const enabled = () => state() === "mergeable"'), "enabled gate intact");
  // …and the tooltip names the reason.
  assert.ok(MERGEBOX.includes('if (state() === "draft") return "draft PRs cannot merge"'), "draft tooltip intact");
});

test("Pull page feeds the live draft flag into every draft reader", () => {
  // Sidebar headline reads the live pr.draft (the mergeabilityDisplay Half-5 shape).
  assert.ok(PULL.includes("draft: pr()?.draft"), "sidebar passes live draft");
  // The toggle renders beside the badge, gated by pullDraftVisibility.
  assert.ok(PULL.includes("pullDraftVisibility"), "toggle gate wired");
  assert.ok(PULL.includes("Mark ready"), "mark-ready affordance renders");
  assert.ok(PULL.includes("Convert to draft"), "convert-to-draft affordance renders");
  // The toggle writes through the SDK update path with the draft key.
  assert.ok(PULL.includes("{ draft: false }"), "mark-ready writes draft:false");
  assert.ok(PULL.includes("{ draft: true }"), "convert writes draft:true");
});

test("PullNew carries the open-as-draft checkbox into the open call", () => {
  assert.ok(PULLNEW.includes("Open as draft"), "checkbox renders");
  assert.ok(PULLNEW.includes("draft: getDraft()"), "checkbox feeds buildOpenCall");
});

// --- Half 6: SDK wire compat ---

test("sdk pulls.open sends draft only when given; update passes draft through", async () => {
  const BASE = "http://api.test";
  const openWith = async (args) => {
    const { fetch, calls } = fakeFetch(() => jsonResponse({ thread: {}, pr: {} }));
    const client = new ReposClient({ base: BASE, fetch, token: "t" });
    await client.repo("o/r").pulls.open({ title: "t", base_ref: "refs/heads/main", head_ref: "refs/heads/topic", ...args });
    return JSON.parse(calls[0].init.body);
  };
  assert.equal((await openWith({ draft: true })).draft, true);
  assert.equal("draft" in (await openWith({})), false);
  const { fetch, calls } = fakeFetch(() => jsonResponse({ thread: {}, pr: {} }));
  const client = new ReposClient({ base: BASE, fetch, token: "t" });
  await client.repo("o/r").pulls.update(7, { draft: true });
  assert.equal(JSON.parse(calls[0].init.body).draft, true);
});
