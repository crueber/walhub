// web/src/lib/settingsNav.js — settings sidebar nav model (issue #123).
//
// Pure module (no Solid, no DOM): the sidebar groups, the tab ids, and the
// hash helpers. All DOM (nav, buttons, aria-current) lives in Settings.jsx;
// this module is the headless-testable rule so `node --test` covers it
// without a DOM, the same split danger.js uses for the typed-confirm gate.

/** Standard settings entries, in sidebar order. WAL moved here from the
 *  main repo tab bar (issue #123): it renders inline as a settings section
 *  and the top-level WAL tab is gone. */
export const SETTINGS_GROUP = [
  { id: "scheduled", label: "Scheduled tasks" },
  { id: "policy", label: "Push policy" },
  { id: "config", label: "Effective config & history" },
  { id: "access", label: "Access" },
  { id: "tokens", label: "CI tokens" },
  { id: "webhooks", label: "Webhooks" },
  { id: "wal", label: "WAL" },
];

/** Danger-zone entries: their own menu section with danger styling, never
 *  mixed into the standard listing. */
export const DANGER_GROUP = [{ id: "danger", label: "Danger Zone" }];

/** Every selectable sidebar entry, sidebar order. */
export const SETTINGS_TABS = [...SETTINGS_GROUP, ...DANGER_GROUP];

/** First tab shown on a bare /settings visit (no hash). */
export const DEFAULT_SETTINGS_TAB = "scheduled";

/**
 * Map a tab id to itself when it names a sidebar entry, else null.
 * Non-strings and unknown ids never resolve — the shell falls back to
 * DEFAULT_SETTINGS_TAB so a stale #hash can never blank the page.
 */
export function resolveSettingsTab(id) {
  if (typeof id !== "string" || id === "") return null;
  return SETTINGS_TABS.some((t) => t.id === id) ? id : null;
}

/**
 * Read a tab id out of a URL hash (`#wal` → `wal`, `#danger` → `danger`).
 * Returns null for empty, missing, or unknown hashes — the caller keeps
 * the current tab instead of navigating.
 */
export function settingsTabIdFromHash(hash) {
  if (typeof hash !== "string" || !hash.startsWith("#")) return null;
  return resolveSettingsTab(hash.slice(1));
}
