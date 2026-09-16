// web/src/lib/pushmirror.js — push-mirror display + form rules (Forgejo #623).
//
// Pure module (no Solid, no DOM): schedule presets (off + the five server
// names), auth kinds, next-sync phrasing, the write-only secret hint
// display, and the create/update validators. All DOM lives in the pages;
// this module is the headless-testable rule so `node --test` covers it
// without a DOM, the same split mirror.js uses.

/** Schedule presets in display order ("" = off/on-push only, the default). */
export const PUSHMIRROR_PRESETS = [
  { id: "", label: "On push only (no schedule)" },
  { id: "hourly", label: "Every hour" },
  { id: "8h", label: "Every 8 hours" },
  { id: "daily", label: "Every day" },
  { id: "weekly", label: "Every week" },
  { id: "monthly", label: "Every month" },
];

/** Default schedule (on-push only — scheduled sync is opt-in). */
export const DEFAULT_PUSHMIRROR_SCHEDULE = "";

/** Auth kinds in display order (server-accepted names). */
export const PUSHMIRROR_AUTH_KINDS = [
  { id: "none", label: "No auth (public upstream / file backup)" },
  { id: "password", label: "Username + password (HTTPS)" },
  { id: "token", label: "Access token (HTTPS)" },
  { id: "ssh", label: "SSH deploy key" },
];

/** True iff the schedule is one the server accepts ("" = off). */
export function isPushMirrorSchedule(id) {
  return PUSHMIRROR_PRESETS.some((p) => p.id === id);
}

/** True iff the auth kind is one the server accepts. */
export function isPushMirrorAuthKind(id) {
  return PUSHMIRROR_AUTH_KINDS.some((p) => p.id === id);
}

/** Display label for a schedule id (the id itself when unknown). */
export function pushMirrorScheduleLabel(id) {
  return PUSHMIRROR_PRESETS.find((p) => p.id === id)?.label ?? String(id ?? "");
}

/** Display label for an auth kind (the id itself when unknown). */
export function pushMirrorAuthLabel(id) {
  return PUSHMIRROR_AUTH_KINDS.find((p) => p.id === id)?.label ?? String(id ?? "");
}

/**
 * Phrase the next sync for the settings line / badge tooltip.
 * Never throws: missing/invalid input degrades to a plain statement.
 * OFF ("") renders as on-push only (never a fire time).
 */
export function formatPushNextSync(mirror, nowMs = Date.now()) {
  if (!mirror) return "";
  if (!mirror.schedule) return "on push only";
  if (mirror.due || !mirror.next_sync_at) {
    return mirror.last_synced_at ? "sync due" : "never synced";
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

/** One-line outcome for the settings tab + badge title. Never throws. */
export function formatPushLastResult(mirror) {
  if (!mirror) return "no push mirror";
  if (!mirror.last_result) return mirror.last_synced_at ? "ok" : "never synced";
  return String(mirror.last_result);
}

/**
 * Validate the create form. Returns {} when valid, else {error}.
 * Never throws.
 */
export function validatePushMirrorCreate({ upstreamUrl, authKind, schedule }) {
  if (!String(upstreamUrl ?? "").trim()) return { error: "upstream URL is required" };
  if (!isPushMirrorAuthKind(authKind ?? "none")) return { error: "pick an auth method" };
  if (!isPushMirrorSchedule(schedule ?? "")) return { error: "pick a schedule" };
  return {};
}

/**
 * Which credential fields the form must show for an auth kind.
 * Secrets are write-only: the form never prefills them from the server
 * (the view carries presence + last-4 hint only).
 */
export function credentialFieldsFor(authKind) {
  switch (authKind) {
    case "password":
      return ["username", "password"];
    case "token":
      return ["username", "token"];
    case "ssh":
      return ["ssh_private_key", "ssh_public_key", "ssh_known_hosts"];
    default:
      return [];
  }
}
