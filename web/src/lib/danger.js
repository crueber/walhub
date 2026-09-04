// web/src/lib/danger.js — typed-confirm matching for the Danger Zone (issue #39).
//
// The match rule is deliberately trivial and total: the typed text must equal
// the expected `owner/name` EXACTLY (case-sensitive, no trimming — a pasted
// name with stray whitespace must not arm the button). All DOM (input,
// disabled button, busy guard) lives in the component; this module is the
// headless-testable rule so `node --test` covers it without a DOM.

/** Exact-match gate for a danger-zone confirm input. */
export function dangerMatches(typed, expected) {
  if (typeof typed !== "string" || typeof expected !== "string") return false;
  if (expected === "") return false;
  return typed === expected;
}
