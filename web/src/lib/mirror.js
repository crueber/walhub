// web/src/lib/mirror.js — mirror display + form rules (Forgejo #240).
//
// Pure module (no Solid, no DOM): schedule presets, next-sync phrasing,
// badge text, and the create-form validator. All DOM lives in the pages;
// this module is the headless-testable rule so `node --test` covers it
// without a DOM, the same split settingsNav.js uses.

/** Schedule presets in display order (server-accepted names; no freeform cron). */
export const MIRROR_PRESETS = [
  { id: "hourly", label: "Every hour" },
  { id: "8h", label: "Every 8 hours" },
  { id: "daily", label: "Every day" },
  { id: "weekly", label: "Every week" },
  { id: "monthly", label: "Every month" },
];

/** Default preset (server creation default). */
export const DEFAULT_MIRROR_PRESET = "daily";

/** True iff the preset is one the server accepts. */
export function isMirrorPreset(id) {
  return MIRROR_PRESETS.some((p) => p.id === id);
}

/** Display label for a preset id (the id itself when unknown). */
export function mirrorPresetLabel(id) {
  return MIRROR_PRESETS.find((p) => p.id === id)?.label ?? String(id ?? "");
}

/**
 * Phrase the next sync for the header badge tooltip / settings line.
 * Never throws: missing/invalid input degrades to a plain statement.
 *
 * @param {object|null} mirror the summary `mirror` view (or null)
 * @param {number} [nowMs] override clock (tests)
 * @returns {string} e.g. "next sync in 3h", "sync due", "never synced"
 */
export function formatNextSync(mirror, nowMs = Date.now()) {
  if (!mirror) return "";
  if (mirror.due || !mirror.next_sync_at) {
    return mirror.last_synced_at ? "sync due" : "never synced — first sync pending";
  }
  const at = Date.parse(mirror.next_sync_at);
  if (!Number.isFinite(at)) return "next sync unknown";
  const ms = at - nowMs;
  if (ms <= 0) return "sync due";
  const mins = Math.floor(ms / 60000);
  if (mins < 1) return "next sync < 1m";
  if (mins < 60) return `next sync in ${mins}m`;
  const hours = Math.floor(mins / 60);
  if (hours < 48) return `next sync in ${hours}h`;
  const days = Math.floor(hours / 24);
  return `next sync in ${days}d`;
}

/**
 * One-line outcome for the settings tab + badge title.
 * Never throws.
 */
export function formatLastResult(mirror) {
  if (!mirror) return "not a mirror";
  if (!mirror.last_result) return mirror.last_synced_at ? "ok" : "never synced";
  return String(mirror.last_result);
}

/**
 * Validate the mirror create form (client-side only; the server
 * re-validates everything). Never throws.
 *
 * @returns {{error?: string}} error names the first problem
 */
export function validateMirrorCreate({ sourceUrl, owner, name, schedule }) {
  if (!String(sourceUrl ?? "").trim()) return { error: "source URL is required" };
  if (!String(owner ?? "").trim() || !String(name ?? "").trim()) return { error: "owner and name are required" };
  if (!isMirrorPreset(schedule)) return { error: "pick a schedule preset" };
  return {};
}

/**
 * Listing-row badge rule (Forgejo #281): the detailed owner listing
 * carries only the mirror flag (+ the upstream URL when the sidecar
 * parses) — never a per-row summary — so <RepoRow> turns the flag
 * into badge text through this pure helper. Headless-testable (no
 * Solid, no DOM); the pages stay thin.
 *
 * @param {{mirror?: boolean, upstream?: string}|null} row the listing row's mirror fields
 * @returns {{show: boolean, label: string, title: string}} title names
 * the upstream when known (the accessible label); label is the chip text
 */
export function mirrorRowBadge(row) {
  if (!row?.mirror) return { show: false, label: "", title: "" };
  const up = String(row.upstream ?? "").trim();
  return {
    show: true,
    label: "mirror",
    title: up ? `mirror of ${up} · pull-only` : "mirror · pull-only",
  };
}
