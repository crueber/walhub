// web/src/lib/labels.js — label toggle + color-lookup helpers (issue #45).
//
// Pure presentation logic over docs/features/02 §3.1 shapes: label names
// are immutable identities (unique case-insensitively), thread.labels is
// a sorted unique string array, and a deleted label still renders as the
// bare string (no header rewriting). Headless (no Solid, no DOM) so
// node --test covers it.

/**
 * toggleLabel(applied, name) → string[]: the label set after toggling
 * `name`. Removes case-insensitively (names are unique
 * case-insensitively per 02 §3.1) preserving stored spellings, adds with
 * the given spelling; result is sorted, unique — the PATCH contract.
 */
export function toggleLabel(applied, name) {
  const want = String(name ?? "");
  const lower = want.toLowerCase();
  const rest = (applied ?? []).filter((l) => String(l).toLowerCase() !== lower);
  if (rest.length !== (applied ?? []).length) return [...rest].sort();
  return [...rest, want].sort();
}

/**
 * labelColorMap(labels) → Map: lowercase name → 6-hex color. Lookup is
 * case-insensitive because names are unique case-insensitively while
 * thread headers keep the stored spelling (02 §3.1).
 */
export function labelColorMap(labels) {
  const map = new Map();
  for (const l of labels ?? []) {
    if (l?.name) map.set(String(l.name).toLowerCase(), l.color);
  }
  return map;
}

/**
 * labelColor(map, name) → 6-hex color or null. Null = unknown label
 * (e.g. deleted after application) — callers render the bare string,
 * never a broken swatch (02 §3.1 self-heal rendering).
 */
export function labelColor(map, name) {
  return map?.get(String(name ?? "").toLowerCase()) ?? null;
}
