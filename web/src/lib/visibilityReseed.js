/**
 * Reseed-when-clean visibility tracking (Forgejo #394).
 *
 * The Settings visibility select was write-once-seeded: it copied the
 * shared `access:{full}` entry exactly once (while its own signal was
 * still null) and never followed the entry afterward, so an Access-tab
 * save, another tab's save, or poll revalidation delivering a newer doc
 * left the select showing stale truth next to a correct header badge
 * (proven by devtools: entry body `{"version":7,"visibility":"private"}`
 * beside a select rendering "public"). A second defect made it worse:
 * the select rendered its no-data-yet null state as "public", so
 * "loading" was indistinguishable from "public".
 *
 * The rule, shared by the Settings select and the Access tab (which has
 * the same baseline/dirty infrastructure and the same hole — it seeds
 * once and never follows the live entry):
 * - unseeded (baseline null) = loading — the UI renders a neutral
 *   skeleton/disabled select, never a public-looking default;
 * - first doc seeds BOTH the value and the baseline;
 * - every later doc reseeds BOTH while the form is clean
 *   (value === baseline) — the display follows server truth;
 * - a dirty form (user edits pending) is NEVER clobbered — the user's
 *   value wins until save (rebase) or explicit reload;
 * - `?? "public"` applies ONLY to a genuinely-missing visibility on a
 *   PRESENT doc, never to no-data-yet (null/undefined doc → null).
 *
 * Pure and headless (no Solid import): components map their signals
 * through `reseed` inside a `createEffect` over the shared entry and
 * only set signals when a field actually changes, so the effect
 * settles instead of looping. Covered by
 * `web/test/unit/visibility-reseed.test.js`.
 */

/** Strict default comparator for tracked values. */
export function sameValue(a, b) {
  return a === b;
}

/**
 * docVisibility(doc) → the select spelling for a shared-entry doc:
 * null when there is no doc yet (loading — the caller must NOT render
 * "public"), `doc.visibility ?? "public"` for a present doc (the ONLY
 * place the public default applies).
 */
export function docVisibility(doc) {
  if (doc === undefined || doc === null) return null;
  return doc.visibility ?? "public";
}

/** createTrack() → an unseeded `{ value, base }` pair (loading state). */
export function createTrack() {
  return { value: null, base: null };
}

/** isSeeded(t) → the baseline has arrived (the form is showing truth, not loading). */
export function isSeeded(t) {
  return t.base !== null && t.base !== undefined;
}

/**
 * isDirty(t, equal?) → seeded AND the value differs from the baseline
 * (the "unsaved changes" marker predicate). Unseeded is never dirty —
 * loading is not an edit.
 */
export function isDirty(t, equal = sameValue) {
  if (!isSeeded(t)) return false;
  return !equal(t.value, t.base);
}

/**
 * reseed(t, next, equal?) → the next tracked pair for a freshly
 * delivered value (`next` already extracted, e.g. via docVisibility):
 * - next null/undefined (no data yet) → unchanged;
 * - unseeded → seed BOTH value and baseline (first truth);
 * - clean (value === baseline) → reseed BOTH (follow truth);
 * - dirty → return t UNCHANGED (same reference — never clobber edits).
 */
export function reseed(t, next, equal = sameValue) {
  if (next === null || next === undefined) return t;
  if (!isSeeded(t)) return { value: next, base: next };
  if (equal(t.value, t.base)) return { value: next, base: next };
  return t;
}

/**
 * rebase(t, value) → adopt a saved/confirmed value as the new baseline
 * (call on successful save and on explicit reload/reseed-from-truth so
 * the form settles back to clean at the confirmed value).
 */
export function rebase(t, value) {
  return { value, base: value };
}
