// web/src/lib/milestones.js — milestone picker helpers (issue #119).
//
// Pure presentation logic over docs/features/02 §3.2 shapes: milestone
// ids ride the wire as stored (`<id:06x>` hex, no decimal twin), the
// thread carries the id (or null), and PATCH takes `{milestone: <id>}`
// to set or `{milestone: null}` to clear (explicit null — an absent key
// is a no-op). Headless (no Solid, no DOM) so node --test covers it.

/**
 * milestoneTitle(milestones, id) → string|null: the display title for
 * a thread's milestone id. Null stays null (no milestone); an id
 * missing from the repo set (e.g. deleted) renders as the bare id —
 * the same self-heal stance as unknown labels (02 §3.1).
 */
export function milestoneTitle(milestones, id) {
  if (id == null) return null;
  const found = (milestones ?? []).find((m) => m?.id === id);
  return found?.title ?? id;
}

/**
 * milestoneDisplay(milestones, id) → null | {pending, text, unknown?}:
 * the sidebar-chip decision for a thread's milestone id (issue #259).
 * `undefined` means the repo milestone set has not loaded yet — the
 * caller renders a placeholder, never the bare id (no id flash on cold
 * loads). A loaded set missing the id (e.g. deleted) renders the bare
 * id — the same self-heal stance as milestoneTitle. Null stays null.
 */
export function milestoneDisplay(milestones, id) {
  if (id == null) return null;
  if (milestones === undefined) return { pending: true, text: null };
  const found = (milestones ?? []).find((m) => m?.id === id);
  if (found) return { pending: false, text: found.title ?? id };
  return { pending: false, text: id, unknown: true };
}
/**
 * milestonePatch(current, id) → {milestone} | null: the PATCH body to
 * move a thread from `current` to `id` (null clears). Returns null
 * when nothing would change, so callers skip the round trip (and the
 * server would omit the event as a no-op anyway).
 */
export function milestonePatch(current, id) {
  const want = id ?? null;
  if ((current ?? null) === want) return null;
  return { milestone: want };
}
