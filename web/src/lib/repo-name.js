// web/src/lib/repo-name.js — repo-name charset live validation (Forgejo #486).
// Headless-testable: no Solid, no DOM — importable in Node.
//
// validateRepoChars(name) mirrors the NAME half of the server-enforced rule
// (git.ParseRepoId via sdk validateRepoName: [A-Za-z0-9._-], 1–100 chars,
// no leading dot, not "..", optional .git suffix stripped) but returns an
// error ONLY for the invalid-character case: empty stays "" so the live
// region never nags about required — the disabled submit buttons on
// New/Import already cover that. The message states the rule (the
// lib/orgs.js validateOrgName precedent), so the pages need no always-on
// helper span: charset guidance lives entirely in the error, shown live
// only while the entered name is actually invalid.

/** Repo-name segment rule, mirroring git.ParseRepoId (ASCII, 1–100 chars). */
export const REPO_NAME_RE = /^[A-Za-z0-9._-]+$/;

/** Rule text shown only while the entered name is actually invalid. */
export const REPO_NAME_RULE =
  "repository names are letters, digits, and . _ -, 1–100 characters, no leading dot";

/**
 * validateRepoChars(name) → "" when the name is empty (required rides the
 * disabled submit) or valid; otherwise REPO_NAME_RULE. Never throws.
 *
 * @param {string} name the raw name-field value
 * @returns {string} "" or the rule text
 */
export function validateRepoChars(name) {
  const raw = String(name ?? "").trim();
  if (!raw) return "";
  const n = raw.replace(/\.git$/, "");
  if (!n || n === ".." || n.startsWith(".") || n.length > 100 || !REPO_NAME_RE.test(n)) {
    return REPO_NAME_RULE;
  }
  return "";
}
