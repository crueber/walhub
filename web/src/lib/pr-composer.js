// web/src/lib/pr-composer.js — PR composer direction + endpoint state (Forgejo #328).
//
// Pure helpers over the composer state; DOM-free so `node --test` covers
// them, PullNew.jsx keeps only fetch + render. The composer speaks From/To
// (From = where the changes come from = today's head; To = where they go =
// today's base) while the wire keeps the frozen names (`head_ref`,
// `base_ref`, `?base=`/`?head=` — law 12: wire keys never change meaning).
//
// Endpoint model: each side is {repo: "owner/name", ref: full refname}.
// The open call always targets the To (base) repo's client; a From repo
// different from the To repo feeds `fork: {repo}` (the OpenInput.ForkInfo
// path — the backend is ready, the composer just never sent one).
// Reachability note: same-repo unreachable heads 422 ("head commit not
// reachable — push first"); cross-fork unreachable heads open fine with
// HeadPublished=false (fork-local head, the PR page shows "From branch
// pending — push first"). So the "unreachable" UI error is the same-repo
// 422 surfaced inline, direction-mapped, never a bare status code.

import { toShortRef } from "./compare.js";

/** Composer label for the head side (changes come FROM here). */
export const FROM_LABEL = "From";

/** Composer label for the base side (changes go TO here). */
export const TO_LABEL = "To";

/** P6 ladder copy for permission filtering (canonical: components/perms.jsx). */
const LADDER = ["read", "triage", "write", "maintain", "admin"];

/** Rank a role (null/unknown → -1, below every real role). */
export function roleRank(role) {
  return LADDER.indexOf(role ?? "");
}

/** True when role reaches want on the P6 ladder. */
export function roleAtLeast(role, want) {
  return roleRank(role) >= roleRank(want);
}

const clean = (s) => String(s ?? "").trim();

/**
 * buildOpenCall({fromRepo, fromRef, toRepo, toRef, title, body}) →
 * {baseRepo, payload, cross}: the SDK `pulls.open` call. baseRepo names
 * the client to open through (`repos.repo(baseRepo)`); payload keeps the
 * frozen wire keys; `fork` is set exactly when the From repo differs from
 * the To repo. `cross` mirrors that decision for the caller.
 */
export function buildOpenCall({ fromRepo, fromRef, toRepo, toRef, title, body }) {
  const fr = clean(fromRepo);
  const tr = clean(toRepo);
  const cross = fr !== "" && tr !== "" && fr !== tr;
  const payload = {
    title,
    base_ref: clean(toRef),
    head_ref: clean(fromRef),
  };
  const b = clean(body);
  if (b) payload.body = b;
  if (cross) payload.fork = { repo: fr };
  return { baseRepo: tr, payload, cross };
}

/**
 * sameEndpoint(fromRepo, fromRef, toRepo, toRef) → true when both sides
 * name the same repo AND the same ref. A shared branch name across two
 * repos (both `refs/heads/main`) is NOT the same endpoint.
 */
export function sameEndpoint(fromRepo, fromRef, toRepo, toRef) {
  return clean(fromRepo) === clean(toRepo) && clean(fromRef) === clean(toRef);
}

/** True when the From repo differs from the To repo (fork path armed). */
export function isCrossRepo(fromRepo, toRepo) {
  const fr = clean(fromRepo);
  const tr = clean(toRepo);
  return fr !== "" && tr !== "" && fr !== tr;
}

/**
 * buildRepoOptions(currentFull, owner, names) → ["owner/name", …]:
 * current first, then the owner's listing in server order, deduped.
 * Bare names gain the owner prefix; already-qualified names pass through
 * (defensive — the listing serves bare strings).
 */
export function buildRepoOptions(currentFull, owner, names) {
  const out = [];
  const seen = new Set();
  const push = (full) => {
    const f = clean(full);
    if (!f || seen.has(f)) return;
    seen.add(f);
    out.push(f);
  };
  push(currentFull);
  for (const n of names ?? []) {
    const s = clean(n);
    if (!s) continue;
    push(s.includes("/") ? s : `${clean(owner)}/${s}`);
  }
  return out;
}

/**
 * withCurrent(list, current) → list with current first, deduped. The
 * selected repo must always be choosable even when a role probe failed.
 */
export function withCurrent(list, current) {
  const c = clean(current);
  const rest = (list ?? []).map(clean).filter((f) => f !== "" && f !== c);
  return c ? [c, ...rest] : rest;
}

/**
 * filterReposByRole(entries, minRole) → repos whose resolved role reaches
 * minRole. entries: [{repo, role}]. To-side callers pass "write" (OpenPR
 * requires base write); From-side callers pass "read" (any non-null role
 * passes — read is the ladder bottom, null/unknown never does).
 */
export function filterReposByRole(entries, minRole) {
  return (entries ?? [])
    .filter((e) => e && roleAtLeast(e.role, minRole))
    .map((e) => e.repo);
}

/**
 * applyEndpointAction(state, action) → the repo→ref state machine: picking
 * a repo re-keys that side (a new repo means a new branch namespace, so a
 * carried-over ref would point nowhere — it resets to ""); re-picking the
 * same repo keeps the ref; picking a ref only sets the ref.
 * state: {repo, ref}; action: {type: "select-repo", repo} |
 * {type: "select-ref", ref}.
 */
export function applyEndpointAction(state, action) {
  const cur = { repo: clean(state?.repo), ref: clean(state?.ref) };
  if (action?.type === "select-repo") {
    const repo = clean(action.repo);
    if (repo === cur.repo) return cur;
    return { repo, ref: "" };
  }
  if (action?.type === "select-ref") {
    return { repo: cur.repo, ref: clean(action.ref) };
  }
  return cur;
}

/**
 * fmtEndpoint(repo, ref, currentFull) → display text: short ref alone on
 * the current repo, `repo@short` across repos (so the cross-repo preview
 * names the fork without leaking `refs/heads/` anywhere).
 */
export function fmtEndpoint(repo, ref, currentFull) {
  const short = toShortRef(clean(ref));
  if (!isCrossRepo(repo, currentFull)) return short;
  return `${clean(repo)}@${short}`;
}

/**
 * openErrorMessage(err) → the inline composer error: the server text
 * direction-mapped (From/To, never base/head or a bare status). The
 * unreachable-head 422 names the fix (push the From branch); the dup-pair
 * 409 points at the pulls list; unknown revisions echo the server detail.
 */
export function openErrorMessage(err) {
  const msg = clean(err?.message ?? err);
  if (/not reachable|push first/i.test(msg)) {
    return "The From branch's commits aren't in the To repo — push the From branch first.";
  }
  if (err?.status === 409 || /already pairs/i.test(msg)) {
    return "An open pull request already compares this From and To — find it in the pulls list.";
  }
  if (err?.status === 404 || /unknown revision|no such ref/i.test(msg)) {
    return `Unknown revision — check the From and To branch names. (${msg})`;
  }
  return `Couldn't open: ${msg || "unknown error"}`;
}

/**
 * previewErrorMessage(side, err) → per-side compare failure: a 404 names
 * the side whose branch is missing (cross-repo previews fetch the From
 * history from the fork's client, so the missing side matters); anything
 * else carries the detail.
 */
export function previewErrorMessage(side, err) {
  const msg = clean(err?.message ?? err);
  if (err?.status === 404) return `No such ${side} branch — check the name.`;
  return `Couldn't compare: ${msg || "unknown error"}`;
}
